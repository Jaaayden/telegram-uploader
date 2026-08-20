package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	coreapp "github.com/jayden/telegram-video-uploader/internal/app"
	"github.com/jayden/telegram-video-uploader/internal/model"
	tgtransport "github.com/jayden/telegram-video-uploader/internal/telegram"
)

func TestUploadConcurrencyOptionMapping(t *testing.T) {
	options := uploadConcurrencyOptions()
	wantOptions := []string{
		uploadConcurrencyCompatibilityLabel,
		uploadConcurrencyBalancedLabel,
		uploadConcurrencyFastLabel,
	}
	if len(options) != len(wantOptions) {
		t.Fatalf("upload concurrency options = %#v, want %#v", options, wantOptions)
	}
	for index := range wantOptions {
		if options[index] != wantOptions[index] {
			t.Fatalf("option[%d] = %q, want %q", index, options[index], wantOptions[index])
		}
	}

	tests := []struct {
		option string
		value  int
	}{
		{option: uploadConcurrencyCompatibilityLabel, value: tgtransport.UploadConcurrencyCompatibility},
		{option: uploadConcurrencyBalancedLabel, value: tgtransport.UploadConcurrencyBalanced},
		{option: uploadConcurrencyFastLabel, value: tgtransport.UploadConcurrencyFast},
	}
	for _, test := range tests {
		got, ok := uploadConcurrencyForOption(test.option)
		if !ok || got != test.value {
			t.Errorf("uploadConcurrencyForOption(%q) = (%d, %v), want (%d, true)", test.option, got, ok, test.value)
		}
		if got := uploadConcurrencyOptionFor(test.value); got != test.option {
			t.Errorf("uploadConcurrencyOptionFor(%d) = %q, want %q", test.value, got, test.option)
		}
	}
	if _, ok := uploadConcurrencyForOption("未知档位"); ok {
		t.Fatal("unknown upload concurrency option was accepted")
	}
	if got := normalizeUploadConcurrency(0); got != tgtransport.DefaultUploadConcurrency {
		t.Fatalf("normalizeUploadConcurrency(0) = %d, want %d", got, tgtransport.DefaultUploadConcurrency)
	}
}

func TestRefreshQueueRowsShowsNamesProgressAndRebindsVisibleRow(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildQueue()
	jobs := []model.Job{
		{ID: "first", Position: 0, Name: "01-first.mp4", Size: 100, State: model.JobQueued},
		{ID: "second", Position: 1, Name: "02-second.mp4", Size: 200, State: model.JobQueued},
	}
	u.refreshQueueRows(jobs)

	if len(u.queueJobs) != len(jobs) || u.queueList.Length() != len(jobs) {
		t.Fatalf("queue length = %d/%d, want %d", len(u.queueJobs), u.queueList.Length(), len(jobs))
	}
	first := newJobRow()
	u.updateJobRow(first, u.queueJobs[0])
	if !strings.Contains(first.name.Text, jobs[0].Name) {
		t.Fatalf("first row name = %q, want %q", first.name.Text, jobs[0].Name)
	}
	if first.progress.Value != 0 {
		t.Fatalf("initial first progress = %v, want 0", first.progress.Value)
	}

	jobs[0].State = model.JobUploading
	jobs[0].Uploaded = 50
	jobs[0].BytesPerSecond = 25
	u.refreshQueueRows(jobs)
	u.updateJobRow(first, u.queueJobs[0])
	if !strings.Contains(first.name.Text, jobs[0].Name) {
		t.Fatalf("refreshed first row name = %q, want %q", first.name.Text, jobs[0].Name)
	}
	if first.progress.Value != 0.5 {
		t.Fatalf("refreshed first progress = %v, want 0.5", first.progress.Value)
	}
	if !strings.Contains(first.status.Text, "25 B/s") {
		t.Fatalf("refreshed first status = %q, want upload speed", first.status.Text)
	}
}

func TestVirtualQueueListCreatesOnlyVisibleRows(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildQueue()
	jobs := make([]model.Job, 1000)
	for index := range jobs {
		jobs[index] = model.Job{
			ID:       fmt.Sprintf("job-%04d", index),
			Position: index,
			Name:     fmt.Sprintf("video-%04d.mp4", index),
			Size:     100,
			State:    model.JobQueued,
		}
	}
	u.snapshot = coreapp.Snapshot{Jobs: jobs}
	u.refreshQueueRows(jobs)
	template := u.queueList.CreateItem()
	if _, ok := template.(*fyne.Container); !ok {
		t.Fatalf("queue template type = %T, want native *fyne.Container for cross-platform rendering", template)
	}
	u.queueList.UpdateItem(0, template)
	row, ok := jobRowFromObject(template)
	if !ok || !strings.Contains(row.name.Text, jobs[0].Name) {
		t.Fatalf("native queue template was not populated: row=%+v", row)
	}
	if markup := test.RenderObjectToMarkup(template); !strings.Contains(markup, "取消") || !strings.Contains(markup, "0%") {
		t.Fatalf("rendered native queue row does not contain populated child widgets; markup=%s", markup)
	}

	window := test.NewWindow(u.queueList)
	defer window.Close()
	window.Resize(fyne.NewSize(900, 480))
	window.Show()
	test.WidgetRenderer(u.queueList)

	if u.queueRowCreateCount == 0 || u.queueRowCreateCount >= len(jobs) {
		t.Fatalf("created queue rows = %d, want a visible pool smaller than %d jobs", u.queueRowCreateCount, len(jobs))
	}
	if len(u.queueRowPool) == 0 || u.queueRowPool[0].boundID == "" || u.queueRowPool[0].name.Text == "文件名" {
		t.Fatalf("visible native rows were not populated: pool=%d", len(u.queueRowPool))
	}
}

func TestMainStatusHeaderStaysOnOneLine(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildFields()
	statusLine := u.buildMainStatusLine()

	if u.readinessHint.Wrapping != fyne.TextWrapOff || u.readinessHint.Truncation != fyne.TextTruncateEllipsis {
		t.Fatalf("readiness label wrapping/truncation = %v/%v, want off/ellipsis", u.readinessHint.Wrapping, u.readinessHint.Truncation)
	}
	maxLabelHeight := max(u.mainConnection.MinSize().Height, u.mainChannel.MinSize().Height, u.readinessHint.MinSize().Height)
	if statusLine.MinSize().Height > maxLabelHeight {
		t.Fatalf("status line minimum height = %.1f, want one label line %.1f", statusLine.MinSize().Height, maxLabelHeight)
	}
}

func TestLocalFileURLSupportsUnixAndWindowsPaths(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/tmp/Telegram Video Uploader/logs", want: "file:///tmp/Telegram%20Video%20Uploader/logs"},
		{path: `C:\Users\Jayden\AppData\Roaming\TelegramVideoUploader\logs`, want: "file:///C:/Users/Jayden/AppData/Roaming/TelegramVideoUploader/logs"},
	}
	for _, test := range tests {
		if got := localFileURL(test.path).String(); got != test.want {
			t.Errorf("localFileURL(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestVirtualQueueProgressUpdatesReuseVisibleRows(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildQueue()
	jobs := make([]model.Job, 1000)
	for index := range jobs {
		jobs[index] = model.Job{
			ID:       fmt.Sprintf("job-%04d", index),
			Position: index,
			Name:     fmt.Sprintf("video-%04d.mp4", index),
			Size:     1000,
			State:    model.JobQueued,
		}
	}
	u.snapshot = coreapp.Snapshot{Jobs: jobs}
	u.refreshQueueRows(jobs)
	window := test.NewWindow(u.queueList)
	defer window.Close()
	window.Resize(fyne.NewSize(900, 480))
	window.Show()
	test.WidgetRenderer(u.queueList)
	created := u.queueRowCreateCount

	jobs[0].State = model.JobUploading
	for update := 1; update <= 100; update++ {
		jobs[0].Uploaded = int64(update * 10)
		jobs[0].BytesPerSecond = 100
		u.snapshot = coreapp.Snapshot{Running: true, ActiveID: jobs[0].ID, Jobs: jobs}
		u.refreshQueueRows(jobs)
	}
	if u.queueRowCreateCount != created {
		t.Fatalf("progress updates created %d additional rows, want the visible row pool reused", u.queueRowCreateCount-created)
	}
}

func TestApplyProgressUpdateMergesLatestActiveAttemptWithoutFullRefresh(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildQueue()
	u.progress = widget.NewProgressBar()
	u.progressSummary = widget.NewLabel("")
	job := model.Job{
		ID:             "active",
		Position:       0,
		Name:           "active.mp4",
		Size:           1000,
		Uploaded:       100,
		BytesPerSecond: 5,
		State:          model.JobUploading,
	}
	u.snapshot = coreapp.Snapshot{
		Jobs:           []model.Job{job},
		ActiveID:       job.ID,
		ActiveAttempt:  2,
		Running:        true,
		TotalBytes:     1000,
		DoneBytes:      100,
		BytesPerSecond: 5,
	}
	u.refreshQueueRows(u.snapshot.Jobs)

	u.applyProgressUpdate(coreapp.ProgressUpdate{
		JobID:          job.ID,
		AttemptID:      2,
		Uploaded:       400,
		BytesPerSecond: 20,
	})
	if got := u.queueJobs[0].Uploaded; got != 400 {
		t.Fatalf("local job uploaded = %d, want 400", got)
	}
	if got := u.snapshot.DoneBytes; got != 400 {
		t.Fatalf("aggregate done bytes = %d, want 400", got)
	}
	if got := u.snapshot.BytesPerSecond; got != 20 {
		t.Fatalf("aggregate speed = %v, want 20", got)
	}
	if got := u.progress.Value; got != 0.4 {
		t.Fatalf("overall progress = %v, want 0.4", got)
	}

	// A retry callback and a terminal-state callback must not overwrite the
	// current local state, even when they arrive after the latest update.
	u.applyProgressUpdate(coreapp.ProgressUpdate{
		JobID:          job.ID,
		AttemptID:      1,
		Uploaded:       900,
		BytesPerSecond: 90,
	})
	if got := u.queueJobs[0].Uploaded; got != 400 {
		t.Fatalf("stale retry changed local progress to %d, want 400", got)
	}
	u.snapshot.Jobs[0].State = model.JobSent
	u.queueJobs[0].State = model.JobSent
	u.applyProgressUpdate(coreapp.ProgressUpdate{
		JobID:          job.ID,
		AttemptID:      2,
		Uploaded:       1000,
		BytesPerSecond: 100,
	})
	if got := u.queueJobs[0].Uploaded; got != 400 {
		t.Fatalf("terminal callback changed local progress to %d, want 400", got)
	}
}

func TestReusedQueueRowBindsCheckboxToLatestJobID(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildQueue()
	row := newJobRow()
	first := model.Job{ID: "first", Name: "first.mp4", State: model.JobQueued}
	second := model.Job{ID: "second", Name: "second.mp4", State: model.JobQueued}
	u.updateJobRow(row, first)
	u.updateJobRow(row, second)
	row.selected.SetChecked(true)

	if u.selectedJobs["first"] || !u.selectedJobs["second"] {
		t.Fatalf("selection after row reuse = %#v, want only second job", u.selectedJobs)
	}
}

func TestConnectionConfigurationAndTransientErrorClassification(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildFields()
	if u.hasCompleteConnectionConfig() {
		t.Fatal("empty settings were treated as a complete connection configuration")
	}
	u.apiID.SetText("12345")
	u.apiHash.SetText("hash")
	u.botToken.SetText("12345:token")
	if !u.hasCompleteConnectionConfig() {
		t.Fatal("complete settings were not recognized")
	}
	if !isTransientConnectionError(io.EOF) {
		t.Fatal("EOF was not classified as transient")
	}
	if isTransientConnectionError(context.Canceled) {
		t.Fatal("intentional cancellation was classified as transient")
	}
	if isTransientConnectionError(errors.New("BOT_TOKEN_INVALID")) {
		t.Fatal("a configuration/authentication error was classified as transient")
	}
	if !shouldRetryConnection(nil, nil, false) {
		t.Fatal("an unexpected clean client stop was not scheduled for reconnect")
	}
	if shouldRetryConnection(nil, context.Canceled, false) || shouldRetryConnection(io.EOF, nil, true) {
		t.Fatal("an intentional stop or closed window was scheduled for reconnect")
	}
}

func TestAutoConnectPolicyStartsOnlyWithUsableSavedConfiguration(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	tests := []struct {
		name          string
		configure     func(*window)
		settingsErr   error
		credentialErr error
		wantStart     bool
		wantConfigErr bool
	}{
		{
			name: "complete credentials",
			configure: func(u *window) {
				u.apiID.SetText("12345")
				u.apiHash.SetText("hash")
				u.botToken.SetText("12345:token")
			},
			wantStart: true,
		},
		{name: "missing credentials", configure: func(u *window) { u.apiID.SetText("12345") }},
		{
			name:        "settings file error",
			settingsErr: errors.New("invalid settings"),
			configure: func(u *window) {
				u.apiID.SetText("12345")
				u.apiHash.SetText("hash")
				u.botToken.SetText("12345:token")
			},
		},
		{
			name:          "keyring error",
			credentialErr: errors.New("keyring unavailable"),
			configure: func(u *window) {
				u.apiID.SetText("12345")
				u.apiHash.SetText("hash")
				u.botToken.SetText("12345:token")
			},
		},
		{
			name: "enabled proxy without address",
			configure: func(u *window) {
				u.apiID.SetText("12345")
				u.apiHash.SetText("hash")
				u.botToken.SetText("12345:token")
				u.proxyEnabled.SetChecked(true)
			},
			wantConfigErr: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			started := make(chan connectionRequest, 1)
			u := &window{
				settingsLoadErr:   testCase.settingsErr,
				credentialLoadErr: testCase.credentialErr,
				connectionStarter: func(request connectionRequest, _ bool, _ uint64) { started <- request },
			}
			u.buildFields()
			testCase.configure(u)
			u.autoConnectIfConfigured()
			if (u.connectionConfigErr != nil) != testCase.wantConfigErr {
				t.Fatalf("connection configuration error = %v, want present=%v", u.connectionConfigErr, testCase.wantConfigErr)
			}

			if testCase.wantStart {
				select {
				case request := <-started:
					if request.appID != 12345 || request.apiHash != "hash" || request.botToken != "12345:token" {
						t.Fatalf("connection request = %+v, want saved credentials", request)
					}
				case <-time.After(time.Second):
					t.Fatal("complete saved configuration did not start an automatic connection")
				}
				return
			}
			select {
			case request := <-started:
				t.Fatalf("automatic connection unexpectedly started: %+v", request)
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func TestAutomaticEnsureConnectionDoesNotRetryConfigurationError(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()
	started := make(chan struct{}, 1)
	u := &window{
		connectionConfigErr: errors.New("invalid bot token"),
		connectionStarter: func(connectionRequest, bool, uint64) {
			started <- struct{}{}
		},
	}
	u.buildFields()
	u.apiID.SetText("12345")
	u.apiHash.SetText("hash")
	u.botToken.SetText("12345:invalid")
	u.ensureConnection()
	select {
	case <-started:
		t.Fatal("configuration error was retried automatically")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestAutomaticReconnectUsesLastAppliedConfiguration(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	started := make(chan connectionRequest, 2)
	u := &window{connectionStarter: func(request connectionRequest, _ bool, _ uint64) { started <- request }}
	u.buildFields()
	u.apiID.SetText("12345")
	u.apiHash.SetText("saved-hash")
	u.botToken.SetText("12345:saved-token")
	u.connectWithMode(true)
	receive := func() connectionRequest {
		select {
		case request := <-started:
			return request
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for connection attempt")
			return connectionRequest{}
		}
	}
	first := receive()

	u.clientMu.Lock()
	u.connecting = false
	u.clientMu.Unlock()
	// Unsaved edits must not disable reconnecting with the last configuration
	// that was actually applied to a client.
	u.apiHash.SetText("")
	u.botToken.SetText("")
	u.ensureConnection()
	second := receive()
	if second != first {
		t.Fatalf("automatic reconnect request = %+v, want last applied %+v", second, first)
	}
}

func TestUploadConcurrencyChangeUpdatesPendingReconnect(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()
	pending := connectionRequest{uploadConcurrency: tgtransport.UploadConcurrencyCompatibility}
	u := &window{
		paths:                coreapp.Paths{Settings: filepath.Join(t.TempDir(), "settings.json")},
		settings:             coreapp.Settings{UploadConcurrency: tgtransport.UploadConcurrencyCompatibility},
		appliedConnection:    pending,
		hasAppliedConnection: true,
		pendingReconnect:     &pending,
	}
	u.buildFields()
	u.handleUploadConcurrencyChanged(uploadConcurrencyFastLabel)

	u.clientMu.RLock()
	applied := u.appliedConnection.uploadConcurrency
	pendingValue := u.pendingReconnect.uploadConcurrency
	u.clientMu.RUnlock()
	if applied != tgtransport.UploadConcurrencyFast || pendingValue != tgtransport.UploadConcurrencyFast {
		t.Fatalf("updated concurrency = applied %d, pending %d; want %d", applied, pendingValue, tgtransport.UploadConcurrencyFast)
	}
}

func TestManualReconnectImmediatelyMakesOldGatewayUnavailable(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	controller := coreapp.NewController(nil, nil)
	client := &tgtransport.Client{}
	controller.SetGateway(client)
	cancelled := false
	u := &window{controller: controller, connected: true, client: client, clientCancel: func() { cancelled = true }}
	u.buildFields()
	u.buildQueue()
	u.apiID.SetText("12345")
	u.apiHash.SetText("new-hash")
	u.botToken.SetText("12345:new-token")
	u.connectWithMode(false)

	if u.connected || !u.isConnecting() || !cancelled {
		t.Fatalf("manual reconnect state: connected=%v connecting=%v cancelled=%v", u.connected, u.isConnecting(), cancelled)
	}
	if err := controller.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "连接 Telegram") {
		t.Fatalf("Start() after reconnect request = %v, want old gateway unavailable", err)
	}
}

func TestManualReconnectUsesLiveControllerRunningState(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()
	fyneWindow := application.NewWindow("connection guard test")
	defer fyneWindow.Close()

	store := &blockingQueueStore{
		jobs: []model.Job{{
			ID:       "active-job",
			Name:     "active.mp4",
			Path:     "unused-because-start-is-cancelled.mp4",
			Size:     1,
			RandomID: 991,
			State:    model.JobQueued,
		}},
		channel:     model.Channel{ID: -1001, AccessHash: 7, Title: "test"},
		saveEntered: make(chan struct{}),
		releaseSave: make(chan struct{}),
	}
	controller := coreapp.NewController(store, nil)
	if err := controller.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	controller.SetGateway(&tgtransport.Client{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDone := make(chan error, 1)
	go func() { startDone <- controller.Start(ctx) }()
	select {
	case <-store.saveEntered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Start persistence")
	}

	startedConnection := make(chan struct{}, 1)
	u := &window{
		application: application,
		window:      fyneWindow,
		controller:  controller,
		// Deliberately stale: the guard must consult Controller.Snapshot().
		snapshot: coreapp.Snapshot{Running: false},
		connectionStarter: func(connectionRequest, bool, uint64) {
			startedConnection <- struct{}{}
		},
	}
	u.buildFields()
	u.apiID.SetText("12345")
	u.apiHash.SetText("new-hash")
	u.botToken.SetText("12345:new-token")
	u.connectWithMode(false)
	select {
	case <-startedConnection:
		t.Fatal("manual reconnect started while the live controller was running")
	case <-time.After(20 * time.Millisecond):
	}

	cancel()
	close(store.releaseSave)
	if err := <-startDone; err != nil {
		t.Fatalf("Start() error = %v", err)
	}
}

func TestForceCloseUsesLiveControllerRunningState(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()
	fyneWindow := application.NewWindow("close guard test")

	store := &blockingQueueStore{
		jobs: []model.Job{{
			ID:       "close-active-job",
			Name:     "close-active.mp4",
			Path:     "unused-because-close-cancels-before-run.mp4",
			Size:     1,
			RandomID: 992,
			State:    model.JobQueued,
		}},
		channel:     model.Channel{ID: -1002, AccessHash: 8, Title: "test"},
		saveEntered: make(chan struct{}),
		releaseSave: make(chan struct{}),
	}
	controller := coreapp.NewController(store, nil)
	if err := controller.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	controller.SetGateway(&tgtransport.Client{})
	startDone := make(chan error, 1)
	go func() { startDone <- controller.Start(context.Background()) }()
	select {
	case <-store.saveEntered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Start persistence")
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	u := &window{
		application: application,
		window:      fyneWindow,
		controller:  controller,
		rootCtx:     rootCtx,
		rootCancel:  rootCancel,
		// Deliberately stale: forceClose must inspect the controller directly.
		snapshot: coreapp.Snapshot{Running: false},
	}
	closeDone := make(chan struct{})
	go func() {
		u.forceClose()
		close(closeDone)
	}()
	// Ensure the shutdown goroutine has actually entered shutdown before the
	// Start save is released. Without this synchronization the queue can fully
	// fail before forceClose is ever scheduled, which does not exercise the
	// intended close-during-start boundary.
	deadline := time.Now().Add(time.Second)
	for !u.isClosed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !u.isClosed() {
		t.Fatal("forceClose did not enter shutdown")
	}
	time.Sleep(10 * time.Millisecond)
	close(store.releaseSave)
	if err := <-startDone; err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("forceClose did not finish")
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshot := controller.Snapshot()
		if !snapshot.Running && len(snapshot.Jobs) == 1 && snapshot.Jobs[0].State == model.JobCancelled {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue after forceClose = %+v, want cancelled live queue", controller.Snapshot())
}

func TestShutdownIsIdempotentAndCancelsLifetimeResources(t *testing.T) {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	clientCancels := 0
	moveCancels := 0
	u := &window{
		rootCtx:      rootCtx,
		rootCancel:   rootCancel,
		clientCancel: func() { clientCancels++ },
		moveCancel:   func() { moveCancels++ },
	}

	u.shutdown(false, false)
	u.shutdown(false, false)
	select {
	case <-rootCtx.Done():
	default:
		t.Fatal("shutdown did not cancel the root context")
	}
	if clientCancels != 1 || moveCancels != 1 {
		t.Fatalf("shutdown cancellation counts = client:%d move:%d, want one each", clientCancels, moveCancels)
	}
	if !u.isClosed() {
		t.Fatal("shutdown did not mark the window closed")
	}
}

func TestConnectionWorkerGateStopsNewWorkersAndWaitsForExistingWorkers(t *testing.T) {
	u := &window{}
	release := make(chan struct{})
	if !u.startConnectionWorker(func() { <-release }) {
		t.Fatal("first connection worker was rejected")
	}
	u.stopConnectionWorkers()
	if u.startConnectionWorker(func() {}) {
		t.Fatal("connection worker started after the gate was stopped")
	}
	if u.waitForConnectionWorkers(10 * time.Millisecond) {
		t.Fatal("worker wait completed before the active worker exited")
	}
	close(release)
	if !u.waitForConnectionWorkers(time.Second) {
		t.Fatal("worker wait did not complete after the active worker exited")
	}
}

func TestSettingsNavigationUsesDedicatedSameWindowPage(t *testing.T) {
	u := &window{
		mainPage:     container.NewVBox(),
		settingsPage: container.NewVBox(),
	}
	u.settingsPage.Hide()
	u.showSettingsPage()
	if u.settingsPage.Hidden || !u.mainPage.Hidden {
		t.Fatal("showSettingsPage did not switch from queue to settings")
	}
	u.showMainPage()
	if u.mainPage.Hidden || !u.settingsPage.Hidden {
		t.Fatal("showMainPage did not switch back to queue")
	}
}

func TestControllerSnapshotDispatchCoalescesToLatestState(t *testing.T) {
	u := &window{}
	first := coreapp.Snapshot{ActiveID: "first"}
	latest := coreapp.Snapshot{ActiveID: "latest"}

	if !u.enqueueControllerSnapshot(first) {
		t.Fatal("first snapshot did not request a UI dispatch")
	}
	for i := 0; i < 1000; i++ {
		if u.enqueueControllerSnapshot(coreapp.Snapshot{ActiveID: "stale"}) {
			t.Fatal("intermediate snapshot queued a duplicate UI dispatch")
		}
	}
	if u.enqueueControllerSnapshot(latest) {
		t.Fatal("latest snapshot queued a duplicate UI dispatch")
	}
	got, ok := u.takeControllerSnapshot()
	if !ok || got.ActiveID != latest.ActiveID {
		t.Fatalf("dispatched snapshot = (%+v, %v), want latest active ID %q", got, ok, latest.ActiveID)
	}
	if _, ok := u.takeControllerSnapshot(); ok {
		t.Fatal("empty dispatcher returned a snapshot")
	}
	if !u.enqueueControllerSnapshot(first) {
		t.Fatal("dispatcher did not accept a new snapshot after draining")
	}
}

func TestControllerProgressDispatchCoalescesToLatestUpdate(t *testing.T) {
	u := &window{}
	first := coreapp.ProgressUpdate{JobID: "job", AttemptID: 1, Uploaded: 10}
	latest := coreapp.ProgressUpdate{JobID: "job", AttemptID: 1, Uploaded: 90}
	if !u.enqueueControllerProgress(first) {
		t.Fatal("first progress did not request a UI dispatch")
	}
	for i := 0; i < 1000; i++ {
		if u.enqueueControllerProgress(coreapp.ProgressUpdate{JobID: "job", AttemptID: 1, Uploaded: int64(i)}) {
			t.Fatal("intermediate progress queued a duplicate UI dispatch")
		}
	}
	if u.enqueueControllerProgress(latest) {
		t.Fatal("latest progress queued a duplicate UI dispatch")
	}
	got, ok := u.takeControllerProgress()
	if !ok || got != latest {
		t.Fatalf("dispatched progress = (%+v, %v), want latest %+v", got, ok, latest)
	}
	if _, ok := u.takeControllerProgress(); ok {
		t.Fatal("empty progress dispatcher returned an update")
	}
}

func TestJobStatusShowsRetryWaitInsteadOfZeroUploadSpeed(t *testing.T) {
	job := model.Job{
		State: model.JobUploading,
		Size:  100,
		Error: "连接中断，5 秒后自动重试（2/5）",
	}
	if got := compactJobStatus(job); got != job.Error {
		t.Fatalf("compactJobStatus() = %q, want retry detail %q", got, job.Error)
	}
	if got := jobStatus(job); got != job.Error {
		t.Fatalf("jobStatus() = %q, want retry detail %q", got, job.Error)
	}
}

func TestCompactUploadingStatusIncludesTransferredTotalAndSpeed(t *testing.T) {
	job := model.Job{
		State:          model.JobUploading,
		Uploaded:       512 * 1024 * 1024,
		Size:           1024 * 1024 * 1024,
		BytesPerSecond: 10 * 1024 * 1024,
	}
	want := "上传中 · 512.0 MiB / 1.0 GiB · 10.0 MiB/s"
	if got := compactJobStatus(job); got != want {
		t.Fatalf("compactJobStatus() = %q, want %q", got, want)
	}
}

func TestJobFractionKeepsPartialProgressAcrossStates(t *testing.T) {
	states := []model.JobState{
		model.JobUploading,
		model.JobSending,
		model.JobFailed,
		model.JobConfirming,
		model.JobCancelled,
		model.JobInterrupted,
	}
	for _, state := range states {
		job := model.Job{Size: 100, Uploaded: 40, State: state}
		if got := jobFraction(job); got != 0.4 {
			t.Errorf("jobFraction(%s) = %v, want 0.4", state, got)
		}
	}
	if got := jobFraction(model.Job{Size: 100, State: model.JobSent}); got != 1 {
		t.Errorf("jobFraction(sent) = %v, want 1", got)
	}
}

func TestFormatSummaryDoesNotReportHundredPercentAfterFailure(t *testing.T) {
	snapshot := coreapp.Snapshot{
		Jobs: []model.Job{
			{State: model.JobSent},
			{State: model.JobSent},
			{State: model.JobSent},
			{State: model.JobFailed},
		},
		DoneBytes:  30,
		TotalBytes: 40,
	}
	got := formatSummary(snapshot)
	if !strings.Contains(got, "30 B / 40 B") || !strings.Contains(got, "已完成 3 / 4") {
		t.Fatalf("formatSummary() = %q, want stable failed-file denominator", got)
	}
}

func TestJobStatusShowsCompatibleTruncatedMP4Warning(t *testing.T) {
	job := model.Job{
		State:    model.JobUploading,
		Size:     100,
		Uploaded: 25,
		Metadata: model.VideoMetadata{TruncatedMediaData: true},
	}
	got := jobStatus(job)
	if !strings.Contains(got, "上传中") || !strings.Contains(got, "源 MP4 尾部结构不完整") || !strings.Contains(got, "原样传输") {
		t.Fatalf("jobStatus() = %q, want upload state and visible compatibility warning", got)
	}
}

func TestCompactQueueRowStaysWithinOneLineHeight(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	row := newJobRow()
	if got := row.MinSize().Height; got > 56 {
		t.Fatalf("compact row minimum height = %.1f, want at most 56", got)
	}
}

func TestQueueSelectionUsesJobIDsAndPrunesRemovedRows(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildFields()
	u.buildQueue()
	jobs := []model.Job{
		{ID: "first", Position: 0, Name: "first.mp4", Size: 100, State: model.JobQueued},
		{ID: "second", Position: 1, Name: "second.mp4", Size: 200, State: model.JobSent},
	}
	u.snapshot = coreapp.Snapshot{Jobs: jobs}
	u.refreshQueueRows(jobs)
	u.selectAllJobs()
	if len(u.selectedJobs) != 2 {
		t.Fatalf("selection = %#v, want both job IDs selected", u.selectedJobs)
	}
	if u.removeSelected.Disabled() || u.removeCompleted.Disabled() {
		t.Fatal("queue management buttons did not enable for selected/completed jobs")
	}

	remaining := []model.Job{jobs[1]}
	remaining[0].Position = 0
	u.snapshot = coreapp.Snapshot{Jobs: remaining}
	u.refreshQueueRows(remaining)
	if u.selectedJobs["first"] {
		t.Fatalf("removed job selection was retained: selection=%#v", u.selectedJobs)
	}
	if _, exists := u.queueIndex["first"]; exists {
		t.Fatal("removed job remained in the virtual-list index")
	}
	if !u.selectedJobs["second"] {
		t.Fatal("remaining job lost its ID-based selection")
	}
}

func TestResetJobCountsIncludeOnlySafeRecoverableStates(t *testing.T) {
	jobs := []model.Job{
		{ID: "cancelled", State: model.JobCancelled},
		{ID: "failed", State: model.JobFailed},
		{ID: "interrupted", State: model.JobInterrupted},
		{ID: "skipped", State: model.JobSkipped},
		{ID: "confirming", State: model.JobConfirming},
		{ID: "sent", State: model.JobSent},
		{ID: "moved", State: model.JobMoved},
		{ID: "oversize", State: model.JobOversize},
		{ID: "queued", State: model.JobQueued},
	}
	counts := resetJobCountsFor(jobs, map[string]bool{
		"cancelled": true,
		"sent":      true,
	})
	if counts.Cancelled != 1 || counts.Failed != 2 || counts.Skipped != 1 || counts.All != 4 || counts.Selected != 1 {
		t.Fatalf("reset counts = %+v, want cancelled=1 failed=2 skipped=1 all=4 selected=1", counts)
	}
}

func TestResetControlAllowsRecoverableJobsWhileRunning(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildFields()
	u.buildQueue()
	u.snapshot = coreapp.Snapshot{Jobs: []model.Job{{ID: "cancelled", State: model.JobCancelled}}}
	u.refreshQueueRows(u.snapshot.Jobs)
	u.updateActionAvailability()
	if u.resetJobsButton.Disabled() {
		t.Fatal("reset button disabled for an idle cancelled task")
	}

	u.snapshot.Running = true
	u.updateActionAvailability()
	if u.resetJobsButton.Disabled() {
		t.Fatal("reset button disabled while the queue is running")
	}

	u.snapshot = coreapp.Snapshot{Jobs: []model.Job{{ID: "sent", State: model.JobSent}}}
	u.refreshQueueRows(u.snapshot.Jobs)
	u.updateActionAvailability()
	if !u.resetJobsButton.Disabled() {
		t.Fatal("reset button enabled when the queue only contains completed tasks")
	}
}

func TestRunnableQueueKeepsStartEnabledWhileTelegramIsDisconnected(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildFields()
	u.buildQueue()
	u.snapshot = coreapp.Snapshot{
		Channel: model.Channel{ID: 1},
		Jobs:    []model.Job{{ID: "queued", State: model.JobQueued}},
	}
	u.refreshQueueRows(u.snapshot.Jobs)
	u.updateActionAvailability()
	if u.startButton.Disabled() {
		t.Fatal("start button disabled for a runnable disconnected queue; clicking it should explain how to connect")
	}
	if !u.bindButton.Disabled() {
		t.Fatal("bind button enabled without a Telegram connection")
	}
}

func TestQueueSelectionAndCompletedRemovalStayAvailableWhileUploading(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{connected: true}
	u.buildFields()
	u.buildQueue()
	u.snapshot = coreapp.Snapshot{
		Running:  true,
		ActiveID: "active",
		Channel:  model.Channel{ID: 1},
		Jobs: []model.Job{
			{ID: "done", Position: 0, State: model.JobSent},
			{ID: "active", Position: 1, State: model.JobUploading, Size: 100},
			{ID: "queued", Position: 2, State: model.JobQueued},
		},
	}
	u.refreshQueueRows(u.snapshot.Jobs)
	u.selectAllJobs()
	u.updateActionAvailability()

	if len(u.selectedJobs) != 3 {
		t.Fatalf("selected jobs = %#v, want all jobs selectable while uploading", u.selectedJobs)
	}
	if u.removeSelected.Disabled() || u.removeCompleted.Disabled() || u.clearQueueButton.Disabled() {
		t.Fatal("safe queue-management controls were disabled while uploading")
	}
}

func TestCompactRowRebindsSingleActionAcrossStates(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildQueue()
	job := model.Job{ID: "job", Position: 0, Name: "video.mp4", Size: 100, State: model.JobQueued}
	u.refreshQueueRows([]model.Job{job})
	row := newJobRow()
	u.updateJobRow(row, job)
	if row.action.Text != "取消" {
		t.Fatalf("queued action = %q, want 取消", row.action.Text)
	}
	job.State = model.JobFailed
	u.refreshQueueRows([]model.Job{job})
	u.updateJobRow(row, job)
	if row.action.Text != "处理…" {
		t.Fatalf("failed action = %q, want 处理…", row.action.Text)
	}
	job.State = model.JobSent
	u.refreshQueueRows([]model.Job{job})
	u.updateJobRow(row, job)
	if row.action.Text != "详情" {
		t.Fatalf("sent action = %q, want 详情", row.action.Text)
	}
	job.State = model.JobInterrupted
	u.refreshQueueRows([]model.Job{job})
	u.updateJobRow(row, job)
	if row.action.Text != "重试" {
		t.Fatalf("interrupted action = %q, want 重试", row.action.Text)
	}
}

func TestCanonicalPathKeyNormalizesEquivalentPaths(t *testing.T) {
	base := t.TempDir()
	plain := filepath.Join(base, "video.mp4")
	equivalent := filepath.Join(base, ".", "video.mp4")
	if got, want := canonicalPathKey(equivalent), canonicalPathKey(plain); got != want {
		t.Fatalf("canonicalPathKey() = %q, want %q", got, want)
	}
}

type blockingQueueStore struct {
	jobs        []model.Job
	channel     model.Channel
	paused      bool
	saveEntered chan struct{}
	releaseSave chan struct{}
	blockOnce   sync.Once
}

func (s *blockingQueueStore) Save(jobs []model.Job, channel model.Channel, paused bool) error {
	s.jobs = append([]model.Job(nil), jobs...)
	s.channel = channel
	s.paused = paused
	s.blockOnce.Do(func() {
		close(s.saveEntered)
		<-s.releaseSave
	})
	return nil
}

func (s *blockingQueueStore) Load() ([]model.Job, model.Channel, bool, error) {
	return append([]model.Job(nil), s.jobs...), s.channel, s.paused, nil
}

func TestCandidateChoicesSelectOnlyFilesNotAlreadyQueued(t *testing.T) {
	base := t.TempDir()
	existingPath := filepath.Join(base, "existing.mp4")
	existing := []model.Job{{ID: "existing", Path: existingPath}}
	candidates := []model.Job{
		{ID: "duplicate", Path: filepath.Join(base, ".", "existing.mp4")},
		{ID: "new", Path: filepath.Join(base, "new.mp4")},
	}
	choices := newCandidateChoices(existing, candidates)
	if len(choices) != 2 {
		t.Fatalf("candidate choice count = %d, want 2", len(choices))
	}
	if !choices[0].duplicate || choices[0].selected {
		t.Fatalf("duplicate choice = %+v, want disabled by default", choices[0])
	}
	if choices[1].duplicate || !choices[1].selected {
		t.Fatalf("new choice = %+v, want selected by default", choices[1])
	}
}

func TestScanningOnlyPreventsStartingAnotherFolderScan(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{connected: true}
	u.buildFields()
	u.buildQueue()
	u.snapshot = coreapp.Snapshot{
		Channel: model.Channel{ID: 1},
		Jobs: []model.Job{
			{ID: "queued", State: model.JobQueued},
			{ID: "oversize", State: model.JobOversize},
		},
	}
	u.scanMu.Lock()
	u.scanning = true
	u.scanMu.Unlock()
	u.refreshQueueRows(u.snapshot.Jobs)
	u.updateActionAvailability()

	if !u.chooseFolderButton.Disabled() || u.startButton.Disabled() || u.moveButton.Disabled() {
		t.Fatalf("scan availability: add=%v start=%v move=%v, want only add disabled", u.chooseFolderButton.Disabled(), u.startButton.Disabled(), u.moveButton.Disabled())
	}
}

func TestPauseControlsReflectRunningAndPausedStates(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{connected: true, scheduler: newScheduleCoordinator(nil)}
	defer u.scheduler.Stop()
	u.buildFields()
	u.buildQueue()
	job := model.Job{ID: "queued", Position: 0, Name: "queued.mp4", Size: 100, State: model.JobQueued}

	u.snapshot = coreapp.Snapshot{
		Running: true,
		Channel: model.Channel{ID: 1},
		Jobs:    []model.Job{job},
	}
	u.updateActionAvailability()
	if u.pauseButton.Disabled() {
		t.Fatal("pause button disabled while queue is running")
	}
	if !u.startButton.Disabled() {
		t.Fatal("start button enabled while queue is running")
	}
	if got := u.startButton.Text; got != "开始上传" {
		t.Fatalf("running start button text = %q, want 开始上传", got)
	}

	u.snapshot.PauseRequested = true
	u.updateActionAvailability()
	if !u.pauseButton.Disabled() {
		t.Fatal("pause button enabled while pause request is pending")
	}

	u.applySnapshot(coreapp.Snapshot{
		Paused:  true,
		Channel: model.Channel{ID: 1},
		Jobs:    []model.Job{job},
	})
	if got := u.startButton.Text; got != "继续上传" {
		t.Fatalf("paused start button text = %q, want 继续上传", got)
	}
	if u.startButton.Disabled() {
		t.Fatal("continue button disabled for a runnable paused queue")
	}
	if u.pauseButton.Disabled() != true {
		t.Fatal("pause button enabled while queue is idle and paused")
	}
	if !u.scheduleButton.Disabled() {
		t.Fatal("schedule button enabled while queue is manually paused")
	}
	if u.selectAllButton.Disabled() || u.clearQueueButton.Disabled() {
		t.Fatal("queue editing stayed disabled after the queue became paused and idle")
	}
	if !strings.Contains(u.operationLabel.Text, "队列已暂停") {
		t.Fatalf("paused operation status = %q, want explicit paused guidance", u.operationLabel.Text)
	}
}

func TestScheduledStartWaitsForManualContinueWhenPaused(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	startAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, testCase := range []struct {
		name           string
		paused         bool
		pauseRequested bool
	}{
		{name: "paused idle", paused: true},
		{name: "pause requested", pauseRequested: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			coordinator := newScheduleCoordinator(nil)
			defer coordinator.Stop()
			coordinator.HoldDue(startAt)
			u := &window{
				connected: true,
				scheduler: coordinator,
				snapshot: coreapp.Snapshot{
					Running:        testCase.pauseRequested,
					Paused:         testCase.paused || testCase.pauseRequested,
					PauseRequested: testCase.pauseRequested,
					Channel:        model.Channel{ID: 1},
					Jobs:           []model.Job{{ID: "queued", State: model.JobQueued}},
				},
			}
			u.buildFields()
			u.buildQueue()
			u.updateScheduleStatus()
			if !strings.Contains(u.scheduleLabel.Text, "等待手动继续") {
				t.Fatalf("schedule status = %q, want manual-continue guidance", u.scheduleLabel.Text)
			}

			u.tryScheduledStart()
			stateAt, set, due := coordinator.State()
			if !set || !due || !stateAt.Equal(startAt) {
				t.Fatalf("scheduled start was consumed while paused: (%v, %v, %v)", stateAt, set, due)
			}
		})
	}
}

func TestParseScheduledTimeUsesRequestedLocalZone(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	got, err := parseScheduledTime("2026-08-17", "09:35", location)
	if err != nil {
		t.Fatal(err)
	}
	if got.Location() != location || got.Year() != 2026 || got.Month() != time.August || got.Day() != 17 || got.Hour() != 9 || got.Minute() != 35 {
		t.Fatalf("parsed schedule = %v, want 2026-08-17 09:35 in requested zone", got)
	}
	if _, err := parseScheduledTime("2026/08/17", "9:35", location); err == nil {
		t.Fatal("invalid schedule format was accepted")
	}
}

func TestBeginUploadsRestoresScheduleWhenControllerCannotStart(t *testing.T) {
	tests := []struct {
		name      string
		startAt   time.Time
		scheduled bool
		wantDue   bool
	}{
		{name: "manual start preserves future schedule", startAt: time.Now().Add(time.Hour).Truncate(time.Second), scheduled: false, wantDue: false},
		{name: "scheduled start remains due", startAt: time.Now().Add(-time.Minute).Truncate(time.Second), scheduled: true, wantDue: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settingsPath := filepath.Join(t.TempDir(), "settings.json")
			coordinator := newScheduleCoordinator(nil)
			defer coordinator.Stop()
			if test.wantDue {
				coordinator.HoldDue(test.startAt)
			} else {
				coordinator.Set(test.startAt)
			}
			u := &window{
				controller: coreapp.NewController(nil, nil),
				paths:      coreapp.Paths{Settings: settingsPath},
				settings:   coreapp.Settings{ScheduledStartUnix: test.startAt.Unix()},
				rootCtx:    context.Background(),
				scheduler:  coordinator,
			}
			if err := u.beginUploads(test.scheduled); err == nil {
				t.Fatal("beginUploads() error = nil without Telegram gateway")
			}
			loaded, err := coreapp.LoadSettings(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.ScheduledStartUnix != test.startAt.Unix() {
				t.Fatalf("persisted schedule = %d, want %d", loaded.ScheduledStartUnix, test.startAt.Unix())
			}
			stateAt, set, due := coordinator.State()
			if !set || due != test.wantDue || !stateAt.Equal(test.startAt) {
				t.Fatalf("restored schedule = (%v, %v, %v), want (%v, true, %v)", stateAt, set, due, test.startAt, test.wantDue)
			}
		})
	}
}

func TestNextConnectionRetryDelayUsesBoundedBackoff(t *testing.T) {
	tests := []struct {
		previous time.Duration
		want     time.Duration
	}{
		{previous: 0, want: 5 * time.Second},
		{previous: 5 * time.Second, want: 10 * time.Second},
		{previous: 10 * time.Second, want: 20 * time.Second},
		{previous: 20 * time.Second, want: 40 * time.Second},
		{previous: 40 * time.Second, want: time.Minute},
		{previous: time.Minute, want: time.Minute},
		{previous: 10 * time.Minute, want: time.Minute},
	}
	for _, test := range tests {
		if got := nextConnectionRetryDelay(test.previous); got != test.want {
			t.Fatalf("nextConnectionRetryDelay(%v) = %v, want %v", test.previous, got, test.want)
		}
	}
}
