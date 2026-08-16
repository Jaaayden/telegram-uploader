package ui

import (
	"strings"
	"testing"

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
	if first == nil || first.name.Text != jobs[0].Name {
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
	if first.name.Text != jobs[0].Name {
		t.Fatalf("refreshed first row name = %q, want %q", first.name.Text, jobs[0].Name)
	}
	if first.progress.Value != 0.5 {
		t.Fatalf("refreshed first progress = %v, want 0.5", first.progress.Value)
	}
	if !strings.Contains(first.status.Text, "50 B / 100 B") {
		t.Fatalf("refreshed first status = %q, want byte progress", first.status.Text)
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
