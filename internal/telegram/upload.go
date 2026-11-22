package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/Jaaayden/telegram-uploader/internal/model"
)

const (
	// Telegram recommends 512 KiB parts to reduce protocol overhead. This is
	// also the protocol maximum, so throughput profiles vary concurrency rather
	// than attempting to increase the part size.
	uploadPartSize                    = uploader.MaximumPartSize
	uploadConnectionPoolMaximum int64 = UploadConcurrencyFast
	progressUpdateInterval            = 100 * time.Millisecond
	// speedWindow is how far back the displayed transfer rate looks. A longer
	// window is smoother; a lifetime average would hide slowdowns and speedups.
	speedWindow = 5 * time.Second
)

type speedSample struct {
	at    time.Time
	bytes int64
}

type uploadProgressReporter struct {
	mu         sync.Mutex
	started    time.Time
	lastUpdate time.Time
	lastBytes  int64
	interval   time.Duration
	callback   func(model.Progress)
	pending    model.Progress
	hasPending bool
	closed     bool
	wake       chan struct{}
	stop       chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
	now        func() time.Time
	samples    []speedSample
}

func newUploadProgressReporter(callback func(model.Progress)) *uploadProgressReporter {
	p := &uploadProgressReporter{
		now:      time.Now,
		started:  time.Now(),
		interval: progressUpdateInterval,
		callback: callback,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go p.dispatch()
	return p
}

// startAt tells the reporter that an upload resumes with bytes already on the
// server, so they are not counted as transferred during this attempt.
func (p *uploadProgressReporter) startAt(bytes int64) {
	p.mu.Lock()
	p.lastBytes = bytes
	p.samples = append(p.samples[:0], speedSample{at: p.started, bytes: bytes})
	p.mu.Unlock()
}

func (p *uploadProgressReporter) Chunk(ctx context.Context, state uploader.ProgressState) error {
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return err
	}
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	if state.Uploaded < p.lastBytes {
		p.mu.Unlock()
		return nil
	}
	p.lastBytes = state.Uploaded
	now := p.now()
	complete := state.Total > 0 && state.Uploaded >= state.Total
	if !complete && !p.lastUpdate.IsZero() && now.Sub(p.lastUpdate) < p.interval {
		p.mu.Unlock()
		return nil
	}
	p.lastUpdate = now
	if p.callback == nil {
		p.mu.Unlock()
		return nil
	}
	speed := p.recordSample(now, state.Uploaded)
	p.pending = model.Progress{
		BytesDone:      state.Uploaded,
		BytesTotal:     state.Total,
		BytesPerSecond: speed,
		At:             now,
	}
	p.hasPending = true
	p.mu.Unlock()

	// gotd invokes Chunk synchronously from every upload worker. Only signal a
	// single-slot dispatcher here; rendering and durable queue writes must never
	// consume a network worker or make other workers wait behind a callback.
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}

// recordSample adds a point and returns the rate over the last speedWindow.
// With a single sample it falls back to the average since the upload started.
// The caller must hold p.mu.
func (p *uploadProgressReporter) recordSample(at time.Time, uploaded int64) float64 {
	p.samples = append(p.samples, speedSample{at: at, bytes: uploaded})
	cutoff := at.Add(-speedWindow)
	for len(p.samples) > 2 && p.samples[1].at.Before(cutoff) {
		p.samples = p.samples[1:]
	}
	base := speedSample{at: p.started}
	if len(p.samples) > 1 {
		base = p.samples[0]
	}
	elapsed := at.Sub(base.at).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(uploaded-base.bytes) / elapsed
}

func (p *uploadProgressReporter) dispatch() {
	defer close(p.done)
	for {
		select {
		case <-p.wake:
			p.deliverLatest()
		case <-p.stop:
			p.deliverLatest()
			return
		}
	}
}

func (p *uploadProgressReporter) deliverLatest() {
	for {
		p.mu.Lock()
		if !p.hasPending {
			p.mu.Unlock()
			return
		}
		progress := p.pending
		p.hasPending = false
		callback := p.callback
		p.mu.Unlock()
		if callback != nil {
			callback(progress)
		}
	}
}

// Close flushes the latest coalesced progress and waits until no callback can
// outlive UploadVideo. In the successful path this is the boundary that makes
// the durable JobSending transition complete before messages.sendMedia.
func (p *uploadProgressReporter) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.stop)
	})
	<-p.done
}

func newUploadEngine(api uploader.Client, progress uploader.Progress, threads int) *uploader.Uploader {
	threads = normalizeUploadConcurrency(threads)
	return uploader.NewUploader(api).
		WithPartSize(uploadPartSize).
		WithThreads(threads).
		WithProgress(progress)
}

func (c *Client) UploadVideo(ctx context.Context, request UploadRequest, onProgress func(model.Progress)) (int, error) {
	var messageID int
	err := c.withUploadedVideo(ctx, request, onProgress, func(file *uploadedVideo) error {
		if file.request.BeforeSend != nil {
			if err := file.request.BeforeSend(); err != nil {
				return err
			}
		}
		// Cancellation can race with a durable BeforeSend callback. Re-check at
		// the exact submission boundary so a completed local fsync never causes
		// a deliberately cancelled job to enter messages.sendMedia.
		if err := file.ctx.Err(); err != nil {
			return err
		}
		submit := func() (tg.UpdatesClass, error) {
			return message.NewSender(file.api).
				WithUploader(file.engine).
				To(file.peer()).
				RandomID(file.request.RandomID).
				Media(file.ctx, file.media)
		}
		updatesResult, err := submitUploaded(c, file, submit)
		if err != nil {
			var submitted *submitError
			if !errors.As(err, &submitted) {
				return err
			}
			return fmt.Errorf("%w：提交频道消息失败：%w", ErrSendOutcomeUnknown, submitted.err)
		}
		id, err := unpack.MessageID(updatesResult, nil)
		if err != nil {
			return fmt.Errorf("%w：消息已发送但无法读取消息 ID：%w", ErrSendOutcomeUnknown, err)
		}
		messageID = id
		return nil
	})
	if err != nil {
		return 0, err
	}
	return messageID, nil
}

// StageVideo uploads a video and attaches it to the channel with
// messages.uploadMedia, without posting it. The result goes into SendAlbum
// later. No channel message exists until then, so every failure here is
// safe to retry.
func (c *Client) StageVideo(ctx context.Context, request UploadRequest, onProgress func(model.Progress)) (model.StagedMedia, error) {
	var staged model.StagedMedia
	err := c.withUploadedVideo(ctx, request, onProgress, func(file *uploadedVideo) error {
		submit := func() (tg.MessageMediaClass, error) {
			return message.NewSender(file.api).
				WithUploader(file.engine).
				To(file.peer()).
				UploadMedia(file.ctx, file.media)
		}
		result, err := submitUploaded(c, file, submit)
		if err != nil {
			var submitted *submitError
			if !errors.As(err, &submitted) {
				return err
			}
			if ctxErr := file.ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// messages.uploadMedia never creates a message: retrying is safe.
			return fmt.Errorf("%w：Telegram 未能保存上传的视频：%w", ErrUploadData, submitted.err)
		}
		staged, err = stagedFromMedia(result)
		if err != nil {
			return fmt.Errorf("%w：%w", ErrUploadData, err)
		}
		return nil
	})
	if err != nil {
		return model.StagedMedia{}, err
	}
	return staged, nil
}

func stagedFromMedia(media tg.MessageMediaClass) (model.StagedMedia, error) {
	documentMedia, ok := media.(*tg.MessageMediaDocument)
	if !ok {
		return model.StagedMedia{}, fmt.Errorf("Telegram 返回了意外的媒体类型 %T", media)
	}
	document, ok := documentMedia.Document.(*tg.Document)
	if !ok {
		return model.StagedMedia{}, errors.New("Telegram 没有返回上传后的视频文件")
	}
	return model.StagedMedia{
		DocumentID:    document.ID,
		AccessHash:    document.AccessHash,
		FileReference: append([]byte(nil), document.FileReference...),
	}, nil
}

// submitError is an error returned by the final Telegram request itself
// rather than by the preparation around it.
type submitError struct{ err error }

func (e *submitError) Error() string { return e.err.Error() }
func (e *submitError) Unwrap() error { return e.err }

// uploadedVideo is a video whose parts Telegram has confirmed. The final
// request (sendMedia or uploadMedia) is up to the caller.
type uploadedVideo struct {
	ctx     context.Context
	api     *tg.Client
	engine  *uploader.Uploader
	request UploadRequest
	media   *message.VideoDocumentBuilder
	// repair sends parts of an earlier session again under the same file ID;
	// nil when this attempt did not continue an earlier one.
	repair func() error
}

func (v *uploadedVideo) peer() tg.InputPeerClass {
	return &tg.InputPeerChannel{ChannelID: v.request.Channel.ID, AccessHash: v.request.Channel.AccessHash}
}

// submit runs the final request. When Telegram answers that parts of an
// earlier session are missing (which means nothing was created), it sends
// them again under the same file ID and tries once more. An error that
// remains after that and is not about missing parts comes from the request
// itself; the caller decides what it means.
func submitUploaded[T any](c *Client, v *uploadedVideo, request func() (T, error)) (T, error) {
	var zero T
	if err := c.reserveSendSlot(v.ctx); err != nil {
		return zero, err
	}
	result, err := request()
	if isMissingPartsError(err) && v.repair != nil {
		if repairErr := v.repair(); repairErr != nil {
			if ctxErr := v.ctx.Err(); ctxErr != nil {
				// Stopped while restoring parts; no submission is in flight.
				return zero, ctxErr
			}
			return zero, fmt.Errorf("%w：补传被 Telegram 清理的分片失败：%w", ErrUploadData, repairErr)
		}
		if err := c.reserveSendSlot(v.ctx); err != nil {
			return zero, err
		}
		result, err = request()
	}
	if isMissingPartsError(err) {
		// Still missing (or nothing to repair with): nothing was created.
		// Forget the checkpoint so the retry uploads the whole file afresh.
		if v.request.OnCheckpoint != nil {
			v.request.OnCheckpoint(ResumeToken{})
		}
		return zero, fmt.Errorf("%w：Telegram 已清理之前上传的分片，需要重新上传：%w", ErrUploadData, err)
	}
	if err != nil {
		return zero, &submitError{err: err}
	}
	return result, nil
}

// withUploadedVideo validates the request, transfers the file parts and hands
// the confirmed upload to finish, all inside one admitted request.
func (c *Client) withUploadedVideo(ctx context.Context, request UploadRequest, onProgress func(model.Progress), finish func(*uploadedVideo) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.RandomID == 0 {
		return errors.New("random_id 不能为空")
	}
	requestCtx, release, err := c.beginRequest(ctx)
	if err != nil {
		return err
	}
	defer release()
	// Snapshot the profile at the request boundary. A setting change while this
	// video is validating or uploading must apply to the next video, matching
	// the UI contract.
	uploadConcurrency := c.currentUploadConcurrency()
	api, err := c.connectedAPI()
	if err != nil {
		return err
	}
	uploadAPI, err := c.connectedUploadAPI()
	if err != nil {
		return err
	}
	validatedChannel, err := c.validateChannel(requestCtx, request.Channel)
	if err != nil {
		return err
	}
	request.Channel = validatedChannel
	var info os.FileInfo
	if request.File != nil {
		info, err = request.File.Stat()
	} else {
		info, err = os.Stat(request.Path)
	}
	if err != nil {
		return fmt.Errorf("读取视频文件失败：%w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("视频路径不是普通文件")
	}
	c.mu.RLock()
	maxUploadBytes := c.identity.MaxUploadBytes
	c.mu.RUnlock()
	if maxUploadBytes > 0 && info.Size() > maxUploadBytes {
		return fmt.Errorf("视频大小 %d 字节超过当前 Bot 上传上限 %d 字节", info.Size(), maxUploadBytes)
	}
	request = prepareUploadRequest(request)

	select {
	case c.uploadSem <- struct{}{}:
		defer func() { <-c.uploadSem }()
	case <-requestCtx.Done():
		return requestCtx.Err()
	}

	progress := newUploadProgressReporter(onProgress)
	uploadEngine := newUploadEngine(uploadAPI, progress, uploadConcurrency)
	var inputFile tg.InputFileClass
	// Set when this attempt continued an earlier one: how to send the earlier
	// parts again should Telegram turn out to have dropped them.
	var repair func() error
	if request.File != nil && info.Size() > bigFileThreshold {
		// Big files go through the resumable path so a pause or a dropped
		// connection continues where it stopped.
		resumedParts := 0
		if request.Resume.usable(info.Size(), uploadPartSize) {
			resumedParts = request.Resume.Parts
			progress.startAt(request.Resume.ResumedBytes())
		}
		big := &resumableUpload{
			api:        uploadAPI,
			file:       request.File,
			name:       info.Name(),
			size:       info.Size(),
			partSize:   uploadPartSize,
			threads:    uploadConcurrency,
			limiter:    c.cfg.RateLimiter,
			progress:   progress.Chunk,
			checkpoint: request.OnCheckpoint,
		}
		var fileID int64
		inputFile, fileID, err = big.run(requestCtx, request.Resume)
		if resumedParts > 0 {
			repair = func() error { return big.repair(requestCtx, fileID, resumedParts, request.OnRepair) }
		}
	} else {
		inputFile, err = c.uploadFileData(requestCtx, uploadEngine, request, info)
	}
	progress.Close()
	if err != nil {
		return fmt.Errorf("%w：上传视频数据失败：%w", ErrUploadData, err)
	}
	if err := requestCtx.Err(); err != nil {
		return err
	}

	document := message.UploadedDocument(inputFile, styling.Plain(request.Caption)).
		Filename(request.Name).
		MIME("video/mp4")
	video := document.Video().
		Duration(time.Duration(request.Metadata.DurationSeconds)*time.Second).
		Resolution(request.Metadata.Width, request.Metadata.Height)
	if request.Metadata.SupportsStreaming {
		video.SupportsStreaming()
	}
	return finish(&uploadedVideo{
		ctx:     requestCtx,
		api:     api,
		engine:  uploadEngine,
		request: request,
		media:   video,
		repair:  repair,
	})
}

// uploadFileData transfers the file parts. With a rate limiter configured the
// file is read through a pacing reader; otherwise gotd reads it directly.
func (c *Client) uploadFileData(ctx context.Context, engine *uploader.Uploader, request UploadRequest, info os.FileInfo) (tg.InputFileClass, error) {
	limiter := c.cfg.RateLimiter
	if limiter == nil {
		if request.File != nil {
			return engine.FromFile(ctx, request.File)
		}
		return engine.FromPath(ctx, request.Path)
	}
	file := request.File
	if file == nil {
		opened, err := os.Open(filepath.Clean(request.Path))
		if err != nil {
			return nil, fmt.Errorf("打开视频文件失败：%w", err)
		}
		defer opened.Close()
		file = opened
	}
	reader := &throttledReader{ctx: ctx, reader: file, limiter: limiter}
	return engine.Upload(ctx, uploader.NewUpload(info.Name(), reader, info.Size()))
}

// prepareUploadRequest fills only the Telegram document filename. Caption is
// intentionally left untouched because an empty caption is a valid explicit
// choice (for example, a source file whose complete name is ".mp4").
func prepareUploadRequest(request UploadRequest) UploadRequest {
	if request.Name == "" {
		request.Name = filepath.Base(request.Path)
	}
	return request
}

func (c *Client) reserveSendSlot(ctx context.Context) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	wait := time.Until(c.lastSend.Add(time.Second))
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.lastSend = time.Now()
	return nil
}
