package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

func TestFormatRate(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "n/a"},
		{-5, "n/a"},
		{512, "512 B/s"},
		{1536, "1.50 KiB/s"},
		{12.5 * 1024 * 1024, "12.50 MiB/s"},
		{3 * 1024 * 1024 * 1024, "3.00 GiB/s"},
	}
	for _, tc := range cases {
		if got := formatRate(tc.in); got != tc.want {
			t.Errorf("formatRate(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTransferRate(t *testing.T) {
	if got := transferRate(10*1024*1024, 2*time.Second); got != 5*1024*1024 {
		t.Fatalf("rate = %v", got)
	}
	if transferRate(100, 0) != 0 || transferRate(0, time.Second) != 0 || transferRate(-1, time.Second) != 0 {
		t.Fatal("degenerate inputs must give 0")
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{1234 * time.Millisecond, "1.2s"},
		{83400 * time.Millisecond, "1m23s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h2m3s"},
	}
	for _, tc := range cases {
		if got := formatElapsed(tc.in); got != tc.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUploadStatsAverage(t *testing.T) {
	var s uploadStats
	s.add(100*1024*1024, 10*time.Second)
	s.add(50*1024*1024, 5*time.Second)
	if s.jobs != 2 || s.bytes != 150*1024*1024 || s.elapsed != 15*time.Second {
		t.Fatalf("stats = %+v", s)
	}
	if got := formatRate(transferRate(s.bytes, s.elapsed)); got != "10.00 MiB/s" {
		t.Fatalf("average = %s", got)
	}
}

// An uploaded queue must leave the end time, elapsed time and average speed in
// app.log, once per video and once as a queue summary.
func TestQueueWritesUploadSummaryToAppLog(t *testing.T) {
	session, err := diagnostics.Start(t.TempDir())
	if err != nil {
		t.Fatalf("diagnostics.Start() error = %v", err)
	}
	logPath := session.AppLogPath()

	jobs, _ := fixtureJobs(t, []fixtureJob{
		{Name: "log-one.mp4", RandomID: 7601},
		{Name: "log-two.mp4", RandomID: 7602},
	})
	store := &memoryQueueStore{jobs: jobs, channel: testChannel()}
	gateway := &fakeGateway{}
	gateway.upload = func(ctx context.Context, request tgtransport.UploadRequest, _ func(model.Progress)) (int, error) {
		time.Sleep(60 * time.Millisecond)
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
		return int(request.RandomID), nil
	}
	controller := newLoadedController(t, store, gateway)
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForSnapshot(t, controller, func(s Snapshot) bool { return allJobsState(s.Jobs, model.JobSent) })
	if err := controller.WaitStopped(context.Background()); err != nil {
		t.Fatalf("WaitStopped() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("session.Close() error = %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var sent, stops []string
	for _, line := range strings.Split(string(data), "\n") {
		var record struct{ Message string }
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		switch {
		case strings.HasPrefix(record.Message, "job_sent "):
			sent = append(sent, record.Message)
		case strings.HasPrefix(record.Message, "queue_stop "):
			stops = append(stops, record.Message)
		}
	}
	if len(sent) != 2 || len(stops) != 1 {
		t.Fatalf("job_sent lines = %d, queue_stop lines = %d, want 2 and 1\n%s", len(sent), len(stops), data)
	}
	for _, line := range sent {
		for _, field := range []string{"finished_at=", "elapsed=", "avg_speed=", "sent_bytes="} {
			if !strings.Contains(line, field) {
				t.Errorf("job_sent line lacks %s: %s", field, line)
			}
		}
		if strings.Contains(line, "avg_speed=n/a") {
			t.Errorf("job_sent line has no speed: %s", line)
		}
	}
	if !strings.Contains(stops[0], "sent=2 ") || strings.Contains(stops[0], "avg_speed=n/a") {
		t.Errorf("queue_stop summary is wrong: %s", stops[0])
	}
	t.Log(sent[0])
	t.Log(stops[0])
}
