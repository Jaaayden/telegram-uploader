package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	want := Settings{APIID: 42, ProxyEnabled: true, ProxyAddress: "127.0.0.1:1080", ProxyUsername: "u", LastFolder: "/tmp/videos", ScheduledStartUnix: 1_787_070_600}
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
