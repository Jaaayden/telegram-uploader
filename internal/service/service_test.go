package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/demo"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/mover"
	"github.com/Jaaayden/telegram-uploader/internal/platform"
	"github.com/Jaaayden/telegram-uploader/internal/queue"
	"github.com/Jaaayden/telegram-uploader/internal/scanner"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

// fakeClient is a controllable Telegram transport.
type fakeClient struct {
	config  tgtransport.Config
	ready   chan struct{}
	fail    chan error
	binding chan tgtransport.BindingEvent
	upload  func(context.Context, tgtransport.UploadRequest, func(model.Progress)) (int, error)

	mu          sync.Mutex
	concurrency int
}

func newFakeClient(config tgtransport.Config) *fakeClient {
	return &fakeClient{
		config:  config,
		ready:   make(chan struct{}),
		fail:    make(chan error, 1),
		binding: make(chan tgtransport.BindingEvent, 1),
	}
}

func (c *fakeClient) Run(ctx context.Context) error {
	select {
	case err := <-c.fail:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *fakeClient) WaitReady(ctx context.Context) (tgtransport.Identity, error) {
	select {
	case <-c.ready:
		return tgtransport.Identity{BotID: 1, Username: "test_bot", MaxUploadBytes: demo.MaxUploadBytes, MaxUploadExact: true}, nil
	case <-ctx.Done():
		return tgtransport.Identity{}, ctx.Err()
	}
}

func (c *fakeClient) BeginChannelBinding() (string, error)           { return "TGUP_VERIFY_TEST", nil }
func (c *fakeClient) BindingEvents() <-chan tgtransport.BindingEvent { return c.binding }
func (c *fakeClient) ValidateChannel(_ context.Context, channel model.Channel) (model.Channel, error) {
	return channel, nil
}

func (c *fakeClient) SetUploadConcurrency(value int) int {
	c.mu.Lock()
	c.concurrency = value
	c.mu.Unlock()
	return value
}

func (c *fakeClient) UploadVideo(ctx context.Context, request tgtransport.UploadRequest, progress func(model.Progress)) (int, error) {
	if c.upload != nil {
		return c.upload(ctx, request, progress)
	}
	if request.BeforeSend != nil {
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
	}
	return 7, nil
}

type fakeHost struct {
	mu     sync.Mutex
	folder string
	files  []string
	quit   int
}

func (h *fakeHost) PickFolder(string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.folder, nil
}
func (h *fakeHost) PickFiles(string) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.files, nil
}
func (h *fakeHost) OpenPath(string) error { return nil }
func (h *fakeHost) OpenURL(string) error  { return nil }
func (h *fakeHost) Quit() {
	h.mu.Lock()
	h.quit++
	h.mu.Unlock()
}

type noSleep struct{}

func (noSleep) Stop() error { return nil }

type harness struct {
	t          *testing.T
	service    *Service
	controller *coreapp.Controller
	host       *fakeHost
	paths      coreapp.Paths
	root       string

	mu      sync.Mutex
	clients []*fakeClient
	events  []emitted
}

type emitted struct {
	name    string
	payload any
}

func newHarness(t *testing.T, secrets Secrets) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{t: t, host: &fakeHost{}, root: root}
	h.paths = coreapp.Paths{
		Root:     root,
		Settings: filepath.Join(root, "settings.json"),
		Queue:    filepath.Join(root, "queue.json"),
		Session:  filepath.Join(root, "session.enc"),
	}
	h.controller = coreapp.NewController(queue.NewStore(h.paths.Queue), mover.New())
	if err := h.controller.Load(); err != nil {
		t.Fatal(err)
	}
	if secrets == nil {
		secrets = demo.NewSecrets()
	}
	service, err := New(Options{
		Controller: h.controller,
		Paths:      h.paths,
		Secrets:    secrets,
		Host:       h.host,
		NewClient: func(config tgtransport.Config, _ tgtransport.Events) (Client, error) {
			if config.AppID == 666 {
				return nil, errors.New("API ID 无效")
			}
			client := newFakeClient(config)
			h.mu.Lock()
			h.clients = append(h.clients, client)
			h.mu.Unlock()
			return client, nil
		},
		PreventSleep: func() (platform.SleepGuard, error) { return noSleep{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	service.SetEmitter(func(name string, payload any) {
		h.mu.Lock()
		h.events = append(h.events, emitted{name, payload})
		h.mu.Unlock()
	})
	h.service = service
	t.Cleanup(func() { service.Shutdown(true) })
	return h
}

func (h *harness) client(index int) *fakeClient {
	h.t.Helper()
	var found *fakeClient
	h.eventually("client "+fmt.Sprint(index), func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.clients) > index {
			found = h.clients[index]
		}
		return found != nil
	})
	return found
}

func (h *harness) eventually(what string, condition func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) status() string { return h.service.State().Connection.Status }

func (h *harness) call(method string, params any) (any, error) {
	h.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.service.Handle(method, raw)
}

func (h *harness) saveCredentials(apiID int) {
	h.t.Helper()
	if err := h.service.updateSettings(func(settings *coreapp.Settings) { settings.APIID = apiID }); err != nil {
		h.t.Fatal(err)
	}
}

// seed writes n sparse videos and queues them.
func (h *harness) seed(n int) []model.Job {
	h.t.Helper()
	folder := filepath.Join(h.root, "videos")
	samples := make([]demo.Sample, n)
	for index := range samples {
		samples[index] = demo.Sample{Name: fmt.Sprintf("video %04d.mp4", index+1), Size: 4096}
	}
	if err := demo.WriteSamples(folder, samples); err != nil {
		h.t.Fatal(err)
	}
	jobs, err := scanner.Scan(folder, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.controller.AddJobs(jobs); err != nil {
		h.t.Fatal(err)
	}
	return jobs
}

func (h *harness) connect() *fakeClient {
	h.t.Helper()
	h.saveCredentials(42)
	h.service.Start()
	client := h.client(0)
	close(client.ready)
	h.eventually("connected", func() bool { return h.status() == "connected" })
	return client
}

func TestStartsUnconfiguredWithoutCredentials(t *testing.T) {
	h := newHarness(t, &demo.Secrets{})
	h.service.Start()
	if got := h.status(); got != "unconfigured" {
		t.Fatalf("status = %q, want unconfigured", got)
	}
	if err := h.service.StartUploads(); err == nil {
		t.Fatal("starting an empty, unconfigured queue must fail")
	}
}

func TestAutoConnectsAndReportsIdentity(t *testing.T) {
	h := newHarness(t, nil)
	h.connect()
	state := h.service.State()
	if state.Connection.Bot != "test_bot" || state.Connection.MaxBytes != demo.MaxUploadBytes {
		t.Fatalf("unexpected connection view: %+v", state.Connection)
	}
	if state.Settings.HasAPIHash != true || state.Settings.HasBotToken != true {
		t.Fatal("stored secrets must be reported as present")
	}
	encoded, _ := json.Marshal(state)
	for _, secret := range []string{"demo-api-hash", "1:demo-token"} {
		if contains(string(encoded), secret) {
			t.Fatalf("state leaks the secret %q to the interface", secret)
		}
	}
}

func contains(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestTransientFailureSchedulesRetryAndConfigErrorDoesNot(t *testing.T) {
	h := newHarness(t, nil)
	client := h.connect()
	client.fail <- io.ErrUnexpectedEOF
	h.eventually("retrying", func() bool { return h.status() == "retrying" })
	if h.service.State().Connection.RetryAt == 0 {
		t.Fatal("a retry must expose when it will happen")
	}

	// Reconnect immediately instead of waiting for the back-off timer.
	if err := h.service.Reconnect(); err != nil {
		t.Fatal(err)
	}
	second := h.client(1)
	second.fail <- errors.New("AUTH_KEY_UNREGISTERED")
	h.eventually("error", func() bool { return h.status() == "error" })
	time.Sleep(50 * time.Millisecond)
	h.mu.Lock()
	clients := len(h.clients)
	h.mu.Unlock()
	if clients != 2 {
		t.Fatalf("a permanent error must not be retried; %d clients were created", clients)
	}
}

func TestSaveConnectionReplacesRunningClient(t *testing.T) {
	h := newHarness(t, nil)
	first := h.connect()
	token := "2:another-token"
	if err := h.service.SaveConnection(ConnectionInput{APIID: 43, BotToken: &token, ProxyEnabled: true, ProxyAddress: "127.0.0.1:1080"}); err != nil {
		t.Fatal(err)
	}
	second := h.client(1)
	if second == first || second.config.AppID != 43 || second.config.BotToken != token || second.config.Proxy == nil {
		t.Fatalf("replacement client has the wrong configuration: %+v", second.config)
	}
	if second.config.APIHash != "demo-api-hash" {
		t.Fatal("an empty API Hash field must keep the stored one")
	}
	close(second.ready)
	h.eventually("reconnected", func() bool { return h.status() == "connected" })
	if h.service.settingsCopy().APIID != 43 {
		t.Fatal("settings were not persisted")
	}

	if err := h.service.SaveConnection(ConnectionInput{APIID: 0}); err == nil {
		t.Fatal("an invalid API ID must be rejected")
	}
	if err := h.service.SaveConnection(ConnectionInput{APIID: 5, ProxyEnabled: true}); err == nil {
		t.Fatal("an enabled proxy without address must be rejected")
	}
}

func TestInvalidConfigurationIsReportedNotRetried(t *testing.T) {
	h := newHarness(t, nil)
	h.saveCredentials(666)
	h.service.Start()
	h.eventually("error", func() bool { return h.status() == "error" })
}

func TestUploadLimitIsLiveAndPersisted(t *testing.T) {
	h := newHarness(t, nil)
	client := h.connect()
	if client.config.RateLimiter == nil || client.config.RateLimiter.Limit() != 0 {
		t.Fatal("the client must share the service's limiter, initially unlimited")
	}
	if _, err := h.call("setUploadLimit", map[string]int{"kbps": 2048}); err != nil {
		t.Fatal(err)
	}
	if got := client.config.RateLimiter.Limit(); got != 2048<<10 {
		t.Fatalf("limiter = %d bytes/s, want %d", got, 2048<<10)
	}
	saved, err := coreapp.LoadSettings(h.paths.Settings)
	if err != nil || saved.UploadLimitKBps != 2048 {
		t.Fatalf("limit not persisted: %+v %v", saved, err)
	}
	for _, bad := range []int{-1, coreapp.MaxUploadLimitKBps + 1} {
		if _, err := h.call("setUploadLimit", map[string]int{"kbps": bad}); err == nil {
			t.Fatalf("limit %d must be rejected", bad)
		}
	}
	if _, err := h.call("setConcurrency", map[string]int{"value": 12}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	concurrency := client.concurrency
	client.mu.Unlock()
	if concurrency != 12 {
		t.Fatalf("client concurrency = %d, want 12", concurrency)
	}
	if _, err := h.call("setConcurrency", map[string]int{"value": 5}); err == nil {
		t.Fatal("unsupported concurrency must be rejected")
	}
}

func TestInitReturnsFullQueueAndLaterEventsOnlyChanges(t *testing.T) {
	h := newHarness(t, nil)
	jobs := h.seed(6)
	h.connect()
	result, err := h.call("init", nil)
	if err != nil {
		t.Fatal(err)
	}
	init := result.(InitView)
	if len(init.Jobs.Upsert) != 6 || len(init.Jobs.Order) != 6 {
		t.Fatalf("init returned %d rows / %d ids, want 6 / 6", len(init.Jobs.Upsert), len(init.Jobs.Order))
	}

	if err := h.controller.CancelJob(jobs[2].ID); err != nil {
		t.Fatal(err)
	}
	var delta *JobsDelta
	h.eventually("jobs delta", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, event := range h.events {
			if candidate, ok := event.payload.(*JobsDelta); ok && event.name == "jobs" && candidate.Rev > init.Jobs.Rev {
				delta = candidate
			}
		}
		return delta != nil
	})
	if len(delta.Upsert) != 1 || delta.Upsert[0].ID != jobs[2].ID || delta.Upsert[0].State != string(model.JobCancelled) {
		t.Fatalf("delta should carry exactly the cancelled row: %+v", delta.Upsert)
	}
	if delta.Order != nil {
		t.Fatal("the order must not be resent when membership did not change")
	}

	message, err := h.service.RemoveJobs([]string{jobs[0].ID})
	if err != nil || message == "" {
		t.Fatalf("RemoveJobs = %q, %v", message, err)
	}
	h.eventually("order delta", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, event := range h.events {
			if candidate, ok := event.payload.(*JobsDelta); ok && len(candidate.Order) == 5 {
				return true
			}
		}
		return false
	})
}

func (h *harness) waitForSnapshot(jobs int) {
	h.t.Helper()
	h.eventually("snapshot", func() bool {
		h.service.mu.Lock()
		defer h.service.mu.Unlock()
		return len(h.service.snapshot.Jobs) == jobs
	})
}

func TestAddFolderMarksDuplicatesAndSkipsThemByDefault(t *testing.T) {
	h := newHarness(t, nil)
	h.seed(3)
	h.connect()
	h.waitForSnapshot(3)
	h.host.folder = filepath.Join(h.root, "videos")
	if err := demo.WriteSamples(h.host.folder, []demo.Sample{{Name: "video 0009.mp4", Size: 4096}, {Name: "video 0010.mp4", Size: 4096}}); err != nil {
		t.Fatal(err)
	}
	scan, err := h.service.AddFolder()
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Candidates) != 5 {
		t.Fatalf("scan found %d candidates, want 5", len(scan.Candidates))
	}
	for index, candidate := range scan.Candidates {
		if want := index < 3; candidate.Duplicate != want || candidate.Replaceable != want {
			t.Fatalf("candidate %d duplicate/replaceable = %v/%v, want %v", index, candidate.Duplicate, candidate.Replaceable, want)
		}
		if candidate.Duplicate && candidate.DuplicateState != string(model.JobQueued) {
			t.Fatalf("candidate %d reports state %q", index, candidate.DuplicateState)
		}
	}
	// Index 0 is a duplicate and 99 is out of range; only index 4 is added.
	message, err := h.service.AddCandidates(scan.Token, []int{0, 4, 4, 99}, false)
	if err != nil || message != "已添加 1 个视频" {
		t.Fatalf("AddCandidates = %q, %v", message, err)
	}
	if _, err := h.service.AddCandidates(scan.Token, []int{3}, false); err == nil {
		t.Fatal("a scan token must be single-use")
	}
	if got := len(h.controller.Snapshot().Jobs); got != 4 {
		t.Fatalf("queue has %d jobs, want 4", got)
	}

	h.host.folder = ""
	cancelled, err := h.service.AddFolder()
	if err != nil || !cancelled.Cancelled {
		t.Fatalf("a dismissed folder chooser must report cancellation: %+v %v", cancelled, err)
	}
}

func TestReplacingDuplicatesRequeuesThemAsNewEntries(t *testing.T) {
	h := newHarness(t, nil)
	jobs := h.seed(3)
	h.connect()
	h.waitForSnapshot(3)
	// One entry was already handled; replacing it must queue it again.
	if err := h.controller.CancelJob(jobs[1].ID); err != nil {
		t.Fatal(err)
	}
	h.eventually("cancelled", func() bool {
		h.service.mu.Lock()
		defer h.service.mu.Unlock()
		return h.service.snapshot.Jobs[1].State == model.JobCancelled
	})

	h.host.files = []string{jobs[1].Path, jobs[2].Path}
	scan, err := h.service.AddFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Candidates) != 2 || !scan.Candidates[0].Duplicate || scan.Candidates[0].DuplicateState != string(model.JobCancelled) {
		t.Fatalf("unexpected scan: %+v", scan.Candidates)
	}
	message, err := h.service.AddCandidates(scan.Token, []int{0, 1}, true)
	if err != nil {
		t.Fatal(err)
	}
	if message != "已添加 2 个视频（其中 2 个替换了队列里的旧条目）" {
		t.Fatalf("message = %q", message)
	}
	queue := h.controller.Snapshot().Jobs
	if len(queue) != 3 {
		t.Fatalf("queue has %d jobs, want 3 (replaced, not duplicated)", len(queue))
	}
	if queue[0].ID != jobs[0].ID {
		t.Fatal("the untouched entry must keep its place")
	}
	for _, job := range queue[1:] {
		if job.ID == jobs[1].ID || job.ID == jobs[2].ID {
			t.Fatal("replaced entries must be new jobs with fresh identities")
		}
		if job.State != model.JobQueued {
			t.Fatalf("replaced entry is %q, want queued", job.State)
		}
	}
}

func TestEntriesInFlightAreNeverReplaced(t *testing.T) {
	h := newHarness(t, nil)
	jobs := h.seed(2)
	if err := h.controller.SetChannel(demo.Channel); err != nil {
		t.Fatal(err)
	}
	client := h.connect()
	release := make(chan struct{})
	client.upload = func(ctx context.Context, _ tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		select {
		case <-release:
			return 1, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	defer close(release)
	h.waitForSnapshot(2)
	if err := h.service.StartUploads(); err != nil {
		t.Fatal(err)
	}
	h.eventually("uploading", func() bool {
		h.service.mu.Lock()
		defer h.service.mu.Unlock()
		return h.service.snapshot.Jobs[0].State == model.JobUploading
	})

	scan, err := h.service.ScanPaths([]string{jobs[0].Path, jobs[1].Path})
	if err != nil {
		t.Fatal(err)
	}
	if scan.Candidates[0].Replaceable || !scan.Candidates[1].Replaceable {
		t.Fatalf("only the waiting entry is replaceable: %+v", scan.Candidates)
	}
	message, err := h.service.AddCandidates(scan.Token, []int{0, 1}, true)
	if err != nil {
		t.Fatal(err)
	}
	if message != "已添加 1 个视频（其中 1 个替换了队列里的旧条目）；1 个仍在队列中处理，已跳过" {
		t.Fatalf("message = %q", message)
	}
	queue := h.controller.Snapshot().Jobs
	if len(queue) != 2 || queue[0].ID != jobs[0].ID || queue[0].State != model.JobUploading {
		t.Fatalf("the uploading entry must be left alone: %+v", queue)
	}
}

func TestSimilarFilesAndDroppedPathsAreReported(t *testing.T) {
	h := newHarness(t, nil)
	h.seed(2)
	h.connect()
	h.waitForSnapshot(2)
	// A copy of a queued video in another folder: same name and size.
	other := filepath.Join(h.root, "copies")
	if err := demo.WriteSamples(other, []demo.Sample{{Name: "video 0001.mp4", Size: 4096}, {Name: "fresh.mp4", Size: 4096}}); err != nil {
		t.Fatal(err)
	}
	h.service.DropPaths([]string{other, filepath.Join(other, "fresh.mp4"), filepath.Join(h.root, "settings.json")})
	var scan ScanView
	h.eventually("scan event", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, event := range h.events {
			if view, ok := event.payload.(ScanView); ok && event.name == "scan" {
				scan = view
				return true
			}
		}
		return false
	})
	if len(scan.Candidates) != 2 || scan.Ignored != 1 {
		t.Fatalf("drop: %d candidates, %d ignored; want 2 and 1: %+v", len(scan.Candidates), scan.Ignored, scan)
	}
	for _, candidate := range scan.Candidates {
		if candidate.Duplicate {
			t.Fatalf("a different path is not a duplicate: %+v", candidate)
		}
		if want := candidate.Name == "video 0001.mp4"; candidate.Similar != want {
			t.Fatalf("%s similar = %v, want %v", candidate.Name, candidate.Similar, want)
		}
	}
}

// TestScanAgainstLargeQueueStaysLinear guards the duplicate check: comparing
// every candidate with every queued entry would be quadratic.
func TestScanAgainstLargeQueueStaysLinear(t *testing.T) {
	const queued, dropped = 5000, 300
	h := newHarness(t, nil)
	folder := filepath.Join(h.root, "drop")
	samples := make([]demo.Sample, dropped)
	paths := make([]string, dropped)
	for index := range samples {
		samples[index] = demo.Sample{Name: fmt.Sprintf("clip %04d.mp4", index), Size: 4096}
		paths[index] = filepath.Join(folder, samples[index].Name)
	}
	if err := demo.WriteSamples(folder, samples); err != nil {
		t.Fatal(err)
	}
	h.service.mu.Lock()
	h.service.snapshot.Jobs = make([]model.Job, queued)
	for index := range h.service.snapshot.Jobs {
		path := fmt.Sprintf("/videos/video %05d.mp4", index)
		if index < 100 {
			path = paths[index] // the first hundred dropped files are queued already
		}
		h.service.snapshot.Jobs[index] = model.Job{ID: fmt.Sprintf("job-%05d", index), Name: filepath.Base(path), Path: path, Size: 700 << 20, State: model.JobQueued}
	}
	h.service.mu.Unlock()

	started := time.Now()
	scan, err := h.service.ScanPaths(paths)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	duplicates := 0
	for _, candidate := range scan.Candidates {
		if candidate.Duplicate {
			duplicates++
		}
	}
	t.Logf("%d dropped files against %d queued: %v (includes reading %d files from disk)", dropped, queued, elapsed, dropped)
	if duplicates != 100 || len(scan.Candidates) != dropped {
		t.Fatalf("found %d duplicates among %d candidates, want 100 of %d", duplicates, len(scan.Candidates), dropped)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("scan took %v", elapsed)
	}
}

func TestStartUploadsRequiresConnectionAndChannelThenRuns(t *testing.T) {
	h := newHarness(t, nil)
	h.seed(2)
	h.saveCredentials(42)
	h.service.Start()
	client := h.client(0)
	h.eventually("snapshot", func() bool {
		h.service.mu.Lock()
		defer h.service.mu.Unlock()
		return h.service.stats.Runnable
	})
	if err := h.service.StartUploads(); err == nil {
		t.Fatal("uploads must not start before Telegram is connected")
	}
	close(client.ready)
	h.eventually("connected", func() bool { return h.status() == "connected" })
	if err := h.service.StartUploads(); err == nil {
		t.Fatal("uploads must not start before a channel is bound")
	}
	client.binding <- tgtransport.BindingEvent{Channel: demo.Channel}
	h.eventually("channel bound", func() bool { return h.service.State().Channel.ID == demo.Channel.ID })
	if err := h.service.StartUploads(); err != nil {
		t.Fatal(err)
	}
	h.eventually("all sent", func() bool {
		snapshot := h.controller.Snapshot()
		for _, job := range snapshot.Jobs {
			if job.State != model.JobSent {
				return false
			}
		}
		return !snapshot.Running
	})
}

func TestDueScheduleStartsOnceEverythingIsReady(t *testing.T) {
	h := newHarness(t, nil)
	h.seed(1)
	if err := h.controller.SetChannel(demo.Channel); err != nil {
		t.Fatal(err)
	}
	if err := h.service.SetSchedule(time.Now().Add(-time.Minute).Unix()); err == nil {
		t.Fatal("a schedule in the past must be rejected")
	}
	// Persist a schedule that is already due, as after a restart.
	if err := h.service.updateSettings(func(settings *coreapp.Settings) {
		settings.APIID = 42
		settings.ScheduledStartUnix = time.Now().Add(-time.Second).Unix()
	}); err != nil {
		t.Fatal(err)
	}
	h.service.Start()
	if view := h.service.State().Schedule; view.At == 0 || !view.Due {
		t.Fatalf("restored schedule should be due: %+v", view)
	}
	close(h.client(0).ready)
	h.eventually("scheduled upload finished", func() bool {
		jobs := h.controller.Snapshot().Jobs
		return len(jobs) == 1 && jobs[0].State == model.JobSent
	})
	h.eventually("schedule consumed", func() bool {
		return h.service.State().Schedule.At == 0 && h.service.settingsCopy().ScheduledStartUnix == 0
	})
}

func TestCloseRequestAsksWhileUploading(t *testing.T) {
	h := newHarness(t, nil)
	h.seed(1)
	if err := h.controller.SetChannel(demo.Channel); err != nil {
		t.Fatal(err)
	}
	client := h.connect()
	release := make(chan struct{})
	client.upload = func(ctx context.Context, _ tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		select {
		case <-release:
			return 1, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if !h.service.CloseRequested() {
		t.Fatal("an idle application must close without asking")
	}
	h.eventually("runnable", func() bool {
		h.service.mu.Lock()
		defer h.service.mu.Unlock()
		return h.service.stats.Runnable
	})
	if err := h.service.StartUploads(); err != nil {
		t.Fatal(err)
	}
	h.eventually("running", func() bool { return h.controller.Snapshot().Running })
	if h.service.CloseRequested() {
		t.Fatal("closing during an upload must ask first")
	}
	if _, err := h.call("quit", nil); err != nil {
		t.Fatal(err)
	}
	if h.host.quit != 1 {
		t.Fatal("quit must close the window")
	}
	if _, err := h.call("init", nil); err == nil {
		t.Fatal("requests after shutdown must be refused")
	}
	close(release)
}

// TestJobDiffScalesWithChangesNotQueueLength guards the hot path that runs on
// every queue change: one pass over the queue and a payload that only contains
// the changed rows.
func TestJobDiffScalesWithChangesNotQueueLength(t *testing.T) {
	const total = 5000
	h := newHarness(t, nil)
	jobs := make([]model.Job, total)
	for index := range jobs {
		jobs[index] = model.Job{
			ID: fmt.Sprintf("job-%05d", index), Name: fmt.Sprintf("video %05d.mp4", index),
			Path: fmt.Sprintf("/videos/video %05d.mp4", index), Size: 700 << 20, State: model.JobQueued,
		}
	}
	s := h.service
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Jobs = jobs
	full := s.fullJobsLocked()
	if len(full.Upsert) != total || len(full.Order) != total {
		t.Fatalf("full sync returned %d rows", len(full.Upsert))
	}
	if delta := s.diffJobsLocked(); delta != nil {
		t.Fatal("an unchanged queue must produce no event")
	}

	s.snapshot.Jobs[2500].State = model.JobUploading
	started := time.Now()
	delta := s.diffJobsLocked()
	elapsed := time.Since(started)
	if delta == nil || len(delta.Upsert) != 1 || delta.Order != nil {
		t.Fatalf("one changed row must produce a one-row delta: %+v", delta)
	}
	encoded, _ := json.Marshal(delta)
	fullEncoded, _ := json.Marshal(full)
	t.Logf("%d jobs: diff took %v; delta payload %d bytes, full payload %d bytes", total, elapsed, len(encoded), len(fullEncoded))
	if len(encoded) > 1024 {
		t.Fatalf("delta payload is %d bytes for one changed row", len(encoded))
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("diff over %d jobs took %v", total, elapsed)
	}

	s.snapshot.Jobs = s.snapshot.Jobs[1:]
	delta = s.diffJobsLocked()
	if delta == nil || len(delta.Order) != total-1 || len(delta.Upsert) != 0 {
		t.Fatal("a removal must resend the order and no rows")
	}
	if len(s.sentJobs) != total-1 {
		t.Fatalf("baseline keeps %d rows after a removal; it must not grow without bound", len(s.sentJobs))
	}
}

// A schedule can be set while uploading: the queue pauses at once and goes on
// by itself when the time comes — no cancelling or resetting needed.
func TestScheduleOnRunningQueuePausesNowAndResumesAtThatTime(t *testing.T) {
	h := newHarness(t, nil)
	h.seed(2)
	if err := h.controller.SetChannel(demo.Channel); err != nil {
		t.Fatal(err)
	}
	client := h.connect()
	started := make(chan struct{})
	var once sync.Once
	client.upload = func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		interrupted := false
		once.Do(func() {
			close(started)
			<-ctx.Done() // still uploading when the schedule is set
			interrupted = true
		})
		if interrupted {
			return 0, ctx.Err()
		}
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		return 1, nil
	}
	h.waitForSnapshot(2)
	if err := h.service.StartUploads(); err != nil {
		t.Fatal(err)
	}
	<-started

	at := time.Now().Add(2 * time.Second)
	if err := h.service.SetSchedule(at.Unix()); err != nil {
		t.Fatalf("a schedule must be accepted while uploading: %v", err)
	}
	h.eventually("paused", func() bool {
		snapshot := h.controller.Snapshot()
		return !snapshot.Running && snapshot.Paused && snapshot.Jobs[0].State == model.JobInterrupted
	})
	if view := h.service.State().Schedule; view.At == 0 || view.Due {
		t.Fatalf("the schedule should be pending: %+v", view)
	}
	h.eventually("resumed and finished at the scheduled time", func() bool {
		snapshot := h.controller.Snapshot()
		return !snapshot.Running && snapshot.Jobs[0].State == model.JobSent && snapshot.Jobs[1].State == model.JobSent
	})
	if time.Now().Before(at.Truncate(time.Second)) {
		t.Fatal("the queue resumed before the scheduled time")
	}
	if h.service.State().Schedule.At != 0 {
		t.Fatal("the schedule must be consumed once it fired")
	}
}

func TestScheduleOnPausedQueueResumesIt(t *testing.T) {
	h := newHarness(t, nil)
	h.seed(1)
	if err := h.controller.SetChannel(demo.Channel); err != nil {
		t.Fatal(err)
	}
	h.connect()
	h.waitForSnapshot(1)
	// Paused before anything ran, e.g. started by mistake and paused at once.
	if err := h.service.PauseUploads(false); err != nil {
		t.Fatal(err)
	}
	h.eventually("paused", func() bool { return h.controller.Snapshot().Paused })
	if err := h.service.SetSchedule(time.Now().Add(2 * time.Second).Unix()); err != nil {
		t.Fatalf("a schedule must be accepted on a paused queue: %v", err)
	}
	h.eventually("resumed by the schedule", func() bool {
		snapshot := h.controller.Snapshot()
		return !snapshot.Running && !snapshot.Paused && snapshot.Jobs[0].State == model.JobSent
	})
}
