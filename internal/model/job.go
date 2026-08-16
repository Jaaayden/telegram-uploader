package model

import "time"

// JobState is persisted. Values must therefore remain backward compatible.
type JobState string

const (
	JobQueued      JobState = "queued"
	JobOversize    JobState = "oversize"
	JobUploading   JobState = "uploading"
	JobSending     JobState = "sending"
	JobConfirming  JobState = "confirming"
	JobSent        JobState = "sent"
	JobCancelled   JobState = "cancelled"
	JobFailed      JobState = "failed"
	JobSkipped     JobState = "skipped"
	JobMoving      JobState = "moving"
	JobMoved       JobState = "moved"
	JobInterrupted JobState = "interrupted"
)

// VideoMetadata contains the fields needed to make Telegram render an
// uploaded MP4 as a video, plus non-fatal source-container diagnostics.
// Duration is expressed in whole seconds.
type VideoMetadata struct {
	DurationSeconds    int  `json:"duration_seconds"`
	Width              int  `json:"width"`
	Height             int  `json:"height"`
	SupportsStreaming  bool `json:"supports_streaming"`
	TruncatedMediaData bool `json:"truncated_media_data,omitempty"`
}

// Job is a snapshot of one file in an upload run. Path, Size and ModTime are
// checked again immediately before upload so changed files are never sent by
// accident.
type Job struct {
	ID              string        `json:"id"`
	Position        int           `json:"position"`
	Path            string        `json:"path"`
	Name            string        `json:"name"`
	Size            int64         `json:"size"`
	ModTime         time.Time     `json:"mod_time"`
	State           JobState      `json:"state"`
	Uploaded        int64         `json:"uploaded"`
	BytesPerSecond  float64       `json:"-"`
	RandomID        int64         `json:"random_id"`
	MessageID       int           `json:"message_id,omitempty"`
	ChannelID       int64         `json:"channel_id,omitempty"`
	Metadata        VideoMetadata `json:"metadata"`
	Error           string        `json:"error,omitempty"`
	StartedAt       *time.Time    `json:"started_at,omitempty"`
	CompletedAt     *time.Time    `json:"completed_at,omitempty"`
	MoveDestination string        `json:"move_destination,omitempty"`
}

// Progress is emitted by long-running upload and move operations. The UI may
// coalesce updates, but BytesDone is always monotonic within one operation.
type Progress struct {
	BytesDone      int64
	BytesTotal     int64
	BytesPerSecond float64
	At             time.Time
}

// Channel is the minimum durable Telegram peer information needed to address
// a private channel without resolving it again after every launch.
type Channel struct {
	ID         int64  `json:"id"`
	AccessHash int64  `json:"access_hash"`
	Title      string `json:"title"`
}
