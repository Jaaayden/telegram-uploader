package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/jayden/telegram-video-uploader/internal/model"
	tgtransport "github.com/jayden/telegram-video-uploader/internal/telegram"
)

func TestControllerUploadsSeriallyWithExactRequestAndProgress(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "01-first.mp4", RandomID: 7101},
		{Name: "02-second.mp4", RandomID: 7102},
	})
	channel := model.Channel{ID: -100123, AccessHash: 456, Title: "test channel"}
	store := &memoryQueueStore{jobs: jobs, channel: channel}
	gateway := &fakeGateway{}
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, progress func(model.Progress)) (int, error) {
		info, err := os.Stat(request.Path)
		if err != nil {
			return 0, err
		}
		progress(model.Progress{
			BytesDone:      info.Size() / 2,
			BytesTotal:     info.Size(),
			BytesPerSecond: 128,
			At:             time.Now(),
		})
		progress(model.Progress{
			BytesDone:      info.Size(),
			BytesTotal:     info.Size(),
			BytesPerSecond: 128,
			At:             time.Now(),
		})
		if request.BeforeSend == nil {
			return 0, errors.New("missing pre-send durability callback")
		}
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		return int(request.RandomID), nil
	}

	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && allJobsState(snapshot.Jobs, model.JobSent)
	})

	calls := gateway.Calls()
	if len(calls) != len(jobs) {
		t.Fatalf("UploadVideo calls = %d, want %d", len(calls), len(jobs))
	}
	if gateway.MaxActive() != 1 {
		t.Fatalf("maximum concurrent uploads = %d, want serial uploads", gateway.MaxActive())
	}
	for i, call := range calls {
		if call.Name != jobs[i].Name {
			t.Errorf("call %d Name = %q, want %q", i, call.Name, jobs[i].Name)
		}
		if want := strings.TrimSuffix(jobs[i].Name, filepath.Ext(jobs[i].Name)); call.Caption != want {
			t.Errorf("call %d Caption = %q, want filename without extension %q", i, call.Caption, want)
		}
		if call.RandomID != jobs[i].RandomID {
			t.Errorf("call %d RandomID = %d, want %d", i, call.RandomID, jobs[i].RandomID)
		}
		if call.Channel != channel {
			t.Errorf("call %d Channel = %+v, want %+v", i, call.Channel, channel)
		}
	}

	var total int64
	for _, job := range jobs {
		total += job.Size
	}
	if final.DoneBytes != total || final.TotalBytes != total {
		t.Fatalf("final byte progress = %d/%d, want %d/%d", final.DoneBytes, final.TotalBytes, total, total)
	}
	for i, job := range final.Jobs {
		if job.State != model.JobSent {
			t.Errorf("job %d state = %s, want sent", i, job.State)
		}
		if job.Uploaded != jobs[i].Size {
			t.Errorf("job %d Uploaded = %d, want %d", i, job.Uploaded, jobs[i].Size)
		}
		if job.MessageID != int(jobs[i].RandomID) {
			t.Errorf("job %d MessageID = %d, want %d", i, job.MessageID, jobs[i].RandomID)
		}
		if job.ChannelID != channel.ID {
			t.Errorf("job %d ChannelID = %d, want %d", i, job.ChannelID, channel.ID)
		}
	}
}

func TestCaptionFromFilenameRemovesOnlyMP4Suffix(t *testing.T) {
	tests := map[string]string{
		"episode.01.mp4": "episode.01",
		"video.MP4":      "video",
		"trailer.mov":    "trailer.mov",
		"no-extension":   "no-extension",
		".mp4":           "",
	}
	for name, want := range tests {
		if got := captionFromFilename(name); got != want {
			t.Errorf("captionFromFilename(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestControllerPauseFinishesActiveJobThenStopsAndResumes(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "pause-first.mp4", RandomID: 7111},
		{Name: "pause-second.mp4", RandomID: 7112},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var signal sync.Once
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, progress func(model.Progress)) (int, error) {
		if request.RandomID == jobs[0].RandomID {
			signal.Do(func() { close(firstStarted) })
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		info, err := os.Stat(request.Path)
		if err != nil {
			return 0, err
		}
		progress(model.Progress{BytesDone: info.Size(), BytesTotal: info.Size(), At: time.Now()})
		return int(request.RandomID), nil
	}

	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first upload")
	}
	if err := controller.Pause(); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	paused := controller.Snapshot()
	if !paused.Running || !paused.Paused || !paused.PauseRequested {
		t.Fatalf("snapshot after Pause() = %+v, want running pause request", paused)
	}
	if paused.Jobs[1].State != model.JobQueued {
		t.Fatalf("pending job after Pause() = %+v, want queued", paused.Jobs[1])
	}

	close(releaseFirst)
	stopped := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && snapshot.Paused && snapshot.Jobs[0].State == model.JobSent && snapshot.Jobs[1].State == model.JobQueued
	})
	if stopped.PauseRequested {
		t.Fatalf("pause request remained set after queue stopped: %+v", stopped)
	}
	if calls := gateway.Calls(); len(calls) != 1 {
		t.Fatalf("UploadVideo calls after soft pause = %d, want 1", len(calls))
	}
	if err := controller.Resume(); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if snapshot := controller.Snapshot(); snapshot.Paused || snapshot.PauseRequested {
		t.Fatalf("snapshot after Resume() = %+v, want active pause cleared", snapshot)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() after Resume() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && allJobsState(snapshot.Jobs, model.JobSent)
	})
	if final.Jobs[1].Error != "" {
		t.Fatalf("resumed job error = %q, want empty", final.Jobs[1].Error)
	}
	if calls := gateway.Calls(); len(calls) != 2 || calls[1].RandomID != jobs[1].RandomID {
		t.Fatalf("UploadVideo calls after Resume() = %#v, want second job only", calls)
	}
}

func TestControllerPauseIdlePersistsAndIsIdempotent(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "idle-pause.mp4", RandomID: 7121}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})

	if err := controller.Pause(); err != nil {
		t.Fatalf("first Pause() error = %v", err)
	}
	savesAfterFirstPause := store.savesCount()
	if err := controller.Pause(); err != nil {
		t.Fatalf("second Pause() error = %v", err)
	}
	if got := store.savesCount(); got != savesAfterFirstPause {
		t.Fatalf("idempotent Pause() performed %d additional saves", got-savesAfterFirstPause)
	}
	snapshot := controller.Snapshot()
	if snapshot.Running || !snapshot.Paused || snapshot.PauseRequested {
		t.Fatalf("idle paused snapshot = %+v", snapshot)
	}
	if err := controller.Resume(); err != nil {
		t.Fatalf("first Resume() error = %v", err)
	}
	savesAfterFirstResume := store.savesCount()
	if err := controller.Resume(); err != nil {
		t.Fatalf("second Resume() error = %v", err)
	}
	if got := store.savesCount(); got != savesAfterFirstResume {
		t.Fatalf("idempotent Resume() performed %d additional saves", got-savesAfterFirstResume)
	}
	snapshot = controller.Snapshot()
	if snapshot.Paused || snapshot.PauseRequested || snapshot.Jobs[0].Error != "" {
		t.Fatalf("idle resumed snapshot = %+v", snapshot)
	}
}

func TestControllerPauseAndResumeRollbackOnPersistenceFailure(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "pause-persist.mp4", RandomID: 7123}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})
	saveFailure := errors.New("simulated pause persistence failure")

	store.saveErr = saveFailure
	if err := controller.Pause(); !errors.Is(err, saveFailure) {
		t.Fatalf("Pause() error = %v, want %v", err, saveFailure)
	}
	if snapshot := controller.Snapshot(); snapshot.Paused || snapshot.PauseRequested {
		t.Fatalf("failed Pause() left in-memory pause active: %+v", snapshot)
	}
	if _, _, paused, err := store.Load(); err != nil || paused {
		t.Fatalf("failed Pause() persisted state = (%v, %v), want unpaused", paused, err)
	}

	store.saveErr = nil
	if err := controller.Pause(); err != nil {
		t.Fatalf("Pause() before Resume test error = %v", err)
	}
	store.saveErr = saveFailure
	if err := controller.Resume(); !errors.Is(err, saveFailure) {
		t.Fatalf("Resume() error = %v, want %v", err, saveFailure)
	}
	if snapshot := controller.Snapshot(); !snapshot.Paused || snapshot.PauseRequested {
		t.Fatalf("failed Resume() did not restore pause: %+v", snapshot)
	}
	store.saveErr = nil
	if _, _, paused, err := store.Load(); err != nil || !paused {
		t.Fatalf("failed Resume() persisted state = (%v, %v), want paused", paused, err)
	}
}

func TestControllerStartFailureRestoresPausedQueue(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "paused-start-failure.mp4", RandomID: 7124}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})
	if err := controller.Pause(); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	saveFailure := errors.New("simulated start persistence failure")
	store.saveErr = saveFailure
	if err := controller.Start(context.Background()); !errors.Is(err, saveFailure) {
		t.Fatalf("Start() error = %v, want %v", err, saveFailure)
	}
	if snapshot := controller.Snapshot(); snapshot.Running || !snapshot.Paused || snapshot.PauseRequested {
		t.Fatalf("snapshot after failed paused Start() = %+v, want idle paused", snapshot)
	}
}

func TestControllerLoadRestoresPersistedPause(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "restore-pause.mp4", RandomID: 7122}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	first := newLoadedController(t, store, &fakeGateway{})
	if err := first.Pause(); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}

	restored := NewController(store, nil)
	if err := restored.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snapshot := restored.Snapshot(); !snapshot.Paused || snapshot.PauseRequested || snapshot.Running {
		t.Fatalf("restored snapshot = %+v, want idle paused queue", snapshot)
	}
}

func TestControllerLoadRejectsActiveQueue(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "load-active.mp4", RandomID: 7123}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	gateway := &fakeGateway{upload: func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return int(request.RandomID), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active upload")
	}
	if err := controller.Load(); err == nil {
		t.Fatal("Load() error = nil while queue is active")
	}
	active := controller.Snapshot()
	if !active.Running || active.ActiveID != jobs[0].ID || len(active.Jobs) != 1 {
		t.Fatalf("active queue changed after rejected Load(): %+v", active)
	}
	close(release)
	waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && snapshot.Jobs[0].State == model.JobSent
	})
}

func TestControllerCancelAllClearsPendingPause(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "cancel-pause-active.mp4", RandomID: 7131},
		{Name: "cancel-pause-pending.mp4", RandomID: 7132},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	started := make(chan struct{})
	var signal sync.Once
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if request.RandomID == jobs[0].RandomID {
			signal.Do(func() { close(started) })
			<-ctx.Done()
			return 0, ctx.Err()
		}
		return int(request.RandomID), nil
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active upload")
	}
	if err := controller.Pause(); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	if err := controller.CancelAll(); err != nil {
		t.Fatalf("CancelAll() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && !snapshot.Paused && !snapshot.PauseRequested && len(snapshot.Jobs) == 2 &&
			snapshot.Jobs[0].State == model.JobCancelled && snapshot.Jobs[1].State == model.JobCancelled
	})
	for i, job := range final.Jobs {
		if job.Error != "" {
			t.Errorf("cancelled job %d error = %q, want empty", i, job.Error)
		}
	}
}

func TestControllerCancelAllPersistenceFailureLeavesMemoryAndDiskUnchanged(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "cancel-save-queued.mp4", RandomID: 7135},
		{Name: "cancel-save-failed.mp4", RandomID: 7136, State: model.JobFailed},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel(), paused: true}
	controller := newLoadedController(t, store, &fakeGateway{})
	store.mu.Lock()
	store.saveErr = errors.New("queue disk unavailable")
	store.mu.Unlock()

	if err := controller.CancelAll(); err == nil {
		t.Fatal("CancelAll() error = nil, want persistence failure")
	}
	snapshot := controller.Snapshot()
	if !snapshot.Paused || snapshot.Jobs[0].State != model.JobQueued || snapshot.Jobs[1].State != model.JobFailed {
		t.Fatalf("in-memory queue after failed CancelAll() = %+v, want original states and pause", snapshot)
	}
	persisted := store.SnapshotJobs()
	if persisted[0].State != model.JobQueued || persisted[1].State != model.JobFailed {
		t.Fatalf("persisted queue after failed CancelAll() = %+v, want original states", persisted)
	}
}

func TestControllerCancelAllDoesNotRaceIdleQueueMutation(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "existing.mp4", RandomID: 7133}})
	candidates, _ := fixtureJobs(t, []fixtureJob{{Name: "candidate.mp4", RandomID: 7134}})
	candidates[0].ID = "candidate-job"
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})
	saveEntered := make(chan struct{})
	releaseSave := make(chan struct{})
	var once sync.Once
	store.saveHook = func([]model.Job) {
		once.Do(func() { close(saveEntered) })
		<-releaseSave
	}

	addDone := make(chan error, 1)
	go func() {
		_, err := controller.AddJobs(candidates)
		addDone <- err
	}()
	select {
	case <-saveEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for AddJobs persistence")
	}
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- controller.CancelAll() }()
	select {
	case err := <-cancelDone:
		t.Fatalf("CancelAll() returned before the structural save completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseSave)
	if err := <-addDone; err != nil {
		t.Fatalf("AddJobs() error = %v", err)
	}
	if err := <-cancelDone; err != nil {
		t.Fatalf("CancelAll() error = %v", err)
	}
	for _, job := range controller.Snapshot().Jobs {
		if job.ID == jobs[0].ID && job.State != model.JobCancelled {
			t.Fatalf("CancelAll did not cancel existing queue state: %+v", job)
		}
	}
}

func TestControllerPauseResumeIsRaceSafe(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "pause-race.mp4", RandomID: 7141}})
	controller := newLoadedController(t, &memoryQueueStore{jobs: jobs, channel: testChannel()}, &fakeGateway{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if index%2 == 0 {
				_ = controller.Pause()
			} else {
				_ = controller.Resume()
			}
			_ = controller.Snapshot()
		}(i)
	}
	wg.Wait()
}

func TestApplyUploadLimitPreservesHistoryAndJobIdentity(t *testing.T) {
	jobs := []model.Job{
		{ID: "queued", Name: "queued.mp4", Size: 100, State: model.JobQueued, RandomID: 11},
		{ID: "interrupted", Name: "interrupted.mp4", Size: 200, State: model.JobInterrupted, Uploaded: 80, RandomID: 12},
		{
			ID: "formerly-oversize", Name: "small.mp4", Size: 50,
			State: model.JobOversize, Uploaded: 49, BytesPerSecond: 12,
			RandomID: 13, Metadata: model.VideoMetadata{Width: 1920},
			MoveDestination: "/stale/destination.mp4",
		},
		{ID: "sent", Name: "sent.mp4", Size: 500, State: model.JobSent, Uploaded: 500, RandomID: 14},
		{ID: "failed", Name: "failed.mp4", Size: 500, State: model.JobFailed, Uploaded: 20, RandomID: 15},
	}
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := NewController(store, nil)
	if err := controller.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := controller.ApplyUploadLimit(150); err != nil {
		t.Fatalf("ApplyUploadLimit() error = %v", err)
	}

	got := controller.Snapshot().Jobs
	wantStates := []model.JobState{
		model.JobQueued,
		model.JobOversize,
		model.JobQueued,
		model.JobSent,
		model.JobFailed,
	}
	for i := range got {
		if got[i].State != wantStates[i] {
			t.Errorf("job %d state = %s, want %s", i, got[i].State, wantStates[i])
		}
		if got[i].ID != jobs[i].ID || got[i].RandomID != jobs[i].RandomID {
			t.Errorf("job %d identity changed: got ID=%q randomID=%d", i, got[i].ID, got[i].RandomID)
		}
	}
	if got[1].Uploaded != 0 {
		t.Errorf("new oversize job Uploaded = %d, want 0", got[1].Uploaded)
	}
	if got[2].Uploaded != 0 || got[2].BytesPerSecond != 0 || got[2].Metadata != (model.VideoMetadata{}) || got[2].MoveDestination != "" {
		t.Errorf("re-enabled job retained transient state: %+v", got[2])
	}
	if got[4].Uploaded != jobs[4].Uploaded {
		t.Errorf("failed history Uploaded = %d, want %d", got[4].Uploaded, jobs[4].Uploaded)
	}
}

func TestApplyUploadLimitDoesNotPublishFailedPersistence(t *testing.T) {
	jobs := []model.Job{{ID: "queued", Name: "queued.mp4", Size: 200, State: model.JobQueued, RandomID: 16}}
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})
	store.mu.Lock()
	store.saveErr = errors.New("queue disk unavailable")
	store.mu.Unlock()

	if err := controller.ApplyUploadLimit(100); err == nil {
		t.Fatal("ApplyUploadLimit() error = nil, want persistence failure")
	}
	if got := controller.Snapshot().Jobs; !reflect.DeepEqual(got, jobs) {
		t.Fatalf("jobs after failed upload-limit save = %+v, want %+v", got, jobs)
	}
}

func TestApplyUploadLimitDuringUploadUpdatesPendingJobsOnly(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "limit-active.mp4", RandomID: 7211},
		{Name: "limit-pending.mp4", RandomID: 7212},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	gateway := &fakeGateway{upload: func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if request.RandomID == jobs[0].RandomID {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return int(request.RandomID), nil
	}}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active upload")
	}

	if err := controller.ApplyUploadLimit(jobs[1].Size - 1); err != nil {
		t.Fatalf("ApplyUploadLimit() during upload error = %v", err)
	}
	snapshot := controller.Snapshot()
	if snapshot.Jobs[0].State != model.JobUploading || snapshot.Jobs[1].State != model.JobOversize {
		t.Fatalf("states after live limit update = %+v, want active upload preserved and pending job oversize", snapshot.Jobs)
	}
	close(release)
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && snapshot.Jobs[0].State == model.JobSent && snapshot.Jobs[1].State == model.JobOversize
	})
	if calls := gateway.Calls(); len(calls) != 1 {
		t.Fatalf("UploadVideo calls = %d, want oversize pending job skipped", len(calls))
	}
	if final.Jobs[1].Uploaded != 0 {
		t.Fatalf("oversize pending job uploaded bytes = %d, want 0", final.Jobs[1].Uploaded)
	}
}

func TestApplyUploadLimitDoesNotFailJobSelectedBeforeReservation(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "selected-before-limit.mp4", RandomID: 7213}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	controller := newLoadedController(t, store, gateway)

	selected := make(chan struct{})
	release := make(chan struct{})
	controller.beforeUploadAttempt = func(model.Job) {
		close(selected)
		<-release
	}

	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-selected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for queue selection")
	}

	if err := controller.ApplyUploadLimit(jobs[0].Size - 1); err != nil {
		t.Fatalf("ApplyUploadLimit() after selection error = %v", err)
	}
	if state := controller.Snapshot().Jobs[0].State; state != model.JobOversize {
		t.Fatalf("state after live limit update = %s, want %s", state, model.JobOversize)
	}
	close(release)

	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running
	})
	if final.Jobs[0].State != model.JobOversize {
		t.Fatalf("final state = %s, want %s", final.Jobs[0].State, model.JobOversize)
	}
	if final.Jobs[0].Error != "" || final.LastError != "" {
		t.Fatalf("eligibility change was reported as a failure: job error=%q, last error=%q", final.Jobs[0].Error, final.LastError)
	}
	if calls := gateway.Calls(); len(calls) != 0 {
		t.Fatalf("UploadVideo calls = %d, want selected oversize job skipped", len(calls))
	}
}

func TestControllerScanDoesNotPublishFailedPersistence(t *testing.T) {
	oldJobs, _ := fixtureJobs(t, []fixtureJob{{Name: "old.mp4", RandomID: 17}})
	_, scanFolder := fixtureJobs(t, []fixtureJob{{Name: "new.mp4", RandomID: 18}})
	store := &memoryQueueStore{jobs: oldJobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})
	store.mu.Lock()
	store.saveErr = errors.New("queue disk unavailable")
	store.mu.Unlock()

	if err := controller.Scan(scanFolder, 0); err == nil {
		t.Fatal("Scan() error = nil, want persistence failure")
	}
	if got := controller.Snapshot().Jobs; !reflect.DeepEqual(got, oldJobs) {
		t.Fatalf("jobs after failed scan save = %+v, want %+v", got, oldJobs)
	}
}

func TestControllerAddJobsDeduplicatesPathsAndReindexes(t *testing.T) {
	existing, _ := fixtureJobs(t, []fixtureJob{
		{Name: "already-sent.mp4", RandomID: 8101, State: model.JobSent, Uploaded: 4},
		{Name: "waiting.mp4", RandomID: 8102, State: model.JobQueued},
	})
	candidates, _ := fixtureJobs(t, []fixtureJob{{Name: "from-another-folder.mp4", RandomID: 8103}})
	candidates[0].ID = "job-from-another-folder"

	store := &memoryQueueStore{jobs: existing, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})
	duplicate := existing[0]
	duplicate.ID = "new-duplicate-id"
	duplicate.RandomID = 9999
	duplicate.State = model.JobQueued
	duplicate.Path = filepath.Join(filepath.Dir(existing[0].Path), ".", filepath.Base(existing[0].Path))

	if _, err := controller.AddJobs([]model.Job{duplicate, candidates[0], candidates[0]}); err != nil {
		t.Fatalf("AddJobs() error = %v", err)
	}

	got := controller.Snapshot().Jobs
	if len(got) != 3 {
		t.Fatalf("queue length = %d, want 3 after path de-duplication", len(got))
	}
	if got[0].ID != existing[0].ID || got[0].RandomID != existing[0].RandomID || got[0].State != model.JobSent {
		t.Fatalf("existing history was replaced: %+v, want %+v", got[0], existing[0])
	}
	if got[2].ID != candidates[0].ID || got[2].RandomID != candidates[0].RandomID {
		t.Fatalf("new candidate identity changed: %+v, want %+v", got[2], candidates[0])
	}
	for i, job := range got {
		if job.Position != i {
			t.Errorf("job %d Position = %d, want %d", i, job.Position, i)
		}
	}
	if saved := store.SnapshotJobs(); len(saved) != 3 || saved[2].Path != candidates[0].Path {
		t.Fatalf("persisted queue = %#v, want appended candidate", saved)
	}
}

func TestControllerQueueRemovalOperationsReindexAndPersist(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "sent.mp4", RandomID: 8201, State: model.JobSent, Uploaded: 4},
		{Name: "moved.mp4", RandomID: 8202, State: model.JobMoved, Uploaded: 4},
		{Name: "failed.mp4", RandomID: 8203, State: model.JobFailed, Error: "failed"},
		{Name: "queued.mp4", RandomID: 8204, State: model.JobQueued},
		{Name: "cancelled.mp4", RandomID: 8205, State: model.JobCancelled},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})

	if _, err := controller.RemoveCompleted(); err != nil {
		t.Fatalf("RemoveCompleted() error = %v", err)
	}
	got := controller.Snapshot().Jobs
	if len(got) != 3 {
		t.Fatalf("queue length after RemoveCompleted() = %d, want 3", len(got))
	}
	for i, job := range got {
		if job.State == model.JobSent || job.State == model.JobMoved {
			t.Errorf("completed job %d remained in queue: %+v", i, job)
		}
		if job.Position != i {
			t.Errorf("job %d Position = %d, want %d", i, job.Position, i)
		}
	}

	if _, err := controller.RemoveJobs([]string{jobs[3].ID, "does-not-exist"}); err != nil {
		t.Fatalf("RemoveJobs() error = %v", err)
	}
	got = controller.Snapshot().Jobs
	if len(got) != 2 || got[0].Name != "failed.mp4" || got[1].Name != "cancelled.mp4" {
		t.Fatalf("queue after RemoveJobs() = %#v, want failed/cancelled", got)
	}
	for i, job := range got {
		if job.Position != i {
			t.Errorf("remaining job %d Position = %d, want %d", i, job.Position, i)
		}
	}

	if _, err := controller.ClearQueue(); err != nil {
		t.Fatalf("ClearQueue() error = %v", err)
	}
	if got := controller.Snapshot(); len(got.Jobs) != 0 {
		t.Fatalf("snapshot after ClearQueue() = %+v, want empty queue", got)
	}
	if saved := store.SnapshotJobs(); len(saved) != 0 {
		t.Fatalf("persisted queue after ClearQueue() = %#v, want empty", saved)
	}
}

func TestControllerResetJobsByModePreservesSafeHistory(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "queued.mp4", RandomID: 8251, State: model.JobQueued},
		{Name: "cancelled.mp4", RandomID: 8252, State: model.JobCancelled},
		{Name: "failed.mp4", RandomID: 8253, State: model.JobFailed},
		{Name: "interrupted.mp4", RandomID: 8254, State: model.JobInterrupted},
		{Name: "skipped.mp4", RandomID: 8255, State: model.JobSkipped},
		{Name: "confirming.mp4", RandomID: 8256, State: model.JobConfirming},
		{Name: "sent.mp4", RandomID: 8257, State: model.JobSent},
		{Name: "moved.mp4", RandomID: 8258, State: model.JobMoved},
		{Name: "oversize.mp4", RandomID: 8259, State: model.JobOversize},
	})
	now := time.Now()
	for i := 1; i <= 4; i++ {
		jobs[i].Uploaded = 3
		jobs[i].BytesPerSecond = 9
		jobs[i].MessageID = 42
		jobs[i].ChannelID = -10042
		jobs[i].Metadata = model.VideoMetadata{Width: 1920, Height: 1080}
		jobs[i].Error = "old state"
		jobs[i].StartedAt = &now
		jobs[i].CompletedAt = &now
		jobs[i].MoveDestination = "/tmp/old.mp4"
	}

	tests := []struct {
		name      string
		mode      ResetMode
		ids       []string
		wantReset map[int]bool
		wantCount int
	}{
		{name: "selected", mode: ResetSelected, ids: []string{jobs[1].ID, jobs[6].ID}, wantReset: map[int]bool{1: true}, wantCount: 1},
		{name: "cancelled", mode: ResetCancelled, wantReset: map[int]bool{1: true}, wantCount: 1},
		{name: "failed and interrupted", mode: ResetFailed, wantReset: map[int]bool{2: true, 3: true}, wantCount: 2},
		{name: "skipped", mode: ResetSkipped, wantReset: map[int]bool{4: true}, wantCount: 1},
		{name: "all recoverable", mode: ResetAllRecoverable, wantReset: map[int]bool{1: true, 2: true, 3: true, 4: true}, wantCount: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryQueueStore{jobs: cloneJobs(jobs), channel: testChannel(), paused: true}
			controller := newLoadedController(t, store, &fakeGateway{})
			count, err := controller.ResetJobs(test.mode, test.ids)
			if err != nil {
				t.Fatalf("ResetJobs() error = %v", err)
			}
			if count != test.wantCount {
				t.Fatalf("ResetJobs() count = %d, want %d", count, test.wantCount)
			}

			got := controller.Snapshot()
			if !got.Paused {
				t.Fatal("ResetJobs() cleared the durable pause state")
			}
			for i := range got.Jobs {
				if got.Jobs[i].ID != jobs[i].ID || got.Jobs[i].RandomID != jobs[i].RandomID || got.Jobs[i].Path != jobs[i].Path || got.Jobs[i].Position != jobs[i].Position {
					t.Errorf("job %d identity changed: got %+v, want %+v", i, got.Jobs[i], jobs[i])
				}
				if !test.wantReset[i] {
					if !reflect.DeepEqual(got.Jobs[i], jobs[i]) {
						t.Errorf("non-target job %d changed: got %+v, want %+v", i, got.Jobs[i], jobs[i])
					}
					continue
				}
				reset := got.Jobs[i]
				if reset.State != model.JobQueued || reset.Uploaded != 0 || reset.BytesPerSecond != 0 || reset.MessageID != 0 || reset.ChannelID != 0 || reset.Metadata != (model.VideoMetadata{}) || reset.Error != "" || reset.StartedAt != nil || reset.CompletedAt != nil || reset.MoveDestination != "" {
					t.Errorf("reset job %d retained transient state: %+v", i, reset)
				}
			}
			if saved := store.SnapshotJobs(); !reflect.DeepEqual(saved, got.Jobs) {
				t.Fatalf("persisted reset queue = %+v, want %+v", saved, got.Jobs)
			}
		})
	}
}

func TestControllerCancelAllCanBeResetAndUploadedWithoutRescanning(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "first.mp4", RandomID: 8271},
		{Name: "second.mp4", RandomID: 8272},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	controller := newLoadedController(t, store, gateway)

	if err := controller.CancelAll(); err != nil {
		t.Fatalf("CancelAll() error = %v", err)
	}
	if !allJobsState(controller.Snapshot().Jobs, model.JobCancelled) {
		t.Fatalf("jobs after CancelAll() = %+v, want cancelled", controller.Snapshot().Jobs)
	}
	count, err := controller.ResetJobs(ResetCancelled, nil)
	if err != nil {
		t.Fatalf("ResetJobs() error = %v", err)
	}
	if count != len(jobs) || !allJobsState(controller.Snapshot().Jobs, model.JobQueued) {
		t.Fatalf("jobs after reset = %+v, count %d; want %d queued jobs", controller.Snapshot().Jobs, count, len(jobs))
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() after reset error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && allJobsState(snapshot.Jobs, model.JobSent)
	})
	if len(final.Jobs) != len(jobs) || len(gateway.Calls()) != len(jobs) {
		t.Fatalf("upload after reset = %d jobs, %d calls; want %d", len(final.Jobs), len(gateway.Calls()), len(jobs))
	}
}

func TestControllerQueueRemovalCancelsActiveAndContinues(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "active.mp4", RandomID: 8301}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	started := make(chan struct{})
	gateway.upload = func(ctx context.Context, _ tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active upload")
	}
	result, err := controller.RemoveJobs([]string{jobs[0].ID})
	if err != nil {
		t.Fatalf("RemoveJobs() error = %v", err)
	}
	if result.Removed != 0 || len(result.PendingRemovalIDs) != 1 || result.PendingRemovalIDs[0] != jobs[0].ID {
		t.Fatalf("RemoveJobs() result = %+v, want active pending removal", result)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 0 && len(snapshot.PendingRemovalIDs) == 0
	})
	if final.LastError != "" {
		t.Fatalf("final queue error = %q, want empty", final.LastError)
	}
}

func TestMixedRemovalPersistenceFailureDoesNotCancelActiveJob(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Controller, []model.Job) (RemovalResult, error)
	}{
		{
			name: "remove selected",
			run: func(controller *Controller, jobs []model.Job) (RemovalResult, error) {
				return controller.RemoveJobs([]string{jobs[0].ID, jobs[1].ID})
			},
		},
		{
			name: "clear queue",
			run: func(controller *Controller, _ []model.Job) (RemovalResult, error) {
				return controller.ClearQueue()
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobs, _ := fixtureJobs(t, []fixtureJob{
				{Name: "active-persist-failure.mp4", RandomID: 8311},
				{Name: "queued-persist-failure.mp4", RandomID: 8312},
			})
			store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
			started := make(chan struct{})
			release := make(chan struct{})
			cancelled := make(chan struct{})
			var startedOnce sync.Once
			var cancelledOnce sync.Once
			gateway := &fakeGateway{upload: func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
				startedOnce.Do(func() { close(started) })
				select {
				case <-release:
					return int(request.RandomID), nil
				case <-ctx.Done():
					cancelledOnce.Do(func() { close(cancelled) })
					return 0, ctx.Err()
				}
			}}
			controller := newLoadedController(t, store, gateway)
			if err := controller.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for active upload")
			}

			beforeDisk := store.SnapshotJobs()
			saveFailure := errors.New("simulated removal save failure")
			store.mu.Lock()
			store.saveErr = saveFailure
			store.mu.Unlock()

			result, err := test.run(controller, jobs)
			if !errors.Is(err, saveFailure) {
				t.Fatalf("removal error = %v, want %v", err, saveFailure)
			}
			if result.Removed != 0 || len(result.PendingRemovalIDs) != 0 {
				t.Fatalf("RemovalResult = %+v, want no committed or pending changes", result)
			}
			snapshot := controller.Snapshot()
			if snapshot.ActiveID != jobs[0].ID || snapshot.Jobs[0].State != model.JobUploading || len(snapshot.PendingRemovalIDs) != 0 {
				t.Fatalf("active job changed after failed removal: %+v", snapshot)
			}
			assertSameJobSequence(t, store.SnapshotJobs(), beforeDisk)
			select {
			case <-cancelled:
				t.Fatal("active upload was cancelled despite failed queue persistence")
			default:
			}

			store.mu.Lock()
			store.saveErr = nil
			store.mu.Unlock()
			close(release)
			final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
				return !snapshot.Running && len(snapshot.Jobs) == 2 && allJobsState(snapshot.Jobs, model.JobSent)
			})
			if len(final.PendingRemovalIDs) != 0 {
				t.Fatalf("final pending removals = %v, want none", final.PendingRemovalIDs)
			}
		})
	}
}

func TestControllerQueueMutationsPublishOnlyAfterPersistenceSucceeds(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "sent.mp4", RandomID: 8401, State: model.JobSent, Uploaded: 4},
		{Name: "queued.mp4", RandomID: 8402, State: model.JobQueued},
		{Name: "failed.mp4", RandomID: 8404, State: model.JobFailed, Error: "failed"},
	})
	candidates, _ := fixtureJobs(t, []fixtureJob{{Name: "candidate.mp4", RandomID: 8403}})
	candidates[0].ID = "candidate-job"
	saveFailure := errors.New("simulated queue write failure")

	tests := []struct {
		name string
		run  func(*Controller) error
	}{
		{name: "add", run: func(controller *Controller) error { _, err := controller.AddJobs(candidates); return err }},
		{name: "remove selected", run: func(controller *Controller) error { _, err := controller.RemoveJobs([]string{jobs[1].ID}); return err }},
		{name: "remove completed", run: func(controller *Controller) error { _, err := controller.RemoveCompleted(); return err }},
		{name: "clear", run: func(controller *Controller) error { _, err := controller.ClearQueue(); return err }},
		{name: "reset", run: func(controller *Controller) error {
			_, err := controller.ResetJobs(ResetAllRecoverable, nil)
			return err
		}},
		{name: "retry", run: func(controller *Controller) error { return controller.Retry(jobs[2].ID) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryQueueStore{jobs: cloneJobs(jobs), channel: testChannel()}
			controller := newLoadedController(t, store, &fakeGateway{})
			before := controller.Snapshot().Jobs
			store.saveErr = saveFailure
			if err := test.run(controller); !errors.Is(err, saveFailure) {
				t.Fatalf("queue operation error = %v, want %v", err, saveFailure)
			}
			after := controller.Snapshot().Jobs
			assertSameJobSequence(t, after, before)
			assertSameJobSequence(t, store.SnapshotJobs(), before)
		})
	}
}

func TestRemovalResultsCountOnlyCommittedChanges(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "queued-result.mp4", RandomID: 8405},
		{Name: "sent-result.mp4", RandomID: 8406, State: model.JobSent},
	})
	tests := []struct {
		name string
		run  func(*Controller) (RemovalResult, error)
	}{
		{name: "selected", run: func(controller *Controller) (RemovalResult, error) {
			return controller.RemoveJobs([]string{jobs[0].ID})
		}},
		{name: "completed", run: func(controller *Controller) (RemovalResult, error) {
			return controller.RemoveCompleted()
		}},
		{name: "clear", run: func(controller *Controller) (RemovalResult, error) {
			return controller.ClearQueue()
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryQueueStore{jobs: cloneJobs(jobs), channel: testChannel()}
			controller := newLoadedController(t, store, &fakeGateway{})
			store.mu.Lock()
			store.saveErr = errors.New("queue disk unavailable")
			store.mu.Unlock()
			result, err := test.run(controller)
			if err == nil {
				t.Fatal("removal error = nil, want persistence failure")
			}
			if result.Removed != 0 {
				t.Fatalf("RemovalResult.Removed = %d, want 0 committed changes", result.Removed)
			}
		})
	}
}

func TestControllerSetChannelPublishesOnlyAfterPersistenceSucceeds(t *testing.T) {
	oldChannel := model.Channel{ID: -1001, AccessHash: 11, Title: "old"}
	newChannel := model.Channel{ID: -1002, AccessHash: 22, Title: "new"}
	store := &memoryQueueStore{channel: oldChannel}
	controller := newLoadedController(t, store, &fakeGateway{})

	store.mu.Lock()
	store.saveErr = errors.New("queue disk unavailable")
	store.mu.Unlock()
	if err := controller.SetChannel(newChannel); err == nil {
		t.Fatal("SetChannel() error = nil, want persistence failure")
	}
	if got := controller.Snapshot().Channel; got != oldChannel {
		t.Fatalf("channel after failed save = %+v, want %+v", got, oldChannel)
	}
	store.mu.Lock()
	if store.channel != oldChannel {
		t.Fatalf("persisted channel after failed save = %+v, want %+v", store.channel, oldChannel)
	}
	store.saveErr = nil
	store.mu.Unlock()

	if err := controller.SetChannel(newChannel); err != nil {
		t.Fatalf("SetChannel() error = %v", err)
	}
	if got := controller.Snapshot().Channel; got != newChannel {
		t.Fatalf("channel after successful save = %+v, want %+v", got, newChannel)
	}
}

func TestControllerAddJobsRejectsInvalidOrDuplicateIdentity(t *testing.T) {
	existing, _ := fixtureJobs(t, []fixtureJob{{Name: "existing.mp4", RandomID: 8501}})
	baseCandidates, _ := fixtureJobs(t, []fixtureJob{{Name: "candidate.mp4", RandomID: 8502}})
	baseCandidates[0].ID = "candidate-job"

	tests := []struct {
		name   string
		mutate func(*model.Job)
	}{
		{name: "empty ID", mutate: func(job *model.Job) { job.ID = "" }},
		{name: "duplicate ID", mutate: func(job *model.Job) { job.ID = existing[0].ID }},
		{name: "zero RandomID", mutate: func(job *model.Job) { job.RandomID = 0 }},
		{name: "duplicate RandomID", mutate: func(job *model.Job) { job.RandomID = existing[0].RandomID }},
		{name: "invalid state", mutate: func(job *model.Job) { job.State = model.JobSent }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryQueueStore{jobs: cloneJobs(existing), channel: testChannel()}
			controller := newLoadedController(t, store, &fakeGateway{})
			candidate := baseCandidates[0]
			test.mutate(&candidate)
			if _, err := controller.AddJobs([]model.Job{candidate}); err == nil {
				t.Fatal("AddJobs() error = nil, want identity validation failure")
			}
			assertSameJobSequence(t, controller.Snapshot().Jobs, existing)
			assertSameJobSequence(t, store.SnapshotJobs(), existing)
		})
	}
}

func assertSameJobSequence(t *testing.T, got, want []model.Job) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("job count = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index].ID != want[index].ID || got[index].State != want[index].State || got[index].Path != want[index].Path || got[index].Position != want[index].Position {
			t.Fatalf("job %d = %+v, want %+v", index, got[index], want[index])
		}
	}
}

func TestSnapshotProgressUsesStableQueueTotal(t *testing.T) {
	controller := NewController(nil, nil)
	controller.jobs = []model.Job{
		{Size: 100, State: model.JobQueued},
		{Size: 200, State: model.JobFailed, Uploaded: 50},
		{Size: 300, State: model.JobConfirming, Uploaded: 250},
		{Size: 400, State: model.JobSent},
		{Size: 500, State: model.JobCancelled, Uploaded: 600},
		{Size: 600, State: model.JobOversize, Uploaded: -1},
		{Size: 700, State: model.JobMoved},
	}

	snapshot := controller.Snapshot()
	if snapshot.TotalBytes != 2800 {
		t.Fatalf("TotalBytes = %d, want 2800", snapshot.TotalBytes)
	}
	if snapshot.DoneBytes != 1900 {
		t.Fatalf("DoneBytes = %d, want 1900", snapshot.DoneBytes)
	}
}

func TestControllerCancelOneJobContinuesWithNext(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "cancel-me.mp4", RandomID: 7201},
		{Name: "continue.mp4", RandomID: 7202},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	firstStarted := make(chan struct{})
	var signalFirst sync.Once
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, progress func(model.Progress)) (int, error) {
		if request.RandomID == jobs[0].RandomID {
			signalFirst.Do(func() { close(firstStarted) })
			<-ctx.Done()
			return 0, ctx.Err()
		}
		info, err := os.Stat(request.Path)
		if err != nil {
			return 0, err
		}
		progress(model.Progress{BytesDone: info.Size(), BytesTotal: info.Size(), At: time.Now()})
		return 7202, nil
	}

	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first upload")
	}
	if err := controller.CancelJob(jobs[0].ID); err != nil {
		t.Fatalf("CancelJob() error = %v", err)
	}

	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 2 &&
			snapshot.Jobs[0].State == model.JobCancelled &&
			snapshot.Jobs[1].State == model.JobSent
	})
	if final.Jobs[0].Error != "" {
		t.Fatalf("cancelled job error = %q, want empty", final.Jobs[0].Error)
	}
	calls := gateway.Calls()
	if len(calls) != 2 || calls[0].RandomID != jobs[0].RandomID || calls[1].RandomID != jobs[1].RandomID {
		t.Fatalf("UploadVideo order = %#v, want random IDs [%d %d]", calls, jobs[0].RandomID, jobs[1].RandomID)
	}
}

func TestControllerNonRetryableFailureContinuesQueue(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "failure.mp4", RandomID: 7301},
		{Name: "continues.mp4", RandomID: 7302},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	failure := errors.New("upload rejected")
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if request.RandomID == jobs[0].RandomID {
			return 0, failure
		}
		return 7302, nil
	}

	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 2 &&
			snapshot.Jobs[0].State == model.JobFailed && snapshot.Jobs[1].State == model.JobSent
	})
	if final.LastError != failure.Error() || final.Jobs[0].Error != failure.Error() {
		t.Fatalf("failure = lastError %q, jobError %q; want %q", final.LastError, final.Jobs[0].Error, failure.Error())
	}
	if calls := gateway.Calls(); len(calls) != 2 {
		t.Fatalf("UploadVideo calls = %d, want failed task skipped and next task sent", len(calls))
	}
}

func TestControllerFailureStatePersistenceErrorStopsLaterQueue(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "failure-save-error.mp4", RandomID: 7303},
		{Name: "must-remain-queued.mp4", RandomID: 7304},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	persistFailure := errors.New("disk unavailable after upload failure")
	var armFailure sync.Once
	store.saveHook = func(saved []model.Job) {
		if len(saved) == 0 || saved[0].State != model.JobUploading {
			return
		}
		armFailure.Do(func() {
			store.mu.Lock()
			store.saveErr = persistFailure
			store.mu.Unlock()
		})
	}
	gateway := &fakeGateway{upload: func(_ context.Context, _ tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		return 0, errors.New("upload rejected")
	}}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 2 && snapshot.Jobs[0].State == model.JobFailed
	})
	if final.Jobs[1].State != model.JobQueued {
		t.Fatalf("second job state = %s, want queued after failed terminal persistence", final.Jobs[1].State)
	}
	if calls := gateway.Calls(); len(calls) != 1 {
		t.Fatalf("UploadVideo calls = %d, want queue stopped after first failure", len(calls))
	}
}

func TestControllerPersistenceFailureStopsBeforeSendingNextJob(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "persistence-failure.mp4", RandomID: 7341},
		{Name: "must-not-send.mp4", RandomID: 7342},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	persistFailure := errors.New("disk unavailable")
	var armFailure sync.Once
	store.saveHook = func([]model.Job) {
		armFailure.Do(func() {
			store.mu.Lock()
			store.saveErr = persistFailure
			store.mu.Unlock()
		})
	}
	gateway := &fakeGateway{}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 2 && snapshot.Jobs[0].State == model.JobFailed
	})
	if final.Jobs[1].State != model.JobQueued {
		t.Fatalf("second job state = %s, want queued after persistence failure", final.Jobs[1].State)
	}
	if calls := gateway.Calls(); len(calls) != 0 {
		t.Fatalf("UploadVideo calls = %d, want no send without persisted active state", len(calls))
	}
}

func TestControllerPreSendPersistenceFailurePreventsMessageSubmission(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "pre-send-persist.mp4", RandomID: 7351}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	persistFailure := errors.New("disk failed at pre-send boundary")
	submitted := make(chan struct{}, 1)
	gateway := &fakeGateway{}
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, progress func(model.Progress)) (int, error) {
		progress(model.Progress{BytesDone: jobs[0].Size, BytesTotal: jobs[0].Size, At: time.Now()})
		store.mu.Lock()
		store.saveErr = persistFailure
		store.mu.Unlock()
		if request.BeforeSend == nil {
			return 0, errors.New("missing pre-send durability callback")
		}
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		submitted <- struct{}{}
		return int(request.RandomID), nil
	}

	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 1 && snapshot.Jobs[0].State == model.JobFailed
	})
	select {
	case <-submitted:
		t.Fatal("message submission continued after pre-send state persistence failed")
	default:
	}
	if !strings.Contains(final.Jobs[0].Error, "保存消息提交状态失败") {
		t.Fatalf("failed job error = %q, want pre-send persistence detail", final.Jobs[0].Error)
	}
}

func TestControllerRetriesPreSendFailureThenContinuesQueue(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "retry.mp4", RandomID: 7351},
		{Name: "after-retry.mp4", RandomID: 7352},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	var mu sync.Mutex
	firstCalls := 0
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if request.RandomID == jobs[0].RandomID {
			mu.Lock()
			firstCalls++
			call := firstCalls
			mu.Unlock()
			if call <= 2 {
				return 0, fmt.Errorf("%w: temporary disconnect", tgtransport.ErrUploadData)
			}
		}
		return int(request.RandomID), nil
	}

	controller := newLoadedController(t, store, gateway)
	controller.uploadRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second}
	var waited []time.Duration
	controller.uploadRetryWait = func(_ context.Context, delay time.Duration) error {
		waited = append(waited, delay)
		return nil
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && allJobsState(snapshot.Jobs, model.JobSent)
	})
	if len(final.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(final.Jobs))
	}
	if !reflect.DeepEqual(waited, controller.uploadRetryDelays) {
		t.Fatalf("retry waits = %v, want %v", waited, controller.uploadRetryDelays)
	}
	if calls := gateway.Calls(); len(calls) != 4 {
		t.Fatalf("UploadVideo calls = %d, want 3 attempts plus next job", len(calls))
	}
}

func TestControllerExhaustedRetriesFailCurrentAndContinue(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "offline.mp4", RandomID: 7361},
		{Name: "still-runs.mp4", RandomID: 7362},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if request.RandomID == jobs[0].RandomID {
			return 0, fmt.Errorf("%w: connection reset", tgtransport.ErrUploadData)
		}
		return int(request.RandomID), nil
	}

	controller := newLoadedController(t, store, gateway)
	controller.uploadRetryDelays = []time.Duration{time.Second, 2 * time.Second}
	controller.uploadRetryWait = func(context.Context, time.Duration) error { return nil }
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 2 &&
			snapshot.Jobs[0].State == model.JobFailed && snapshot.Jobs[1].State == model.JobSent
	})
	if !strings.Contains(final.Jobs[0].Error, "自动重试 2 次后仍失败") {
		t.Fatalf("failed job error = %q, want retry exhaustion detail", final.Jobs[0].Error)
	}
	if calls := gateway.Calls(); len(calls) != 4 {
		t.Fatalf("UploadVideo calls = %d, want 3 failed attempts plus next job", len(calls))
	}
}

func TestControllerCancelJobInterruptsRetryWaitAndContinues(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "cancel-retry.mp4", RandomID: 7371},
		{Name: "after-cancel.mp4", RandomID: 7372},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if request.RandomID == jobs[0].RandomID {
			return 0, fmt.Errorf("%w: offline", tgtransport.ErrUploadData)
		}
		return int(request.RandomID), nil
	}
	waitStarted := make(chan struct{})
	var signalWait sync.Once

	controller := newLoadedController(t, store, gateway)
	controller.uploadRetryDelays = []time.Duration{time.Hour}
	controller.uploadRetryWait = func(ctx context.Context, _ time.Duration) error {
		signalWait.Do(func() { close(waitStarted) })
		<-ctx.Done()
		return ctx.Err()
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-waitStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for retry backoff")
	}
	if err := controller.CancelJob(jobs[0].ID); err != nil {
		t.Fatalf("CancelJob() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 2 &&
			snapshot.Jobs[0].State == model.JobCancelled && snapshot.Jobs[1].State == model.JobSent
	})
	if final.Jobs[0].Error != "" {
		t.Fatalf("cancelled retry error = %q, want empty", final.Jobs[0].Error)
	}
	if calls := gateway.Calls(); len(calls) != 2 {
		t.Fatalf("UploadVideo calls = %d, want one cancelled attempt plus next job", len(calls))
	}
}

func TestControllerPauseInterruptsRetryWaitAndCanResume(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "pause-retry.mp4", RandomID: 7381},
		{Name: "after-pause.mp4", RandomID: 7382},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	var mu sync.Mutex
	firstCalls := 0
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if request.RandomID == jobs[0].RandomID {
			mu.Lock()
			firstCalls++
			call := firstCalls
			mu.Unlock()
			if call == 1 {
				return 0, fmt.Errorf("%w: offline", tgtransport.ErrUploadData)
			}
		}
		return int(request.RandomID), nil
	}
	waitStarted := make(chan struct{})
	var signalWait sync.Once

	controller := newLoadedController(t, store, gateway)
	controller.uploadRetryDelays = []time.Duration{time.Hour}
	controller.uploadRetryWait = func(ctx context.Context, _ time.Duration) error {
		signalWait.Do(func() { close(waitStarted) })
		<-ctx.Done()
		return ctx.Err()
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-waitStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for retry backoff")
	}
	if err := controller.Pause(); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	paused := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && snapshot.Paused && len(snapshot.Jobs) == 2 &&
			snapshot.Jobs[0].State == model.JobInterrupted && snapshot.Jobs[1].State == model.JobQueued
	})
	if !strings.Contains(paused.Jobs[0].Error, "暂停") {
		t.Fatalf("paused retry error = %q, want pause guidance", paused.Jobs[0].Error)
	}
	if err := controller.Resume(); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && allJobsState(snapshot.Jobs, model.JobSent)
	})
}

func TestControllerRetryUsesLatestGateway(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "reconnect.mp4", RandomID: 7391}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	oldGateway := &fakeGateway{}
	oldGateway.upload = func(context.Context, tgtransport.UploadRequest, func(model.Progress)) (int, error) {
		return 0, tgtransport.ErrNotConnected
	}
	newGateway := &fakeGateway{}
	newGateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		return int(request.RandomID), nil
	}

	controller := newLoadedController(t, store, oldGateway)
	controller.uploadRetryDelays = []time.Duration{time.Second}
	controller.uploadRetryWait = func(context.Context, time.Duration) error {
		controller.SetGateway(newGateway)
		return nil
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && allJobsState(snapshot.Jobs, model.JobSent)
	})
	if calls := oldGateway.Calls(); len(calls) != 1 {
		t.Fatalf("old gateway calls = %d, want 1", len(calls))
	}
	if calls := newGateway.Calls(); len(calls) != 1 {
		t.Fatalf("new gateway calls = %d, want 1", len(calls))
	}
}

func TestControllerCancelAllCancelsActiveAndPendingJobs(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "active.mp4", RandomID: 7401},
		{Name: "pending.mp4", RandomID: 7402},
		{Name: "failed.mp4", RandomID: 7403, State: model.JobFailed},
		{Name: "skipped.mp4", RandomID: 7404, State: model.JobSkipped},
		{Name: "already-sent.mp4", RandomID: 7405, State: model.JobSent},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	started := make(chan struct{})
	var signal sync.Once
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		signal.Do(func() { close(started) })
		<-ctx.Done()
		return 0, ctx.Err()
	}

	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active upload")
	}
	controller.CancelAll()

	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		if snapshot.Running || len(snapshot.Jobs) != len(jobs) {
			return false
		}
		for i, job := range snapshot.Jobs {
			want := model.JobCancelled
			if i == len(jobs)-1 {
				want = model.JobSent
			}
			if job.State != want {
				return false
			}
		}
		return true
	})
	for i := range final.Jobs[:len(jobs)-1] {
		if final.Jobs[i].Error != "" {
			t.Errorf("cancelled job %d error = %q, want empty", i, final.Jobs[i].Error)
		}
	}
	if calls := gateway.Calls(); len(calls) != 1 {
		t.Fatalf("UploadVideo calls = %d, want active upload only", len(calls))
	}
}

func TestControllerFailureCannotOverwriteConcurrentCancellation(t *testing.T) {
	tests := []struct {
		name          string
		state         model.JobState
		cancelAll     bool
		cancelJobID   string
		wantLastError string
	}{
		{name: "already cancelled", state: model.JobCancelled},
		{name: "cancel all committed", state: model.JobUploading, cancelAll: true},
		{name: "single job cancellation", state: model.JobUploading, cancelJobID: "job-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "cancel-race.mp4", RandomID: 7431, State: test.state}})
			store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
			controller := newLoadedController(t, store, &fakeGateway{})
			controller.cancelAllRequested = test.cancelAll
			controller.cancelJobID = test.cancelJobID

			if err := controller.failJob(jobs[0].ID, errors.New("stale upload failure")); err != nil {
				t.Fatalf("failJob() error = %v", err)
			}
			snapshot := controller.Snapshot()
			if got := snapshot.Jobs[0]; got.State != model.JobCancelled || got.Error != "" {
				t.Fatalf("job after stale failure = %+v, want cancelled without error", got)
			}
			if snapshot.LastError != test.wantLastError {
				t.Fatalf("LastError = %q, want %q", snapshot.LastError, test.wantLastError)
			}
			if got := store.SnapshotJobs()[0]; got.State != model.JobCancelled || got.Error != "" {
				t.Fatalf("persisted job after stale failure = %+v, want cancelled without error", got)
			}
		})
	}
}

func TestPersistCandidateIncludesLatestActiveProgress(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "active-progress.mp4", RandomID: 7432, State: model.JobUploading}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})
	controller.mu.Lock()
	controller.activeID = jobs[0].ID
	controller.jobs[0].Uploaded = 80
	controller.jobs[0].BytesPerSecond = 12
	revision := controller.queueRevision
	stale := cloneJobs(controller.jobs)
	stale[0].Uploaded = 10
	stale[0].BytesPerSecond = 2
	channel := controller.channel
	paused := controller.paused
	controller.mu.Unlock()

	if err := controller.persistCandidate(stale, channel, paused, revision); err != nil {
		t.Fatalf("persistCandidate() error = %v", err)
	}
	persisted := store.SnapshotJobs()
	if got := persisted[0]; got.Uploaded != 80 || got.BytesPerSecond != 12 {
		t.Fatalf("persisted active progress = %d at %v B/s, want 80 at 12 B/s", got.Uploaded, got.BytesPerSecond)
	}
}

func TestControllerWaitStoppedIncludesFinalPersistence(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "wait-stopped.mp4", RandomID: 7441}})
	activateSaveBlock := make(chan struct{})
	releaseSave := make(chan struct{})
	saveEntered := make(chan struct{}, 1)
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	store.saveHook = func([]model.Job) {
		select {
		case <-activateSaveBlock:
			select {
			case saveEntered <- struct{}{}:
			default:
			}
			<-releaseSave
		default:
		}
	}

	gateway := &fakeGateway{}
	started := make(chan struct{})
	allowUploadReturn := make(chan struct{})
	var startedOnce sync.Once
	gateway.upload = func(ctx context.Context, _ tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		<-allowUploadReturn
		return 0, ctx.Err()
	}

	controller := newLoadedController(t, store, gateway)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controller.WaitStopped(cancelled); err != nil {
		t.Fatalf("WaitStopped() while idle = %v, want nil", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active upload")
	}
	if err := controller.WaitStopped(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitStopped() while running = %v, want context.Canceled", err)
	}
	if err := controller.CancelAll(); err != nil {
		t.Fatalf("CancelAll() error = %v", err)
	}
	close(activateSaveBlock)
	close(allowUploadReturn)
	select {
	case <-saveEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for final persistence")
	}
	if err := controller.WaitStopped(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitStopped() during final persistence = %v, want context.Canceled", err)
	}
	close(releaseSave)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer waitCancel()
	if err := controller.WaitStopped(waitCtx); err != nil {
		t.Fatalf("WaitStopped() after persistence = %v", err)
	}
	snapshot := controller.Snapshot()
	if snapshot.Running || len(snapshot.Jobs) != 1 || snapshot.Jobs[0].State != model.JobCancelled {
		t.Fatalf("final snapshot = %+v, want stopped cancelled job", snapshot)
	}
	persisted := store.SnapshotJobs()
	if len(persisted) != 1 || persisted[0].State != model.JobCancelled {
		t.Fatalf("persisted jobs = %+v, want cancelled job", persisted)
	}
}

func TestControllerParentCancellationMarksActiveJobInterrupted(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "parent-cancel.mp4", RandomID: 7451}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	started := make(chan struct{})
	var signal sync.Once
	gateway.upload = func(ctx context.Context, _ tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		signal.Do(func() { close(started) })
		<-ctx.Done()
		return 0, ctx.Err()
	}

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(parent); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active upload")
	}
	cancel()

	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 1 && snapshot.Jobs[0].State == model.JobInterrupted
	})
	if final.Jobs[0].Error == "" {
		t.Fatal("interrupted job error is empty, want recovery guidance")
	}
}

func TestControllerRunQueueCancelsContextAfterNaturalCompletion(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "natural-context-cleanup.mp4", RandomID: 7452}})
	controller := newLoadedController(t, &memoryQueueStore{jobs: jobs, channel: testChannel()}, &fakeGateway{})

	cancelled := make(chan struct{})
	go controller.runQueue(context.Background(), func() {
		close(cancelled)
	})

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for natural queue completion cancellation")
	}
	if snapshot := controller.Snapshot(); snapshot.Running || snapshot.Jobs[0].State != model.JobSent {
		t.Fatalf("snapshot after natural queue completion = %+v, want stopped sent queue", snapshot)
	}
}

func TestControllerRunQueueCancelsContextAfterEarlyReturn(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "early-context-cleanup.mp4", RandomID: 7453}})
	controller := newLoadedController(t, &memoryQueueStore{jobs: jobs, channel: testChannel()}, &fakeGateway{})

	ctx, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	cancelled := make(chan struct{})
	go controller.runQueue(ctx, func() {
		close(cancelled)
	})

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for early queue return cancellation")
	}
	if snapshot := controller.Snapshot(); snapshot.Running {
		t.Fatalf("snapshot after early queue return = %+v, want stopped queue", snapshot)
	}
}

func TestControllerIgnoresLateProgressAfterTerminalState(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "already-sent.mp4", RandomID: 7461, State: model.JobSent, Uploaded: 1},
		{Name: "already-cancelled.mp4", RandomID: 7462, State: model.JobCancelled, Uploaded: 2},
	})
	controller := newLoadedController(t, &memoryQueueStore{jobs: jobs, channel: testChannel()}, &fakeGateway{})

	controller.applyProgress(jobs[0].ID, model.Progress{
		BytesDone:      jobs[0].Size,
		BytesTotal:     jobs[0].Size,
		BytesPerSecond: 999,
		At:             time.Now(),
	})
	controller.applyProgress(jobs[1].ID, model.Progress{
		BytesDone:      jobs[1].Size,
		BytesTotal:     jobs[1].Size,
		BytesPerSecond: 999,
		At:             time.Now(),
	})

	snapshot := controller.Snapshot()
	if got := snapshot.Jobs[0]; got.State != model.JobSent || got.Uploaded != jobs[0].Uploaded || got.BytesPerSecond != 0 {
		t.Fatalf("late progress changed sent job: %+v", got)
	}
	if got := snapshot.Jobs[1]; got.State != model.JobCancelled || got.Uploaded != jobs[1].Uploaded || got.BytesPerSecond != 0 {
		t.Fatalf("late progress changed cancelled job: %+v", got)
	}
}

func TestControllerSendOutcomeUnknownEntersConfirming(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "unknown-outcome.mp4", RandomID: 7471},
		{Name: "must-not-send.mp4", RandomID: 7472},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	gateway.upload = func(context.Context, tgtransport.UploadRequest, func(model.Progress)) (int, error) {
		return 0, tgtransport.ErrSendOutcomeUnknown
	}

	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 2 &&
			snapshot.Jobs[0].State == model.JobConfirming && snapshot.Jobs[1].State == model.JobQueued
	})
	if final.Jobs[0].Error == "" || final.LastError == "" {
		t.Fatalf("confirming job error = %q, last error = %q; want recovery guidance", final.Jobs[0].Error, final.LastError)
	}
	if saved := store.SnapshotJobs(); len(saved) != 2 || saved[0].State != model.JobConfirming || saved[1].State != model.JobQueued {
		t.Fatalf("persisted state = %#v, want confirming", saved)
	}
	if calls := gateway.Calls(); len(calls) != 1 {
		t.Fatalf("UploadVideo calls = %d, want no automatic retry after unknown send outcome", len(calls))
	}
}

func TestControllerIgnoresProgressFromPreviousRetryAttempt(t *testing.T) {
	controller := NewController(nil, nil)
	controller.jobs = []model.Job{{ID: "job", Size: 100, State: model.JobUploading}}
	controller.activeID = "job"
	controller.activeAttempt = 2

	controller.applyProgressForAttempt("job", 1, model.Progress{
		BytesDone:      100,
		BytesTotal:     100,
		BytesPerSecond: 999,
		At:             time.Now(),
	})
	if got := controller.Snapshot().Jobs[0]; got.Uploaded != 0 || got.State != model.JobUploading {
		t.Fatalf("stale attempt changed job: %+v", got)
	}

	controller.applyProgressForAttempt("job", 2, model.Progress{
		BytesDone:      25,
		BytesTotal:     100,
		BytesPerSecond: 50,
		At:             time.Now(),
	})
	if got := controller.Snapshot().Jobs[0]; got.Uploaded != 25 || got.BytesPerSecond != 50 {
		t.Fatalf("current attempt progress = %+v, want 25 bytes at 50 B/s", got)
	}
}

func TestControllerProgressUsesLatestSingleSlotWithoutSnapshot(t *testing.T) {
	controller := NewController(nil, nil)
	controller.jobs = []model.Job{{ID: "job", Size: 100, State: model.JobUploading}}
	controller.activeID = "job"
	controller.activeAttempt = 7

	for _, uploaded := range []int64{10, 40, 80} {
		controller.applyProgressForAttempt("job", 7, model.Progress{
			BytesDone:      uploaded,
			BytesTotal:     100,
			BytesPerSecond: float64(uploaded),
			At:             time.Now(),
		})
	}

	select {
	case <-controller.Updates():
		t.Fatal("pure progress unexpectedly emitted a complete snapshot")
	default:
	}
	select {
	case got := <-controller.ProgressUpdates():
		if got.JobID != "job" || got.AttemptID != 7 || got.Uploaded != 80 || got.BytesPerSecond != 80 {
			t.Fatalf("latest progress = %+v, want job attempt 7 at 80 bytes and 80 B/s", got)
		}
	default:
		t.Fatal("pure progress did not emit a lightweight update")
	}
	select {
	case <-controller.ProgressUpdates():
		t.Fatal("progress channel retained an older update")
	default:
	}
}

func TestControllerDoesNotPersistIntermediateUploadProgress(t *testing.T) {
	store := &memoryQueueStore{}
	controller := NewController(store, nil)
	controller.jobs = []model.Job{{ID: "job", Size: 100, State: model.JobUploading}}
	controller.activeID = "job"
	controller.activeAttempt = 1

	for _, uploaded := range []int64{10, 40, 80} {
		controller.applyProgressForAttempt("job", 1, model.Progress{
			BytesDone:      uploaded,
			BytesTotal:     100,
			BytesPerSecond: 50,
			At:             time.Now(),
		})
	}
	if got := store.savesCount(); got != 0 {
		t.Fatalf("intermediate progress saves = %d, want 0", got)
	}

	controller.applyProgressForAttempt("job", 1, model.Progress{
		BytesDone:      100,
		BytesTotal:     100,
		BytesPerSecond: 50,
		At:             time.Now(),
	})
	if got := store.savesCount(); got != 0 {
		t.Fatalf("final byte progress saves = %d, want 0", got)
	}
	if err := controller.prepareSendForAttempt("job", 1); err != nil {
		t.Fatalf("prepareSendForAttempt() error = %v", err)
	}
	if got := store.savesCount(); got != 1 {
		t.Fatalf("explicit pre-send transition saves = %d, want 1", got)
	}
	if got := store.SnapshotJobs()[0]; got.State != model.JobSending || got.Uploaded != 100 {
		t.Fatalf("persisted final progress = %+v, want sending at 100 bytes", got)
	}

	// Repeated or stale callbacks must not add disk writes or regress speed.
	controller.applyProgressForAttempt("job", 1, model.Progress{
		BytesDone:      80,
		BytesTotal:     100,
		BytesPerSecond: 1,
		At:             time.Now(),
	})
	if got := store.savesCount(); got != 1 {
		t.Fatalf("repeated/stale progress saves = %d, want 1", got)
	}
	if got := controller.Snapshot().Jobs[0]; got.Uploaded != 100 || got.BytesPerSecond != 0 {
		t.Fatalf("stale progress changed current snapshot: %+v", got)
	}
}

func TestControllerConfirmingJobsCanRetryOrMarkSent(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "retry-confirming.mp4", RandomID: 7501, State: model.JobConfirming, Uploaded: 9, Error: "check channel"},
		{Name: "mark-confirming.mp4", RandomID: 7502, State: model.JobConfirming, Uploaded: 11, Error: "check channel"},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})

	if err := controller.Retry(jobs[0].ID); err != nil {
		t.Fatalf("Retry(confirming) error = %v", err)
	}
	if err := controller.MarkSent(jobs[1].ID); err != nil {
		t.Fatalf("MarkSent(confirming) error = %v", err)
	}

	snapshot := controller.Snapshot()
	if got := snapshot.Jobs[0]; got.State != model.JobQueued || got.Uploaded != 0 || got.BytesPerSecond != 0 || got.Error != "" {
		t.Fatalf("retried confirming job = %+v, want queued with cleared transient state", got)
	}
	if got := snapshot.Jobs[1]; got.State != model.JobSent || got.Uploaded != jobs[1].Size || got.CompletedAt == nil || got.Error != "" {
		t.Fatalf("marked confirming job = %+v, want sent with completion", got)
	}
}

func TestControllerQueueEditsWhileUploadingUseStableIDs(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "active-edit.mp4", RandomID: 8701},
		{Name: "remove-edit.mp4", RandomID: 8702},
	})
	candidates, _ := fixtureJobs(t, []fixtureJob{{Name: "append-edit.mp4", RandomID: 8703}})
	candidates[0].ID = "append-edit-job"
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if request.RandomID == jobs[0].RandomID {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return int(request.RandomID), nil
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active upload")
	}
	if added, err := controller.AddJobs(candidates); err != nil || added != 1 {
		t.Fatalf("AddJobs() = (%d, %v), want (1, nil)", added, err)
	}
	removed, err := controller.RemoveJobs([]string{jobs[1].ID})
	if err != nil || removed.Removed != 1 || len(removed.PendingRemovalIDs) != 0 {
		t.Fatalf("RemoveJobs() = (%+v, %v), want immediate removal", removed, err)
	}
	close(release)
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 2 &&
			snapshot.Jobs[0].ID == jobs[0].ID && snapshot.Jobs[0].State == model.JobSent &&
			snapshot.Jobs[1].ID == candidates[0].ID && snapshot.Jobs[1].State == model.JobSent
	})
	if len(gateway.Calls()) != 2 {
		t.Fatalf("UploadVideo calls = %d, want active plus appended job", len(gateway.Calls()))
	}
	if final.Jobs[0].Position != 0 || final.Jobs[1].Position != 1 {
		t.Fatalf("final positions = %+v, want reindexed queue", final.Jobs)
	}
}

func TestControllerStructuralEditPreservesConcurrentActiveProgress(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "progress-active.mp4", RandomID: 8721}})
	candidates, _ := fixtureJobs(t, []fixtureJob{{Name: "progress-append.mp4", RandomID: 8722}})
	candidates[0].ID = "progress-append-job"
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	started := make(chan struct{})
	emitProgress := make(chan struct{})
	progressDone := make(chan struct{})
	releaseUpload := make(chan struct{})
	var startedOnce sync.Once
	var progressOnce sync.Once
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, progress func(model.Progress)) (int, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-emitProgress:
			info, err := os.Stat(request.Path)
			if err != nil {
				return 0, err
			}
			progress(model.Progress{BytesDone: info.Size() / 2, BytesTotal: info.Size(), BytesPerSecond: 77, At: time.Now()})
			progressOnce.Do(func() { close(progressDone) })
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		select {
		case <-releaseUpload:
			return int(request.RandomID), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active upload")
	}

	saveEntered := make(chan struct{})
	releaseSave := make(chan struct{})
	var saveOnce sync.Once
	store.saveHook = func([]model.Job) {
		saveOnce.Do(func() { close(saveEntered) })
		<-releaseSave
	}
	addDone := make(chan error, 1)
	go func() {
		_, err := controller.AddJobs(candidates)
		addDone <- err
	}()
	select {
	case <-saveEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for structural save")
	}
	close(emitProgress)
	select {
	case <-progressDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active progress callback")
	}
	close(releaseSave)
	if err := <-addDone; err != nil {
		t.Fatalf("AddJobs() error = %v", err)
	}
	active := controller.Snapshot().Jobs[0]
	if active.Uploaded == 0 || active.BytesPerSecond != 77 {
		t.Fatalf("active progress after structural edit = %+v, want progress preserved", active)
	}
	close(releaseUpload)
	waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && allJobsState(snapshot.Jobs, model.JobSent)
	})
}

func TestControllerRemovingEarlierCompletedJobReindexesActiveProgress(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "earlier-sent.mp4", RandomID: 8741, State: model.JobSent},
		{Name: "active-reindex.mp4", RandomID: 8742},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	started := make(chan struct{})
	progressRelease := make(chan struct{})
	var startedOnce sync.Once
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, progress func(model.Progress)) (int, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-progressRelease:
			info, err := os.Stat(request.Path)
			if err != nil {
				return 0, err
			}
			progress(model.Progress{BytesDone: info.Size() / 2, BytesTotal: info.Size(), BytesPerSecond: 88, At: time.Now()})
			return int(request.RandomID), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active upload")
	}
	saveEntered := make(chan struct{})
	releaseSave := make(chan struct{})
	var saveOnce sync.Once
	store.saveHook = func([]model.Job) {
		saveOnce.Do(func() { close(saveEntered) })
		<-releaseSave
	}
	removeDone := make(chan error, 1)
	go func() {
		_, err := controller.RemoveJobs([]string{jobs[0].ID})
		removeDone <- err
	}()
	select {
	case <-saveEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for removal persistence")
	}
	close(progressRelease)
	// Let the callback update the active record while the structural Save is
	// blocked; commitCandidate must retain that progress but use the new index.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snapshot := controller.Snapshot(); len(snapshot.Jobs) > 1 && snapshot.Jobs[1].Uploaded > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseSave)
	if err := <-removeDone; err != nil {
		t.Fatalf("RemoveJobs() error = %v", err)
	}
	active := controller.Snapshot().Jobs[0]
	if active.Position != 0 || active.Uploaded == 0 || active.BytesPerSecond != 88 {
		t.Fatalf("active after earlier removal = %+v, want position 0 with progress", active)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 1 && snapshot.Jobs[0].State == model.JobSent
	})
	if final.Jobs[0].Position != 0 {
		t.Fatalf("final active position = %d, want 0", final.Jobs[0].Position)
	}
}

func TestControllerPendingRemovalKeepsUnknownOutcomeForConfirmation(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "unknown-remove.mp4", RandomID: 8711}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	started := make(chan struct{})
	gateway.upload = func(ctx context.Context, _ tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		close(started)
		<-ctx.Done()
		return 0, tgtransport.ErrSendOutcomeUnknown
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upload")
	}
	result, err := controller.RemoveJobs([]string{jobs[0].ID})
	if err != nil || len(result.PendingRemovalIDs) != 1 {
		t.Fatalf("RemoveJobs() = (%+v, %v), want pending active removal", result, err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 1 && snapshot.Jobs[0].State == model.JobConfirming && len(snapshot.PendingRemovalIDs) == 0
	})
	if final.Jobs[0].Error == "" {
		t.Fatal("confirming job error is empty")
	}
}

func TestControllerPendingRemovalDuringSentPersistenceRemovesRecordAndContinues(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "sent-persist-remove.mp4", RandomID: 8723},
		{Name: "sent-persist-next.mp4", RandomID: 8724},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	sentSaveEntered := make(chan struct{})
	releaseSentSave := make(chan struct{})
	var blockSentSave sync.Once
	store.saveHook = func(saved []model.Job) {
		if len(saved) == 0 || saved[0].ID != jobs[0].ID || saved[0].State != model.JobSent {
			return
		}
		blockSentSave.Do(func() {
			close(sentSaveEntered)
			<-releaseSentSave
		})
	}

	gateway := &fakeGateway{}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-sentSaveEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for sent-state persistence")
	}

	result, err := controller.RemoveJobs([]string{jobs[0].ID})
	if err != nil || result.Removed != 0 || len(result.PendingRemovalIDs) != 1 || result.PendingRemovalIDs[0] != jobs[0].ID {
		t.Fatalf("RemoveJobs() = (%+v, %v), want first job pending safe deletion", result, err)
	}
	close(releaseSentSave)

	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 1 &&
			snapshot.Jobs[0].ID == jobs[1].ID && snapshot.Jobs[0].State == model.JobSent &&
			len(snapshot.PendingRemovalIDs) == 0
	})
	if final.Jobs[0].Position != 0 {
		t.Fatalf("remaining job position = %d, want 0", final.Jobs[0].Position)
	}
	if calls := gateway.Calls(); len(calls) != 2 {
		t.Fatalf("UploadVideo calls = %d, want both jobs sent", len(calls))
	}
}

func TestControllerPendingRemovalDuringFailedPersistenceRemovesRecord(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "failed-persist-remove.mp4", RandomID: 8725}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	failedSaveEntered := make(chan struct{})
	releaseFailedSave := make(chan struct{})
	var blockFailedSave sync.Once
	store.saveHook = func(saved []model.Job) {
		if len(saved) == 0 || saved[0].State != model.JobFailed {
			return
		}
		blockFailedSave.Do(func() {
			close(failedSaveEntered)
			<-releaseFailedSave
		})
	}
	gateway := &fakeGateway{upload: func(context.Context, tgtransport.UploadRequest, func(model.Progress)) (int, error) {
		return 0, errors.New("non-transient upload failure")
	}}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-failedSaveEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for failed-state persistence")
	}
	result, err := controller.RemoveJobs([]string{jobs[0].ID})
	if err != nil || result.Removed != 0 || len(result.PendingRemovalIDs) != 1 {
		t.Fatalf("RemoveJobs() = (%+v, %v), want failed active job pending deletion", result, err)
	}
	close(releaseFailedSave)
	waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 0 && len(snapshot.PendingRemovalIDs) == 0
	})
}

func TestControllerPendingRemovalPersistenceFailureKeepsTerminalRecord(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "remove-save-failure.mp4", RandomID: 8731}})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	started := make(chan struct{})
	var startedOnce sync.Once
	gateway.upload = func(ctx context.Context, _ tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		return 0, ctx.Err()
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upload")
	}
	store.mu.Lock()
	store.saveErr = errors.New("queue disk unavailable")
	store.mu.Unlock()
	result, err := controller.RemoveJobs([]string{jobs[0].ID})
	if err != nil || len(result.PendingRemovalIDs) != 1 {
		t.Fatalf("RemoveJobs() = (%+v, %v), want pending active removal", result, err)
	}
	final := waitForSnapshot(t, controller, func(snapshot Snapshot) bool {
		return !snapshot.Running && len(snapshot.Jobs) == 1 && snapshot.Jobs[0].State == model.JobCancelled && len(snapshot.PendingRemovalIDs) == 0 && snapshot.LastError != ""
	})
	if final.Jobs[0].Error != "" {
		t.Fatalf("cancelled record error = %q, want empty terminal error", final.Jobs[0].Error)
	}
}

func TestControllerPendingRemovalCannotBeResetOrRetried(t *testing.T) {
	job := model.Job{ID: "pending-delete", State: model.JobCancelled, Name: "pending.mp4", RandomID: 8732}
	controller := NewController(nil, nil)
	controller.mu.Lock()
	controller.jobs = []model.Job{job}
	controller.pendingRemoval[job.ID] = struct{}{}
	controller.mu.Unlock()

	if count, err := controller.ResetJobs(ResetSelected, []string{job.ID}); err != nil || count != 0 {
		t.Fatalf("ResetJobs() = (%d, %v), want pending task left unchanged", count, err)
	}
	if err := controller.Retry(job.ID); err == nil {
		t.Fatal("Retry() error = nil for a task pending deletion")
	}
	if got := controller.Snapshot().Jobs[0].State; got != model.JobCancelled {
		t.Fatalf("pending task state = %s, want cancelled", got)
	}
}

func TestControllerMoveOversizeFiles(t *testing.T) {
	jobs, root := fixtureJobs(t, []fixtureJob{
		{Name: "oversize-01.mp4", RandomID: 7601, State: model.JobOversize},
		{Name: "oversize-02.mp4", RandomID: 7602, State: model.JobOversize},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})
	destinationDir := filepath.Join(root, "moved")
	if err := os.Mkdir(destinationDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var progress []model.Progress
	if err := controller.MoveOversize(context.Background(), destinationDir, func(update model.Progress) {
		progress = append(progress, update)
	}); err != nil {
		t.Fatalf("MoveOversize() error = %v", err)
	}

	var total int64
	for _, job := range jobs {
		total += job.Size
	}
	if len(progress) == 0 {
		t.Fatal("MoveOversize() emitted no progress")
	}
	var previous int64
	for _, update := range progress {
		if update.BytesDone < previous {
			t.Fatalf("progress regressed from %d to %d", previous, update.BytesDone)
		}
		if update.BytesTotal != total {
			t.Fatalf("progress total = %d, want %d", update.BytesTotal, total)
		}
		previous = update.BytesDone
	}
	if previous != total {
		t.Fatalf("final progress = %d/%d, want complete move", previous, total)
	}

	snapshot := controller.Snapshot()
	for i, job := range snapshot.Jobs {
		wantPath := filepath.Join(destinationDir, jobs[i].Name)
		if job.State != model.JobMoved {
			t.Errorf("job %d state = %s, want moved", i, job.State)
		}
		if job.Path != wantPath {
			t.Errorf("job %d path = %q, want %q", i, job.Path, wantPath)
		}
		if _, err := os.Stat(wantPath); err != nil {
			t.Errorf("moved file %q: %v", wantPath, err)
		}
		if _, err := os.Stat(jobs[i].Path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("source %q still exists or stat failed: %v", jobs[i].Path, err)
		}
	}
}

func TestControllerMoveOversizePersistsMovingIntent(t *testing.T) {
	jobs, root := fixtureJobs(t, []fixtureJob{{Name: "persist-intent.mp4", RandomID: 7651, State: model.JobOversize}})
	destinationDir := filepath.Join(root, "moving-destination")
	if err := os.Mkdir(destinationDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var sawIntent bool
	var intentSource, intentDestination string
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	store.saveHook = func(saved []model.Job) {
		for _, job := range saved {
			if job.State != model.JobMoving {
				continue
			}
			sawIntent = true
			intentSource = job.Path
			intentDestination = job.MoveDestination
			if _, err := os.Stat(job.Path); err != nil {
				t.Errorf("moving intent source is not present: %v", err)
			}
			if _, err := os.Stat(job.MoveDestination); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("moving intent destination exists before move or stat failed: %v", err)
			}
		}
	}
	controller := newLoadedController(t, store, &fakeGateway{})
	if err := controller.MoveOversize(context.Background(), destinationDir, nil); err != nil {
		t.Fatalf("MoveOversize() error = %v", err)
	}
	if !sawIntent {
		t.Fatal("queue never persisted a moving intent")
	}
	wantDestination := filepath.Join(destinationDir, jobs[0].Name)
	if intentSource != jobs[0].Path || intentDestination != wantDestination {
		t.Fatalf("moving intent = source %q destination %q, want %q %q", intentSource, intentDestination, jobs[0].Path, wantDestination)
	}
}

func TestControllerLoadReconcilesMovingJobsByFilesystemState(t *testing.T) {
	root := t.TempDir()
	sourceKept := filepath.Join(root, "source-kept.mp4")
	destinationKept := filepath.Join(root, "destination-kept.mp4")
	sourceMissing := filepath.Join(root, "source-missing.mp4")
	destinationPresent := filepath.Join(root, "destination-present.mp4")
	for _, path := range []string{sourceKept, destinationPresent} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	jobs := []model.Job{
		{
			ID:              "moving-source-kept",
			Position:        0,
			Path:            sourceKept,
			Name:            filepath.Base(sourceKept),
			State:           model.JobMoving,
			MoveDestination: destinationKept,
		},
		{
			ID:              "moving-destination-present",
			Position:        1,
			Path:            sourceMissing,
			Name:            filepath.Base(sourceMissing),
			State:           model.JobMoving,
			MoveDestination: destinationPresent,
		},
	}
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := NewController(store, nil)
	if err := controller.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	snapshot := controller.Snapshot()
	if got := snapshot.Jobs[0]; got.State != model.JobOversize || got.Path != sourceKept || got.MoveDestination != "" || got.Error == "" {
		t.Fatalf("source-kept recovery = %+v, want oversize with source preserved", got)
	}
	if got := snapshot.Jobs[1]; got.State != model.JobMoved || got.Path != destinationPresent || got.MoveDestination != "" || got.Error != "" {
		t.Fatalf("destination-present recovery = %+v, want moved to destination", got)
	}
}

func TestControllerMoveOversizeRejectsUnsafeJobNames(t *testing.T) {
	unsafeNames := []string{
		"../escape.mp4",
		".." + string(os.PathSeparator) + "escape.mp4",
		filepath.Join(string(os.PathSeparator), "escape.mp4"),
		".",
		"..",
	}
	for _, name := range unsafeNames {
		name := name
		t.Run(name, func(t *testing.T) {
			jobs, root := fixtureJobs(t, []fixtureJob{{Name: "source.mp4", RandomID: 7661, State: model.JobOversize}})
			jobs[0].Name = name
			destinationDir := filepath.Join(root, "safe-destination")
			if err := os.Mkdir(destinationDir, 0o755); err != nil {
				t.Fatal(err)
			}
			controller := newLoadedController(t, &memoryQueueStore{jobs: jobs, channel: testChannel()}, &fakeGateway{})
			if err := controller.MoveOversize(context.Background(), destinationDir, nil); err == nil {
				t.Fatal("MoveOversize() error = nil, want unsafe filename rejection")
			}
			if got := controller.Snapshot().Jobs[0]; got.State != model.JobOversize {
				t.Fatalf("job state = %s, want oversize after rejected move", got.State)
			}
			if _, err := os.Stat(jobs[0].Path); err != nil {
				t.Fatalf("source file was changed or removed: %v", err)
			}
		})
	}
}

type fixtureJob struct {
	Name     string
	RandomID int64
	State    model.JobState
	Uploaded int64
	Error    string
}

func fixtureJobs(t *testing.T, specs []fixtureJob) ([]model.Job, string) {
	t.Helper()
	root := t.TempDir()
	jobs := make([]model.Job, len(specs))
	for i, spec := range specs {
		path := filepath.Join(root, spec.Name)
		writeMP4Fixture(t, path)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		state := spec.State
		if state == "" {
			state = model.JobQueued
		}
		jobs[i] = model.Job{
			ID:       fmt.Sprintf("job-%d", i+1),
			Position: i,
			Path:     path,
			Name:     spec.Name,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			State:    state,
			Uploaded: spec.Uploaded,
			RandomID: spec.RandomID,
			Error:    spec.Error,
		}
	}
	return jobs, root
}

func writeMP4Fixture(t *testing.T, path string) {
	t.Helper()
	file := mp4.NewFile()
	ftyp := mp4.CreateFtyp()
	moov := mp4.NewMoovBox()
	mvhd := mp4.CreateMvhd()
	mvhd.Timescale = 1000
	mvhd.Duration = 2500
	moov.AddChild(mvhd)

	track := mp4.CreateEmptyTrak(1, 1000, "video", "und")
	track.Mdia.Mdhd.Duration = 2500
	track.Mdia.Minf.Stbl.Stts.SampleCount = []uint32{1}
	track.Mdia.Minf.Stbl.Stts.SampleTimeDelta = []uint32{2500}
	if err := track.Mdia.Minf.Stbl.Stsc.AddEntry(1, 1, 1); err != nil {
		t.Fatal(err)
	}
	track.Mdia.Minf.Stbl.Stsz.SampleUniformSize = 4
	track.Mdia.Minf.Stbl.Stsz.SampleNumber = 1
	track.Mdia.Minf.Stbl.Stco.ChunkOffset = []uint32{0}
	track.Tkhd.Width = mp4.Fixed32(1920 << 16)
	track.Tkhd.Height = mp4.Fixed32(1080 << 16)
	moov.AddChild(track)

	mdat := &mp4.MdatBox{Data: []byte{0x00, 0x01, 0x02, 0x03}}
	file.AddChild(ftyp, 0)
	file.AddChild(moov, ftyp.Size())
	file.AddChild(mdat, ftyp.Size()+moov.Size())

	output, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Encode(output); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

type memoryQueueStore struct {
	mu       sync.Mutex
	jobs     []model.Job
	channel  model.Channel
	paused   bool
	saves    int
	history  [][]model.Job
	saveHook func([]model.Job)
	saveErr  error
}

func (s *memoryQueueStore) Save(jobs []model.Job, channel model.Channel, paused bool) error {
	s.mu.Lock()
	if s.saveErr != nil {
		err := s.saveErr
		s.mu.Unlock()
		return err
	}
	s.jobs = cloneJobs(jobs)
	s.channel = channel
	s.paused = paused
	s.saves++
	s.history = append(s.history, cloneJobs(jobs))
	hook := s.saveHook
	s.mu.Unlock()
	if hook != nil {
		hook(cloneJobs(jobs))
	}
	return nil
}

func (s *memoryQueueStore) Load() ([]model.Job, model.Channel, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneJobs(s.jobs), s.channel, s.paused, nil
}

func (s *memoryQueueStore) SnapshotJobs() []model.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneJobs(s.jobs)
}

func (s *memoryQueueStore) savesCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

type fakeGateway struct {
	mu        sync.Mutex
	calls     []tgtransport.UploadRequest
	upload    func(context.Context, tgtransport.UploadRequest, func(model.Progress)) (int, error)
	active    int
	maxActive int
}

func (g *fakeGateway) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (g *fakeGateway) WaitReady(context.Context) (tgtransport.Identity, error) {
	return tgtransport.Identity{}, nil
}

func (g *fakeGateway) BeginChannelBinding() (string, error) { return "", nil }

func (g *fakeGateway) BindingEvents() <-chan tgtransport.BindingEvent {
	return make(chan tgtransport.BindingEvent)
}

func (g *fakeGateway) ValidateChannel(_ context.Context, channel model.Channel) (model.Channel, error) {
	return channel, nil
}

func (g *fakeGateway) UploadVideo(ctx context.Context, request tgtransport.UploadRequest, progress func(model.Progress)) (int, error) {
	g.mu.Lock()
	g.calls = append(g.calls, request)
	g.active++
	if g.active > g.maxActive {
		g.maxActive = g.active
	}
	upload := g.upload
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active--
		g.mu.Unlock()
	}()
	if upload == nil {
		return 1, nil
	}
	return upload(ctx, request, progress)
}

func (g *fakeGateway) Calls() []tgtransport.UploadRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]tgtransport.UploadRequest(nil), g.calls...)
}

func (g *fakeGateway) MaxActive() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxActive
}

func newLoadedController(t *testing.T, store *memoryQueueStore, gateway *fakeGateway) *Controller {
	t.Helper()
	controller := NewController(store, nil)
	if err := controller.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	controller.SetGateway(gateway)
	return controller
}

func testChannel() model.Channel {
	return model.Channel{ID: -100999, AccessHash: 12345, Title: "controller test channel"}
}

func cloneJobs(jobs []model.Job) []model.Job {
	copyJobs := append([]model.Job(nil), jobs...)
	for i := range copyJobs {
		if jobs[i].StartedAt != nil {
			started := *jobs[i].StartedAt
			copyJobs[i].StartedAt = &started
		}
		if jobs[i].CompletedAt != nil {
			completed := *jobs[i].CompletedAt
			copyJobs[i].CompletedAt = &completed
		}
	}
	return copyJobs
}

func allJobsState(jobs []model.Job, state model.JobState) bool {
	if len(jobs) == 0 {
		return false
	}
	for _, job := range jobs {
		if job.State != state {
			return false
		}
	}
	return true
}

func waitForSnapshot(t *testing.T, controller *Controller, predicate func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := controller.Snapshot()
		if predicate(snapshot) {
			return snapshot
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for controller snapshot: %+v", controller.Snapshot())
	return Snapshot{}
}
