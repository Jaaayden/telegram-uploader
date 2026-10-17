package buildinfo

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestConstants(t *testing.T) {
	if Name != "Telegram Video Uploader" {
		t.Fatalf("Name = %q, want %q", Name, "Telegram Video Uploader")
	}
	if Version != "1.2.1" {
		t.Fatalf("Version = %q, want %q", Version, "1.2.1")
	}
	if Build != 4 {
		t.Fatalf("Build = %d, want %d", Build, 4)
	}
	if RepositoryURL != "https://github.com/Jaaayden/telegram-uploader" {
		t.Fatalf("RepositoryURL = %q, want %q", RepositoryURL, "https://github.com/Jaaayden/telegram-uploader")
	}
}

func TestPackagingMetadataMatchesConstants(t *testing.T) {
	root := repositoryRoot(t)

	fyneData, err := os.ReadFile(filepath.Join(root, "FyneApp.toml"))
	if err != nil {
		t.Fatalf("read FyneApp.toml: %v", err)
	}
	if got := tomlStringValue(string(fyneData), "Version"); got != Version {
		t.Fatalf("FyneApp.toml Version = %q, want %q", got, Version)
	}
	if got := tomlIntegerValue(string(fyneData), "Build"); got != Build {
		t.Fatalf("FyneApp.toml Build = %d, want %d", got, Build)
	}
	if got := tomlStringValue(string(fyneData), "Name"); got != Name {
		t.Fatalf("FyneApp.toml Name = %q, want %q", got, Name)
	}

	plistData, err := os.ReadFile(filepath.Join(root, "packaging", "macos", "Info.plist"))
	if err != nil {
		t.Fatalf("read packaging/macos/Info.plist: %v", err)
	}
	plistValues, err := parsePlistValues(plistData)
	if err != nil {
		t.Fatalf("parse packaging/macos/Info.plist: %v", err)
	}
	if got := plistValues["CFBundleShortVersionString"]; got != Version {
		t.Fatalf("Info.plist CFBundleShortVersionString = %q, want %q", got, Version)
	}
	if got := plistValues["CFBundleVersion"]; got != strconv.Itoa(Build) {
		t.Fatalf("Info.plist CFBundleVersion = %q, want %q", got, strconv.Itoa(Build))
	}
}

// repositoryRoot locates the checkout from this test's source path instead
// of relying on the process working directory used by the Go test runner.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	for dir := filepath.Dir(source); ; dir = filepath.Dir(dir) {
		if isRegularFile(filepath.Join(dir, "FyneApp.toml")) &&
			isRegularFile(filepath.Join(dir, "packaging", "macos", "Info.plist")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	t.Fatalf("could not locate repository root from %q", source)
	return ""
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func tomlStringValue(data, key string) string {
	prefix := key + " = \""
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, "\"") {
			return strings.TrimSuffix(strings.TrimPrefix(line, prefix), "\"")
		}
	}
	return ""
}

func tomlIntegerValue(data, key string) int {
	prefix := key + " = "
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		if err == nil {
			return value
		}
	}
	return 0
}

func parsePlistValues(data []byte) (map[string]string, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	values := make(map[string]string)
	var key string
	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				return values, nil
			}
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return nil, err
			}
		case "string", "integer":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return nil, err
			}
			if key != "" {
				values[key] = value
				key = ""
			}
		}
	}
}
