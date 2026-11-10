package buildinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func TestVersionMatchesVersionFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "VERSION"))
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	// Release builds inject a different string; this guards the default that
	// "go build" and "go run" report.
	if want := strings.TrimSpace(string(data)); want != Version {
		t.Fatalf("buildinfo.Version = %q, VERSION file = %q", Version, want)
	}
}

func TestVersionIsSemantic(t *testing.T) {
	parts := strings.Split(Version, ".")
	if len(parts) != 3 {
		t.Fatalf("Version %q is not MAJOR.MINOR.PATCH", Version)
	}
	for _, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			t.Fatalf("Version %q has a non-numeric component", Version)
		}
	}
}

func TestInfoPlistUsesVersionPlaceholders(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "packaging", "macos", "Info.plist"))
	if err != nil {
		t.Fatalf("read Info.plist: %v", err)
	}
	// The packaging scripts substitute the placeholder, so the plist can never
	// disagree with VERSION.
	if strings.Count(string(data), "@VERSION@") != 2 {
		t.Fatal("Info.plist must use @VERSION@ for both version keys")
	}
	if !strings.Contains(string(data), "com.jayden.telegramvideouploader") {
		t.Fatal("Info.plist lost the bundle identifier")
	}
}

func TestRepositoryConstants(t *testing.T) {
	if RepositoryURL != "https://github.com/Jaaayden/telegram-uploader" {
		t.Fatalf("RepositoryURL = %q", RepositoryURL)
	}
}
