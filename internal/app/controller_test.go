package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		if call.Caption != jobs[i].Name {
			t.Errorf("call %d Caption = %q, want exact filename %q", i, call.Caption, jobs[i].Name)
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

	if err := controller.AddJobs([]model.Job{duplicate, candidates[0], candidates[0]}); err != nil {
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

	if err := controller.RemoveCompleted(); err != nil {
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

	if err := controller.RemoveJobs([]string{jobs[3].ID, "does-not-exist"}); err != nil {
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

	if err := controller.ClearQueue(); err != nil {
		t.Fatalf("ClearQueue() error = %v", err)
	}
	if got := controller.Snapshot(); len(got.Jobs) != 0 {
		t.Fatalf("snapshot after ClearQueue() = %+v, want empty queue", got)
	}
	if saved := store.SnapshotJobs(); len(saved) != 0 {
		t.Fatalf("persisted queue after ClearQueue() = %#v, want empty", saved)
	}
}

func TestControllerQueueRemovalRequiresIdleQueue(t *testing.T) {
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
	if err := controller.RemoveJobs([]string{jobs[0].ID}); err == nil {
		t.Fatal("RemoveJobs() error = nil while queue is running")
	}
	if err := controller.RemoveCompleted(); err == nil {
		t.Fatal("RemoveCompleted() error = nil while queue is running")
	}
	if err := controller.ClearQueue(); err == nil {
		t.Fatal("ClearQueue() error = nil while queue is running")
	}
	if err := controller.CancelAll(); err != nil {
		t.Fatalf("CancelAll() error = %v", err)
	}
	waitForSnapshot(t, controller, func(snapshot Snapshot) bool { return !snapshot.Running })
}

func TestControllerQueueMutationsPublishOnlyAfterPersistenceSucceeds(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "sent.mp4", RandomID: 8401, State: model.JobSent, Uploaded: 4},
		{Name: "queued.mp4", RandomID: 8402, State: model.JobQueued},
	})
	candidates, _ := fixtureJobs(t, []fixtureJob{{Name: "candidate.mp4", RandomID: 8403}})
	candidates[0].ID = "candidate-job"
	saveFailure := errors.New("simulated queue write failure")

	tests := []struct {
		name string
		run  func(*Controller) error
	}{
		{name: "add", run: func(controller *Controller) error { return controller.AddJobs(candidates) }},
		{name: "remove selected", run: func(controller *Controller) error { return controller.RemoveJobs([]string{jobs[1].ID}) }},
		{name: "remove completed", run: func(controller *Controller) error { return controller.RemoveCompleted() }},
		{name: "clear", run: func(controller *Controller) error { return controller.ClearQueue() }},
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
			if err := controller.AddJobs([]model.Job{candidate}); err == nil {
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

func TestControllerFailurePausesQueue(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "failure.mp4", RandomID: 7301},
		{Name: "must-wait.mp4", RandomID: 7302},
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
		return !snapshot.Running && len(snapshot.Jobs) == 2 && snapshot.Jobs[0].State == model.JobFailed
	})
	if final.Jobs[1].State != model.JobQueued {
		t.Fatalf("second job state = %s, want queued after failure pause", final.Jobs[1].State)
	}
	if final.LastError != failure.Error() || final.Jobs[0].Error != failure.Error() {
		t.Fatalf("failure = lastError %q, jobError %q; want %q", final.LastError, final.Jobs[0].Error, failure.Error())
	}
	if calls := gateway.Calls(); len(calls) != 1 {
		t.Fatalf("UploadVideo calls = %d, want queue paused after first failure", len(calls))
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

func TestControllerIgnoresLateProgressAfterTerminalState(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "already-sent.mp4", RandomID: 7461, State: model.JobSent, Uploaded: 1},
		{Name: "already-cancelled.mp4", RandomID: 7462, State: model.JobCancelled, Uploaded: 2},
	})
	controller := newLoadedController(t, &memoryQueueStore{jobs: jobs, channel: testChannel()}, &fakeGateway{})

	controller.applyProgress(0, jobs[0].ID, model.Progress{
		BytesDone:      jobs[0].Size,
		BytesTotal:     jobs[0].Size,
		BytesPerSecond: 999,
		At:             time.Now(),
	})
	controller.applyProgress(1, jobs[1].ID, model.Progress{
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
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "unknown-outcome.mp4", RandomID: 7471}})
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
		return !snapshot.Running && len(snapshot.Jobs) == 1 && snapshot.Jobs[0].State == model.JobConfirming
	})
	if final.Jobs[0].Error == "" || final.LastError == "" {
		t.Fatalf("confirming job error = %q, last error = %q; want recovery guidance", final.Jobs[0].Error, final.LastError)
	}
	if saved := store.SnapshotJobs(); len(saved) != 1 || saved[0].State != model.JobConfirming {
		t.Fatalf("persisted state = %#v, want confirming", saved)
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
	saves    int
	history  [][]model.Job
	saveHook func([]model.Job)
	saveErr  error
}

func (s *memoryQueueStore) Save(jobs []model.Job, channel model.Channel) error {
	s.mu.Lock()
	if s.saveErr != nil {
		err := s.saveErr
		s.mu.Unlock()
		return err
	}
	s.jobs = cloneJobs(jobs)
	s.channel = channel
	s.saves++
	s.history = append(s.history, cloneJobs(jobs))
	hook := s.saveHook
	s.mu.Unlock()
	if hook != nil {
		hook(cloneJobs(jobs))
	}
	return nil
}

func (s *memoryQueueStore) Load() ([]model.Job, model.Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneJobs(s.jobs), s.channel, nil
}

func (s *memoryQueueStore) SnapshotJobs() []model.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneJobs(s.jobs)
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
