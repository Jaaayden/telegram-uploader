// Package queue persists the upload queue between application runs.
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jayden/telegram-video-uploader/internal/model"
)

const currentSchemaVersion = 1

// ErrUnsupportedSchemaVersion indicates that a queue file uses a schema this
// version of the application cannot safely interpret.
var ErrUnsupportedSchemaVersion = errors.New("queue: unsupported schema version")

// Store identifies a queue file. It is a string type intentionally so that
// Store(path) is a convenient constructor while keeping the on-disk path
// immutable for the lifetime of a store value.
type Store string

// NewStore is an explicit constructor for callers that prefer constructor
// naming. Store(path) is equivalent.
func NewStore(path string) Store {
	return Store(path)
}

type document struct {
	SchemaVersion int           `json:"schema_version"`
	Jobs          []model.Job   `json:"jobs"`
	Channel       model.Channel `json:"channel"`
}

// loadDocument contains pointers for version fields so a missing version is
// distinguishable from the valid zero value. Version is accepted as a read
// alias for early files that used the shorter field name; new files always
// use schema_version.
type loadDocument struct {
	SchemaVersion *int          `json:"schema_version"`
	Version       *int          `json:"version"`
	Jobs          []model.Job   `json:"jobs"`
	Channel       model.Channel `json:"channel"`
}

// Save atomically persists jobs and the Telegram channel metadata. The
// temporary file is created beside the destination with mode 0600, flushed to
// stable storage before rename, and removed on every unsuccessful path.
func (s Store) Save(jobs []model.Job, channel model.Channel) (err error) {
	path := string(s)
	if path == "" {
		return errors.New("queue: empty store path")
	}

	payload, err := json.Marshal(document{
		SchemaVersion: currentSchemaVersion,
		Jobs:          jobs,
		Channel:       channel,
	})
	if err != nil {
		return fmt.Errorf("queue: encode store: %w", err)
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("queue: create temporary store: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("queue: set temporary store permissions: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		return fmt.Errorf("queue: write temporary store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("queue: sync temporary store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("queue: close temporary store: %w", err)
	}
	closed = true

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("queue: replace store: %w", err)
	}
	committed = true

	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("queue: sync store directory: %w", err)
	}
	return nil
}

// Load reads the queue file. A missing file is an empty queue. Interrupted
// work is never resumed blindly: jobs that were active when the process
// stopped are marked interrupted and their transient byte count is cleared,
// while RandomID is retained for the caller's recovery logic.
func (s Store) Load() ([]model.Job, model.Channel, error) {
	path := string(s)
	if path == "" {
		return nil, model.Channel{}, errors.New("queue: empty store path")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []model.Job{}, model.Channel{}, nil
		}
		return nil, model.Channel{}, fmt.Errorf("queue: read store: %w", err)
	}

	var raw loadDocument
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, model.Channel{}, fmt.Errorf("queue: decode store: %w", err)
	}

	version, ok := schemaVersion(raw.SchemaVersion, raw.Version)
	if !ok {
		return nil, model.Channel{}, fmt.Errorf("%w: missing schema version", ErrUnsupportedSchemaVersion)
	}
	if version != currentSchemaVersion {
		return nil, model.Channel{}, fmt.Errorf("%w: %d", ErrUnsupportedSchemaVersion, version)
	}

	for i := range raw.Jobs {
		switch raw.Jobs[i].State {
		case model.JobUploading:
			raw.Jobs[i].State = model.JobInterrupted
			raw.Jobs[i].Uploaded = 0
		case model.JobSending, model.JobConfirming:
			raw.Jobs[i].State = model.JobConfirming
			raw.Jobs[i].Error = "程序在消息提交阶段中断；请先检查频道，避免重复发送"
		}
	}

	return raw.Jobs, raw.Channel, nil
}

func schemaVersion(schemaVersion, version *int) (int, bool) {
	if schemaVersion != nil && version != nil && *schemaVersion != *version {
		return 0, false
	}
	if schemaVersion != nil {
		return *schemaVersion, true
	}
	if version != nil {
		return *version, true
	}
	return 0, false
}

// syncDirectory makes the rename durable on platforms/filesystems that
// support directory fsync. Windows does not support opening a directory as a
// syncable file, and some filesystems report the same limitation as EINVAL or
// "operation not supported"; those cases are safe to ignore after the file
// itself has already been synced and renamed.
func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil && !directorySyncUnsupported(syncErr) {
		return syncErr
	}
	return closeErr
}

func directorySyncUnsupported(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "invalid argument") ||
		strings.Contains(message, "not supported") ||
		strings.Contains(message, "function not implemented")
}
