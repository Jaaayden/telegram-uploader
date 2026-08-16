package queue

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jayden/telegram-video-uploader/internal/model"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	started := time.Date(2026, 8, 16, 10, 11, 12, 123456789, time.UTC)
	completed := started.Add(time.Minute)
	jobs := []model.Job{{
		ID:        "job-1",
		Position:  3,
		Path:      "/videos/a.mp4",
		Name:      "a.mp4",
		Size:      1234,
		ModTime:   started,
		State:     model.JobSent,
		Uploaded:  1234,
		RandomID:  9876,
		MessageID: 42,
		ChannelID: 99,
		Metadata: model.VideoMetadata{
			DurationSeconds:    12,
			Width:              1920,
			Height:             1080,
			SupportsStreaming:  true,
			TruncatedMediaData: true,
		},
		StartedAt:   &started,
		CompletedAt: &completed,
	}}
	channel := model.Channel{ID: -100123, AccessHash: 456, Title: "Videos"}

	store := Store(path)
	if err := store.Save(jobs, channel, true); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	gotJobs, gotChannel, gotPaused, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !equalJobs(gotJobs, jobs) {
		t.Fatalf("jobs mismatch:\n got %#v\nwant %#v", gotJobs, jobs)
	}
	if gotChannel != channel {
		t.Fatalf("channel = %#v, want %#v", gotChannel, channel)
	}
	if !gotPaused {
		t.Fatal("paused = false, want true")
	}

	var saved map[string]json.RawMessage
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("saved JSON is invalid: %v", err)
	}
	var version int
	if err := json.Unmarshal(saved["schema_version"], &version); err != nil || version != currentSchemaVersion {
		t.Fatalf("schema_version = %d, error = %v", version, err)
	}
}

func TestStoreLoadLegacyDocumentDefaultsToUnpaused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	legacy := []byte(`{"schema_version":1,"jobs":[],"channel":{}}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, _, paused, err := Store(path).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if paused {
		t.Fatal("legacy queue loaded as paused, want false")
	}
}

func TestStoreSaveUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not provide Unix permission semantics")
	}
	path := filepath.Join(t.TempDir(), "queue.json")
	if err := Store(path).Save(nil, model.Channel{}, false); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
}

func TestStoreLoadRecoversActiveJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	jobs := []model.Job{
		{ID: "uploading", State: model.JobUploading, Uploaded: 100, RandomID: 1},
		{ID: "sending", State: model.JobSending, Uploaded: 200, RandomID: 2},
		{ID: "confirming", State: model.JobConfirming, Uploaded: 300, RandomID: 3},
		{ID: "sent", State: model.JobSent, Uploaded: 400, RandomID: 4},
		{ID: "queued", State: model.JobQueued, Uploaded: 500, RandomID: 5},
	}
	if err := Store(path).Save(jobs, model.Channel{}, false); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, _, _, err := Store(path).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for i, job := range got {
		switch job.ID {
		case "uploading":
			if job.State != model.JobInterrupted || job.Uploaded != 0 {
				t.Errorf("job %q = state %q, uploaded %d; want interrupted, 0", job.ID, job.State, job.Uploaded)
			}
		case "sending", "confirming":
			if job.State != model.JobConfirming || job.Uploaded != jobs[i].Uploaded || job.Error == "" {
				t.Errorf("job %q = state %q, uploaded %d, error %q; want confirming with original progress and recovery warning", job.ID, job.State, job.Uploaded, job.Error)
			}
		}
		switch job.ID {
		case "uploading", "sending", "confirming":
			if job.RandomID != int64(i+1) {
				t.Errorf("job %q RandomID = %d, want %d", job.ID, job.RandomID, i+1)
			}
		default:
			if job.Uploaded != jobs[i].Uploaded || job.State != jobs[i].State || job.RandomID != jobs[i].RandomID {
				t.Errorf("job %q changed unexpectedly: got %#v want %#v", job.ID, job, jobs[i])
			}
		}
	}
}

func TestStoreLoadMissingIsEmpty(t *testing.T) {
	store := Store(filepath.Join(t.TempDir(), "missing.json"))
	jobs, channel, paused, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs length = %d, want 0", len(jobs))
	}
	if channel != (model.Channel{}) {
		t.Fatalf("channel = %#v, want zero value", channel)
	}
	if paused {
		t.Fatal("paused = true for missing queue, want false")
	}
}

func TestStoreLoadRejectsCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"jobs":[`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, _, _, err := Store(path).Load(); err == nil {
		t.Fatal("Load() error = nil, want corrupt JSON error")
	}
}

func TestStoreLoadRejectsUnknownNewerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	if err := Store(path).Save(nil, model.Channel{}, false); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	raw["schema_version"] = currentSchemaVersion + 1
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, _, _, err := Store(path).Load(); !errors.Is(err, ErrUnsupportedSchemaVersion) {
		t.Fatalf("Load() error = %v, want ErrUnsupportedSchemaVersion", err)
	}
}

func TestStoreSaveLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.json")
	if err := Store(path).Save(nil, model.Channel{}, false); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".queue.json.tmp-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func equalJobs(a, b []model.Job) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Position != b[i].Position || a[i].Path != b[i].Path ||
			a[i].Name != b[i].Name || a[i].Size != b[i].Size || !a[i].ModTime.Equal(b[i].ModTime) ||
			a[i].State != b[i].State || a[i].Uploaded != b[i].Uploaded || a[i].RandomID != b[i].RandomID ||
			a[i].MessageID != b[i].MessageID || a[i].ChannelID != b[i].ChannelID || a[i].Metadata != b[i].Metadata ||
			a[i].Error != b[i].Error || !sameTime(a[i].StartedAt, b[i].StartedAt) || !sameTime(a[i].CompletedAt, b[i].CompletedAt) {
			return false
		}
	}
	return true
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
