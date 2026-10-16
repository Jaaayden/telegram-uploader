// Package applog configures the process-wide structured logger. A GUI build
// has no visible stderr (notably on Windows with -H=windowsgui), so diagnostics
// go to a size-limited file in the application data directory.
package applog

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	// FileName is the active log file inside the application data directory.
	FileName = "app.log"
	// MaxBytes is the size at which the previous log is rotated at startup.
	MaxBytes = 2 << 20
)

// Init installs a text slog handler writing to dir/app.log and returns a
// closer for the file. If the log file cannot be opened it falls back to
// stderr and still returns a usable closer, so logging never blocks startup.
// Log lines must never contain credentials; callers log only error text.
func Init(dir string) io.Closer {
	file, err := openRotated(filepath.Join(dir, FileName))
	var out io.Writer = os.Stderr
	var closer io.Closer = nopCloser{}
	if err == nil {
		out = file
		closer = file
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err != nil {
		slog.Warn("log file unavailable; logging to stderr", "error", err)
	}
	return closer
}

// openRotated opens path for appending with mode 0600. A file already larger
// than MaxBytes is renamed to path+".1" (replacing any older rotation) first.
func openRotated(path string) (*os.File, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > MaxBytes {
		if err := os.Rename(path, path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
