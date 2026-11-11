package telegram

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	// UploadPartSize is the size of every part but the last.
	UploadPartSize = uploadPartSize
	// bigFileThreshold is where Telegram switches to upload.saveBigFilePart.
	// Only those uploads can be resumed; smaller files finish in seconds.
	bigFileThreshold = 10 << 20
	// ResumeWindow is how long a checkpoint is trusted. Telegram keeps
	// uploaded parts "between several minutes and several hours"; an older
	// checkpoint would upload the rest only to learn at the very end that the
	// first parts are gone, so after this window the upload starts over.
	ResumeWindow = 30 * time.Minute
)

// ResumeToken identifies a partially uploaded big file on Telegram's side:
// the file ID chosen for it and how many leading parts were confirmed. The
// zero value means "start a new upload".
type ResumeToken struct {
	FileID int64
	// Parts is the number of parts confirmed without a gap from the start.
	Parts int
	// PartSize and Size guard against resuming with a different layout.
	PartSize int
	Size     int64
	// At is when the newest part was confirmed.
	At time.Time
}

// Fresh reports whether the token can still continue an upload of a file of
// size bytes with this client's part size.
func (t ResumeToken) Fresh(size int64, now time.Time) bool {
	return t.usable(size, uploadPartSize, now)
}

// ResumedBytes is how much of the file the token already covers.
func (t ResumeToken) ResumedBytes() int64 {
	return min(int64(t.Parts)*int64(t.PartSize), t.Size)
}

// usable reports whether the token can continue an upload of size bytes.
func (t ResumeToken) usable(size int64, partSize int, now time.Time) bool {
	return t.FileID != 0 && t.Parts > 0 && t.PartSize == partSize && t.Size == size &&
		int64(t.Parts)*int64(partSize) < size+int64(partSize) &&
		!t.At.IsZero() && now.Sub(t.At) < ResumeWindow
}

// bigPartSaver is the one RPC the resumable uploader needs.
type bigPartSaver interface {
	UploadSaveBigFilePart(ctx context.Context, request *tg.UploadSaveBigFilePartRequest) (bool, error)
}

// resumableUpload transfers a big file part by part with a caller-supplied
// file ID, which is what makes continuing later possible: gotd's uploader
// always picks a fresh ID and starts at part zero.
type resumableUpload struct {
	api      bigPartSaver
	file     io.ReaderAt
	name     string
	size     int64
	partSize int
	threads  int
	limiter  *RateLimiter
	// progress receives the confirmed byte count (it may arrive out of order).
	progress func(ctx context.Context, state uploader.ProgressState) error
	// checkpoint receives every advance of the gap-free prefix.
	checkpoint func(ResumeToken)
	now        func() time.Time
}

func newFileID() (int64, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(raw[:])), nil
}

// run uploads every part that token does not already cover and returns the
// input file for messages.sendMedia.
func (u *resumableUpload) run(ctx context.Context, token ResumeToken) (tg.InputFileClass, error) {
	if u.now == nil {
		u.now = time.Now
	}
	totalParts := int((u.size + int64(u.partSize) - 1) / int64(u.partSize))
	fileID, firstPart := token.FileID, token.Parts
	if !token.usable(u.size, u.partSize, u.now()) {
		id, err := newFileID()
		if err != nil {
			return nil, fmt.Errorf("生成文件标识失败：%w", err)
		}
		fileID, firstPart = id, 0
	}
	firstPart = min(firstPart, totalParts)

	partBytes := func(part int) int64 {
		if part == totalParts-1 {
			return u.size - int64(part)*int64(u.partSize)
		}
		return int64(u.partSize)
	}
	var (
		mu        sync.Mutex
		done      = make([]bool, totalParts)
		prefix    = firstPart
		confirmed = min(int64(firstPart)*int64(u.partSize), u.size)
	)
	if u.progress != nil && confirmed > 0 {
		// Show the resumed position at once instead of starting from zero.
		_ = u.progress(ctx, uploader.ProgressState{Uploaded: confirmed, Total: u.size})
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	parts := make(chan int)
	var (
		wg       sync.WaitGroup
		failOnce sync.Once
		failure  error
	)
	fail := func(err error) {
		failOnce.Do(func() {
			failure = err
			cancel()
		})
	}
	// Buffers are reused per worker: memory stays at threads × part size.
	for worker := 0; worker < max(u.threads, 1); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buffer := make([]byte, u.partSize)
			for part := range parts {
				length := int(partBytes(part))
				if _, err := u.file.ReadAt(buffer[:length], int64(part)*int64(u.partSize)); err != nil && !(errors.Is(err, io.EOF) && part == totalParts-1) {
					fail(fmt.Errorf("读取视频文件失败：%w", err))
					return
				}
				if err := u.pace(ctx, length); err != nil {
					fail(err)
					return
				}
				if err := u.savePart(ctx, fileID, part, totalParts, buffer[:length]); err != nil {
					fail(err)
					return
				}
				mu.Lock()
				done[part] = true
				confirmed += int64(length)
				uploaded := confirmed
				advanced := false
				for prefix < totalParts && done[prefix] {
					prefix++
					advanced = true
				}
				resume := ResumeToken{FileID: fileID, Parts: prefix, PartSize: u.partSize, Size: u.size, At: u.now()}
				mu.Unlock()
				if advanced && u.checkpoint != nil {
					u.checkpoint(resume)
				}
				if u.progress != nil {
					if err := u.progress(ctx, uploader.ProgressState{Uploaded: uploaded, Total: u.size}); err != nil {
						fail(err)
						return
					}
				}
			}
		}()
	}
feed:
	for part := firstPart; part < totalParts; part++ {
		select {
		case parts <- part:
		case <-ctx.Done():
			break feed
		}
	}
	close(parts)
	wg.Wait()
	if failure != nil {
		return nil, failure
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &tg.InputFileBig{ID: fileID, Parts: totalParts, Name: u.name}, nil
}

// pace spends the rate-limit allowance in small slices so several workers
// share it smoothly.
func (u *resumableUpload) pace(ctx context.Context, length int) error {
	for length > 0 {
		chunk := min(length, rateLimitChunk)
		if err := u.limiter.Wait(ctx, chunk); err != nil {
			return err
		}
		length -= chunk
	}
	return nil
}

func (u *resumableUpload) savePart(ctx context.Context, fileID int64, part, totalParts int, data []byte) error {
	for {
		saved, err := u.api.UploadSaveBigFilePart(ctx, &tg.UploadSaveBigFilePartRequest{
			FileID:         fileID,
			FilePart:       part,
			FileTotalParts: totalParts,
			Bytes:          data,
		})
		if flood, floodErr := tgerr.FloodWait(ctx, err); floodErr != nil {
			if flood {
				continue
			}
			return fmt.Errorf("上传分片 %d 失败：%w", part, floodErr)
		}
		// A false result means the part was not stored; send it again.
		if saved {
			return nil
		}
	}
}

// isMissingPartsError reports whether Telegram rejected the final request
// because uploaded parts are gone or inconsistent. That rejection happens
// before any message is created, so re-uploading cannot duplicate a post.
func isMissingPartsError(err error) bool {
	rpcErr, ok := tgerr.As(err)
	if !ok {
		return false
	}
	return strings.HasPrefix(rpcErr.Type, "FILE_PART") || strings.HasPrefix(rpcErr.Message, "FILE_PART")
}
