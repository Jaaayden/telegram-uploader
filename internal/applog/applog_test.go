package applog

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitWritesToFile(t *testing.T) {
	dir := t.TempDir()
	closer := Init(dir)
	slog.Info("hello", "k", "v")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || !strings.Contains(string(data), "hello") {
		t.Fatalf("log = %q, err = %v", data, err)
	}
}

func TestOpenRotatedRotatesLargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, make([]byte, MaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openRotated(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("new log: %v, %v", info, err)
	}
	if info, err := os.Stat(path + ".1"); err != nil || info.Size() != MaxBytes+1 {
		t.Fatalf("rotated log: %v, %v", info, err)
	}
}

func TestInitFallsBackWhenDirectoryMissing(t *testing.T) {
	closer := Init(filepath.Join(t.TempDir(), "missing"))
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
}
