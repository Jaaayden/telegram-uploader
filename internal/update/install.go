package update

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Installed describes a completed swap. The previous version is kept beside
// the new one until CleanupPrevious runs in the next healthy start, so a
// broken update can be undone by renaming it back.
type Installed struct {
	// Path is what was replaced: the .app bundle on macOS, the .exe on Windows.
	Path string
	// PreviousPath is where the replaced version was kept.
	PreviousPath string
	relaunch     func(waitPID int) error
}

// Relaunch starts the new version. It is told to wait for waitPID (this
// process) to exit first, so two instances never share the queue.
func (i *Installed) Relaunch(waitPID int) error {
	if i == nil || i.relaunch == nil {
		return errors.New("无法重新启动应用")
	}
	return i.relaunch(waitPID)
}

type target struct {
	// path is the bundle or executable to replace.
	path string
	// previous is where the old version is parked.
	previous string
	bundle   bool
}

// CanInstall reports whether the running copy can replace itself. The error
// is user-facing and wraps ErrNotInstallable.
func (u *Updater) CanInstall() error {
	_, err := u.locate()
	return err
}

func (u *Updater) locate() (target, error) {
	goos, goarch := u.platform()
	if AssetNameFor(goos, goarch) == "" {
		return target{}, fmt.Errorf("%w：没有适用于 %s/%s 的安装包", ErrNotInstallable, goos, goarch)
	}
	exe, err := u.executable()
	if err != nil {
		return target{}, err
	}
	if goos == "darwin" {
		macOS := filepath.Dir(exe)
		contents := filepath.Dir(macOS)
		bundle := filepath.Dir(contents)
		if filepath.Base(macOS) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(bundle, ".app") {
			return target{}, fmt.Errorf("%w：程序不是从 .app 应用包运行的", ErrNotInstallable)
		}
		// ToSlash keeps the check independent of the host's path separator.
		if strings.Contains(filepath.ToSlash(bundle), "/AppTranslocation/") {
			return target{}, fmt.Errorf("%w：请先把应用拖到“应用程序”文件夹，再从那里打开", ErrNotInstallable)
		}
		parent := filepath.Dir(bundle)
		if err := checkWritable(parent); err != nil {
			return target{}, fmt.Errorf("%w：没有 %s 的写入权限", ErrNotInstallable, parent)
		}
		return target{
			path:     bundle,
			previous: filepath.Join(parent, "."+filepath.Base(bundle)+".previous"),
			bundle:   true,
		}, nil
	}
	dir := filepath.Dir(exe)
	if err := checkWritable(dir); err != nil {
		return target{}, fmt.Errorf("%w：没有 %s 的写入权限，请把程序放到可写的文件夹", ErrNotInstallable, dir)
	}
	return target{path: exe, previous: exe + ".old"}, nil
}

func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".tvu-write-test-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

// Install replaces the running application with a file returned by Download.
func (u *Updater) Install(staged string) (*Installed, error) {
	destination, err := u.locate()
	if err != nil {
		return nil, err
	}
	var prepared string
	var cleanup func()
	if destination.bundle {
		prepared, cleanup, err = extractBundle(staged, filepath.Dir(destination.path))
	} else {
		prepared = destination.path + ".new"
		cleanup = func() { _ = os.Remove(prepared) }
		err = copyFile(staged, prepared, 0o755)
	}
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, fmt.Errorf("安装更新失败：%w", err)
	}
	defer cleanup()

	if err := os.RemoveAll(destination.previous); err != nil {
		return nil, fmt.Errorf("安装更新失败：无法清理旧备份：%w", err)
	}
	// A running executable (and an open .app) can be renamed on both systems
	// even though it cannot be overwritten on Windows.
	if err := os.Rename(destination.path, destination.previous); err != nil {
		return nil, fmt.Errorf("安装更新失败：无法移开当前版本：%w", err)
	}
	if err := os.Rename(prepared, destination.path); err != nil {
		if rollbackErr := os.Rename(destination.previous, destination.path); rollbackErr != nil {
			return nil, fmt.Errorf("安装更新失败：%w；恢复旧版本也失败：%v，旧版本位于 %s", err, rollbackErr, destination.previous)
		}
		return nil, fmt.Errorf("安装更新失败，已恢复当前版本：%w", err)
	}

	installed := &Installed{Path: destination.path, PreviousPath: destination.previous}
	path := destination.path
	if destination.bundle {
		installed.relaunch = func(waitPID int) error {
			return exec.Command("/usr/bin/open", "-n", path, "--args", WaitPIDFlag, strconv.Itoa(waitPID)).Start()
		}
	} else {
		installed.relaunch = func(waitPID int) error {
			command := exec.Command(path, WaitPIDFlag, strconv.Itoa(waitPID))
			command.Dir = filepath.Dir(path)
			return command.Start()
		}
	}
	return installed, nil
}

// CleanupPrevious deletes the version kept by the last Install. Call it once
// the new version has started successfully.
func (u *Updater) CleanupPrevious() {
	destination, err := u.locate()
	if err != nil {
		return
	}
	_ = os.RemoveAll(destination.previous)
	if !destination.bundle {
		_ = os.Remove(destination.path + ".new")
	}
}

func copyFile(source, destination string, perm os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// extractBundle unpacks the single .app in archive into a temporary directory
// inside parent (the same volume as the destination, so the final rename is
// atomic). It returns the extracted bundle and a cleanup function.
func extractBundle(archive, parent string) (string, func(), error) {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return "", nil, fmt.Errorf("无法打开更新包：%w", err)
	}
	defer reader.Close()
	workDir, err := os.MkdirTemp(parent, ".tvu-update-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(workDir) }

	var bundleName string
	var totalBytes uint64
	for _, entry := range reader.File {
		name := filepath.ToSlash(entry.Name)
		// ditto stores resource forks in a parallel __MACOSX tree.
		if strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return "", cleanup, fmt.Errorf("更新包包含不安全的路径 %q", entry.Name)
		}
		top, _, _ := strings.Cut(name, "/")
		if !strings.HasSuffix(top, ".app") {
			return "", cleanup, fmt.Errorf("更新包内容异常：%q", entry.Name)
		}
		if bundleName == "" {
			bundleName = top
		} else if bundleName != top {
			return "", cleanup, errors.New("更新包里有多个应用")
		}
		totalBytes += entry.UncompressedSize64
		if totalBytes > 4*maxAssetBytes {
			return "", cleanup, errors.New("更新包解压后大小异常")
		}
		destination := filepath.Join(workDir, filepath.FromSlash(name))
		mode := entry.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return "", cleanup, err
			}
		case mode&os.ModeSymlink != 0:
			if err := extractSymlink(entry, destination); err != nil {
				return "", cleanup, err
			}
		case mode.IsRegular():
			if err := extractFile(entry, destination); err != nil {
				return "", cleanup, err
			}
		default:
			return "", cleanup, fmt.Errorf("更新包包含不支持的文件类型：%q", entry.Name)
		}
	}
	if bundleName == "" {
		return "", cleanup, errors.New("更新包里没有应用")
	}
	bundle := filepath.Join(workDir, bundleName)
	executables, err := os.ReadDir(filepath.Join(bundle, "Contents", "MacOS"))
	if err != nil || len(executables) == 0 {
		return "", cleanup, errors.New("更新包里的应用不完整")
	}
	if _, err := os.Stat(filepath.Join(bundle, "Contents", "Info.plist")); err != nil {
		return "", cleanup, errors.New("更新包里的应用不完整")
	}
	return bundle, cleanup, nil
}

func extractFile(entry *zip.File, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	source, err := entry.Open()
	if err != nil {
		return err
	}
	defer source.Close()
	perm := entry.Mode().Perm()
	if perm == 0 {
		perm = 0o644
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm|0o600)
	if err != nil {
		return err
	}
	// The size was bounded while iterating; the limit also covers archives
	// whose headers under-report it.
	if _, err := io.Copy(out, io.LimitReader(source, 4*maxAssetBytes)); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func extractSymlink(entry *zip.File, destination string) error {
	source, err := entry.Open()
	if err != nil {
		return err
	}
	defer source.Close()
	raw, err := io.ReadAll(io.LimitReader(source, 4096))
	if err != nil {
		return err
	}
	link := string(raw)
	// A link may only point inside the bundle it lives in.
	resolved := filepath.Join(filepath.Dir(filepath.FromSlash(entry.Name)), filepath.FromSlash(link))
	if filepath.IsAbs(link) || !filepath.IsLocal(resolved) {
		return fmt.Errorf("更新包包含不安全的链接 %q", entry.Name)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	return os.Symlink(link, destination)
}
