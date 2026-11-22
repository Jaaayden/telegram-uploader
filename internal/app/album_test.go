package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"

	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

type albumSendCall struct {
	channel model.Channel
	items   []tgtransport.AlbumItem
}

type fakeAlbumGateway struct {
	*fakeGateway

	albumMu   sync.Mutex
	stageFunc func(context.Context, tgtransport.UploadRequest, func(model.Progress)) (model.StagedMedia, error)
	sendFunc  func(context.Context, model.Channel, []tgtransport.AlbumItem) ([]int, error)
	stages    []tgtransport.UploadRequest
	sends     []albumSendCall
}

func newFakeAlbumGateway() *fakeAlbumGateway {
	return &fakeAlbumGateway{fakeGateway: &fakeGateway{}}
}

func (g *fakeAlbumGateway) StageVideo(ctx context.Context, request tgtransport.UploadRequest, progress func(model.Progress)) (model.StagedMedia, error) {
	g.albumMu.Lock()
	g.stages = append(g.stages, request)
	fn := g.stageFunc
	index := len(g.stages)
	g.albumMu.Unlock()
	if fn != nil {
		return fn(ctx, request, progress)
	}
	return model.StagedMedia{
		DocumentID:    int64(1000 + index),
		AccessHash:    int64(2000 + index),
		FileReference: []byte{byte(index)},
	}, nil
}

func (g *fakeAlbumGateway) SendAlbum(ctx context.Context, channel model.Channel, items []tgtransport.AlbumItem) ([]int, error) {
	copied := make([]tgtransport.AlbumItem, len(items))
	for i, item := range items {
		copied[i] = item
		if item.Media.FileReference != nil {
			copied[i].Media.FileReference = append([]byte(nil), item.Media.FileReference...)
		}
	}
	g.albumMu.Lock()
	g.sends = append(g.sends, albumSendCall{channel: channel, items: copied})
	fn := g.sendFunc
	g.albumMu.Unlock()
	if fn != nil {
		return fn(ctx, channel, copied)
	}
	ids := make([]int, len(items))
	for i := range items {
		ids[i] = 500 + i + 1
	}
	return ids, nil
}

func (g *fakeAlbumGateway) StageCalls() []tgtransport.UploadRequest {
	g.albumMu.Lock()
	defer g.albumMu.Unlock()
	return append([]tgtransport.UploadRequest(nil), g.stages...)
}

func (g *fakeAlbumGateway) SendCalls() []albumSendCall {
	g.albumMu.Lock()
	defer g.albumMu.Unlock()
	return append([]albumSendCall(nil), g.sends...)
}

func newLoadedAlbumController(t *testing.T, store *memoryQueueStore, gateway tgtransport.Gateway) *Controller {
	t.Helper()
	controller := NewController(store, nil)
	if err := controller.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	controller.SetGateway(gateway)
	return controller
}

func TestAlbumStagesAndSendsInOrder(t *testing.T) {
	session, err := diagnostics.Start(t.TempDir())
	if err != nil {
		t.Fatalf("diagnostics.Start() error = %v", err)
	}
	logPath := session.AppLogPath()

	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "01-intro.mp4", RandomID: 8001},
		{Name: "02-middle.mp4", RandomID: 8002},
		{Name: "03-outro.mp4", RandomID: 8003},
	})
	for i := range jobs {
		jobs[i].GroupID = "grp-alpha"
	}
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := newFakeAlbumGateway()
	gateway.stageFunc = func(_ context.Context, request tgtransport.UploadRequest, progress func(model.Progress)) (model.StagedMedia, error) {
		time.Sleep(25 * time.Millisecond)
		if progress != nil {
			progress(model.Progress{BytesDone: 10, BytesTotal: 10, BytesPerSecond: 100, At: time.Now()})
		}
		return model.StagedMedia{
			DocumentID:    request.RandomID * 10,
			AccessHash:    99,
			FileReference: []byte{byte(request.RandomID & 0xff)},
		}, nil
	}
	var persistedSending bool
	gateway.sendFunc = func(_ context.Context, _ model.Channel, items []tgtransport.AlbumItem) ([]int, error) {
		saved := store.SnapshotJobs()
		persistedSending = len(saved) == 3 &&
			saved[0].State == model.JobSending &&
			saved[1].State == model.JobSending &&
			saved[2].State == model.JobSending
		ids := make([]int, len(items))
		for i, item := range items {
			ids[i] = int(item.RandomID) + 100
		}
		return ids, nil
	}

	controller := newLoadedAlbumController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	final := waitForSnapshot(t, controller, func(s Snapshot) bool {
		return !s.Running && allJobsState(s.Jobs, model.JobSent)
	})
	if err := controller.WaitStopped(context.Background()); err != nil {
		t.Fatalf("WaitStopped() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("session.Close() error = %v", err)
	}

	if !persistedSending {
		t.Fatal("expected all album jobs to be persisted as JobSending before SendAlbum")
	}
	if len(gateway.Calls()) != 0 {
		t.Fatalf("UploadVideo calls = %d, want 0", len(gateway.Calls()))
	}
	stages := gateway.StageCalls()
	if len(stages) != 3 {
		t.Fatalf("StageVideo calls = %d, want 3", len(stages))
	}
	sends := gateway.SendCalls()
	if len(sends) != 1 {
		t.Fatalf("SendAlbum calls = %d, want 1", len(sends))
	}
	wantCaptions := []string{"01-intro", "02-middle", "03-outro"}
	for i, item := range sends[0].items {
		if item.Caption != wantCaptions[i] {
			t.Errorf("item %d caption = %q, want %q", i, item.Caption, wantCaptions[i])
		}
		if item.RandomID != jobs[i].RandomID {
			t.Errorf("item %d randomID = %d, want %d", i, item.RandomID, jobs[i].RandomID)
		}
		if item.Media.DocumentID != jobs[i].RandomID*10 {
			t.Errorf("item %d documentID = %d, want %d", i, item.Media.DocumentID, jobs[i].RandomID*10)
		}
	}
	for i, job := range final.Jobs {
		if job.MessageID != int(jobs[i].RandomID)+100 {
			t.Errorf("job %d MessageID = %d, want %d", i, job.MessageID, int(jobs[i].RandomID)+100)
		}
		if job.ChannelID != testChannel().ID {
			t.Errorf("job %d ChannelID = %d, want %d", i, job.ChannelID, testChannel().ID)
		}
		if job.Staged != nil {
			t.Errorf("job %d Staged not cleared after send", i)
		}
		if job.Uploaded != job.Size {
			t.Errorf("job %d Uploaded = %d, want %d", i, job.Uploaded, job.Size)
		}
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var sentLines, stopLines []string
	for _, line := range strings.Split(string(data), "\n") {
		var rec struct{ Message string }
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if strings.HasPrefix(rec.Message, "job_sent ") {
			sentLines = append(sentLines, rec.Message)
		} else if strings.HasPrefix(rec.Message, "queue_stop ") {
			stopLines = append(stopLines, rec.Message)
		}
	}
	if len(sentLines) != 3 || len(stopLines) != 1 {
		t.Fatalf("job_sent=%d queue_stop=%d, want 3 and 1\n%s", len(sentLines), len(stopLines), data)
	}
	if !strings.Contains(stopLines[0], "sent=3 ") || strings.Contains(stopLines[0], "avg_speed=n/a") {
		t.Fatalf("unexpected queue_stop summary: %s", stopLines[0])
	}
}

func TestAddMergedJobsChunkingAndOversize(t *testing.T) {
	makeSpecs := func(n int) []fixtureJob {
		specs := make([]fixtureJob, n)
		for i := range specs {
			specs[i] = fixtureJob{
				Name:     fmt.Sprintf("%02d.mp4", i+1),
				RandomID: int64(9000 + i + 1),
			}
		}
		return specs
	}

	t.Run("23 videos split into 10+10+3", func(t *testing.T) {
		jobs, _ := fixtureJobs(t, makeSpecs(23))
		controller := NewController(&memoryQueueStore{}, nil)
		res, err := controller.AddMergedJobs(jobs)
		if err != nil {
			t.Fatalf("AddMergedJobs() error = %v", err)
		}
		if res.Added != 23 || res.Groups != 3 || res.Singles != 0 {
			t.Fatalf("AddResult = %+v, want {Added:23 Groups:3 Singles:0}", res)
		}
		got := controller.Snapshot().Jobs
		groups := make(map[string]int)
		for _, job := range got {
			if job.GroupID == "" {
				t.Fatalf("job %s unexpectedly has empty GroupID", job.ID)
			}
			groups[job.GroupID]++
		}
		if len(groups) != 3 {
			t.Fatalf("distinct groups = %d, want 3", len(groups))
		}
		for i := 0; i < 10; i++ {
			if got[i].GroupID != got[0].GroupID {
				t.Fatalf("first chunk mismatch at %d", i)
			}
		}
		for i := 10; i < 20; i++ {
			if got[i].GroupID != got[10].GroupID || got[i].GroupID == got[0].GroupID {
				t.Fatalf("second chunk mismatch at %d", i)
			}
		}
		for i := 20; i < 23; i++ {
			if got[i].GroupID != got[20].GroupID || got[i].GroupID == got[10].GroupID {
				t.Fatalf("third chunk mismatch at %d", i)
			}
		}
	})

	t.Run("11 videos split into 10+1", func(t *testing.T) {
		jobs, _ := fixtureJobs(t, makeSpecs(11))
		controller := NewController(&memoryQueueStore{}, nil)
		res, err := controller.AddMergedJobs(jobs)
		if err != nil {
			t.Fatalf("AddMergedJobs() error = %v", err)
		}
		if res.Added != 11 || res.Groups != 1 || res.Singles != 1 {
			t.Fatalf("AddResult = %+v, want {Added:11 Groups:1 Singles:1}", res)
		}
		got := controller.Snapshot().Jobs
		for i := 0; i < 10; i++ {
			if got[i].GroupID == "" || got[i].GroupID != got[0].GroupID {
				t.Fatalf("job %d group = %q, want %q", i, got[i].GroupID, got[0].GroupID)
			}
		}
		if got[10].GroupID != "" {
			t.Fatalf("trailing single job got GroupID %q, want empty", got[10].GroupID)
		}
	})

	t.Run("oversize excluded from grouping", func(t *testing.T) {
		jobs, _ := fixtureJobs(t, makeSpecs(3))
		jobs[1].State = model.JobOversize
		controller := NewController(&memoryQueueStore{}, nil)
		res, err := controller.AddMergedJobs(jobs)
		if err != nil {
			t.Fatalf("AddMergedJobs() error = %v", err)
		}
		if res.Added != 3 || res.Groups != 1 || res.Singles != 0 {
			t.Fatalf("AddResult = %+v, want {Added:3 Groups:1 Singles:0}", res)
		}
		got := controller.Snapshot().Jobs
		if got[1].GroupID != "" {
			t.Fatalf("oversize job got GroupID %q", got[1].GroupID)
		}
		if got[0].GroupID == "" || got[0].GroupID != got[2].GroupID {
			t.Fatalf("eligible jobs not grouped together: %q vs %q", got[0].GroupID, got[2].GroupID)
		}
	})
}

func TestAlbumPauseNowPreservesStagedAndResumes(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "a.mp4", RandomID: 11},
		{Name: "b.mp4", RandomID: 12},
		{Name: "c.mp4", RandomID: 13},
	})
	for i := range jobs {
		jobs[i].GroupID = "grp-resume"
	}
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := newFakeAlbumGateway()
	secondStarted := make(chan struct{})
	gateway.stageFunc = func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (model.StagedMedia, error) {
		if request.RandomID == 12 && request.Resume.FileID == 0 {
			request.OnCheckpoint(checkpointFor(jobs[1], 1))
			close(secondStarted)
			<-ctx.Done()
			return model.StagedMedia{}, ctx.Err()
		}
		return model.StagedMedia{DocumentID: request.RandomID * 100, AccessHash: 1, FileReference: []byte("ref")}, nil
	}

	controller := newLoadedAlbumController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-secondStarted
	if err := controller.PauseNow(); err != nil {
		t.Fatal(err)
	}
	paused := waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running })
	if paused.Jobs[0].State != model.JobQueued || paused.Jobs[0].Staged == nil || paused.Jobs[0].Uploaded != paused.Jobs[0].Size {
		t.Fatalf("first staged job not preserved across PauseNow: %+v", paused.Jobs[0])
	}
	if paused.Jobs[1].State != model.JobInterrupted || paused.Jobs[1].UploadFileID != 4242 || paused.Jobs[1].UploadParts != 1 {
		t.Fatalf("second interrupted job lost checkpoint: %+v", paused.Jobs[1])
	}
	if paused.Jobs[2].State != model.JobQueued || paused.Jobs[2].Staged != nil {
		t.Fatalf("third job state = %+v", paused.Jobs[2])
	}

	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running && allJobsState(s.Jobs, model.JobSent) })
	if len(done.Jobs) != 3 {
		t.Fatal("expected 3 jobs")
	}
	stages := gateway.StageCalls()
	// 1st staged, 2nd interrupted, 2nd resumed, 3rd staged -> 4 total calls; 1st must NOT be re-staged.
	if len(stages) != 4 {
		t.Fatalf("StageVideo calls = %d, want 4", len(stages))
	}
	if stages[2].RandomID != 12 || stages[2].Resume.FileID != 4242 {
		t.Fatalf("resumed second job did not carry checkpoint: %+v", stages[2])
	}
	sends := gateway.SendCalls()
	if len(sends) != 1 || len(sends[0].items) != 3 {
		t.Fatalf("SendAlbum calls = %+v, want 1 call with 3 items", sends)
	}
}

func TestAlbumSoftPauseAndPauseNowDuringSendAlbum(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "one.mp4", RandomID: 21},
		{Name: "two.mp4", RandomID: 22},
	})
	for i := range jobs {
		jobs[i].GroupID = "grp-pause"
	}
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := newFakeAlbumGateway()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	sending := make(chan struct{})
	releaseSend := make(chan struct{})

	gateway.stageFunc = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (model.StagedMedia, error) {
		if request.RandomID == 21 {
			close(firstStarted)
			<-releaseFirst
		}
		return model.StagedMedia{DocumentID: request.RandomID, AccessHash: 1}, nil
	}
	gateway.sendFunc = func(ctx context.Context, _ model.Channel, items []tgtransport.AlbumItem) ([]int, error) {
		close(sending)
		<-releaseSend
		if ctx.Err() != nil {
			return nil, tgtransport.ErrSendOutcomeUnknown
		}
		return []int{601, 602}, nil
	}

	controller := newLoadedAlbumController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-firstStarted
	if err := controller.Pause(); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	paused := waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running })
	if paused.Jobs[0].Staged == nil || paused.Jobs[1].Staged != nil {
		t.Fatalf("soft pause should stop after staging first video: %+v", paused.Jobs)
	}

	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-sending
	// PauseNow must NOT interrupt an active SendAlbum submission.
	if err := controller.PauseNow(); err != nil {
		t.Fatal(err)
	}
	close(releaseSend)
	done := waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running && allJobsState(s.Jobs, model.JobSent) })
	if !done.Paused {
		t.Fatal("PauseNow paused intent should be recorded even though active SendAlbum finished")
	}
}

func TestAlbumUnknownOutcomeMarksWholeBatchConfirming(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "a.mp4", RandomID: 31},
		{Name: "b.mp4", RandomID: 32},
		{Name: "c.mp4", RandomID: 33},
	})
	jobs[0].GroupID = "grp-confirm"
	jobs[1].GroupID = "grp-confirm"
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := newFakeAlbumGateway()
	gateway.sendFunc = func(context.Context, model.Channel, []tgtransport.AlbumItem) ([]int, error) {
		return nil, fmt.Errorf("%w：network drop", tgtransport.ErrSendOutcomeUnknown)
	}

	controller := newLoadedAlbumController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := waitForSnapshot(t, controller, func(s Snapshot) bool {
		return !s.Running && s.Jobs[0].State == model.JobConfirming
	})
	if snap.Jobs[0].State != model.JobConfirming || snap.Jobs[1].State != model.JobConfirming {
		t.Fatalf("album jobs = %s, %s; want both confirming", snap.Jobs[0].State, snap.Jobs[1].State)
	}
	if snap.Jobs[2].State != model.JobQueued {
		t.Fatalf("following job state = %s, want queued (queue stopped)", snap.Jobs[2].State)
	}
	if err := controller.MarkSent(snap.Jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := controller.Snapshot().Jobs[0]; got.State != model.JobSent || got.Staged != nil {
		t.Fatalf("after MarkSent = %+v", got)
	}
}

func TestAlbumStaleMediaIndexRestagesOnlyExpiredItem(t *testing.T) {
	t.Run("restages only expired index and succeeds", func(t *testing.T) {
		jobs, _ := fixtureJobs(t, []fixtureJob{
			{Name: "1.mp4", RandomID: 41},
			{Name: "2.mp4", RandomID: 42},
			{Name: "3.mp4", RandomID: 43},
		})
		for i := range jobs {
			jobs[i].GroupID = "grp-stale"
		}
		store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
		gateway := newFakeAlbumGateway()
		var sendCount int
		gateway.sendFunc = func(_ context.Context, _ model.Channel, items []tgtransport.AlbumItem) ([]int, error) {
			sendCount++
			if sendCount == 1 {
				return nil, fmt.Errorf("%w：%w", tgtransport.ErrAlbumRejected, tgerr.New(400, "FILE_REFERENCE_0_EXPIRED"))
			}
			return []int{701, 702, 703}, nil
		}

		controller := newLoadedAlbumController(t, store, gateway)
		if err := controller.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitForSnapshot(t, controller, func(s Snapshot) bool { return !s.Running && allJobsState(s.Jobs, model.JobSent) })
		stages := gateway.StageCalls()
		if len(stages) != 4 {
			t.Fatalf("StageVideo calls = %d, want 4 (1, 2, 3, then only 1)", len(stages))
		}
		if stages[3].RandomID != 41 {
			t.Fatalf("restaged RandomID = %d, want 41", stages[3].RandomID)
		}
		if sends := gateway.SendCalls(); len(sends) != 2 {
			t.Fatalf("SendAlbum calls = %d, want 2", len(sends))
		}
	})

	t.Run("repeated expiry fails instead of looping", func(t *testing.T) {
		jobs, _ := fixtureJobs(t, []fixtureJob{
			{Name: "1.mp4", RandomID: 51},
			{Name: "2.mp4", RandomID: 52},
		})
		for i := range jobs {
			jobs[i].GroupID = "grp-stale-loop"
		}
		store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
		gateway := newFakeAlbumGateway()
		gateway.sendFunc = func(context.Context, model.Channel, []tgtransport.AlbumItem) ([]int, error) {
			return nil, fmt.Errorf("%w：%w", tgtransport.ErrAlbumRejected, tgerr.New(400, "FILE_REFERENCE_0_EXPIRED"))
		}

		controller := newLoadedAlbumController(t, store, gateway)
		if err := controller.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		snap := waitForSnapshot(t, controller, func(s Snapshot) bool {
			return !s.Running && allJobsState(s.Jobs, model.JobFailed)
		})
		if len(gateway.SendCalls()) != 2 {
			t.Fatalf("SendAlbum calls = %d, want 2 (initial + 1 restage attempt)", len(gateway.SendCalls()))
		}
		if snap.Jobs[0].Staged != nil || snap.Jobs[1].Staged != nil {
			t.Fatal("failed album jobs must clear Staged")
		}
	})
}

func TestAlbumRejectedFailsBatchAndContinuesQueue(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "bad1.mp4", RandomID: 61},
		{Name: "bad2.mp4", RandomID: 62},
		{Name: "good.mp4", RandomID: 63},
	})
	jobs[0].GroupID = "grp-bad"
	jobs[1].GroupID = "grp-bad"
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := newFakeAlbumGateway()
	gateway.sendFunc = func(context.Context, model.Channel, []tgtransport.AlbumItem) ([]int, error) {
		return nil, fmt.Errorf("%w：%w", tgtransport.ErrAlbumRejected, tgerr.New(400, "MEDIA_CAPTION_TOO_LONG"))
	}
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		return 888, nil
	}

	controller := newLoadedAlbumController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := waitForSnapshot(t, controller, func(s Snapshot) bool {
		return !s.Running && s.Jobs[2].State == model.JobSent
	})
	if snap.Jobs[0].State != model.JobFailed || snap.Jobs[1].State != model.JobFailed {
		t.Fatalf("album states = %s, %s; want both failed", snap.Jobs[0].State, snap.Jobs[1].State)
	}
	if snap.Jobs[2].MessageID != 888 {
		t.Fatalf("single fallback job MessageID = %d, want 888", snap.Jobs[2].MessageID)
	}
}

func TestAlbumCancelSiblingBeforeAndAfterStaging(t *testing.T) {
	t.Run("cancelled before upload uses UploadVideo", func(t *testing.T) {
		jobs, _ := fixtureJobs(t, []fixtureJob{
			{Name: "keep.mp4", RandomID: 71},
			{Name: "drop.mp4", RandomID: 72},
		})
		jobs[0].GroupID = "grp-one"
		jobs[1].GroupID = "grp-one"
		store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
		gateway := newFakeAlbumGateway()
		gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
			if err := request.BeforeSend(); err != nil {
				return 0, err
			}
			return 901, nil
		}
		controller := newLoadedAlbumController(t, store, gateway)
		if err := controller.CancelJob(jobs[1].ID); err != nil {
			t.Fatal(err)
		}
		if err := controller.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		snap := waitForSnapshot(t, controller, func(s Snapshot) bool {
			return !s.Running && s.Jobs[0].State == model.JobSent
		})
		if snap.Jobs[0].MessageID != 901 || snap.Jobs[1].State != model.JobCancelled {
			t.Fatalf("unexpected states: %+v", snap.Jobs)
		}
		if len(gateway.StageCalls()) != 0 || len(gateway.SendCalls()) != 0 || len(gateway.Calls()) != 1 {
			t.Fatalf("stages=%d sends=%d uploads=%d, want 0/0/1", len(gateway.StageCalls()), len(gateway.SendCalls()), len(gateway.Calls()))
		}
	})

	t.Run("cancelled during second stage sends already staged item via SendAlbum", func(t *testing.T) {
		jobs, _ := fixtureJobs(t, []fixtureJob{
			{Name: "staged.mp4", RandomID: 81},
			{Name: "cancelme.mp4", RandomID: 82},
		})
		jobs[0].GroupID = "grp-two"
		jobs[1].GroupID = "grp-two"
		store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
		gateway := newFakeAlbumGateway()
		secondStarted := make(chan struct{})
		gateway.stageFunc = func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (model.StagedMedia, error) {
			if request.RandomID == 82 {
				close(secondStarted)
				<-ctx.Done()
				return model.StagedMedia{}, ctx.Err()
			}
			return model.StagedMedia{DocumentID: 8100, AccessHash: 1}, nil
		}
		controller := newLoadedAlbumController(t, store, gateway)
		if err := controller.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		<-secondStarted
		if err := controller.CancelJob(jobs[1].ID); err != nil {
			t.Fatal(err)
		}
		snap := waitForSnapshot(t, controller, func(s Snapshot) bool {
			return !s.Running && s.Jobs[0].State == model.JobSent
		})
		if snap.Jobs[1].State != model.JobCancelled {
			t.Fatalf("second job state = %s, want cancelled", snap.Jobs[1].State)
		}
		sends := gateway.SendCalls()
		if len(sends) != 1 || len(sends[0].items) != 1 || sends[0].items[0].RandomID != 81 {
			t.Fatalf("expected 1-item SendAlbum without re-uploading staged job, got %+v", sends)
		}
		if len(gateway.Calls()) != 0 {
			t.Fatalf("unexpected UploadVideo calls: %d", len(gateway.Calls()))
		}
	})
}

func TestSetChannelClearsStagedOnDifferentChannel(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{{Name: "a.mp4", RandomID: 91}})
	jobs[0].GroupID = "grp-ch"
	jobs[0].Staged = &model.StagedMedia{DocumentID: 55, AccessHash: 66, FileReference: []byte{1}}
	jobs[0].Uploaded = jobs[0].Size
	chA := model.Channel{ID: -100111, AccessHash: 1, Title: "Channel A"}
	chARenamed := model.Channel{ID: -100111, AccessHash: 1, Title: "Channel A Renamed"}
	chB := model.Channel{ID: -100222, AccessHash: 2, Title: "Channel B"}

	store := &memoryQueueStore{jobs: jobs, channel: chA}
	controller := newLoadedAlbumController(t, store, newFakeAlbumGateway())

	if err := controller.SetChannel(chARenamed); err != nil {
		t.Fatal(err)
	}
	if got := controller.Snapshot().Jobs[0]; got.Staged == nil || got.Uploaded != got.Size {
		t.Fatalf("same channel ID should preserve Staged: %+v", got)
	}

	if err := controller.SetChannel(chB); err != nil {
		t.Fatal(err)
	}
	if got := controller.Snapshot().Jobs[0]; got.Staged != nil || got.Uploaded != 0 {
		t.Fatalf("switching channel must clear Staged and Uploaded: %+v", got)
	}
}

func TestAlbumFallsBackToUploadVideoWithoutAlbumGateway(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "1.mp4", RandomID: 101},
		{Name: "2.mp4", RandomID: 102},
	})
	jobs[0].GroupID = "grp-fallback"
	jobs[1].GroupID = "grp-fallback"
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	gateway.upload = func(_ context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		return int(request.RandomID), nil
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForSnapshot(t, controller, func(s Snapshot) bool {
		return !s.Running && allJobsState(s.Jobs, model.JobSent)
	})
	if len(gateway.Calls()) != 2 {
		t.Fatalf("UploadVideo calls = %d, want 2", len(gateway.Calls()))
	}
}

func TestAlbumActiveBatchRejectsSingleCancelAndDefersRemove(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "a.mp4", RandomID: 111, Uploaded: 10},
		{Name: "b.mp4", RandomID: 112, Uploaded: 10},
	})
	for i := range jobs {
		jobs[i].GroupID = "grp-active"
		jobs[i].Staged = &model.StagedMedia{DocumentID: int64(111 + i), AccessHash: 1}
	}
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := newFakeAlbumGateway()
	sending := make(chan struct{})
	releaseSend := make(chan struct{})
	gateway.sendFunc = func(ctx context.Context, _ model.Channel, _ []tgtransport.AlbumItem) ([]int, error) {
		close(sending)
		<-releaseSend
		if ctx.Err() != nil {
			return nil, errors.New("SendAlbum should not be cancelled by RemoveJobs")
		}
		return []int{911, 912}, nil
	}

	controller := newLoadedAlbumController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-sending

	if err := controller.CancelJob(jobs[0].ID); err == nil {
		t.Fatal("CancelJob must reject cancelling a member of an in-flight SendAlbum")
	}
	if err := controller.Retry(jobs[1].ID); err == nil {
		t.Fatal("Retry must reject an active SendAlbum member")
	}
	res, err := controller.RemoveJobs([]string{jobs[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 || len(res.PendingRemovalIDs) != 1 || res.PendingRemovalIDs[0] != jobs[0].ID {
		t.Fatalf("unexpected RemoveJobs result: %+v", res)
	}
	close(releaseSend)
	snap := waitForSnapshot(t, controller, func(s Snapshot) bool {
		return !s.Running && len(s.Jobs) == 1
	})
	if snap.Jobs[0].ID != jobs[1].ID || snap.Jobs[0].State != model.JobSent {
		t.Fatalf("remaining job = %+v, want %s sent", snap.Jobs[0], jobs[1].ID)
	}
}
