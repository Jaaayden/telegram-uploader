package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUploadLimitIsNormalizedAndOldFilesStayUnlimited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	// A settings file written before the limit existed.
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"settings":{"api_id":42,"upload_concurrency":8}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UploadLimitKBps != 0 || loaded.DisableUpdateCheck {
		t.Fatalf("old settings must default to unlimited with update checks on: %+v", loaded)
	}

	if err := SaveSettings(path, Settings{UploadLimitKBps: MaxUploadLimitKBps + 5}); err != nil {
		t.Fatal(err)
	}
	if loaded, _ = LoadSettings(path); loaded.UploadLimitKBps != MaxUploadLimitKBps {
		t.Fatalf("limit above the maximum = %d, want clamped", loaded.UploadLimitKBps)
	}
	if err := SaveSettings(path, Settings{UploadLimitKBps: -3}); err != nil {
		t.Fatal(err)
	}
	if loaded, _ = LoadSettings(path); loaded.UploadLimitKBps != 0 {
		t.Fatalf("negative limit = %d, want 0", loaded.UploadLimitKBps)
	}
}

func TestDataDirOverride(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	t.Setenv(DataDirEnv, root)
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.Root != root || filepath.Dir(paths.Queue) != root {
		t.Fatalf("paths ignore %s: %+v", DataDirEnv, paths)
	}
}
