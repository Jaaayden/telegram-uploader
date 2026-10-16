// Package queue persists the upload queue between application runs.
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jayden/telegram-video-uploader/internal/fsutil"
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
	Paused        bool          `json:"paused,omitempty"`
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
	Paused        bool          `json:"paused"`
}

// Save atomically persists jobs, Telegram channel metadata, and the queue's
// explicit paused state. The temporary file is created beside the destination
// with mode 0600, flushed to stable storage before rename, and removed on every
// unsuccessful path.
func (s Store) Save(jobs []model.Job, channel model.Channel, paused bool) (err error) {
	path := string(s)
	if path == "" {
		return errors.New("queue: empty store path")
	}

	payload, err := json.Marshal(document{
		SchemaVersion: currentSchemaVersion,
		Jobs:          jobs,
		Channel:       channel,
		Paused:        paused,
	})
	if err != nil {
		return fmt.Errorf("queue: encode store: %w", err)
	}

	if err := fsutil.AtomicWriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("queue: write store: %w", err)
	}
	return nil
}

// Load reads the queue file. A missing file is an empty, unpaused queue. Interrupted
// work is never resumed blindly: jobs that were active when the process
// stopped are marked interrupted and their transient byte count is cleared,
// while RandomID is retained for the caller's recovery logic.
func (s Store) Load() ([]model.Job, model.Channel, bool, error) {
	path := string(s)
	if path == "" {
		return nil, model.Channel{}, false, errors.New("queue: empty store path")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []model.Job{}, model.Channel{}, false, nil
		}
		return nil, model.Channel{}, false, fmt.Errorf("queue: read store: %w", err)
	}

	var raw loadDocument
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, model.Channel{}, false, fmt.Errorf("queue: decode store: %w", err)
	}

	version, ok := schemaVersion(raw.SchemaVersion, raw.Version)
	if !ok {
		return nil, model.Channel{}, false, fmt.Errorf("%w: missing schema version", ErrUnsupportedSchemaVersion)
	}
	if version != currentSchemaVersion {
		return nil, model.Channel{}, false, fmt.Errorf("%w: %d", ErrUnsupportedSchemaVersion, version)
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

	return raw.Jobs, raw.Channel, raw.Paused, nil
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
