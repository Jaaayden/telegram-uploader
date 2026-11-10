package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/session"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/credentials"
	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

// connectionRequest is one complete set of connection parameters.
type connectionRequest struct {
	appID         int
	apiHash       string
	botToken      string
	proxyEnabled  bool
	proxyAddress  string
	proxyUsername string
	proxyPassword string
}

func formatInt(value int64) string { return strconv.FormatInt(value, 10) }

// storedRequestLocked builds a request from the saved settings and the cached
// secrets. The boolean is false while the configuration is incomplete.
func (s *Service) storedRequestLocked() (connectionRequest, bool) {
	request := connectionRequest{
		appID:         s.settings.APIID,
		apiHash:       s.cached.apiHash,
		botToken:      s.cached.botToken,
		proxyEnabled:  s.settings.ProxyEnabled,
		proxyAddress:  s.settings.ProxyAddress,
		proxyUsername: s.settings.ProxyUsername,
		proxyPassword: s.cached.proxyPassword,
	}
	return request, request.appID > 0 && request.apiHash != "" && request.botToken != ""
}

func (s *Service) connectionViewLocked() ConnectionView {
	view := ConnectionView{}
	if !s.floodWaitTill.IsZero() && time.Now().Before(s.floodWaitTill) {
		view.FloodWaitUntil = s.floodWaitTill.UnixMilli()
	}
	_, configured := s.storedRequestLocked()
	switch {
	case s.connected:
		view.Status = "connected"
		view.Bot = s.identity.Username
		view.MaxBytes = s.identity.MaxUploadBytes
		view.MaxExact = s.identity.MaxUploadExact
	case s.connecting:
		view.Status = "connecting"
	case s.configErr != nil:
		view.Status = "error"
		view.Message = s.configErr.Error()
	case s.retryTimer != nil:
		view.Status = "retrying"
		view.Message = "Telegram 暂时连不上，会自动重试"
		view.RetryAt = s.retryAt.UnixMilli()
	case !configured && s.applied == nil:
		view.Status = "unconfigured"
	default:
		view.Status = "disconnected"
	}
	return view
}

// autoConnect connects at startup when a complete configuration is stored.
func (s *Service) autoConnect() {
	s.mu.Lock()
	blocked := s.settingsLoadErr != nil || s.credentialErr != nil
	_, configured := s.storedRequestLocked()
	s.mu.Unlock()
	if blocked || !configured {
		s.invalidate()
		return
	}
	s.ensureConnection()
}

// ensureConnection starts an automatic connection attempt unless one is
// running, the configuration is known to be wrong, or nothing is configured.
func (s *Service) ensureConnection() {
	s.mu.Lock()
	if s.closed || s.client != nil || s.connecting || s.configErr != nil {
		s.mu.Unlock()
		return
	}
	var request connectionRequest
	if s.applied != nil {
		request = *s.applied
	} else {
		stored, ok := s.storedRequestLocked()
		if !ok {
			s.mu.Unlock()
			s.invalidate()
			return
		}
		request = stored
	}
	s.startAttemptLocked(request, true)
	s.mu.Unlock()
	s.invalidate()
}

// ConnectionInput is the settings form. A nil secret keeps the stored value;
// an empty proxy password clears it.
type ConnectionInput struct {
	APIID         int     `json:"apiId"`
	APIHash       *string `json:"apiHash"`
	BotToken      *string `json:"botToken"`
	ProxyEnabled  bool    `json:"proxyEnabled"`
	ProxyAddress  string  `json:"proxyAddress"`
	ProxyUsername string  `json:"proxyUsername"`
	ProxyPassword *string `json:"proxyPassword"`
}

// SaveConnection validates and stores the connection settings, then connects
// (replacing a running client).
func (s *Service) SaveConnection(input ConnectionInput) error {
	if s.controller.Snapshot().Running {
		return errors.New("上传进行中不能更换 Telegram 凭据；并发和限速可以随时调整")
	}
	s.mu.Lock()
	request := connectionRequest{
		appID:         input.APIID,
		apiHash:       s.cached.apiHash,
		botToken:      s.cached.botToken,
		proxyEnabled:  input.ProxyEnabled,
		proxyAddress:  strings.TrimSpace(input.ProxyAddress),
		proxyUsername: strings.TrimSpace(input.ProxyUsername),
		proxyPassword: s.cached.proxyPassword,
	}
	previous := s.cached
	s.mu.Unlock()
	if input.APIHash != nil && strings.TrimSpace(*input.APIHash) != "" {
		request.apiHash = strings.TrimSpace(*input.APIHash)
	}
	if input.BotToken != nil && strings.TrimSpace(*input.BotToken) != "" {
		request.botToken = strings.TrimSpace(*input.BotToken)
	}
	if input.ProxyPassword != nil {
		request.proxyPassword = *input.ProxyPassword
	}
	if request.appID <= 0 {
		return errors.New("API ID 必须是正整数")
	}
	if request.apiHash == "" || request.botToken == "" {
		return errors.New("API Hash 和 Bot Token 不能为空")
	}
	if request.proxyEnabled && request.proxyAddress == "" {
		return errors.New("已启用 SOCKS5 代理，但代理地址为空")
	}

	if err := s.updateSettings(func(settings *coreapp.Settings) {
		settings.APIID = request.appID
		settings.ProxyEnabled = request.proxyEnabled
		settings.ProxyAddress = request.proxyAddress
		settings.ProxyUsername = request.proxyUsername
	}); err != nil {
		return err
	}
	// Write only the secrets that changed: every keychain write can prompt.
	if request.apiHash != previous.apiHash {
		if err := s.secrets.SetAPIHash(request.apiHash); err != nil {
			return err
		}
	}
	if request.botToken != previous.botToken {
		if err := s.secrets.SetBotToken(request.botToken); err != nil {
			return err
		}
	}
	if request.proxyPassword != previous.proxyPassword {
		if request.proxyPassword == "" {
			if err := s.secrets.DeleteProxyPassword(); err != nil && !errors.Is(err, credentials.ErrNotFound) {
				return err
			}
		} else if err := s.secrets.SetProxyPassword(request.proxyPassword); err != nil {
			return err
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.cached = cachedSecrets{apiHash: request.apiHash, botToken: request.botToken, proxyPassword: request.proxyPassword}
	s.credentialErr = nil
	s.configErr = nil
	s.applied = &request
	s.resetRetryLocked()
	if s.client != nil {
		// Replace the running client: its Run loop starts the pending request
		// once the old connection has fully stopped.
		pending := request
		s.pending = &pending
		s.connecting = true
		s.connected = false
		s.identity = tgtransport.Identity{}
		cancel := s.clientCancel
		s.mu.Unlock()
		s.controller.SetGateway(nil)
		if cancel != nil {
			cancel()
		}
	} else {
		s.startAttemptLocked(request, false)
		s.mu.Unlock()
	}
	s.invalidate()
	return nil
}

// Reconnect retries with the stored configuration after a failure.
func (s *Service) Reconnect() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if s.client != nil || s.connecting {
		s.mu.Unlock()
		return nil
	}
	request, ok := s.storedRequestLocked()
	if s.applied != nil {
		request, ok = *s.applied, true
	}
	if !ok {
		s.mu.Unlock()
		return errors.New("请先填写并保存 Telegram 凭据")
	}
	s.configErr = nil
	s.resetRetryLocked()
	s.startAttemptLocked(request, false)
	s.mu.Unlock()
	s.invalidate()
	return nil
}

// startAttemptLocked creates a client and its three watcher goroutines. The
// transport is published while the lock is held, so a concurrent shutdown
// cannot clear the gateway and then be overwritten by a stale attempt.
func (s *Service) startAttemptLocked(request connectionRequest, automatic bool) {
	if s.closed {
		return
	}
	s.generation++
	generation := s.generation
	s.stopRetryTimerLocked()
	if s.applied == nil {
		applied := request
		s.applied = &applied
	}
	_ = diagnostics.Logf("telegram_connect_requested generation=%d automatic=%t proxy=%t", generation, automatic, request.proxyEnabled)

	var proxy *tgtransport.ProxyConfig
	if request.proxyEnabled && request.proxyAddress != "" {
		proxy = &tgtransport.ProxyConfig{Address: request.proxyAddress, Username: request.proxyUsername, Password: request.proxyPassword}
	}
	client, err := s.newClient(tgtransport.Config{
		AppID:             request.appID,
		APIHash:           request.apiHash,
		BotToken:          request.botToken,
		Proxy:             proxy,
		UploadConcurrency: s.settings.UploadConcurrency,
		RateLimiter:       s.limiter,
		SessionStorage:    s.sessionStorage(),
	}, tgtransport.Events{
		OnConnectionState: func(state tgtransport.ConnectionState) { s.handleConnectionState(generation, state) },
		OnFloodWait:       func(wait time.Duration) { s.handleFloodWait(generation, wait) },
	})
	if err != nil {
		// Invalid parameters cannot be fixed by retrying.
		s.connecting = false
		s.connected = false
		s.configErr = err
		_ = diagnostics.Logf("telegram_connect_failed generation=%d error_type=%T", generation, err)
		return
	}
	ctx, cancel := context.WithCancel(s.rootCtx)
	s.client = client
	s.clientCancel = cancel
	s.connecting = true
	s.connected = false
	s.controller.SetGateway(client)

	s.workers.Add(3)
	go func() { defer s.workers.Done(); s.runClient(client, ctx, generation) }()
	go func() { defer s.workers.Done(); s.waitClientReady(client, ctx, generation) }()
	go func() { defer s.workers.Done(); s.observeBinding(client, ctx, generation) }()
}

// sessionStorage returns the encrypted on-disk session, or a nil interface
// (an in-memory session) for the fake secret stores of tests and the demo.
func (s *Service) sessionStorage() session.Storage {
	store, ok := s.secrets.(*credentials.Store)
	if !ok {
		return nil
	}
	return credentials.NewSessionStorage(store, s.paths.Session)
}

func (s *Service) isCurrentLocked(client Client, generation uint64) bool {
	return !s.closed && s.client == client && s.generation == generation && s.pending == nil
}

func (s *Service) handleConnectionState(generation uint64, state tgtransport.ConnectionState) {
	_ = diagnostics.Logf("telegram_connection_state generation=%d state=%s", generation, state)
	s.mu.Lock()
	if s.closed || s.generation != generation || s.client == nil || s.pending != nil {
		s.mu.Unlock()
		return
	}
	ready := false
	switch state {
	case tgtransport.StateConnecting, tgtransport.StateDisconnected:
		// gotd reconnects by itself; show it as connecting until it is back.
		s.connected = false
		s.connecting = true
	case tgtransport.StateReady:
		if s.identity.Username != "" {
			s.connected = true
			s.connecting = false
			s.resetRetryLocked()
			ready = true
		}
	}
	s.mu.Unlock()
	s.invalidate()
	if ready {
		s.scheduler.RetryDue()
	}
}

func (s *Service) handleFloodWait(generation uint64, wait time.Duration) {
	s.mu.Lock()
	if s.closed || s.generation != generation {
		s.mu.Unlock()
		return
	}
	s.floodWaitTill = time.Now().Add(wait)
	s.mu.Unlock()
	s.invalidate()
}

func (s *Service) runClient(client Client, ctx context.Context, generation uint64) {
	err := client.Run(ctx)
	if errors.Is(err, tgtransport.ErrWrongBotSession) {
		if removeErr := os.Remove(s.paths.Session); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("清理旧 Bot 会话失败：%w", removeErr))
		} else {
			err = fmt.Errorf("%w；旧的加密 Bot 会话已清理，请重新连接", err)
		}
	}

	s.mu.Lock()
	if s.client != client || s.generation != generation {
		// A shutdown or a newer attempt already took over.
		s.mu.Unlock()
		return
	}
	if s.clientCancel != nil {
		// Also stops the readiness and binding observers of a failed Run.
		s.clientCancel()
	}
	s.clientCancel = nil
	s.client = nil
	s.connected = false
	s.identity = tgtransport.Identity{}
	pending := s.pending
	s.pending = nil
	closed := s.closed
	// The controller's retry loop must never reuse a finished one-shot client.
	s.controller.SetGateway(nil)

	retryable := shouldRetryConnection(err, ctx.Err(), closed)
	_ = diagnostics.Logf(
		"telegram_client_stopped generation=%d retryable=%t context_cancelled=%t replaced=%t error_type=%T",
		generation, retryable, ctx.Err() != nil, pending != nil, err,
	)
	var failure error
	switch {
	case closed:
		s.connecting = false
	case pending != nil:
		s.startAttemptLocked(*pending, false)
	case retryable:
		s.connecting = false
		s.configErr = nil
		s.scheduleRetryLocked()
	case err != nil && !errors.Is(err, context.Canceled):
		s.connecting = false
		s.configErr = err
		s.resetRetryLocked()
		failure = err
	default:
		s.connecting = false
		s.resetRetryLocked()
	}
	s.mu.Unlock()
	if failure != nil {
		s.toast("error", "Telegram 连接失败："+failure.Error())
	}
	s.invalidate()
}

func (s *Service) waitClientReady(client Client, ctx context.Context, generation uint64) {
	identity, err := client.WaitReady(ctx)
	if err != nil {
		// runClient owns cleanup and retry scheduling.
		return
	}
	s.mu.Lock()
	if !s.isCurrentLocked(client, generation) {
		s.mu.Unlock()
		return
	}
	s.connecting = false
	s.connected = true
	s.configErr = nil
	s.identity = identity
	s.resetRetryLocked()
	hasJobs := len(s.snapshot.Jobs) > 0
	s.mu.Unlock()
	_ = diagnostics.Logf("telegram_client_ready generation=%d max_upload_bytes=%d", generation, identity.MaxUploadBytes)
	s.invalidate()
	s.scheduler.RetryDue()

	// Entries may have been queued while Telegram was disconnected. Apply the
	// server limit in place; the queue can span several folders, so it is never
	// rebuilt from a rescan.
	if hasJobs || len(s.controller.Snapshot().Jobs) > 0 {
		if err := s.controller.ApplyUploadLimit(identity.MaxUploadBytes); err != nil {
			s.toastError(err)
		}
	}
}

func (s *Service) observeBinding(client Client, ctx context.Context, generation uint64) {
	for {
		select {
		case event, ok := <-client.BindingEvents():
			if !ok {
				return
			}
			s.mu.Lock()
			current := s.isCurrentLocked(client, generation)
			s.mu.Unlock()
			if !current {
				continue
			}
			if event.Err != nil {
				s.toastError(event.Err)
				continue
			}
			if err := s.controller.SetChannel(event.Channel); err != nil {
				s.toastError(err)
				continue
			}
			s.emit("channel-bound", ChannelView{ID: event.Channel.ID, Title: channelTitle(event.Channel)})
			s.invalidate()
			s.scheduler.RetryDue()
		case <-ctx.Done():
			return
		}
	}
}

// BeginBinding returns the one-time code the user posts in the channel.
func (s *Service) BeginBinding() (string, error) {
	s.mu.Lock()
	client := s.client
	connected := s.connected
	s.mu.Unlock()
	if client == nil || !connected {
		return "", errors.New("请先连接 Telegram Bot")
	}
	return client.BeginChannelBinding()
}

func (s *Service) scheduleRetryLocked() {
	if s.closed || s.connected || s.retryTimer != nil {
		return
	}
	s.retryDelay = nextConnectionRetryDelay(s.retryDelay)
	s.retryAt = time.Now().Add(s.retryDelay)
	var timer *time.Timer
	timer = time.AfterFunc(s.retryDelay, func() {
		s.mu.Lock()
		if s.retryTimer == timer {
			s.retryTimer = nil
		}
		s.mu.Unlock()
		s.ensureConnection()
	})
	s.retryTimer = timer
}

func (s *Service) stopRetryTimerLocked() {
	if s.retryTimer != nil {
		s.retryTimer.Stop()
		s.retryTimer = nil
	}
}

func (s *Service) resetRetryLocked() {
	s.retryDelay = 0
	s.stopRetryTimerLocked()
}

func nextConnectionRetryDelay(previous time.Duration) time.Duration {
	if previous < connectionRetryInitial {
		return connectionRetryInitial
	}
	return min(previous*2, connectionRetryMaximum)
}

func isTransientConnectionError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, tgtransport.ErrWrongBotSession) {
		return false
	}
	if errors.Is(err, tgtransport.ErrNotConnected) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func shouldRetryConnection(runErr, contextErr error, closed bool) bool {
	if closed {
		return false
	}
	if runErr == nil {
		return contextErr == nil
	}
	return isTransientConnectionError(runErr)
}

// SetConcurrency changes the part concurrency from the next video on.
func (s *Service) SetConcurrency(value int) (int, error) {
	switch value {
	case tgtransport.UploadConcurrencyCompatibility, tgtransport.UploadConcurrencyBalanced, tgtransport.UploadConcurrencyFast:
	default:
		return 0, errors.New("不支持的并发档位")
	}
	if err := s.updateSettings(func(settings *coreapp.Settings) { settings.UploadConcurrency = value }); err != nil {
		return 0, err
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil {
		value = client.SetUploadConcurrency(value)
	}
	s.invalidate()
	return value, nil
}

// SetUploadLimit changes the rate limit in KiB/s (zero removes it). It takes
// effect immediately, including for the video that is uploading.
func (s *Service) SetUploadLimit(kbps int) error {
	if kbps < 0 || kbps > coreapp.MaxUploadLimitKBps {
		return errors.New("限速值超出范围")
	}
	if err := s.updateSettings(func(settings *coreapp.Settings) { settings.UploadLimitKBps = kbps }); err != nil {
		return err
	}
	s.limiter.SetLimit(limitBytes(kbps))
	s.invalidate()
	return nil
}
