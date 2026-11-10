// Package service is the application layer between the upload engine and the
// user interface. It owns everything a front end needs — the Telegram
// connection lifecycle, queue operations, scheduling, rate limiting and
// updates — and exposes it as JSON requests (Handle) and events (Emit), so the
// interface itself contains no business rules and can be replaced or driven
// from tests.
package service

import (
	"context"
	"errors"
	"sync"
	"time"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/credentials"
	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/platform"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
	"github.com/Jaaayden/telegram-uploader/internal/update"
)

const (
	connectionRetryInitial = 5 * time.Second
	connectionRetryMaximum = time.Minute
	stopTimeout            = 5 * time.Second
	// stateCoalesce merges bursts of small changes into one state event.
	stateCoalesce = 40 * time.Millisecond
	// updateCheckInterval is how often a long-running app looks for a release.
	updateCheckInterval = 24 * time.Hour
)

// Secrets is the part of the credential store the service needs.
type Secrets interface {
	GetBotToken() (string, error)
	SetBotToken(string) error
	GetAPIHash() (string, error)
	SetAPIHash(string) error
	GetProxyPassword() (string, error)
	SetProxyPassword(string) error
	DeleteProxyPassword() error
}

// Client is a Telegram transport whose part concurrency can be changed while
// it is connected.
type Client interface {
	tgtransport.Gateway
	SetUploadConcurrency(int) int
}

// Host provides the few things that need a native window system.
type Host interface {
	// PickFolder shows a folder chooser. It returns "" when cancelled.
	PickFolder(title string) (string, error)
	// OpenPath reveals a local directory in the file manager.
	OpenPath(path string) error
	// OpenURL opens a web page in the default browser.
	OpenURL(url string) error
	// Quit closes the window and ends the event loop.
	Quit()
}

// Emitter delivers an event to the front end. payload is JSON-encodable.
type Emitter func(event string, payload any)

// Options configures a Service. Controller, Paths, Secrets and Host are
// required; the rest have production defaults.
type Options struct {
	Controller *coreapp.Controller
	Paths      coreapp.Paths
	Secrets    Secrets
	Host       Host
	// NewClient builds the Telegram transport. Tests and the demo mode replace
	// it with an in-memory implementation.
	NewClient func(tgtransport.Config, tgtransport.Events) (Client, error)
	// Updater enables in-app updates; nil hides the feature.
	Updater *update.Updater
	// Notices are startup problems to show once the interface is ready.
	Notices []string
	// PreventSleep keeps the machine awake during uploads.
	PreventSleep func() (platform.SleepGuard, error)
}

// Service is safe for concurrent use.
type Service struct {
	controller *coreapp.Controller
	paths      coreapp.Paths
	secrets    Secrets
	host       Host
	newClient  func(tgtransport.Config, tgtransport.Events) (Client, error)
	updater    *update.Updater
	keepAwake  func() (platform.SleepGuard, error)
	limiter    *tgtransport.RateLimiter
	scheduler  *scheduleCoordinator

	rootCtx    context.Context
	rootCancel context.CancelFunc
	workers    sync.WaitGroup

	emitMu  sync.Mutex
	emitter Emitter
	dirty   chan struct{}

	// scheduleStartMu serializes scheduled-start attempts.
	scheduleStartMu sync.Mutex
	// settingsMu serializes read-modify-write cycles of the settings file.
	settingsMu sync.Mutex

	mu sync.Mutex
	// All fields below are guarded by mu.
	closed          bool
	settings        coreapp.Settings
	settingsLoadErr error
	credentialErr   error
	notices         []string

	cached cachedSecrets

	client        Client
	clientCancel  context.CancelFunc
	generation    uint64
	connecting    bool
	connected     bool
	identity      tgtransport.Identity
	configErr     error
	applied       *connectionRequest
	pending       *connectionRequest
	retryDelay    time.Duration
	retryTimer    *time.Timer
	retryAt       time.Time
	floodWaitTill time.Time

	snapshot    coreapp.Snapshot
	stats       queueStats
	jobsRev     uint64
	sentJobs    map[string]JobView
	sentOrder   []string
	frontReady  bool
	scan        *scanResult
	scanning    bool
	scanCounter uint64

	moveActive bool
	moveCancel context.CancelFunc
	moveDone   int64
	moveTotal  int64

	sleepGuard platform.SleepGuard

	update updateState
}

type cachedSecrets struct {
	apiHash       string
	botToken      string
	proxyPassword string
}

// New creates a service. Call Start once the emitter is attached.
func New(options Options) (*Service, error) {
	if options.Controller == nil || options.Secrets == nil || options.Host == nil {
		return nil, errors.New("service: controller, secrets and host are required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		controller: options.Controller,
		paths:      options.Paths,
		secrets:    options.Secrets,
		host:       options.Host,
		newClient:  options.NewClient,
		updater:    options.Updater,
		keepAwake:  options.PreventSleep,
		rootCtx:    ctx,
		rootCancel: cancel,
		dirty:      make(chan struct{}, 1),
		notices:    append([]string(nil), options.Notices...),
		sentJobs:   make(map[string]JobView),
	}
	if s.newClient == nil {
		s.newClient = func(config tgtransport.Config, events tgtransport.Events) (Client, error) {
			return tgtransport.NewClient(config, events)
		}
	}
	if s.keepAwake == nil {
		s.keepAwake = platform.PreventSleep
	}
	if s.updater != nil {
		if s.updater.HTTPClient == nil {
			s.updater.HTTPClient = s.httpClient
		}
		// A staged download does not survive a restart; start clean.
		s.updater.ClearStage()
	}
	s.settings, s.settingsLoadErr = coreapp.LoadSettings(options.Paths.Settings)
	s.limiter = tgtransport.NewRateLimiter(limitBytes(s.settings.UploadLimitKBps))
	s.scheduler = newScheduleCoordinator(func() {
		// The coordinator may call back while the service holds its lock.
		s.spawn(s.tryScheduledStart)
	})
	s.snapshot = s.controller.Snapshot()
	s.stats = computeStats(s.snapshot.Jobs)
	s.loadSecrets()
	return s, nil
}

func limitBytes(kbps int) int64 { return int64(kbps) << 10 }

// SetEmitter attaches the event sink. Events raised before it is attached are
// dropped; the front end fetches the complete state with "init" anyway.
func (s *Service) SetEmitter(emitter Emitter) {
	s.emitMu.Lock()
	s.emitter = emitter
	s.emitMu.Unlock()
}

func (s *Service) emit(event string, payload any) {
	s.emitMu.Lock()
	emitter := s.emitter
	s.emitMu.Unlock()
	if emitter != nil {
		emitter(event, payload)
	}
}

// invalidate schedules a coalesced "state" event.
func (s *Service) invalidate() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *Service) toast(kind, text string) {
	if text == "" {
		return
	}
	s.emit("toast", map[string]string{"kind": kind, "text": text})
}

func (s *Service) toastError(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	s.toast("error", err.Error())
}

// spawn runs fn on a tracked goroutine unless the service is shutting down.
func (s *Service) spawn(fn func()) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		fn()
	}()
	return true
}

func (s *Service) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Start begins observing the controller, restores a persisted schedule,
// connects automatically when credentials are stored and checks for updates.
func (s *Service) Start() {
	if diagnostics.PreviousRunUnclean() {
		s.mu.Lock()
		s.notices = append(s.notices, "检测到上一次运行没有正常结束；诊断日志已保留，可在“设置 → 关于”中打开。")
		s.mu.Unlock()
	}
	s.spawn(s.observeController)
	s.spawn(s.stateLoop)
	s.restoreSchedule()
	s.autoConnect()
	s.spawn(s.updateLoop)
}

func (s *Service) stateLoop() {
	for {
		select {
		case <-s.dirty:
		case <-s.rootCtx.Done():
			return
		}
		timer := time.NewTimer(stateCoalesce)
		select {
		case <-timer.C:
		case <-s.rootCtx.Done():
			timer.Stop()
			return
		}
		// Drain a signal that arrived during the wait; this event covers it.
		select {
		case <-s.dirty:
		default:
		}
		s.emit("state", s.State())
	}
}

func (s *Service) observeController() {
	for {
		select {
		case snapshot, ok := <-s.controller.Updates():
			if !ok {
				return
			}
			s.applySnapshot(snapshot)
		case progress, ok := <-s.controller.ProgressUpdates():
			if !ok {
				return
			}
			s.applyProgress(progress)
		case <-s.rootCtx.Done():
			return
		}
	}
}

func (s *Service) applySnapshot(snapshot coreapp.Snapshot) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.snapshot = snapshot
	s.stats = computeStats(snapshot.Jobs)
	var delta *JobsDelta
	if s.frontReady {
		delta = s.diffJobsLocked()
	}
	running := snapshot.Running
	s.mu.Unlock()

	if delta != nil {
		s.emit("jobs", delta)
	}
	if !running {
		s.stopSleepGuard()
		s.scheduler.RetryDue()
	}
	s.invalidate()
}

func (s *Service) applyProgress(update coreapp.ProgressUpdate) {
	s.mu.Lock()
	if s.closed || update.JobID == "" || s.snapshot.ActiveID != update.JobID || s.snapshot.ActiveAttempt != update.AttemptID {
		s.mu.Unlock()
		return
	}
	var job *model.Job
	for index := range s.snapshot.Jobs {
		if s.snapshot.Jobs[index].ID == update.JobID {
			job = &s.snapshot.Jobs[index]
			break
		}
	}
	if job == nil || (job.State != model.JobUploading && job.State != model.JobSending) || update.Uploaded < job.Uploaded {
		s.mu.Unlock()
		return
	}
	uploaded := min(max(update.Uploaded, 0), max(job.Size, 0))
	previous := min(max(job.Uploaded, 0), max(job.Size, 0))
	s.snapshot.DoneBytes = min(max(s.snapshot.DoneBytes+uploaded-previous, 0), max(s.snapshot.TotalBytes, 0))
	s.snapshot.BytesPerSecond = max(s.snapshot.BytesPerSecond+update.BytesPerSecond-job.BytesPerSecond, 0)
	job.Uploaded = uploaded
	job.BytesPerSecond = update.BytesPerSecond
	if s.snapshot.BytesPerSecond > 0 && s.snapshot.TotalBytes > s.snapshot.DoneBytes {
		s.snapshot.ETA = time.Duration(float64(s.snapshot.TotalBytes-s.snapshot.DoneBytes)/s.snapshot.BytesPerSecond) * time.Second
	} else {
		s.snapshot.ETA = 0
	}
	if view, ok := s.sentJobs[job.ID]; ok {
		// Keep the diff baseline in step so the next structural diff does not
		// resend this row only because its byte counter moved.
		view.Uploaded = uploaded
		s.sentJobs[job.ID] = view
	}
	event := ProgressEvent{
		ID:         job.ID,
		Uploaded:   uploaded,
		Speed:      update.BytesPerSecond,
		DoneBytes:  s.snapshot.DoneBytes,
		TotalBytes: s.snapshot.TotalBytes,
		QueueSpeed: s.snapshot.BytesPerSecond,
		ETASeconds: int64(s.snapshot.ETA / time.Second),
	}
	ready := s.frontReady
	s.mu.Unlock()
	if ready {
		s.emit("progress", event)
	}
}

func (s *Service) stopSleepGuard() {
	s.mu.Lock()
	guard := s.sleepGuard
	s.sleepGuard = nil
	closed := s.closed
	s.mu.Unlock()
	if guard == nil {
		return
	}
	if err := guard.Stop(); err != nil && !closed {
		s.toast("error", "恢复系统休眠设置失败："+err.Error())
	}
}

func (s *Service) settingsCopy() coreapp.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// updateSettings persists a change before applying it in memory, so the file
// and the running state never disagree after a failed write.
func (s *Service) updateSettings(change func(*coreapp.Settings)) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	next := s.settingsCopy()
	change(&next)
	if err := coreapp.SaveSettings(s.paths.Settings, next); err != nil {
		return err
	}
	s.mu.Lock()
	s.settings = next
	s.settingsLoadErr = nil
	s.mu.Unlock()
	return nil
}

func (s *Service) loadSecrets() {
	var firstErr error
	read := func(get func() (string, error)) string {
		value, err := get()
		if err != nil && !errors.Is(err, credentials.ErrNotFound) && firstErr == nil {
			firstErr = err
		}
		if err != nil {
			return ""
		}
		return value
	}
	// Missing entries are normal on first launch and are not errors.
	s.cached.botToken = read(s.secrets.GetBotToken)
	s.cached.apiHash = read(s.secrets.GetAPIHash)
	s.cached.proxyPassword = read(s.secrets.GetProxyPassword)
	s.credentialErr = firstErr
}

// CloseRequested is called when the user tries to close the window. It
// returns true when the window may close immediately; otherwise the front end
// is asked to confirm and will call "quit".
func (s *Service) CloseRequested() bool {
	s.mu.Lock()
	running := s.snapshot.Running
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return true
	}
	if running || s.controller.Snapshot().Running {
		s.emit("confirm-quit", map[string]any{"reason": "uploading"})
		return false
	}
	if at, set, _ := s.scheduler.State(); set {
		s.emit("confirm-quit", map[string]any{"reason": "schedule", "at": at.UnixMilli()})
		return false
	}
	return true
}

// Shutdown stops every background activity. With cancelQueue the running
// upload is cancelled; it reports whether everything stopped in time.
func (s *Service) Shutdown(cancelQueue bool) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return true
	}
	s.closed = true
	s.generation++
	s.connecting = false
	s.connected = false
	s.pending = nil
	clientCancel := s.clientCancel
	s.clientCancel = nil
	s.client = nil
	moveCancel := s.moveCancel
	if s.retryTimer != nil {
		s.retryTimer.Stop()
		s.retryTimer = nil
	}
	updateCancel := s.update.cancel
	s.mu.Unlock()

	running := s.controller.Snapshot().Running
	_ = diagnostics.Logf("service_shutdown cancel_queue=%t queue_running=%t", cancelQueue, running)
	if cancelQueue && running {
		_ = s.controller.CancelAll()
	}
	if moveCancel != nil {
		moveCancel()
	}
	if updateCancel != nil {
		updateCancel()
	}
	if clientCancel != nil {
		clientCancel()
	}
	s.controller.SetGateway(nil)
	s.rootCancel()
	s.scheduler.Stop()
	s.stopSleepGuard()

	deadline := time.Now().Add(stopTimeout)
	clean := true
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	if err := s.controller.WaitStopped(ctx); err != nil {
		_ = diagnostics.Logf("queue_stop_timeout timeout_ms=%d", stopTimeout.Milliseconds())
		clean = false
	}
	cancel()
	done := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		_ = diagnostics.Logf("service_workers_stop_timeout timeout_ms=%d", stopTimeout.Milliseconds())
		clean = false
	}
	return clean
}
