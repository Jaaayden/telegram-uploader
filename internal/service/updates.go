package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	"github.com/Jaaayden/telegram-uploader/internal/update"
)

// updateState.status is one of: "" (never checked), checking, uptodate,
// available, downloading, ready, error.
type updateState struct {
	status      string
	release     update.Release
	message     string
	done, total int64
	staged      string
	cancel      context.CancelFunc
	checkedAt   time.Time
	installHint string
	cleaned     sync.Once
}

// UpdateView describes the updater for the interface.
type UpdateView struct {
	Supported bool   `json:"supported"`
	Status    string `json:"status"`
	Version   string `json:"version,omitempty"`
	Notes     string `json:"notes,omitempty"`
	PageURL   string `json:"pageUrl,omitempty"`
	Message   string `json:"message,omitempty"`
	DoneBytes int64  `json:"doneBytes,omitempty"`
	Total     int64  `json:"totalBytes,omitempty"`
	CheckedAt int64  `json:"checkedAt,omitempty"`
	// InstallHint is set when the update must be installed by hand, with the
	// reason (for example a read-only install location).
	InstallHint string `json:"installHint,omitempty"`
}

func (s *Service) updateViewLocked() UpdateView {
	view := UpdateView{
		Supported:   s.updater != nil,
		Status:      s.update.status,
		Message:     s.update.message,
		DoneBytes:   s.update.done,
		Total:       s.update.total,
		InstallHint: s.update.installHint,
	}
	if !s.update.checkedAt.IsZero() {
		view.CheckedAt = s.update.checkedAt.UnixMilli()
	}
	switch s.update.status {
	case "available", "downloading", "ready":
		view.Version = s.update.release.Version
		view.Notes = s.update.release.Notes
		view.PageURL = s.update.release.PageURL
	case "error":
		// Keep the release so a failed download can be retried.
		view.Version = s.update.release.Version
		view.Notes = s.update.release.Notes
		view.PageURL = s.update.release.PageURL
	}
	return view
}

// httpClient builds the client for update requests. It goes through the
// configured SOCKS5 proxy, because networks that need a proxy for Telegram
// usually need it for GitHub too.
func (s *Service) httpClient() *http.Client {
	settings := s.settingsCopy()
	s.mu.Lock()
	password := s.cached.proxyPassword
	s.mu.Unlock()
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	if settings.ProxyEnabled && settings.ProxyAddress != "" {
		var auth *proxy.Auth
		if settings.ProxyUsername != "" {
			auth = &proxy.Auth{User: settings.ProxyUsername, Password: password}
		}
		if dialer, err := proxy.SOCKS5("tcp", settings.ProxyAddress, auth, &net.Dialer{Timeout: 15 * time.Second}); err == nil {
			transport.Proxy = nil
			if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
				transport.DialContext = contextDialer.DialContext
			} else {
				transport.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
					return dialer.Dial(network, address)
				}
			}
		}
	}
	return &http.Client{Transport: transport}
}

func (s *Service) updateLoop() {
	if s.updater == nil {
		return
	}
	// Leave the first seconds to the connection and the first paint.
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
		case <-s.rootCtx.Done():
			return
		}
		if !s.settingsCopy().DisableUpdateCheck {
			s.checkUpdate(false)
		}
		timer.Reset(updateCheckInterval)
	}
}

// CheckUpdate looks for a newer release on behalf of the user.
func (s *Service) CheckUpdate() error {
	if s.updater == nil {
		return errors.New("这个构建不支持应用内更新")
	}
	s.checkUpdate(true)
	return nil
}

func (s *Service) checkUpdate(manual bool) {
	s.mu.Lock()
	switch s.update.status {
	case "checking", "downloading", "ready":
		// Never discard a download in progress or a staged update.
		s.mu.Unlock()
		return
	}
	previous := s.update.status
	s.update.status = "checking"
	s.update.message = ""
	s.mu.Unlock()
	s.invalidate()

	ctx, cancel := context.WithTimeout(s.rootCtx, 30*time.Second)
	release, newer, err := s.updater.Check(ctx)
	cancel()

	s.mu.Lock()
	s.update.checkedAt = time.Now()
	announce := false
	switch {
	case err != nil:
		_ = diagnostics.Logf("update_check_failed manual=%t error_type=%T", manual, err)
		if manual {
			s.update.status = "error"
			s.update.message = err.Error()
			s.update.release = update.Release{}
		} else if previous == "available" {
			// A background failure must not hide an update already found.
			s.update.status = previous
		} else {
			s.update.status = ""
		}
	case newer:
		announce = !manual && (previous != "available" || s.update.release.Version != release.Version)
		s.update.status = "available"
		s.update.release = release
		s.update.installHint = ""
		if installErr := s.updater.CanInstall(); installErr != nil {
			s.update.installHint = installErr.Error()
		} else if release.AssetURL == "" {
			s.update.installHint = "这个版本没有适用于当前系统的安装包"
		}
	default:
		s.update.status = "uptodate"
		s.update.release = update.Release{}
	}
	version := s.update.release.Version
	s.mu.Unlock()
	if announce {
		s.toast("info", "发现新版本 v"+version+"，可以在“设置 → 关于与更新”里安装")
	}
	s.invalidate()
}

// DownloadUpdate downloads and verifies the available release in the
// background.
func (s *Service) DownloadUpdate() error {
	if s.updater == nil {
		return errors.New("这个构建不支持应用内更新")
	}
	s.mu.Lock()
	if s.update.status != "available" && s.update.status != "error" || s.update.release.AssetURL == "" {
		s.mu.Unlock()
		return errors.New("没有可下载的更新")
	}
	if s.update.installHint != "" {
		hint := s.update.installHint
		s.mu.Unlock()
		return errors.New(hint)
	}
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	release := s.update.release
	ctx, cancel := context.WithCancel(s.rootCtx)
	s.update.status = "downloading"
	s.update.message = ""
	s.update.done, s.update.total = 0, release.AssetSize
	s.update.cancel = cancel
	s.workers.Add(1)
	s.mu.Unlock()
	s.invalidate()

	go func() {
		defer s.workers.Done()
		staged, err := s.updater.Download(ctx, release, func(done, total int64) {
			s.mu.Lock()
			s.update.done, s.update.total = done, total
			s.mu.Unlock()
			s.invalidate()
		})
		cancel()
		s.mu.Lock()
		s.update.cancel = nil
		switch {
		case err == nil:
			s.update.status = "ready"
			s.update.staged = staged
		case errors.Is(err, context.Canceled):
			s.update.status = "available"
		default:
			s.update.status = "error"
			s.update.message = err.Error()
		}
		s.mu.Unlock()
		if err == nil {
			s.toast("success", "更新已下载并校验，重启应用即可完成安装")
		}
		s.invalidate()
	}()
	return nil
}

// CancelUpdateDownload stops a running download.
func (s *Service) CancelUpdateDownload() {
	s.mu.Lock()
	cancel := s.update.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// InstallUpdate swaps in the downloaded version and restarts the application.
func (s *Service) InstallUpdate() error {
	if s.updater == nil {
		return errors.New("这个构建不支持应用内更新")
	}
	s.mu.Lock()
	staged := s.update.staged
	ready := s.update.status == "ready" && staged != ""
	s.mu.Unlock()
	if !ready {
		return errors.New("更新还没有下载完成")
	}
	if s.controller.Snapshot().Running {
		return errors.New("上传进行中；请等队列结束或暂停后再重启更新")
	}
	installed, err := s.updater.Install(staged)
	if err != nil {
		return err
	}
	_ = diagnostics.Logf("update_installed path=%q", installed.Path)
	s.updater.ClearStage()
	// Stop cleanly before the new version starts; it waits for this process.
	s.Shutdown(false)
	if err := installed.Relaunch(os.Getpid()); err != nil {
		_ = diagnostics.Logf("update_relaunch_failed error_type=%T", err)
	}
	s.host.Quit()
	return nil
}

// SetAutoUpdateCheck turns the periodic update check on or off.
func (s *Service) SetAutoUpdateCheck(enabled bool) error {
	if err := s.updateSettings(func(settings *coreapp.Settings) { settings.DisableUpdateCheck = !enabled }); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

// markHealthyStart removes the previous version kept by the last update. It
// runs once, when the interface has loaded, which proves this version works.
func (s *Service) markHealthyStart() {
	if s.updater == nil {
		return
	}
	s.update.cleaned.Do(func() {
		go s.updater.CleanupPrevious()
	})
}
