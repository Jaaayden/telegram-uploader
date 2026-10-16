// Package fsutil contains the small durable-file helpers shared by the queue,
// settings, credential and mover packages.
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// SyncDir flushes directory metadata so a preceding rename survives a crash.
// Windows cannot open a directory for syncing, and some filesystems reject
// directory fsync; those cases are ignored because the file contents have
// already been synced by the caller.
func SyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil && !dirSyncUnsupported(syncErr) {
		return syncErr
	}
	return closeErr
}

func dirSyncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, errors.ErrUnsupported)
}

// AtomicWriteFile durably replaces path with data. The temporary file is
// created beside the destination with the requested permissions, synced before
// the rename, and removed on every unsuccessful path. The returned error wraps
// the failing step; a directory-sync failure after a successful rename is
// returned too, because the new content is then visible but not yet durable.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	closed := false
	committed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	closed = true
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	committed = true
	if err := SyncDir(dir); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
