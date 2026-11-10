package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"2.0.1", "2.0.0", true},
		{"v2.1.0", "2.0.9", true},
		{"3.0.0", "2.9.9", true},
		{"2.0.0", "2.0.0", false},
		{"1.9.9", "2.0.0", false},
		{"2.0.0", "2.0.0-dev.abc1234", true},
		{"2.0.0-rc.1", "2.0.0", false},
		{"2.0.0", "1.2.2-dev.1234567", true},
		{"2.0.0", "dev", true},
		{"nightly", "1.0.0", false},
		{"2.0", "1.0.0", false},
		{"2.0.10", "2.0.9", true},
	}
	for _, tc := range cases {
		if got := IsNewer(tc.candidate, tc.current); got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

func TestAssetNameFor(t *testing.T) {
	if AssetNameFor("darwin", "arm64") != "TelegramVideoUploader-macos-arm64.zip" ||
		AssetNameFor("darwin", "amd64") != "TelegramVideoUploader-macos-x86_64.zip" ||
		AssetNameFor("windows", "amd64") != "TelegramVideoUploader-windows-amd64.exe" ||
		AssetNameFor("linux", "amd64") != "" {
		t.Fatal("unexpected asset mapping")
	}
}

type fakeRelease struct {
	tag    string
	asset  []byte
	sums   string
	status int
}

func serveRelease(t *testing.T, release *fakeRelease) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/app/releases/latest":
			if release.status != 0 {
				w.WriteHeader(release.status)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": release.tag,
				"body":     "- 修复若干问题",
				"html_url": server.URL + "/release-page",
				"assets": []map[string]any{
					{"name": "TelegramVideoUploader-windows-amd64.exe", "browser_download_url": server.URL + "/asset", "size": len(release.asset)},
					{"name": "SHA256SUMS", "browser_download_url": server.URL + "/sums", "size": len(release.sums)},
					{"name": "TelegramVideoUploader-macos-arm64.zip", "browser_download_url": server.URL + "/other", "size": 1},
				},
			})
		case "/asset":
			_, _ = w.Write(release.asset)
		case "/sums":
			_, _ = w.Write([]byte(release.sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func sumLine(data []byte, name string) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + "  " + name + "\n"
}

func newTestUpdater(t *testing.T, server *httptest.Server, current string) *Updater {
	t.Helper()
	return &Updater{
		Repository:    "example/app",
		Current:       current,
		StageDir:      filepath.Join(t.TempDir(), "updates"),
		APIBase:       server.URL,
		GOOS:          "windows",
		GOARCH:        "amd64",
		AllowInsecure: true,
	}
}

func TestCheckAndDownloadVerifiesChecksum(t *testing.T) {
	asset := bytes.Repeat([]byte("new-version"), 4096)
	release := &fakeRelease{tag: "v2.1.0", asset: asset}
	release.sums = "0000  unrelated\n" + sumLine(asset, "TelegramVideoUploader-windows-amd64.exe")
	server := serveRelease(t, release)
	updater := newTestUpdater(t, server, "2.0.0")

	found, newer, err := updater.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !newer || found.Version != "2.1.0" || found.AssetName != "TelegramVideoUploader-windows-amd64.exe" || found.AssetSize != int64(len(asset)) {
		t.Fatalf("unexpected release: newer=%v %+v", newer, found)
	}

	var lastDone, lastTotal int64
	path, err := updater.Download(context.Background(), found, func(done, total int64) { lastDone, lastTotal = done, total })
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, asset) {
		t.Fatalf("staged file differs: %v", err)
	}
	if lastDone != int64(len(asset)) || lastTotal != int64(len(asset)) {
		t.Fatalf("final progress = %d / %d", lastDone, lastTotal)
	}

	updater.Current = "2.1.0"
	if _, newer, err := updater.Check(context.Background()); err != nil || newer {
		t.Fatalf("same version must not be newer: %v %v", newer, err)
	}
}

func TestDownloadRejectsChecksumMismatch(t *testing.T) {
	asset := []byte("tampered payload")
	release := &fakeRelease{tag: "v2.1.0", asset: asset}
	release.sums = sumLine([]byte("the real payload"), "TelegramVideoUploader-windows-amd64.exe")
	server := serveRelease(t, release)
	updater := newTestUpdater(t, server, "2.0.0")
	found, _, err := updater.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The size in the metadata matches, so only the hash can catch this.
	if _, err := updater.Download(context.Background(), found, nil); err == nil || !strings.Contains(err.Error(), "校验") {
		t.Fatalf("Download = %v, want checksum error", err)
	}
	entries, _ := os.ReadDir(updater.StageDir)
	if len(entries) != 0 {
		t.Fatalf("a rejected download left %d files behind", len(entries))
	}
}

func TestDownloadRequiresChecksumEntryAndTrustedHost(t *testing.T) {
	asset := []byte("payload")
	release := &fakeRelease{tag: "v2.1.0", asset: asset, sums: sumLine(asset, "SomethingElse.exe")}
	server := serveRelease(t, release)
	updater := newTestUpdater(t, server, "2.0.0")
	found, _, err := updater.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updater.Download(context.Background(), found, nil); err == nil {
		t.Fatal("a missing checksum entry must fail the download")
	}
	updater.AllowInsecure = false
	if _, err := updater.Download(context.Background(), found, nil); err == nil || !strings.Contains(err.Error(), "GitHub") {
		t.Fatalf("non-GitHub download URL accepted: %v", err)
	}
}

func TestCheckReportsMissingRelease(t *testing.T) {
	server := serveRelease(t, &fakeRelease{status: http.StatusNotFound})
	updater := newTestUpdater(t, server, "2.0.0")
	if _, _, err := updater.Check(context.Background()); err == nil {
		t.Fatal("expected an error for a repository without releases")
	}
}

func TestInstallExecutableSwapsAndKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "TelegramVideoUploader.exe")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(t.TempDir(), "staged.exe")
	if err := os.WriteFile(staged, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	updater := &Updater{GOOS: "windows", GOARCH: "amd64", Executable: func() (string, error) { return exe, nil }}
	if err := updater.CanInstall(); err != nil {
		t.Fatal(err)
	}
	installed, err := updater.Install(staged)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new" {
		t.Fatalf("executable = %q, want new", got)
	}
	if got, _ := os.ReadFile(installed.PreviousPath); string(got) != "old" {
		t.Fatalf("previous = %q, want old", got)
	}
	if _, err := os.Stat(exe + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary .new file was left behind")
	}
	updater.CleanupPrevious()
	if _, err := os.Stat(installed.PreviousPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("CleanupPrevious did not remove the previous version")
	}
}

func writeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, content := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o755)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func makeBundle(t *testing.T, parent, marker string) (bundle, exe string) {
	t.Helper()
	bundle = filepath.Join(parent, "TelegramVideoUploader.app")
	exe = filepath.Join(bundle, "Contents", "MacOS", "TelegramVideoUploader")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bundle, exe
}

func TestInstallBundleReplacesWholeApplication(t *testing.T) {
	parent := t.TempDir()
	// macOS reports /var as a symlink to /private/var; compare resolved paths.
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = resolved
	}
	bundle, exe := makeBundle(t, parent, "old")
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "stale-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive := writeZip(t, map[string]string{
		"TelegramVideoUploader.app/Contents/MacOS/TelegramVideoUploader": "new",
		"TelegramVideoUploader.app/Contents/Info.plist":                  "<plist/>",
		"TelegramVideoUploader.app/Contents/Resources/AppIcon.icns":      "icon",
		"__MACOSX/TelegramVideoUploader.app/._Contents":                  "resource fork",
	})
	updater := &Updater{GOOS: "darwin", GOARCH: "arm64", Executable: func() (string, error) { return exe, nil }}
	installed, err := updater.Install(archive)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Path != bundle {
		t.Fatalf("installed path = %q, want %q", installed.Path, bundle)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new" {
		t.Fatalf("executable = %q, want new", got)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(exe); err != nil || info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("new executable is not executable: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(bundle, "Contents", "stale-file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("files of the old bundle must not survive in the new one")
	}
	if got, _ := os.ReadFile(filepath.Join(installed.PreviousPath, "Contents", "MacOS", "TelegramVideoUploader")); string(got) != "old" {
		t.Fatal("the previous bundle was not kept for rollback")
	}
	leftovers, _ := filepath.Glob(filepath.Join(parent, ".tvu-update-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary extraction directories left behind: %v", leftovers)
	}
}

func TestInstallBundleRejectsUnsafeArchivesAndKeepsCurrentVersion(t *testing.T) {
	cases := map[string]map[string]string{
		"path traversal": {"TelegramVideoUploader.app/../../evil": "x"},
		"two apps": {
			"A.app/Contents/MacOS/a": "x", "A.app/Contents/Info.plist": "x",
			"B.app/Contents/MacOS/b": "x", "B.app/Contents/Info.plist": "x",
		},
		"not an app": {"readme.txt": "x"},
		"incomplete": {"TelegramVideoUploader.app/Contents/Info.plist": "x"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			_, exe := makeBundle(t, parent, "old")
			updater := &Updater{GOOS: "darwin", GOARCH: "arm64", Executable: func() (string, error) { return exe, nil }}
			if _, err := updater.Install(writeZip(t, files)); err == nil {
				t.Fatal("unsafe archive was installed")
			}
			if got, _ := os.ReadFile(exe); string(got) != "old" {
				t.Fatal("a rejected update must leave the current version untouched")
			}
		})
	}
}

func TestCanInstallExplainsUnsupportedLocations(t *testing.T) {
	loose := filepath.Join(t.TempDir(), "tg-video-uploader")
	if err := os.WriteFile(loose, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := &Updater{GOOS: "darwin", GOARCH: "arm64", Executable: func() (string, error) { return loose, nil }}
	if err := updater.CanInstall(); !errors.Is(err, ErrNotInstallable) {
		t.Fatalf("loose binary: %v", err)
	}
	translocated := filepath.Join(t.TempDir(), "AppTranslocation", "X")
	_, exe := makeBundle(t, translocated, "old")
	updater.Executable = func() (string, error) { return exe, nil }
	if err := updater.CanInstall(); !errors.Is(err, ErrNotInstallable) || !strings.Contains(err.Error(), "应用程序") {
		t.Fatalf("translocated app: %v", err)
	}
	updater = &Updater{GOOS: "linux", GOARCH: "amd64"}
	if err := updater.CanInstall(); !errors.Is(err, ErrNotInstallable) {
		t.Fatalf("unsupported platform: %v", err)
	}
}
