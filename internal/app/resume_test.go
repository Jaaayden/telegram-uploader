package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/model"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

func resumeTestJobs(t *testing.T, count int) []model.Job {
	t.Helper()
	dir := t.TempDir()
	jobs := make([]model.Job, count)
	for index := range jobs {
		path := filepath.Join(dir, string(rune('a'+index))+".mp4")
		writeMP4Fixture(t, path)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		jobs[index] = model.Job{
			ID: string(rune('a' + index)), Position: index, Path: path, Name: filepath.Base(path),
			Size: info.Size(), ModTime: info.ModTime(), State: model.JobQueued, RandomID: int64(index + 1),
		}
	}
	return jobs
}

// checkpointFor builds the token a transport would report for this request.
func checkpointFor(job model.Job, parts int) tgtransport.ResumeToken {
	return tgtransport.ResumeToken{FileID: 4242, Parts: parts, PartSize: 512 << 10, Size: job.Size, At: time.Now()}
}

func TestPauseNowInterruptsUploadAndStartResumesIt(t *testing.T) {
	jobs := resumeTestJobs(t, 2)
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	uploading := make(chan struct{}, 4)
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if request.RandomID == 1 && request.Resume.FileID == 0 {
			// First attempt of the first file: report a checkpoint, then hang
			// until interrupted.
			request.OnCheckpoint(checkpointFor(jobs[0], 1))
			uploading <- struct{}{}
			<-ctx.Done()
			return 0, ctx.Err()
		}
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		return 9, nil
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-uploading
	if err := controller.PauseNow(); err != nil {
		t.Fatal(err)
	}
	paused := waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running })
	if !paused.Paused || paused.Jobs[0].State != model.JobInterrupted || paused.Jobs[1].State != model.JobQueued {
		t.Fatalf("after PauseNow: paused=%v states=%s,%s", paused.Paused, paused.Jobs[0].State, paused.Jobs[1].State)
	}
	if paused.Jobs[0].UploadFileID != 4242 || paused.Jobs[0].UploadParts != 1 {
		t.Fatalf("the checkpoint was not kept: %+v", paused.Jobs[0])
	}
	// The interruption is persisted with its resume state.
	saved, _, savedPaused, err := store.Load()
	if err != nil || !savedPaused || saved[0].UploadFileID != 4242 {
		t.Fatalf("persisted state lost the checkpoint: paused=%v job=%+v err=%v", savedPaused, saved[0], err)
	}

	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running && allJobsState(s.Jobs, model.JobSent) })
	calls := gateway.Calls()
	if len(calls) != 3 {
		t.Fatalf("%d upload calls, want 3 (interrupted, resumed, second file)", len(calls))
	}
	if calls[1].Resume.FileID != 4242 || calls[1].Resume.Parts != 1 {
		t.Fatalf("the resumed attempt did not receive the checkpoint: %+v", calls[1].Resume)
	}
	if calls[2].Resume.FileID != 0 {
		t.Fatal("another file must never inherit a checkpoint")
	}
	if done.Jobs[0].UploadFileID != 0 || done.Jobs[0].UploadPartsAt != nil {
		t.Fatal("a sent job must drop its resume state")
	}
}

func TestPauseNowNeverInterruptsMessageSubmission(t *testing.T) {
	jobs := resumeTestJobs(t, 2)
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	sending := make(chan struct{})
	release := make(chan struct{})
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		if request.RandomID == 1 {
			close(sending)
			<-release
		}
		// A cancelled submission would have to be reported as unknown.
		if ctx.Err() != nil {
			return 0, tgtransport.ErrSendOutcomeUnknown
		}
		return 5, nil
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-sending
	if err := controller.PauseNow(); err != nil {
		t.Fatal(err)
	}
	close(release)
	final := waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running })
	if final.Jobs[0].State != model.JobSent {
		t.Fatalf("a file being submitted must finish, got %s", final.Jobs[0].State)
	}
	if final.Jobs[1].State != model.JobQueued || !final.Paused {
		t.Fatalf("the queue must stop before the next file: %s paused=%v", final.Jobs[1].State, final.Paused)
	}
}

func TestInterruptedAttemptRefusesToSend(t *testing.T) {
	jobs := resumeTestJobs(t, 1)
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	uploaded := make(chan struct{})
	proceed := make(chan struct{})
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if request.Resume.FileID != 0 || request.RandomID == 0 {
			return 0, context.Canceled
		}
		// The data finished uploading just as the user paused: the transport
		// has not looked at the context yet and goes on to BeforeSend.
		close(uploaded)
		<-proceed
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		return 3, nil
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-uploaded
	if err := controller.PauseNow(); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	final := waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running })
	if final.Jobs[0].State != model.JobInterrupted {
		t.Fatalf("an interrupted attempt must not post its message, state = %s", final.Jobs[0].State)
	}
}

func TestOldCheckpointIsTriedAndALostOneStartsOver(t *testing.T) {
	jobs := resumeTestJobs(t, 1)
	old := time.Now().Add(-72 * time.Hour)
	jobs[0].State = model.JobInterrupted
	jobs[0].UploadFileID = 4242
	jobs[0].UploadParts = 1
	jobs[0].UploadPartSize = 512 << 10
	jobs[0].UploadPartsAt = &old
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	attempt := 0
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		attempt++
		if attempt == 1 {
			// The transport could not restore the parts under this file ID.
			request.OnCheckpoint(tgtransport.ResumeToken{})
			return 0, tgtransport.ErrUploadData
		}
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		return 4, nil
	}
	controller := newLoadedController(t, store, gateway)
	controller.uploadRetryDelays = []time.Duration{time.Millisecond}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running && s.Jobs[0].State == model.JobSent })
	calls := gateway.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d attempts, want 2", len(calls))
	}
	if calls[0].Resume.FileID != 4242 || calls[0].Resume.Parts != 1 {
		t.Fatalf("a checkpoint must be tried however old it is: %+v", calls[0].Resume)
	}
	if calls[1].Resume.FileID != 0 {
		t.Fatal("after the parts were reported lost the retry must start from scratch")
	}
}

func TestRepairProgressIsShownWhileSending(t *testing.T) {
	jobs := resumeTestJobs(t, 1)
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	repairing := make(chan struct{})
	release := make(chan struct{})
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		request.OnRepair(350, 1000)
		close(repairing)
		<-release
		return 6, nil
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-repairing
	during := waitForSnapshot(t, controller, func(s Snapshot) bool { return s.Jobs[0].Note != "" })
	if during.Jobs[0].State != model.JobSending || during.Jobs[0].Note != "Telegram 清理了之前上传的部分，正在补传 35%" {
		t.Fatalf("state=%s note=%q", during.Jobs[0].State, during.Jobs[0].Note)
	}
	close(release)
	waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running && s.Jobs[0].State == model.JobSent })
}

func TestRetryAndResetDiscardCheckpoint(t *testing.T) {
	jobs := resumeTestJobs(t, 1)
	now := time.Now()
	jobs[0].State = model.JobCancelled
	jobs[0].UploadFileID = 4242
	jobs[0].UploadParts = 1
	jobs[0].UploadPartSize = 512 << 10
	jobs[0].UploadPartsAt = &now
	controller := newLoadedController(t, &memoryQueueStore{jobs: jobs, channel: testChannel()}, &fakeGateway{})
	if err := controller.Retry(jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	job := controller.Snapshot().Jobs[0]
	if job.State != model.JobQueued || job.UploadFileID != 0 || job.UploadPartsAt != nil {
		t.Fatalf("an explicit retry starts over: %+v", job)
	}
}
