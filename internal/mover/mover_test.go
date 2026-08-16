package mover

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jayden/telegram-video-uploader/internal/model"
)

func TestRenameNoReplacePreservesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	destination := filepath.Join(dir, "destination.mp4")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("destination"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(source, destination); !errors.Is(err, os.ErrExist) {
		t.Fatalf("renameNoReplace() error = %v, want os.ErrExist", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "destination" {
		t.Fatalf("destination was overwritten: %q", got)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source should remain: %v", err)
	}
}

func TestMovePrefersRename(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.mp4")
	destination := filepath.Join(root, "destination.mp4")
	const content = "rename-without-copy"
	writeTestFile(t, source, content)

	renameCalls := 0
	var progress []model.Progress
	mover := newMoverWithOps(moveOps{
		rename: func(oldName, newName string) error {
			renameCalls++
			return os.Rename(oldName, newName)
		},
	})
	if err := mover.Move(context.Background(), source, destination, func(p model.Progress) {
		progress = append(progress, p)
	}); err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	if renameCalls != 1 {
		t.Fatalf("rename calls = %d, want 1", renameCalls)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists or stat failed: %v", err)
	}
	if got := readTestFile(t, destination); got != content {
		t.Fatalf("destination content = %q, want %q", got, content)
	}
	if len(progress) != 1 || progress[0].BytesDone != progress[0].BytesTotal {
		t.Fatalf("rename progress = %#v, want one completed event", progress)
	}
}

func TestMoveEXDEVCopiesAndVerifiesBeforeRemovingSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.mp4")
	destination := filepath.Join(root, "destination.mp4")
	const content = "copy-across-file-systems"
	writeTestFile(t, source, content)

	renameCalls := 0
	var progress []model.Progress
	mover := newMoverWithOps(moveOps{
		rename: func(oldName, newName string) error {
			renameCalls++
			if renameCalls == 1 {
				return syscall.EXDEV
			}
			return os.Rename(oldName, newName)
		},
	})
	if err := mover.Move(context.Background(), source, destination, func(p model.Progress) {
		progress = append(progress, p)
	}); err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	if renameCalls != 2 {
		t.Fatalf("rename calls = %d, want EXDEV fallback plus install", renameCalls)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists or stat failed: %v", err)
	}
	if got := readTestFile(t, destination); got != content {
		t.Fatalf("destination content = %q, want %q", got, content)
	}
	if _, err := os.Stat(partialPath(destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file remains or stat failed: %v", err)
	}
	assertMonotonicProgress(t, progress)
	if len(progress) == 0 || progress[len(progress)-1].BytesDone != int64(len(content)) {
		t.Fatalf("last progress = %#v, want completed copy", progress)
	}
}

func TestMoveDoesNotOverwriteExistingDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.mp4")
	destination := filepath.Join(root, "destination.mp4")
	writeTestFile(t, source, "source")
	writeTestFile(t, destination, "keep-existing")

	renameCalled := false
	mover := newMoverWithOps(moveOps{
		rename: func(oldName, newName string) error {
			renameCalled = true
			return os.Rename(oldName, newName)
		},
	})
	err := mover.Move(context.Background(), source, destination)
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("Move() error = %v, want ErrDestinationExists", err)
	}
	if renameCalled {
		t.Fatal("rename was called despite an existing destination")
	}
	if got := readTestFile(t, source); got != "source" {
		t.Fatalf("source content = %q, want source", got)
	}
	if got := readTestFile(t, destination); got != "keep-existing" {
		t.Fatalf("destination content = %q, want keep-existing", got)
	}
}

func TestMoveCancellationCleansPartialAndPreservesSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.mp4")
	destination := filepath.Join(root, "destination.mp4")
	writeTestFile(t, source, strings.Repeat("x", 2*1024*1024))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	renameCalls := 0
	mover := newMoverWithOps(moveOps{
		rename: func(oldName, newName string) error {
			renameCalls++
			if renameCalls == 1 {
				return syscall.EXDEV
			}
			return os.Rename(oldName, newName)
		},
	})
	var progress []model.Progress
	err := mover.Move(ctx, source, destination, func(p model.Progress) {
		progress = append(progress, p)
		if p.BytesDone > 0 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Move() error = %v, want context.Canceled", err)
	}
	if renameCalls != 1 {
		t.Fatalf("rename calls = %d, want only initial EXDEV", renameCalls)
	}
	if got := readTestFile(t, source); len(got) != 2*1024*1024 {
		t.Fatalf("source length = %d, want source preserved", len(got))
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination exists or stat failed: %v", err)
	}
	if _, err := os.Stat(partialPath(destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file remains or stat failed: %v", err)
	}
	assertMonotonicProgress(t, progress)
}

func TestMoveChecksumMismatchCleansPartialAndPreservesSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.mp4")
	destination := filepath.Join(root, "destination.mp4")
	writeTestFile(t, source, "checksum-source")

	renameCalls := 0
	base := defaultMoveOps()
	base.rename = func(oldName, newName string) error {
		renameCalls++
		if renameCalls == 1 {
			return syscall.EXDEV
		}
		return os.Rename(oldName, newName)
	}
	base.open = func(name string) (io.ReadCloser, error) {
		if strings.HasSuffix(name, ".partial") {
			return io.NopCloser(strings.NewReader("corrupt")), nil
		}
		return os.Open(name)
	}
	err := newMoverWithOps(base).Move(context.Background(), source, destination)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Move() error = %v, want ErrChecksumMismatch", err)
	}
	if got := readTestFile(t, source); got != "checksum-source" {
		t.Fatalf("source content = %q, want source preserved", got)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination exists or stat failed: %v", err)
	}
	if _, err := os.Stat(partialPath(destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file remains or stat failed: %v", err)
	}
}

func TestMoveReadErrorCleansPartialAndPreservesSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.mp4")
	destination := filepath.Join(root, "destination.mp4")
	writeTestFile(t, source, "read-error-source")

	renameCalls := 0
	base := defaultMoveOps()
	base.rename = func(oldName, newName string) error {
		renameCalls++
		if renameCalls == 1 {
			return syscall.EXDEV
		}
		return os.Rename(oldName, newName)
	}
	base.open = func(name string) (io.ReadCloser, error) {
		if name == source {
			return &errorReadCloser{err: errors.New("injected read error")}, nil
		}
		return os.Open(name)
	}
	err := newMoverWithOps(base).Move(context.Background(), source, destination)
	if err == nil || !strings.Contains(err.Error(), "injected read error") {
		t.Fatalf("Move() error = %v, want injected read error", err)
	}
	if got := readTestFile(t, source); got != "read-error-source" {
		t.Fatalf("source content = %q, want source preserved", got)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination exists or stat failed: %v", err)
	}
	if _, err := os.Stat(partialPath(destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file remains or stat failed: %v", err)
	}
}

func TestMoveBatchProgressIsAggregateAndMonotonic(t *testing.T) {
	root := t.TempDir()
	sourceOne := filepath.Join(root, "01.mp4")
	sourceTwo := filepath.Join(root, "02.mp4")
	destinationDir := filepath.Join(root, "out")
	if err := os.Mkdir(destinationDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, sourceOne, "one")
	writeTestFile(t, sourceTwo, "two-two")

	var progress []model.Progress
	if err := MoveBatch(context.Background(), []string{sourceOne, sourceTwo}, destinationDir, func(p model.Progress) {
		progress = append(progress, p)
	}); err != nil {
		t.Fatalf("MoveBatch() error = %v", err)
	}
	assertMonotonicProgress(t, progress)
	if len(progress) == 0 || progress[len(progress)-1].BytesDone != 10 || progress[len(progress)-1].BytesTotal != 10 {
		t.Fatalf("last aggregate progress = %#v, want 10/10", progress)
	}
}

type errorReadCloser struct{ err error }

func (r *errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r *errorReadCloser) Close() error             { return nil }

func writeTestFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertMonotonicProgress(t *testing.T, progress []model.Progress) {
	t.Helper()
	for i := 1; i < len(progress); i++ {
		if progress[i].BytesDone < progress[i-1].BytesDone {
			t.Fatalf("progress regressed at %d: %#v then %#v", i, progress[i-1], progress[i])
		}
	}
}
