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
	engine := newUploadEngine(client, nil, DefaultUploadConcurrency)

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
	const threads = UploadConcurrencyBalanced
	started := make(chan struct{}, threads)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer releaseAll()

	client := &blockingUploadClient{started: started, release: release}
	engine := newUploadEngine(client, nil, threads)
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
	for i := 0; i < threads; i++ {
		select {
		case <-started:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d concurrent upload parts", threads)
		}
	}
	if got := client.maximumActive(); got != threads {
		t.Fatalf("maximum active upload parts = %d, want %d", got, threads)
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

func TestUploadConcurrencyProfilesAreNormalized(t *testing.T) {
	tests := []struct {
		value int
		want  int
	}{
		{value: 0, want: DefaultUploadConcurrency},
		{value: UploadConcurrencyCompatibility, want: UploadConcurrencyCompatibility},
		{value: UploadConcurrencyBalanced, want: UploadConcurrencyBalanced},
		{value: UploadConcurrencyFast, want: UploadConcurrencyFast},
		{value: 99, want: DefaultUploadConcurrency},
	}
	for _, test := range tests {
		if got := normalizeUploadConcurrency(test.value); got != test.want {
			t.Errorf("normalizeUploadConcurrency(%d) = %d, want %d", test.value, got, test.want)
		}
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

func TestUploadProgressReporterCoalescesIntermediateUpdatesAndKeepsFinal(t *testing.T) {
	var updates []model.Progress
	reporter := newUploadProgressReporter(func(progress model.Progress) { updates = append(updates, progress) })
	reporter.interval = time.Hour

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
	reporter.Close()
	if len(updates) == 0 || updates[len(updates)-1].BytesDone != 100 {
		t.Fatalf("progress updates = %+v, want a final 100-byte update", updates)
	}
	if len(updates) > 2 {
		t.Fatalf("progress updates = %+v, want at most one intermediate plus final", updates)
	}
}

func TestUploadProgressReporterIgnoresOutOfOrderUpdates(t *testing.T) {
	var updates []model.Progress
	reporter := newUploadProgressReporter(func(progress model.Progress) { updates = append(updates, progress) })
	reporter.interval = 0
	for _, uploaded := range []int64{80, 60, 100} {
		if err := reporter.Chunk(context.Background(), uploader.ProgressState{Uploaded: uploaded, Total: 100}); err != nil {
			t.Fatalf("Chunk(%d) error = %v", uploaded, err)
		}
	}
	reporter.Close()
	if len(updates) == 0 || updates[len(updates)-1].BytesDone != 100 {
		t.Fatalf("progress updates = %+v, want a final 100-byte update", updates)
	}
	for i := 1; i < len(updates); i++ {
		if updates[i].BytesDone < updates[i-1].BytesDone {
			t.Fatalf("progress regressed from %d to %d: %+v", updates[i-1].BytesDone, updates[i].BytesDone, updates)
		}
	}
}

func TestUploadProgressReporterSerializesConcurrentUpdates(t *testing.T) {
	var updates []model.Progress
	reporter := newUploadProgressReporter(func(progress model.Progress) { updates = append(updates, progress) })
	reporter.interval = 0
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
	reporter.Close()
	if len(updates) == 0 || updates[len(updates)-1].BytesDone != 100 {
		t.Fatalf("final progress updates = %+v, want 100 bytes", updates)
	}
	for i := 1; i < len(updates); i++ {
		if updates[i].BytesDone < updates[i-1].BytesDone {
			t.Fatalf("progress regressed from %d to %d: %+v", updates[i-1].BytesDone, updates[i].BytesDone, updates)
		}
	}
}

func TestUploadProgressReporterDoesNotBlockWorkersDuringCallback(t *testing.T) {
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	reporter := newUploadProgressReporter(func(model.Progress) {
		close(callbackStarted)
		<-releaseCallback
	})
	reporter.interval = time.Hour
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- reporter.Chunk(context.Background(), uploader.ProgressState{Uploaded: 10, Total: 100})
	}()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("first progress callback did not start")
	}

	// This update is coalesced. It must still be able to inspect and update the
	// reporter state while the previous callback is deliberately blocked.
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- reporter.Chunk(context.Background(), uploader.ProgressState{Uploaded: 20, Total: 100})
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("coalesced Chunk() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("coalesced Chunk() waited for the previous callback")
	}

	close(releaseCallback)
	reporter.Close()
	if err := <-firstDone; err != nil {
		t.Fatalf("first Chunk() error = %v", err)
	}
}
