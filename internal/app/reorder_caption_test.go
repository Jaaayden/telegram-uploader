package app

import (
	"context"
	"strings"
	"testing"

	"github.com/Jaaayden/telegram-uploader/internal/model"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

func TestUTF16LenAndTruncateCaption(t *testing.T) {
	if got := UTF16Len("abc中文"); got != 5 {
		t.Fatalf("UTF16Len(BMP) = %d, want 5", got)
	}
	// Surrogate pair (4-byte UTF-8 rune) counts as 2 UTF-16 code units.
	emoji := "😀"
	if got := UTF16Len(emoji); got != 2 {
		t.Fatalf("UTF16Len(emoji) = %d, want 2", got)
	}

	exact := strings.Repeat("a", model.MaxCaptionUTF16)
	if got := TruncateCaption(exact, model.MaxCaptionUTF16); got != exact {
		t.Fatal("exact 1024-unit caption should not be truncated")
	}

	over := strings.Repeat("a", model.MaxCaptionUTF16+5)
	got := TruncateCaption(over, model.MaxCaptionUTF16)
	if UTF16Len(got) != model.MaxCaptionUTF16 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated len=%d suffix=%q", UTF16Len(got), got[len(got)-3:])
	}

	// 512 emojis = 1024 UTF-16 units + "a" = 1025 units -> 511 emojis + "…" = 1023 units.
	emojisOver := strings.Repeat(emoji, 512) + "a"
	gotEmoji := TruncateCaption(emojisOver, model.MaxCaptionUTF16)
	if UTF16Len(gotEmoji) != 1023 || !strings.HasSuffix(gotEmoji, "…") {
		t.Fatalf("emoji truncated len=%d", UTF16Len(gotEmoji))
	}
}

func TestEffectiveAlbumCaption(t *testing.T) {
	members := []model.Job{
		{Name: "01.mp4"},
		{Name: ".mp4"}, // empty stem skipped in default list
		{Name: "02.mp4"},
	}
	if got := effectiveAlbumCaption(members); got != "01\n02" {
		t.Fatalf("default album caption = %q, want %q", got, "01\n02")
	}

	longMembers := []model.Job{
		{Name: strings.Repeat("x", 600) + ".mp4"},
		{Name: strings.Repeat("y", 600) + ".mp4"},
	}
	longCap := effectiveAlbumCaption(longMembers)
	if UTF16Len(longCap) != model.MaxCaptionUTF16 || !strings.HasSuffix(longCap, "…") {
		t.Fatalf("long album caption len = %d", UTF16Len(longCap))
	}

	members[0].CaptionCustom = true
	members[0].CustomCaption = "custom group text"
	if got := effectiveAlbumCaption(members); got != "custom group text" {
		t.Fatalf("custom album caption = %q", got)
	}

	members[0].CustomCaption = ""
	if got := effectiveAlbumCaption(members); got != "" {
		t.Fatalf("explicit empty album caption = %q, want empty", got)
	}
}

func TestSetCaptionsAndUploadIntegration(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "single.mp4", RandomID: 101},
		{Name: "empty.mp4", RandomID: 102},
		{Name: "alb1.mp4", RandomID: 103},
		{Name: "alb2.mp4", RandomID: 104},
		{Name: "sent.mp4", RandomID: 105, State: model.JobSent},
	})
	jobs[2].GroupID = "grp-1"
	jobs[3].GroupID = "grp-1"

	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := newFakeAlbumGateway()
	gateway.upload = func(_ context.Context, req tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		if err := req.BeforeSend(); err != nil {
			return 0, err
		}
		return int(req.RandomID), nil
	}
	controller := newLoadedAlbumController(t, store, gateway)

	// Reject > 1024 UTF-16 units.
	tooLong := strings.Repeat("a", model.MaxCaptionUTF16+1)
	if _, err := controller.SetCaptions([]CaptionEdit{{ID: jobs[0].ID, Custom: true, Caption: tooLong}}); err == nil {
		t.Fatal("expected error for over-limit caption")
	}

	// Edit single, explicit empty single, album via second member, and attempt sent job.
	updated, err := controller.SetCaptions([]CaptionEdit{
		{ID: jobs[0].ID, Custom: true, Caption: "自定义单条"},
		{ID: jobs[1].ID, Custom: true, Caption: ""},
		{ID: jobs[3].ID, Custom: true, Caption: "整组标题\n第二行"},
		{ID: jobs[4].ID, Custom: true, Caption: "should skip"},
	})
	if err != nil {
		t.Fatalf("SetCaptions() error = %v", err)
	}
	// jobs[0], jobs[1], jobs[2], jobs[3] = 4 updated; sent job skipped.
	if updated != 4 {
		t.Fatalf("updated = %d, want 4", updated)
	}
	snap := controller.Snapshot()
	if !snap.Jobs[0].CaptionCustom || snap.Jobs[0].CustomCaption != "自定义单条" {
		t.Fatalf("job 0 = %+v", snap.Jobs[0])
	}
	if !snap.Jobs[1].CaptionCustom || snap.Jobs[1].CustomCaption != "" {
		t.Fatalf("job 1 explicit empty = %+v", snap.Jobs[1])
	}
	if !snap.Jobs[2].CaptionCustom || snap.Jobs[2].CustomCaption != "整组标题\n第二行" || !snap.Jobs[3].CaptionCustom {
		t.Fatalf("album jobs not synced: %+v %+v", snap.Jobs[2], snap.Jobs[3])
	}
	if snap.Jobs[4].CaptionCustom {
		t.Fatal("sent job should not be modified")
	}

	// Verify Retry preserves custom caption.
	if err := controller.CancelJob(jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := controller.Retry(jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := controller.Snapshot().Jobs[0]; !got.CaptionCustom || got.CustomCaption != "自定义单条" {
		t.Fatalf("Retry cleared custom caption: %+v", got)
	}

	// Verify actual upload / album send captions.
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForSnapshot(t, controller, func(s Snapshot) bool {
		return !s.Running && allJobsState(s.Jobs, model.JobSent)
	})
	uploads := gateway.Calls()
	if len(uploads) != 2 || uploads[0].Caption != "自定义单条" || uploads[1].Caption != "" {
		t.Fatalf("single uploads captions = %+v", uploads)
	}
	sends := gateway.SendCalls()
	if len(sends) != 1 || sends[0].items[0].Caption != "整组标题\n第二行" || sends[0].items[1].Caption != "" {
		t.Fatalf("album captions = %+v", sends)
	}
}

func TestSetCaptionsRestoreDefault(t *testing.T) {
	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "a.mp4", RandomID: 201},
		{Name: "b.mp4", RandomID: 202},
	})
	jobs[0].GroupID = "grp-r"
	jobs[1].GroupID = "grp-r"
	jobs[0].CaptionCustom = true
	jobs[0].CustomCaption = "custom"
	jobs[1].CaptionCustom = true
	jobs[1].CustomCaption = "custom"

	controller := newLoadedAlbumController(t, &memoryQueueStore{jobs: jobs, channel: testChannel()}, newFakeAlbumGateway())
	n, err := controller.SetCaptions([]CaptionEdit{{ID: jobs[0].ID, Custom: false}})
	if err != nil || n != 2 {
		t.Fatalf("SetCaptions(reset) = %d, %v", n, err)
	}
	for _, j := range controller.Snapshot().Jobs {
		if j.CaptionCustom || j.CustomCaption != "" {
			t.Fatalf("job still has custom caption: %+v", j)
		}
	}
}

func TestReorderJobs(t *testing.T) {
	makeIDs := func(jobs []model.Job) []string {
		ids := make([]string, len(jobs))
		for i, j := range jobs {
			ids[i] = j.ID
		}
		return ids
	}

	t.Run("pinned jobs stay fixed and single/multi unit moves work", func(t *testing.T) {
		jobs, _ := fixtureJobs(t, []fixtureJob{
			{Name: "0-sent.mp4", RandomID: 1, State: model.JobSent},
			{Name: "1-a.mp4", RandomID: 2},
			{Name: "2-g1.mp4", RandomID: 3},
			{Name: "3-g2.mp4", RandomID: 4},
			{Name: "4-b.mp4", RandomID: 5},
			{Name: "5-uploading.mp4", RandomID: 6, State: model.JobUploading},
			{Name: "6-c.mp4", RandomID: 7},
		})
		jobs[2].GroupID = "grp-a"
		jobs[3].GroupID = "grp-a"
		store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
		controller := newLoadedAlbumController(t, store, newFakeAlbumGateway())

		// Move "job-7" (6-c.mp4) to top -> lands right after pinned "job-1" (index 0); "job-6" stays pinned at index 5.
		moved, err := controller.ReorderJobs([]string{"job-7"}, ReorderTop, "")
		if err != nil || !moved {
			t.Fatalf("ReorderTop = %v, %v", moved, err)
		}
		got := makeIDs(controller.Snapshot().Jobs)
		want := []string{"job-1", "job-7", "job-2", "job-3", "job-4", "job-6", "job-5"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("after top: %v, want %v", got, want)
		}

		// Move "job-2" down -> swaps past the whole album unit [job-3, job-4].
		moved, err = controller.ReorderJobs([]string{"job-2"}, ReorderDown, "")
		if err != nil || !moved {
			t.Fatalf("ReorderDown = %v, %v", moved, err)
		}
		got = makeIDs(controller.Snapshot().Jobs)
		want = []string{"job-1", "job-7", "job-3", "job-4", "job-2", "job-6", "job-5"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("after down past album: %v, want %v", got, want)
		}

		// Multi-select job-7 + job-2 -> bottom; job-1 stays pinned at 0, job-6 stays pinned at 5.
		moved, err = controller.ReorderJobs([]string{"job-7", "job-2"}, ReorderBottom, "")
		if err != nil || !moved {
			t.Fatalf("ReorderBottom = %v, %v", moved, err)
		}
		got = makeIDs(controller.Snapshot().Jobs)
		want = []string{"job-1", "job-3", "job-4", "job-5", "job-7", "job-6", "job-2"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("after bottom: %v, want %v", got, want)
		}
		// Positions rebuilt 0..6.
		for i, j := range controller.Snapshot().Jobs {
			if j.Position != i {
				t.Fatalf("job %s Position = %d, want %d", j.ID, j.Position, i)
			}
		}
	})

	t.Run("intra-album reorder and whole-album boundary fallback", func(t *testing.T) {
		jobs, _ := fixtureJobs(t, []fixtureJob{
			{Name: "1-solo.mp4", RandomID: 11},
			{Name: "2-g1.mp4", RandomID: 12},
			{Name: "3-g2.mp4", RandomID: 13},
			{Name: "4-g3.mp4", RandomID: 14},
			{Name: "5-tail.mp4", RandomID: 15},
		})
		for i := 1; i <= 3; i++ {
			jobs[i].GroupID = "grp-x"
		}
		controller := newLoadedAlbumController(t, &memoryQueueStore{jobs: jobs, channel: testChannel()}, newFakeAlbumGateway())

		// Move g2 up inside grp-x -> [solo, g2, g1, g3, tail].
		moved, err := controller.ReorderJobs([]string{"job-3"}, ReorderUp, "")
		if err != nil || !moved {
			t.Fatalf("intra up = %v, %v", moved, err)
		}
		got := makeIDs(controller.Snapshot().Jobs)
		want := []string{"job-1", "job-3", "job-2", "job-4", "job-5"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("after intra up: %v, want %v", got, want)
		}

		// Now job-3 is at top of grp-x; ReorderUp again moves the whole grp-x above solo!
		moved, err = controller.ReorderJobs([]string{"job-3"}, ReorderUp, "")
		if err != nil || !moved {
			t.Fatalf("edge up = %v, %v", moved, err)
		}
		got = makeIDs(controller.Snapshot().Jobs)
		want = []string{"job-3", "job-2", "job-4", "job-1", "job-5"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("after edge up: %v, want %v", got, want)
		}

		// Drag job-5 before job-2 (middle of grp-x) -> snaps before whole grp-x!
		moved, err = controller.ReorderJobs([]string{"job-5"}, ReorderBefore, "job-2")
		if err != nil || !moved {
			t.Fatalf("before album = %v, %v", moved, err)
		}
		got = makeIDs(controller.Snapshot().Jobs)
		want = []string{"job-5", "job-3", "job-2", "job-4", "job-1"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("after drop before middle of album: %v, want %v", got, want)
		}
	})

	t.Run("drop relative to pinned job moves to adjacent movable slot", func(t *testing.T) {
		jobs, _ := fixtureJobs(t, []fixtureJob{
			{Name: "0-sent.mp4", RandomID: 21, State: model.JobSent},
			{Name: "1-a.mp4", RandomID: 22},
			{Name: "2-b.mp4", RandomID: 23},
		})
		controller := newLoadedAlbumController(t, &memoryQueueStore{jobs: jobs, channel: testChannel()}, newFakeAlbumGateway())

		moved, err := controller.ReorderJobs([]string{"job-3"}, ReorderAfter, "job-1")
		if err != nil || !moved {
			t.Fatalf("after pinned = %v, %v", moved, err)
		}
		got := makeIDs(controller.Snapshot().Jobs)
		want := []string{"job-1", "job-3", "job-2"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}
