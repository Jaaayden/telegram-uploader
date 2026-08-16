package scanner

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jayden/telegram-video-uploader/internal/model"
)

func TestScanFiltersTopLevelMP4AndSortsNaturally(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"clip10.MP4":    []byte("ten"),
		"clip2.mp4":     []byte("two"),
		"clip1.Mp4":     []byte("one"),
		"clip01.mp4":    []byte("zero-one"),
		"clip001.mp4":   []byte("zero-zero-one"),
		"视频10.mp4":      []byte("video-ten"),
		"视频2.MP4":       []byte("video-two"),
		"not-video.txt": []byte("ignored"),
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "nested1.mp4"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory.mp4"), 0o700); err != nil {
		t.Fatal(err)
	}

	// A symlink must not become uploadable by following its target. Some
	// restricted environments do not permit creating symlinks; the rest of
	// the scan is still useful there, so only that assertion is conditional.
	symlinkPath := filepath.Join(dir, "link.mp4")
	if err := os.Symlink(filepath.Join(dir, "clip1.Mp4"), symlinkPath); err == nil {
		t.Cleanup(func() { _ = os.Remove(symlinkPath) })
	} else {
		t.Logf("symlink unavailable; skipping symlink assertion: %v", err)
	}

	jobs, err := Scan(dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	wantNames := []string{
		"clip1.Mp4",
		"clip01.mp4",
		"clip001.mp4",
		"clip2.mp4",
		"clip10.MP4",
		"视频2.MP4",
		"视频10.mp4",
	}
	gotNames := make([]string, len(jobs))
	for i, job := range jobs {
		gotNames[i] = job.Name
		if job.Position != i {
			t.Errorf("job %q has position %d, want %d", job.Name, job.Position, i)
		}
		if job.State != model.JobQueued {
			t.Errorf("job %q has state %q, want queued", job.Name, job.State)
		}
		if job.Path != filepath.Join(dir, job.Name) {
			t.Errorf("job %q has path %q", job.Name, job.Path)
		}
		if job.Size != int64(len(files[job.Name])) {
			t.Errorf("job %q has size %d, want %d", job.Name, job.Size, len(files[job.Name]))
		}
		if job.ID == "" {
			t.Errorf("job %q has empty ID", job.Name)
		}
		if job.RandomID == 0 {
			t.Errorf("job %q has zero RandomID", job.Name)
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("names = %#v, want %#v", gotNames, wantNames)
	}
}

func TestScanMarksOnlyFilesOverLimit(t *testing.T) {
	dir := t.TempDir()
	for name, contents := range map[string][]byte{
		"one.mp4": []byte("1"),
		"two.mp4": []byte("22"),
		"ten.mp4": []byte("0123456789"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	jobs, err := Scan(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs, want 3", len(jobs))
	}
	for _, job := range jobs {
		want := model.JobQueued
		if job.Name == "ten.mp4" {
			want = model.JobOversize
		}
		if job.State != want {
			t.Errorf("job %q has state %q, want %q", job.Name, job.State, want)
		}
	}
}

func TestNaturalCompareHandlesVeryLongNumbersAndRawTieBreak(t *testing.T) {
	got := []string{
		"part00000000000000000001.mp4",
		"part1.mp4",
		"part2.mp4",
		"part10.mp4",
	}
	want := []string{
		"part1.mp4",
		"part00000000000000000001.mp4",
		"part2.mp4",
		"part10.mp4",
	}
	for i := 0; i < len(got); i++ {
		for j := 0; j < len(got); j++ {
			if i == j {
				continue
			}
			if (compareNatural(got[i], got[j]) < 0) != (indexOf(want, got[i]) < indexOf(want, got[j])) {
				t.Fatalf("compareNatural(%q, %q) has wrong ordering", got[i], got[j])
			}
		}
	}

	if compareNatural("same1", "same01") >= 0 {
		t.Fatal("shorter equal-valued numeric run should sort first")
	}
	if compareNatural("same01", "same001") >= 0 {
		t.Fatal("shorter equal-valued numeric run should sort first")
	}
	if compareNatural("文件2.mp4", "文件10.mp4") >= 0 {
		t.Fatal("Unicode prefix should not disable natural numeric ordering")
	}
	if compareNatural("Video2.mp4", "video10.mp4") >= 0 {
		t.Fatal("case differences should not disable natural numeric ordering")
	}
	if compareNatural("A1.mp4", "a1.mp4") >= 0 {
		t.Fatal("case-folded ties should use the original filename deterministically")
	}
}

func indexOf(values []string, value string) int {
	for i, candidate := range values {
		if candidate == value {
			return i
		}
	}
	return -1
}
