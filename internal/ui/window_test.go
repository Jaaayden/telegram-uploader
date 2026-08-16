package ui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"
	coreapp "github.com/jayden/telegram-video-uploader/internal/app"
	"github.com/jayden/telegram-video-uploader/internal/model"
)

func TestRefreshQueueRowsShowsNamesProgressAndReusesRows(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildQueue()
	jobs := []model.Job{
		{ID: "first", Position: 0, Name: "01-first.mp4", Size: 100, State: model.JobQueued},
		{ID: "second", Position: 1, Name: "02-second.mp4", Size: 200, State: model.JobQueued},
	}
	u.refreshQueueRows(jobs)

	if len(u.queueRows.Objects) != len(jobs) {
		t.Fatalf("rendered rows = %d, want %d", len(u.queueRows.Objects), len(jobs))
	}
	first := u.jobRows["first"]
	if first == nil || !strings.Contains(first.name.Text, jobs[0].Name) {
		t.Fatalf("first row name = %v, want %q", first, jobs[0].Name)
	}
	if first.progress.Value != 0 {
		t.Fatalf("initial first progress = %v, want 0", first.progress.Value)
	}

	jobs[0].State = model.JobUploading
	jobs[0].Uploaded = 50
	jobs[0].BytesPerSecond = 25
	u.refreshQueueRows(jobs)
	if u.jobRows["first"] != first {
		t.Fatal("progress refresh replaced the stable row widget")
	}
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
	if len(u.selectedJobs) != 2 || !u.jobRows["first"].selected.Checked || !u.jobRows["second"].selected.Checked {
		t.Fatalf("selection = %#v, want both job IDs selected", u.selectedJobs)
	}
	if u.removeSelected.Disabled() || u.removeCompleted.Disabled() {
		t.Fatal("queue management buttons did not enable for selected/completed jobs")
	}

	remaining := []model.Job{jobs[1]}
	remaining[0].Position = 0
	u.snapshot = coreapp.Snapshot{Jobs: remaining}
	u.refreshQueueRows(remaining)
	if u.selectedJobs["first"] || u.jobRows["first"] != nil {
		t.Fatalf("removed job selection/row was retained: selection=%#v rows=%#v", u.selectedJobs, u.jobRows)
	}
	if !u.selectedJobs["second"] || !u.jobRows["second"].selected.Checked {
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

func TestResetControlRequiresIdleRecoverableJobs(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildFields()
	u.buildQueue()
	u.snapshot = coreapp.Snapshot{Jobs: []model.Job{{ID: "cancelled", State: model.JobCancelled}}}
	u.updateActionAvailability()
	if u.resetJobsButton.Disabled() {
		t.Fatal("reset button disabled for an idle cancelled task")
	}

	u.snapshot.Running = true
	u.updateActionAvailability()
	if !u.resetJobsButton.Disabled() {
		t.Fatal("reset button enabled while the queue is running")
	}

	u.snapshot = coreapp.Snapshot{Jobs: []model.Job{{ID: "sent", State: model.JobSent}}}
	u.updateActionAvailability()
	if !u.resetJobsButton.Disabled() {
		t.Fatal("reset button enabled when the queue only contains completed tasks")
	}
}

func TestCompactRowRebindsSingleActionAcrossStates(t *testing.T) {
	application := test.NewApp()
	defer application.Quit()

	u := &window{}
	u.buildQueue()
	job := model.Job{ID: "job", Position: 0, Name: "video.mp4", Size: 100, State: model.JobQueued}
	u.refreshQueueRows([]model.Job{job})
	row := u.jobRows[job.ID]
	if row.action.Text != "取消" {
		t.Fatalf("queued action = %q, want 取消", row.action.Text)
	}
	job.State = model.JobFailed
	u.refreshQueueRows([]model.Job{job})
	if row.action.Text != "处理…" {
		t.Fatalf("failed action = %q, want 处理…", row.action.Text)
	}
	job.State = model.JobSent
	u.refreshQueueRows([]model.Job{job})
	if row.action.Text != "详情" {
		t.Fatalf("sent action = %q, want 详情", row.action.Text)
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

func TestScanningDisablesStartAndMoveActions(t *testing.T) {
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
	u.updateActionAvailability()

	if !u.chooseFolderButton.Disabled() || !u.startButton.Disabled() || !u.moveButton.Disabled() {
		t.Fatalf("scan availability: add=%v start=%v move=%v, want all disabled", u.chooseFolderButton.Disabled(), u.startButton.Disabled(), u.moveButton.Disabled())
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

func TestNextScheduleRetryDelayUsesBoundedBackoff(t *testing.T) {
	tests := []struct {
		previous time.Duration
		want     time.Duration
	}{
		{previous: 0, want: 5 * time.Second},
		{previous: 5 * time.Second, want: 10 * time.Second},
		{previous: 40 * time.Second, want: time.Minute},
		{previous: time.Minute, want: time.Minute},
		{previous: 10 * time.Minute, want: time.Minute},
	}
	for _, test := range tests {
		if got := nextScheduleRetryDelay(test.previous); got != test.want {
			t.Fatalf("nextScheduleRetryDelay(%v) = %v, want %v", test.previous, got, test.want)
		}
	}
}
