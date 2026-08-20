package diagnostics

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
)

// rotatingLogger is intentionally small: app.log is opened only by this
// process, and rotation happens while the same mutex protects every write.
// That keeps records from concurrent upload workers intact.
type rotatingLogger struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	maxBytes int64
	backups  int
}

func newRotatingLogger(path string) (*rotatingLogger, error) {
	l := &rotatingLogger{
		path:     path,
		maxBytes: MaxLogBytes,
		backups:  MaxLogBackups,
	}
	if err := l.openLocked(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *rotatingLogger) openLocked() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := protectFile(l.path); err != nil {
		_ = f.Close()
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.file = f
	l.size = info.Size()
	return nil
}

// Write appends one complete record.  It rotates before the record that would
// exceed the limit, retaining app.log.1 through app.log.3.
func (l *rotatingLogger) Write(data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return ErrClosed
	}
	if l.size > 0 && l.size+int64(len(data)) > l.maxBytes {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}
	n, err := l.file.Write(data)
	l.size += int64(n)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	// A long-running uploader should not lose the latest useful diagnostic
	// line merely because Windows or the host process is terminated.
	return l.file.Sync()
}

func (l *rotatingLogger) rotateLocked() error {
	if l.file == nil {
		return ErrClosed
	}
	var errs []error
	if err := l.file.Sync(); err != nil {
		errs = append(errs, err)
	}
	if err := l.file.Close(); err != nil {
		errs = append(errs, err)
	}
	l.file = nil
	if len(errs) > 0 {
		// Reopen so a transient sync/close error does not leave the logger
		// permanently unusable.
		_ = l.openLocked()
		return errors.Join(errs...)
	}

	for index := l.backups; index >= 1; index-- {
		destination := l.path + "." + strconv.Itoa(index)
		if index == l.backups {
			if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
				_ = l.openLocked()
				return fmt.Errorf("remove old log backup: %w", err)
			}
		}

		source := l.path + "." + strconv.Itoa(index-1)
		if index == 1 {
			source = l.path
		}
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			_ = l.openLocked()
			return fmt.Errorf("stat log during rotation: %w", err)
		}
		// Removing the destination makes this work on Windows too, where
		// Rename cannot replace an existing file.
		if index != l.backups {
			if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
				_ = l.openLocked()
				return fmt.Errorf("remove log backup: %w", err)
			}
		}
		if err := os.Rename(source, destination); err != nil {
			_ = l.openLocked()
			return fmt.Errorf("rotate log: %w", err)
		}
	}

	if err := l.openLocked(); err != nil {
		return fmt.Errorf("reopen app log after rotation: %w", err)
	}
	return nil
}

func (l *rotatingLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	syncErr := l.file.Sync()
	closeErr := l.file.Close()
	l.file = nil
	if syncErr != nil && closeErr != nil {
		return errors.Join(syncErr, closeErr)
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
