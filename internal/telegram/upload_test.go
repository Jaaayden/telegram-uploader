package telegram

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/jayden/telegram-video-uploader/internal/model"
)

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	return len(p), nil
}

type recordingUploadClient struct {
	mu         sync.Mutex
	partSizes  map[int]int
	totalParts map[int]int
}

func (c *recordingUploadClient) UploadSaveFilePart(context.Context, *tg.UploadSaveFilePartRequest) (bool, error) {
	return true, nil
}

func (c *recordingUploadClient) UploadSaveBigFilePart(_ context.Context, request *tg.UploadSaveBigFilePartRequest) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.partSizes == nil {
		c.partSizes = make(map[int]int)
		c.totalParts = make(map[int]int)
	}
	c.partSizes[request.FilePart] = len(request.Bytes)
	c.totalParts[request.FilePart] = request.FileTotalParts
	return true, nil
}

type blockingUploadClient struct {
	started chan struct{}
	release <-chan struct{}

	mu        sync.Mutex
	active    int
	maxActive int
}

func (c *blockingUploadClient) UploadSaveFilePart(context.Context, *tg.UploadSaveFilePartRequest) (bool, error) {
	return true, nil
}

func (c *blockingUploadClient) UploadSaveBigFilePart(ctx context.Context, _ *tg.UploadSaveBigFilePartRequest) (bool, error) {
	c.mu.Lock()
	c.active++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
	c.mu.Unlock()

	select {
	case c.started <- struct{}{}:
	default:
	}

	select {
	case <-c.release:
	case <-ctx.Done():
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
		return false, ctx.Err()
	}

	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return true, nil
}

func (c *blockingUploadClient) maximumActive() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxActive
}

func TestPrepareUploadRequestPreservesExplicitEmptyCaption(t *testing.T) {
	request := prepareUploadRequest(UploadRequest{Path: "/videos/.mp4", Caption: ""})
	if request.Name != ".mp4" {
		t.Fatalf("Name = %q, want .mp4", request.Name)
	}
	if request.Caption != "" {
		t.Fatalf("Caption = %q, want explicit empty caption", request.Caption)
	}
}

func TestUploadEngineUsesRecommendedMaximumPartSize(t *testing.T) {
	const total = int64(10*1024*1024 + uploadPartSize + 1)
	client := &recordingUploadClient{}
	engine := newUploadEngine(client, nil)

	if _, err := engine.Upload(context.Background(), uploader.NewUpload(
		"video.mp4",
		io.LimitReader(zeroReader{}, total),
		total,
	)); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	wantParts := int((total + int64(uploadPartSize) - 1) / int64(uploadPartSize))
	if len(client.partSizes) != wantParts {
		t.Fatalf("uploaded parts = %d, want %d", len(client.partSizes), wantParts)
	}
	for part := 0; part < wantParts; part++ {
		wantSize := uploadPartSize
		if part == wantParts-1 {
			wantSize = int(total - int64(part*uploadPartSize))
		}
		if got := client.partSizes[part]; got != wantSize {
			t.Fatalf("part %d size = %d, want %d", part, got, wantSize)
		}
		if got := client.totalParts[part]; got != wantParts {
			t.Fatalf("part %d total parts = %d, want %d", part, got, wantParts)
		}
	}
}

func TestUploadEngineKeepsConfiguredPartsInFlight(t *testing.T) {
	const total = int64(10*1024*1024 + uploadPartSize + 1)
	started := make(chan struct{}, uploadThreads)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer releaseAll()

	client := &blockingUploadClient{started: started, release: release}
	engine := newUploadEngine(client, nil)
	done := make(chan error, 1)
	go func() {
		_, err := engine.Upload(context.Background(), uploader.NewUpload(
			"video.mp4",
			io.LimitReader(zeroReader{}, total),
			total,
		))
		done <- err
	}()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for i := 0; i < uploadThreads; i++ {
		select {
		case <-started:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d concurrent upload parts", uploadThreads)
		}
	}
	if got := client.maximumActive(); got != uploadThreads {
		t.Fatalf("maximum active upload parts = %d, want %d", got, uploadThreads)
	}

	releaseAll()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Upload() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Upload() did not finish after releasing part RPCs")
	}
}

func TestReserveSendSlotUpdatesTimestampBeforeFirstSend(t *testing.T) {
	client := &Client{}
	before := client.lastSend

	if err := client.reserveSendSlot(context.Background()); err != nil {
		t.Fatalf("reserveSendSlot() error = %v", err)
	}
	if client.lastSend.IsZero() || !client.lastSend.After(before) {
		t.Fatalf("lastSend = %v, want a timestamp newer than %v", client.lastSend, before)
	}
}

func TestReserveSendSlotCancellationDoesNotReserveSlot(t *testing.T) {
	client := &Client{lastSend: time.Now()}
	before := client.lastSend

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.reserveSendSlot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("reserveSendSlot() error = %v, want context.Canceled", err)
	}
	if !client.lastSend.Equal(before) {
		t.Fatalf("lastSend changed to %v after canceled wait; want %v", client.lastSend, before)
	}
}

func TestUploadProgressReporterCoalescesIntermediateUpdatesButKeepsFinal(t *testing.T) {
	var updates []model.Progress
	reporter := &uploadProgressReporter{
		started:  time.Now(),
		interval: time.Hour,
		callback: func(progress model.Progress) { updates = append(updates, progress) },
	}

	states := []uploader.ProgressState{
		{Uploaded: 10, Total: 100},
		{Uploaded: 50, Total: 100},
		{Uploaded: 100, Total: 100},
	}
	for _, state := range states {
		if err := reporter.Chunk(context.Background(), state); err != nil {
			t.Fatalf("Chunk(%+v) error = %v", state, err)
		}
	}
	if len(updates) != 2 || updates[0].BytesDone != 10 || updates[1].BytesDone != 100 {
		t.Fatalf("progress updates = %+v, want first and final only", updates)
	}
}

func TestUploadProgressReporterIgnoresOutOfOrderUpdates(t *testing.T) {
	var updates []model.Progress
	reporter := &uploadProgressReporter{
		started:  time.Now(),
		interval: 0,
		callback: func(progress model.Progress) { updates = append(updates, progress) },
	}
	for _, uploaded := range []int64{80, 60, 100} {
		if err := reporter.Chunk(context.Background(), uploader.ProgressState{Uploaded: uploaded, Total: 100}); err != nil {
			t.Fatalf("Chunk(%d) error = %v", uploaded, err)
		}
	}
	if len(updates) != 2 || updates[0].BytesDone != 80 || updates[1].BytesDone != 100 {
		t.Fatalf("progress updates = %+v, want monotonic [80, 100]", updates)
	}
}

func TestUploadProgressReporterSerializesConcurrentUpdates(t *testing.T) {
	var updates []model.Progress
	reporter := &uploadProgressReporter{
		started:  time.Now(),
		interval: 0,
		callback: func(progress model.Progress) { updates = append(updates, progress) },
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, uploaded := range []int64{10, 70, 30, 90, 20, 60, 100, 80} {
		uploaded := uploaded
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			if err := reporter.Chunk(context.Background(), uploader.ProgressState{Uploaded: uploaded, Total: 100}); err != nil {
				t.Errorf("Chunk(%d) error = %v", uploaded, err)
			}
		}()
	}
	close(start)
	workers.Wait()
	if len(updates) == 0 || updates[len(updates)-1].BytesDone != 100 {
		t.Fatalf("final progress updates = %+v, want 100 bytes", updates)
	}
	for i := 1; i < len(updates); i++ {
		if updates[i].BytesDone < updates[i-1].BytesDone {
			t.Fatalf("progress regressed from %d to %d: %+v", updates[i-1].BytesDone, updates[i].BytesDone, updates)
		}
	}
}
