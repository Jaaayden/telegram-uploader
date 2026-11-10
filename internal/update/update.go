// Package update implements the in-app updater: it asks GitHub for the latest
// release, downloads the asset for this platform, verifies it against the
// release's SHA256SUMS file and swaps it in for the running application.
//
// Trust model: the release metadata, the checksum file and the asset are all
// fetched over HTTPS from GitHub. The checksum protects against truncated or
// corrupted downloads; it is not an independent signature.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPIBase = "https://api.github.com"
	checksumsName  = "SHA256SUMS"
	// maxAssetBytes rejects absurd downloads before they fill the disk. The
	// real assets are a few tens of megabytes.
	maxAssetBytes    = 512 << 20
	maxMetadataBytes = 4 << 20
	// WaitPIDFlag is passed to the relaunched application so it waits for the
	// old process to exit before touching the queue and the Telegram session.
	WaitPIDFlag = "--wait-pid"
)

// ErrNotInstallable means the running copy cannot replace itself, for example
// a development binary or an application in a read-only location. The message
// is user-facing.
var ErrNotInstallable = errors.New("当前安装方式不支持自动更新")

// Release describes the newest published version and its asset for the
// current platform.
type Release struct {
	Version      string `json:"version"`
	Notes        string `json:"notes"`
	PageURL      string `json:"pageUrl"`
	AssetName    string `json:"assetName"`
	AssetURL     string `json:"-"`
	AssetSize    int64  `json:"assetSize"`
	ChecksumsURL string `json:"-"`
}

// Updater is safe for concurrent use as long as its fields are not modified
// after the first call.
type Updater struct {
	// Repository is the GitHub "owner/name" slug.
	Repository string
	// Current is the running version, with or without a leading "v".
	Current string
	// StageDir receives verified downloads.
	StageDir string
	// HTTPClient returns the client for one operation. It is a function so a
	// proxy change in the settings applies to the next check.
	HTTPClient func() *http.Client

	// The fields below exist for tests; the zero values select production
	// behaviour.
	APIBase       string
	GOOS, GOARCH  string
	Executable    func() (string, error)
	AllowInsecure bool
}

// AssetNameFor returns the release asset for a platform, or "" when no
// prebuilt package exists for it.
func AssetNameFor(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "darwin/arm64":
		return "TelegramVideoUploader-macos-arm64.zip"
	case "darwin/amd64":
		return "TelegramVideoUploader-macos-x86_64.zip"
	case "windows/amd64":
		return "TelegramVideoUploader-windows-amd64.exe"
	default:
		return ""
	}
}

func (u *Updater) platform() (string, string) {
	goos, goarch := u.GOOS, u.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

func (u *Updater) client() *http.Client {
	if u.HTTPClient != nil {
		if client := u.HTTPClient(); client != nil {
			return client
		}
	}
	return &http.Client{Timeout: 0}
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// Check fetches the latest stable release. The boolean reports whether it is
// newer than the running version.
func (u *Updater) Check(ctx context.Context) (Release, bool, error) {
	base := u.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	endpoint := strings.TrimRight(base, "/") + "/repos/" + u.Repository + "/releases/latest"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, false, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "telegram-video-uploader-updater")
	response, err := u.client().Do(request)
	if err != nil {
		return Release{}, false, fmt.Errorf("检查更新失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return Release{}, false, errors.New("检查更新失败：还没有发布任何正式版本")
	}
	if response.StatusCode != http.StatusOK {
		return Release{}, false, fmt.Errorf("检查更新失败：GitHub 返回 %s", response.Status)
	}
	var payload githubRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, maxMetadataBytes)).Decode(&payload); err != nil {
		return Release{}, false, fmt.Errorf("检查更新失败：无法解析发布信息：%w", err)
	}
	release := Release{
		Version: strings.TrimPrefix(strings.TrimSpace(payload.TagName), "v"),
		Notes:   strings.TrimSpace(payload.Body),
		PageURL: payload.HTMLURL,
	}
	if _, ok := parseVersion(release.Version); !ok {
		return Release{}, false, fmt.Errorf("检查更新失败：无法识别版本号 %q", payload.TagName)
	}
	goos, goarch := u.platform()
	wanted := AssetNameFor(goos, goarch)
	for _, asset := range payload.Assets {
		switch asset.Name {
		case wanted:
			release.AssetName, release.AssetURL, release.AssetSize = asset.Name, asset.URL, asset.Size
		case checksumsName:
			release.ChecksumsURL = asset.URL
		}
	}
	return release, IsNewer(release.Version, u.Current), nil
}

// Download fetches and verifies the release asset. It returns the path of the
// verified file inside StageDir. progress may be nil.
func (u *Updater) Download(ctx context.Context, release Release, progress func(done, total int64)) (string, error) {
	if release.AssetURL == "" || release.AssetName == "" {
		return "", fmt.Errorf("%w：这个版本没有适用于当前系统的安装包", ErrNotInstallable)
	}
	if release.ChecksumsURL == "" {
		return "", errors.New("下载更新失败：发布中缺少 SHA256SUMS 校验文件")
	}
	if release.AssetSize > maxAssetBytes {
		return "", errors.New("下载更新失败：安装包大小异常")
	}
	for _, raw := range []string{release.AssetURL, release.ChecksumsURL} {
		if err := u.checkDownloadURL(raw); err != nil {
			return "", err
		}
	}
	wantSum, err := u.fetchChecksum(ctx, release)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(u.StageDir, 0o700); err != nil {
		return "", fmt.Errorf("下载更新失败：%w", err)
	}
	// Only one staged download is ever useful; drop leftovers from older runs.
	u.ClearStage()
	if err := os.MkdirAll(u.StageDir, 0o700); err != nil {
		return "", fmt.Errorf("下载更新失败：%w", err)
	}
	target := filepath.Join(u.StageDir, release.AssetName)
	partial := target + ".part"
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("下载更新失败：%w", err)
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(partial)
		}
	}()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, release.AssetURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "telegram-video-uploader-updater")
	response, err := u.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("下载更新失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载更新失败：服务器返回 %s", response.Status)
	}
	total := release.AssetSize
	if total <= 0 {
		total = response.ContentLength
	}
	hasher := sha256.New()
	counter := &progressWriter{total: total, report: progress}
	written, err := io.Copy(io.MultiWriter(file, hasher, counter), io.LimitReader(response.Body, maxAssetBytes+1))
	if err != nil {
		return "", fmt.Errorf("下载更新失败：%w", err)
	}
	if written > maxAssetBytes {
		return "", errors.New("下载更新失败：安装包大小异常")
	}
	if release.AssetSize > 0 && written != release.AssetSize {
		return "", fmt.Errorf("下载更新失败：文件不完整（%d / %d 字节）", written, release.AssetSize)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); !strings.EqualFold(got, wantSum) {
		return "", errors.New("下载更新失败：SHA-256 校验不一致，已丢弃下载的文件")
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("下载更新失败：%w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("下载更新失败：%w", err)
	}
	if err := os.Rename(partial, target); err != nil {
		return "", fmt.Errorf("下载更新失败：%w", err)
	}
	committed = true
	return target, nil
}

// ClearStage removes staged downloads.
func (u *Updater) ClearStage() {
	if u.StageDir != "" {
		_ = os.RemoveAll(u.StageDir)
	}
}

func (u *Updater) checkDownloadURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("下载更新失败：下载地址无效：%w", err)
	}
	if u.AllowInsecure {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme != "https" || (host != "github.com" && !strings.HasSuffix(host, ".githubusercontent.com")) {
		return errors.New("下载更新失败：下载地址不属于 GitHub")
	}
	return nil
}

func (u *Updater) fetchChecksum(ctx context.Context, release Release) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, release.ChecksumsURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "telegram-video-uploader-updater")
	response, err := u.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("下载更新失败：无法获取校验文件：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载更新失败：校验文件返回 %s", response.Status)
	}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, maxMetadataBytes))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		// sha256sum prefixes binary-mode names with "*".
		if strings.TrimPrefix(fields[1], "*") == release.AssetName && len(fields[0]) == sha256.Size*2 {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("下载更新失败：无法读取校验文件：%w", err)
	}
	return "", errors.New("下载更新失败：校验文件中没有这个安装包")
}

type progressWriter struct {
	done, total int64
	last        time.Time
	report      func(done, total int64)
}

func (p *progressWriter) Write(data []byte) (int, error) {
	p.done += int64(len(data))
	if p.report != nil {
		// Ten reports per second are plenty for a progress bar.
		if now := time.Now(); now.Sub(p.last) >= 100*time.Millisecond || (p.total > 0 && p.done >= p.total) {
			p.last = now
			p.report(p.done, p.total)
		}
	}
	return len(data), nil
}

// IsNewer reports whether candidate is a later version than current. A
// pre-release such as "2.0.0-dev.abc1234" is older than "2.0.0". Versions
// that cannot be parsed are never considered newer.
func IsNewer(candidate, current string) bool {
	next, ok := parseVersion(candidate)
	if !ok {
		return false
	}
	have, ok := parseVersion(current)
	if !ok {
		// An unidentifiable build (for example "dev") should still be offered
		// real releases.
		return true
	}
	for i := range next.core {
		if next.core[i] != have.core[i] {
			return next.core[i] > have.core[i]
		}
	}
	return have.prerelease && !next.prerelease
}

type version struct {
	core       [3]int
	prerelease bool
}

func parseVersion(text string) (version, bool) {
	text = strings.TrimPrefix(strings.TrimSpace(text), "v")
	if index := strings.IndexByte(text, '+'); index >= 0 {
		text = text[:index]
	}
	var parsed version
	if index := strings.IndexByte(text, '-'); index >= 0 {
		parsed.prerelease = true
		text = text[:index]
	}
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return version{}, false
		}
		parsed.core[i] = value
	}
	return parsed, true
}

func (u *Updater) executable() (string, error) {
	locate := u.Executable
	if locate == nil {
		locate = os.Executable
	}
	path, err := locate()
	if err != nil {
		return "", fmt.Errorf("%w：无法确定程序位置：%v", ErrNotInstallable, err)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path, nil
}
