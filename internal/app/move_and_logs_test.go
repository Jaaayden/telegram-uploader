package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/mover"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

func TestMoveJobsByModeAndSelection(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "failed.mp4", State: model.JobFailed, RandomID: 8101},
		{Name: "sent.mp4", State: model.JobSent, RandomID: 8102},
		{Name: "confirming.mp4", State: model.JobConfirming, RandomID: 8103},
		{Name: "oversize.mp4", State: model.JobOversize, RandomID: 8104},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})
	var mu sync.Mutex
	var logs []model.LogEntry
	controller.SetLogSink(func(entry model.LogEntry) {
		mu.Lock()
		logs = append(logs, entry)
		mu.Unlock()
	})

	destFailed := t.TempDir()
	moved, err := controller.MoveJobs(context.Background(), MoveFailed, nil, destFailed, nil)
	if err != nil || moved != 1 {
		t.Fatalf("MoveJobs(MoveFailed) = (%d, %v), want (1, nil)", moved, err)
	}
	snap := controller.Snapshot()
	if snap.Jobs[0].State != model.JobMoved || snap.Jobs[0].Path != filepath.Join(destFailed, "failed.mp4") {
		t.Fatalf("failed job after move = %+v", snap.Jobs[0])
	}

	// Attempting to move confirming only must reject without touching disk.
	destConfirm := t.TempDir()
	if _, err := controller.MoveJobs(context.Background(), MoveSelected, []string{jobs[2].ID}, destConfirm, nil); err == nil {
		t.Fatal("MoveJobs on confirming job should fail")
	}

	// Moving sent job by explicit ID succeeds.
	destSent := t.TempDir()
	moved, err = controller.MoveJobs(context.Background(), MoveSelected, []string{jobs[1].ID}, destSent, nil)
	if err != nil || moved != 1 {
		t.Fatalf("MoveJobs(MoveSelected) = (%d, %v), want (1, nil)", moved, err)
	}
	if _, err := os.Stat(filepath.Join(destSent, "sent.mp4")); err != nil {
		t.Fatalf("moved sent file missing at destination: %v", err)
	}

	mu.Lock()
	movedLogs := 0
	for _, entry := range logs {
		if entry.Kind == "moved" {
			movedLogs++
		}
	}
	mu.Unlock()
	if movedLogs != 2 {
		t.Fatalf("moved log entries = %d, want 2", movedLogs)
	}
}

func TestMoveJobsCollisionAndFailureRollback(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	pathA := filepath.Join(dirA, "dup.mp4")
	pathB := filepath.Join(dirB, "dup.mp4")
	writeMP4Fixture(t, pathA)
	writeMP4Fixture(t, pathB)
	infoA, _ := os.Stat(pathA)
	infoB, _ := os.Stat(pathB)

	jobs := []model.Job{
		{ID: "a", Position: 0, Path: pathA, Name: "dup.mp4", Size: infoA.Size(), ModTime: infoA.ModTime(), State: model.JobFailed, RandomID: 1},
		{ID: "b", Position: 1, Path: pathB, Name: "dup.mp4", Size: infoB.Size(), ModTime: infoB.ModTime(), State: model.JobFailed, RandomID: 2},
	}
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	controller := newLoadedController(t, store, &fakeGateway{})
	dest := t.TempDir()
	if _, err := controller.MoveJobs(context.Background(), MoveFailed, nil, dest, nil); err == nil {
		t.Fatal("expected duplicate destination filename error")
	}

	// Verify failure rollback preserves MoveFromState (JobFailed, not JobOversize).
	controller2 := NewController(&memoryQueueStore{jobs: jobs[:1], channel: testChannel()}, mover.New())
	if err := controller2.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := controller2.MoveJobs(context.Background(), MoveFailed, nil, filepath.Join(t.TempDir(), "missing-dest"), nil); err == nil {
		t.Fatal("expected missing destination error")
	}
	if got := controller2.Snapshot().Jobs[0].State; got != model.JobFailed {
		t.Fatalf("rolled back state = %s, want failed", got)
	}

	// Verify reconcileMovingJobs restores MoveFromState on interrupted move.
	reconcileJobs := []model.Job{
		{ID: "m1", Path: pathA, MoveDestination: filepath.Join(dest, "missing.mp4"), State: model.JobMoving, MoveFromState: model.JobFailed},
	}
	if !reconcileMovingJobs(reconcileJobs) {
		t.Fatal("reconcileMovingJobs() = false, want true")
	}
	if reconcileJobs[0].State != model.JobFailed || reconcileJobs[0].MoveFromState != "" {
		t.Fatalf("reconciled job = %+v, want failed with cleared MoveFromState", reconcileJobs[0])
	}
}

func TestJobSentPersistsStatsAndEmitsLogEntries(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "single.mp4", RandomID: 8201},
		{Name: "alb-1.mp4", RandomID: 8202},
		{Name: "alb-2.mp4", RandomID: 8203},
	})
	jobs[1].GroupID = "g1"
	jobs[2].GroupID = "g1"
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeAlbumGateway{
		fakeGateway: &fakeGateway{
			upload: func(ctx context.Context, req tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
				time.Sleep(25 * time.Millisecond)
				if err := req.BeforeSend(); err != nil {
					return 0, err
				}
				return int(req.RandomID), nil
			},
		},
		stageFunc: func(ctx context.Context, req tgtransport.UploadRequest, _ func(model.Progress)) (model.StagedMedia, error) {
			time.Sleep(20 * time.Millisecond)
			return model.StagedMedia{DocumentID: req.RandomID, AccessHash: 1}, nil
		},
		sendFunc: func(ctx context.Context, _ model.Channel, items []tgtransport.AlbumItem) ([]int, error) {
			time.Sleep(10 * time.Millisecond)
			return []int{901, 902}, nil
		},
	}
	controller := newLoadedAlbumController(t, store, gateway)
	var mu sync.Mutex
	var entries []model.LogEntry
	controller.SetLogSink(func(entry model.LogEntry) {
		mu.Lock()
		entries = append(entries, entry)
		mu.Unlock()
	})
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForSnapshot(t, controller, func(s Snapshot) bool { return allJobsState(s.Jobs, model.JobSent) })
	if err := controller.WaitStopped(context.Background()); err != nil {
		t.Fatalf("WaitStopped() error = %v", err)
	}

	snap := controller.Snapshot()
	for i, job := range snap.Jobs {
		if job.CompletedAt == nil || job.UploadElapsedMs <= 0 || job.AvgSpeed <= 0 {
			t.Fatalf("job[%d] missing completion stats: %+v", i, job)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	var sentCount, albumCount, summaryCount int
	for _, entry := range entries {
		switch entry.Kind {
		case "sent":
			sentCount++
		case "album":
			albumCount++
		case "summary":
			summaryCount++
		}
	}
	if sentCount != 3 || albumCount != 1 || summaryCount != 1 {
		t.Fatalf("log entry counts = sent:%d album:%d summary:%d, want 3/1/1 (entries=%+v)", sentCount, albumCount, summaryCount, entries)
	}
}

func TestMoveJobsRejectsRunningQueue(t *testing.T) {
	controller := NewController(&memoryQueueStore{}, mover.New())
	controller.mu.Lock()
	controller.running = true
	controller.mu.Unlock()
	if _, err := controller.MoveJobs(context.Background(), MoveFailed, nil, t.TempDir(), nil); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("expected running queue error, got %v", err)
	}
}
