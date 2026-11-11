package scanner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Jaaayden/telegram-uploader/internal/model"
)

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanPathsMixesFilesAndFoldersWithoutDuplicates(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a", "2.mp4"), 10)
	writeFile(t, filepath.Join(root, "a", "10.mp4"), 10)
	writeFile(t, filepath.Join(root, "a", "notes.txt"), 10)
	writeFile(t, filepath.Join(root, "a", "nested", "deep.mp4"), 10)
	writeFile(t, filepath.Join(root, "b", "clip.MP4"), 30)
	writeFile(t, filepath.Join(root, "b", "cover.jpg"), 10)

	result, err := ScanPaths([]string{
		filepath.Join(root, "b", "clip.MP4"),
		filepath.Join(root, "a"),
		filepath.Join(root, "a", "2.mp4"), // already covered by its folder
		filepath.Join(root, "b", "clip.MP4"),
		filepath.Join(root, "b", "cover.jpg"),
		filepath.Join(root, "missing.mp4"),
		"  ",
	}, 20)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, job := range result.Jobs {
		names = append(names, job.Name)
	}
	want := []string{"2.mp4", "10.mp4", "clip.MP4"}
	if len(names) != len(want) {
		t.Fatalf("jobs = %v, want %v", names, want)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("jobs = %v, want %v (folder order, then natural name order)", names, want)
		}
	}
	if result.Ignored != 2 {
		t.Fatalf("ignored = %d, want 2 (the jpg and the missing file)", result.Ignored)
	}
	if result.Jobs[2].State != model.JobOversize || result.Jobs[0].State != model.JobQueued {
		t.Fatal("the size limit must mark only the larger file as oversize")
	}
	ids := map[string]bool{}
	for index, job := range result.Jobs {
		if job.ID == "" || job.RandomID == 0 || ids[job.ID] || job.Position != index {
			t.Fatalf("job %d has a bad identity or position: %+v", index, job)
		}
		ids[job.ID] = true
	}
}

func TestScanPathsIgnoresSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.mp4")
	writeFile(t, target, 10)
	link := filepath.Join(root, "link.mp4")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks are not available:", err)
	}
	result, err := ScanPaths([]string{link}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Jobs) != 0 || result.Ignored != 1 {
		t.Fatalf("a symlink must be ignored: %+v", result)
	}
}
