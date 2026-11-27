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
	UploadElapsedMs int64         `json:"upload_elapsed_ms,omitempty"`
	AvgSpeed        float64       `json:"avg_speed,omitempty"`
	MoveDestination string        `json:"move_destination,omitempty"`
	MoveFromState   JobState      `json:"move_from_state,omitempty"`
	// Note is a transient remark about the attempt in progress. It is never
	// persisted and only meaningful while the job is being sent.
	Note string `json:"-"`

	// Resume state of a partially uploaded big file: the file ID Telegram
	// knows it by, how many leading parts are confirmed, and when the newest
	// one was. Telegram only keeps parts for a limited time; a resumed upload
	// finds out when it submits and then sends the missing ones again.
	UploadFileID   int64      `json:"upload_file_id,omitempty"`
	UploadParts    int        `json:"upload_parts,omitempty"`
	UploadPartSize int        `json:"upload_part_size,omitempty"`
	UploadPartsAt  *time.Time `json:"upload_parts_at,omitempty"`

	// GroupID ties videos that are posted together as one album (a Telegram
	// media group). Empty means the video is posted as its own message.
	GroupID string `json:"group_id,omitempty"`
	// Staged is set once an album member's data is on Telegram, attached to
	// the channel but not posted yet. The album is posted when every runnable
	// member of the group is staged. It is replaced, never modified in place.
	Staged *StagedMedia `json:"staged,omitempty"`

	// CustomCaption overrides the default filename-derived caption when
	// CaptionCustom is true. CaptionCustom distinguishes an explicit empty
	// caption from the default filename behavior.
	CustomCaption string `json:"custom_caption,omitempty"`
	CaptionCustom bool   `json:"caption_custom,omitempty"`
}

// MaxAlbumSize is the most items Telegram accepts in one media group.
const MaxAlbumSize = 10

// MaxCaptionUTF16 is Telegram's maximum caption length measured in UTF-16
// code units.
const MaxCaptionUTF16 = 1024

// StagedMedia identifies a document uploaded with messages.uploadMedia: it is
// on Telegram's servers but no message refers to it yet.
type StagedMedia struct {
	DocumentID    int64  `json:"document_id"`
	AccessHash    int64  `json:"access_hash"`
	FileReference []byte `json:"file_reference,omitempty"`
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

// LogEntry is one user-facing upload or move activity record shown in the
// in-app Upload Log panel.
type LogEntry struct {
	ID        string  `json:"id"`
	TimeMs    int64   `json:"timeMs"`
	Kind      string  `json:"kind"`
	JobID     string  `json:"jobId,omitempty"`
	Name      string  `json:"name,omitempty"`
	Size      int64   `json:"size,omitempty"`
	SentBytes int64   `json:"sentBytes,omitempty"`
	ElapsedMs int64   `json:"elapsedMs,omitempty"`
	AvgSpeed  float64 `json:"avgSpeed,omitempty"`
	Summary   string  `json:"summary"`
}
