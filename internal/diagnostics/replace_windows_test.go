//go:build windows

package diagnostics

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFileAtomicallyReplacesExistingWindowsTarget(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.tmp")
	destination := filepath.Join(directory, "run-state.json")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("replacement content = %q, want new", data)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists after replacement: %v", err)
	}
}
