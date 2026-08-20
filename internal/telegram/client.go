package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	gotdtelegram "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/updates"
	updateshook "github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
	"golang.org/x/net/proxy"

	"github.com/jayden/telegram-video-uploader/internal/buildinfo"
	"github.com/jayden/telegram-video-uploader/internal/model"
)

type Client struct {
	cfg    Config
	events Events

	raw         *gotdtelegram.Client
	peerManager *peers.Manager
	gaps        *updates.Manager
	flood       *floodGate

	mu        sync.RWMutex
	api       *tg.Client
	uploadAPI *tg.Client
	runCtx    context.Context
	identity  Identity
	runErr    error
	running   bool
	started   bool

	ready     chan struct{}
	runDone   chan struct{}
	readyOnce sync.Once
	doneOnce  sync.Once

	// requestMu protects the admission gate for API requests. A request is
	// counted while holding requestMu before Run is allowed to wait on
	// requestWG; this makes the stop path immune to an Add/Wait race.
	requestMu         sync.Mutex
	requestCtx        context.Context
	requestCancel     context.CancelFunc
	requestWG         sync.WaitGroup
	requestStopDone   chan struct{}
	acceptingRequests bool

	bindingMu         sync.RWMutex
	bindingCode       string
	bindingGeneration uint64
	// bindingValidationGeneration is the binding generation whose validation
	// task is currently in flight. It prevents repeated Telegram updates for
	// the same code from multiplying validation goroutines.
	bindingValidationGeneration uint64
	binding                     chan BindingEvent

	uploadSem chan struct{}
	// uploadConcurrency is read once for each video. The connection pool is
	// created at the largest supported profile and grows lazily, so changing
	// this value does not require reconnecting Telegram.
	uploadConcurrency int
	sendMu            sync.Mutex
	lastSend          time.Time
}

func NewClient(cfg Config, events Events) (*Client, error) {
	if cfg.AppID <= 0 {
		return nil, errors.New("API ID 必须是正整数")
	}
	if strings.TrimSpace(cfg.APIHash) == "" {
		return nil, errors.New("API Hash 不能为空")
	}
	if _, err := botIDFromToken(cfg.BotToken); err != nil {
		return nil, err
	}
	if cfg.SessionStorage == nil {
		cfg.SessionStorage = &session.StorageMemory{}
	}
	cfg.UploadConcurrency = normalizeUploadConcurrency(cfg.UploadConcurrency)

	c := &Client{
		cfg:               cfg,
		events:            events,
		ready:             make(chan struct{}),
		runDone:           make(chan struct{}),
		binding:           make(chan BindingEvent, 4),
		uploadSem:         make(chan struct{}, 1),
		uploadConcurrency: cfg.UploadConcurrency,
	}
	c.flood = &floodGate{onWait: events.OnFloodWait}

	resolver, err := resolverFor(cfg.Proxy)
	if err != nil {
		return nil, err
	}

	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewChannelMessage(c.onNewChannelMessage)

	var handler gotdtelegram.UpdateHandler = gotdtelegram.UpdateHandlerFunc(func(context.Context, tg.UpdatesClass) error {
		return nil
	})
	opts := gotdtelegram.Options{
		SessionStorage: cfg.SessionStorage,
		Resolver:       resolver,
		Middlewares: []gotdtelegram.Middleware{
			c.flood.middleware(),
			updateshook.UpdateHook(func(ctx context.Context, u tg.UpdatesClass) error {
				return handler.Handle(ctx, u)
			}),
		},
		UpdateHandler: gotdtelegram.UpdateHandlerFunc(func(ctx context.Context, u tg.UpdatesClass) error {
			return handler.Handle(ctx, u)
		}),
		OnConnectionState: func(state gotdtelegram.ConnectionState) {
			if c.events.OnConnectionState == nil {
				return
			}
			switch state {
			case gotdtelegram.ConnectionStateConnecting:
				c.events.OnConnectionState(StateConnecting)
			case gotdtelegram.ConnectionStateReady:
				c.events.OnConnectionState(StateReady)
			case gotdtelegram.ConnectionStateDisconnected:
				c.events.OnConnectionState(StateDisconnected)
			}
		},
		Device: gotdtelegram.DeviceConfig{
			DeviceModel:    "Desktop",
			SystemVersion:  "Windows/macOS",
			AppVersion:     buildinfo.Version,
			SystemLangCode: "zh-Hans",
			LangCode:       "zh-Hans",
		},
	}
	c.raw = gotdtelegram.NewClient(cfg.AppID, cfg.APIHash, opts)
	c.peerManager = peers.Options{}.Build(c.raw.API())
	c.gaps = updates.New(updates.Config{
		Handler:      dispatcher,
		AccessHasher: c.peerManager,
	})
	handler = c.peerManager.UpdateHook(c.gaps)
	return c, nil
}

func resolverFor(cfg *ProxyConfig) (dcs.Resolver, error) {
	if cfg == nil || strings.TrimSpace(cfg.Address) == "" {
		return dcs.Plain(dcs.PlainOptions{}), nil
	}
	address := strings.TrimSpace(cfg.Address)
	if _, _, err := net.SplitHostPort(address); err != nil {
		return nil, fmt.Errorf("SOCKS5 地址必须是 host:port：%w", err)
	}
	var auth *proxy.Auth
	if cfg.Username != "" || cfg.Password != "" {
		auth = &proxy.Auth{User: cfg.Username, Password: cfg.Password}
	}
	dialer, err := proxy.SOCKS5("tcp", address, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("创建 SOCKS5 连接失败：%w", err)
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("当前 SOCKS5 实现不支持可取消连接")
	}
	return dcs.Plain(dcs.PlainOptions{Dial: contextDialer.DialContext}), nil
}

func botIDFromToken(token string) (int64, error) {
	token = strings.TrimSpace(token)
	parts := strings.SplitN(token, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, errors.New("Bot Token 格式无效")
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("Bot Token 中的 Bot ID 无效")
	}
	return id, nil
}

func (c *Client) Run(ctx context.Context) (retErr error) {
	c.mu.Lock()
	// gotd clients and the readiness channels below are one-shot. Reconnecting
	// is supported by constructing a fresh Client with the same encrypted
	// session storage.
	if c.started {
		c.mu.Unlock()
		return ErrAlreadyRunning
	}
	c.started = true
	c.running = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.runErr = retErr
		c.api = nil
		c.uploadAPI = nil
		c.runCtx = nil
		c.running = false
		c.mu.Unlock()
		c.doneOnce.Do(func() { close(c.runDone) })
	}()

	return c.raw.Run(ctx, func(runCtx context.Context) error {
		status, err := c.raw.Auth().Status(runCtx)
		if err != nil {
			return fmt.Errorf("读取 Bot 登录状态失败：%w", err)
		}
		if !status.Authorized {
			if _, err := c.raw.Auth().Bot(runCtx, strings.TrimSpace(c.cfg.BotToken)); err != nil {
				return fmt.Errorf("Bot 登录失败：%w", err)
			}
			status, err = c.raw.Auth().Status(runCtx)
			if err != nil {
				return fmt.Errorf("确认 Bot 登录状态失败：%w", err)
			}
		}
		if status.User == nil || !status.User.Bot {
			return errors.New("当前会话不是 Bot 会话")
		}
		expectedID, _ := botIDFromToken(c.cfg.BotToken)
		if status.User.ID != expectedID {
			return ErrWrongBotSession
		}
		if err := c.peerManager.Init(runCtx); err != nil {
			return fmt.Errorf("初始化频道实体缓存失败：%w", err)
		}

		maxBytes, maxUploadExact := c.queryMaxUploadBytes(runCtx)
		// gotd pools open connections on demand. Keeping the ceiling at the
		// largest supported profile lets a setting change take effect for the
		// next video without eagerly creating twelve connections.
		uploadPool, err := c.raw.Pool(uploadConnectionPoolMaximum)
		if err != nil {
			return fmt.Errorf("创建上传连接池失败：%w", err)
		}
		// The pool owns the connections used by both ValidateChannel and
		// UploadVideo. Stop admitting requests, cancel their contexts, and wait
		// for them to return before closing the pool. This is important when the
		// gotd run loop exits while a large upload is still in flight.
		defer func() {
			c.stopRequestLifecycle()
			_ = uploadPool.Close()
		}()

		username, _ := status.User.GetUsername()
		c.mu.Lock()
		c.api = c.raw.API()
		c.uploadAPI = tg.NewClient(uploadPool)
		c.runCtx = runCtx
		c.identity = Identity{BotID: status.User.ID, Username: username, MaxUploadBytes: maxBytes, MaxUploadExact: maxUploadExact}
		c.mu.Unlock()
		c.startRequestLifecycle(runCtx)

		return c.gaps.Run(runCtx, c.raw.API(), status.User.ID, updates.AuthOptions{
			IsBot:  true,
			Forget: true,
			OnStart: func(context.Context) {
				c.readyOnce.Do(func() { close(c.ready) })
			},
		})
	})
}

// startRequestLifecycle opens the request admission gate for one gotd Run
// callback. The callback owns the underlying upload connection pool, so its
// context is the parent of every admitted API request.
func (c *Client) startRequestLifecycle(runCtx context.Context) {
	if runCtx == nil {
		runCtx = context.Background()
	}
	lifetimeCtx, cancel := context.WithCancel(runCtx)
	c.requestMu.Lock()
	// Client.Run is one-shot, but keep this assignment self-contained so the
	// lifecycle can also be exercised by deterministic unit tests.
	c.requestCtx = lifetimeCtx
	c.requestCancel = cancel
	c.requestStopDone = make(chan struct{})
	c.acceptingRequests = true
	c.requestMu.Unlock()
}

// stopRequestLifecycle closes the admission gate before cancelling and
// waiting for active requests. The gate remains closed after this method
// returns, and the method is safe to call more than once.
func (c *Client) stopRequestLifecycle() {
	c.requestMu.Lock()
	if !c.acceptingRequests && c.requestCancel == nil {
		stopDone := c.requestStopDone
		c.requestMu.Unlock()
		if stopDone != nil {
			<-stopDone
		}
		return
	}
	c.acceptingRequests = false
	c.requestCtx = nil
	cancel := c.requestCancel
	c.requestCancel = nil
	stopDone := c.requestStopDone
	c.requestMu.Unlock()

	if cancel != nil {
		cancel()
	}
	c.requestWG.Wait()
	if stopDone != nil {
		close(stopDone)
	}
}

// beginRequest admits one operation against the current Run callback. The
// returned context is cancelled by either the caller or the Run lifecycle;
// release must be called exactly once (normally via defer).
func (c *Client) beginRequest(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}

	c.requestMu.Lock()
	if !c.acceptingRequests || c.requestCtx == nil {
		c.requestMu.Unlock()
		return nil, nil, ErrNotConnected
	}
	lifetimeCtx := c.requestCtx
	// Add is deliberately performed while requestMu is held. stopRequest-
	// Lifecycle takes the same lock before it calls Wait, so no request can be
	// added after the wait begins.
	c.requestWG.Add(1)
	c.requestMu.Unlock()

	requestCtx, cancel := context.WithCancel(ctx)
	stopCancel := context.AfterFunc(lifetimeCtx, cancel)
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			stopCancel()
			cancel()
			c.requestWG.Done()
		})
	}
	return requestCtx, release, nil
}

func normalizeUploadConcurrency(value int) int {
	switch value {
	case UploadConcurrencyCompatibility, UploadConcurrencyBalanced, UploadConcurrencyFast:
		return value
	default:
		return DefaultUploadConcurrency
	}
}

// SetUploadConcurrency changes the part concurrency used by the next video.
// It returns the normalized supported value that was applied.
func (c *Client) SetUploadConcurrency(value int) int {
	value = normalizeUploadConcurrency(value)
	c.mu.Lock()
	c.uploadConcurrency = value
	c.mu.Unlock()
	return value
}

func (c *Client) currentUploadConcurrency() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return normalizeUploadConcurrency(c.uploadConcurrency)
}

func (c *Client) WaitReady(ctx context.Context) (Identity, error) {
	select {
	case <-c.ready:
		c.mu.RLock()
		defer c.mu.RUnlock()
		if !c.running || c.api == nil || c.uploadAPI == nil {
			if c.runErr != nil {
				return Identity{}, c.runErr
			}
			return Identity{}, ErrNotConnected
		}
		return c.identity, nil
	case <-c.runDone:
		c.mu.RLock()
		defer c.mu.RUnlock()
		if c.runErr == nil {
			return Identity{}, ErrNotConnected
		}
		return Identity{}, c.runErr
	case <-ctx.Done():
		return Identity{}, ctx.Err()
	}
}

func (c *Client) connectedAPI() (*tg.Client, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.api == nil {
		return nil, ErrNotConnected
	}
	return c.api, nil
}

func (c *Client) connectedUploadAPI() (*tg.Client, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.uploadAPI == nil {
		return nil, ErrNotConnected
	}
	return c.uploadAPI, nil
}

func (c *Client) BeginChannelBinding() (string, error) {
	if _, err := c.connectedAPI(); err != nil {
		return "", err
	}
	randomBytes := make([]byte, 10)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("生成频道验证码失败：%w", err)
	}
	code := "TGUP_VERIFY_" + hex.EncodeToString(randomBytes)
	c.bindingMu.Lock()
	c.bindingGeneration++
	c.bindingCode = code
	c.bindingMu.Unlock()
	return code, nil
}

func (c *Client) BindingEvents() <-chan BindingEvent { return c.binding }

func (c *Client) claimBindingValidation(message string) (string, uint64, bool) {
	c.bindingMu.Lock()
	defer c.bindingMu.Unlock()

	code := c.bindingCode
	generation := c.bindingGeneration
	if code == "" || strings.TrimSpace(message) != code || c.bindingValidationGeneration == generation {
		return "", 0, false
	}
	c.bindingValidationGeneration = generation
	return code, generation, true
}

func (c *Client) releaseBindingValidation(generation uint64) {
	c.bindingMu.Lock()
	if c.bindingValidationGeneration == generation {
		c.bindingValidationGeneration = 0
	}
	c.bindingMu.Unlock()
}

func (c *Client) bindingValidationContext() (context.Context, context.CancelFunc, bool) {
	c.mu.RLock()
	runCtx := c.runCtx
	c.mu.RUnlock()
	if runCtx == nil {
		return nil, nil, false
	}
	validationCtx, cancel := context.WithTimeout(runCtx, 20*time.Second)
	return validationCtx, cancel, true
}

func (c *Client) sendBindingEvent(event BindingEvent) {
	select {
	case c.binding <- event:
	case <-c.runDone:
	}
}

func (c *Client) onNewChannelMessage(ctx context.Context, entities tg.Entities, update *tg.UpdateNewChannelMessage) error {
	message, ok := update.Message.(*tg.Message)
	if !ok {
		return nil
	}
	peer, ok := message.PeerID.(*tg.PeerChannel)
	if !ok {
		return nil
	}
	code, generation, claimed := c.claimBindingValidation(message.Message)
	if !claimed {
		return nil
	}

	candidate := model.Channel{ID: peer.ChannelID}
	if channel := entities.Channels[peer.ChannelID]; channel != nil {
		candidate.Title = channel.Title
		if !channel.Min {
			candidate.AccessHash = channel.AccessHash
		}
	}
	if candidate.AccessHash == 0 {
		if resolved, err := c.peerManager.ResolveChannelID(ctx, peer.ChannelID); err == nil {
			candidate.Title = resolved.Raw().Title
			if !resolved.Raw().Min {
				candidate.AccessHash = resolved.Raw().AccessHash
			}
		}
	}

	go func(expected string, expectedGeneration uint64, ch model.Channel) {
		defer c.releaseBindingValidation(expectedGeneration)

		c.bindingMu.RLock()
		current := c.bindingCode == expected && c.bindingGeneration == expectedGeneration
		c.bindingMu.RUnlock()
		if !current {
			return
		}
		validateCtx, cancel, ok := c.bindingValidationContext()
		if !ok {
			return
		}
		defer cancel()
		resolved, err := c.ValidateChannel(validateCtx, ch)
		c.bindingMu.Lock()
		current = c.bindingCode == expected && c.bindingGeneration == expectedGeneration
		if current && err == nil {
			c.bindingCode = ""
		}
		c.bindingMu.Unlock()
		if !current {
			return
		}
		c.sendBindingEvent(BindingEvent{Channel: resolved, Err: err})
	}(code, generation, candidate)
	return nil
}

func (c *Client) ValidateChannel(ctx context.Context, channel model.Channel) (model.Channel, error) {
	requestCtx, release, err := c.beginRequest(ctx)
	if err != nil {
		return model.Channel{}, err
	}
	defer release()
	return c.validateChannel(requestCtx, channel)
}

// validateChannel performs the actual API calls using an already-admitted
// request context. UploadVideo uses this helper so channel validation and
// message upload share one request-lifecycle reference and cannot race pool
// shutdown between two separate admissions.
func (c *Client) validateChannel(ctx context.Context, channel model.Channel) (model.Channel, error) {
	api, err := c.connectedAPI()
	if err != nil {
		return model.Channel{}, err
	}
	hashes := make([]int64, 0, 3)
	appendHash := func(hash int64) {
		for _, existing := range hashes {
			if existing == hash {
				return
			}
		}
		hashes = append(hashes, hash)
	}
	appendHash(channel.AccessHash)
	if resolved, resolveErr := c.peerManager.ResolveChannelID(ctx, channel.ID); resolveErr == nil && !resolved.Raw().Min {
		appendHash(resolved.Raw().AccessHash)
	}
	// Telegram documents a zero-hash fallback for peers known to a Bot. Keep
	// it last, after any full entity captured by the peer manager.
	appendHash(0)

	var lastErr error
	for _, hash := range hashes {
		input := &tg.InputChannel{ChannelID: channel.ID, AccessHash: hash}
		result, getErr := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{input})
		if getErr != nil {
			lastErr = getErr
			continue
		}
		for _, chat := range result.GetChats() {
			resolved, ok := chat.(*tg.Channel)
			if !ok || resolved.ID != channel.ID {
				continue
			}
			if !resolved.Broadcast || resolved.Left {
				return model.Channel{}, errors.New("目标不是 Bot 已加入的 Channel")
			}
			rights, ok := resolved.GetAdminRights()
			if !ok || !rights.PostMessages {
				return model.Channel{}, ErrChannelNotWritable
			}
			return model.Channel{ID: resolved.ID, AccessHash: resolved.AccessHash, Title: resolved.Title}, nil
		}
		lastErr = errors.New("Telegram 没有返回目标频道")
	}
	return model.Channel{}, fmt.Errorf("验证频道失败：%w", lastErr)
}
