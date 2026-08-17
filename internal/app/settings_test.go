package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	tgtransport "github.com/jayden/telegram-video-uploader/internal/telegram"
)

func TestSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	want := Settings{APIID: 42, ProxyEnabled: true, ProxyAddress: "127.0.0.1:1080", ProxyUsername: "u", LastFolder: "/tmp/videos", ScheduledStartUnix: 1_787_070_600, UploadConcurrency: tgtransport.UploadConcurrencyFast}
	if err := SaveSettings(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("settings = %#v, want %#v", got, want)
	}
	// Windows reports synthesized Unix permission bits; file privacy is
	// governed by ACLs and cannot be asserted through os.FileMode.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o", info.Mode().Perm())
		}
	}
}

func TestLoadSettingsDefaultsMissingUploadConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"settings":{"api_id":42}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.UploadConcurrency != tgtransport.DefaultUploadConcurrency {
		t.Fatalf("upload concurrency = %d, want default %d", got.UploadConcurrency, tgtransport.DefaultUploadConcurrency)
	}
}

func TestSaveSettingsAlwaysPersistsUploadConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveSettings(path, Settings{APIID: 42}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `"upload_concurrency": ` + strconv.Itoa(tgtransport.DefaultUploadConcurrency)
	if !strings.Contains(string(data), want) {
		t.Fatalf("saved settings missing default upload_concurrency: %s", data)
	}
}
