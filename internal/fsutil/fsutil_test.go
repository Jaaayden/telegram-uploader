package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWriteFileReplacesContentAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("AtomicWriteFile() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("content = %q, err = %v", got, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries, want only the target", len(entries))
	}
}

func TestAtomicWriteFileCleansUpWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	// Renaming a regular file over a directory fails on every platform.
	if err := AtomicWriteFile(target, []byte("x"), 0o600); err == nil {
		t.Fatal("expected an error when the destination is a directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary file leaked: %d entries", len(entries))
	}
}

func TestSyncDir(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Fatalf("SyncDir() error = %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := SyncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("SyncDir() on a missing directory should fail")
		}
	}
}
