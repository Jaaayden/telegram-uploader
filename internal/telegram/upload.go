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

	"github.com/jayden/telegram-video-uploader/internal/model"
)

const (
	// Telegram recommends 512 KiB parts to reduce protocol overhead. This is
	// also the protocol maximum, so throughput profiles vary concurrency rather
	// than attempting to increase the part size.
	uploadPartSize                    = uploader.MaximumPartSize
	uploadConnectionPoolMaximum int64 = UploadConcurrencyFast
	progressUpdateInterval            = 100 * time.Millisecond
)

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
}

func newUploadProgressReporter(callback func(model.Progress)) *uploadProgressReporter {
	p := &uploadProgressReporter{
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
	now := time.Now()
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
	elapsed := now.Sub(p.started).Seconds()
	var speed float64
	if elapsed > 0 {
		speed = float64(state.Uploaded) / elapsed
	}
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
	if ctx == nil {
		ctx = context.Background()
	}
	if request.RandomID == 0 {
		return 0, errors.New("random_id 不能为空")
	}
	requestCtx, release, err := c.beginRequest(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	// Snapshot the profile at the request boundary. A setting change while this
	// video is validating or uploading must apply to the next video, matching
	// the UI contract.
	uploadConcurrency := c.currentUploadConcurrency()
	api, err := c.connectedAPI()
	if err != nil {
		return 0, err
	}
	uploadAPI, err := c.connectedUploadAPI()
	if err != nil {
		return 0, err
	}
	validatedChannel, err := c.validateChannel(requestCtx, request.Channel)
	if err != nil {
		return 0, err
	}
	request.Channel = validatedChannel
	var info os.FileInfo
	if request.File != nil {
		info, err = request.File.Stat()
	} else {
		info, err = os.Stat(request.Path)
	}
	if err != nil {
		return 0, fmt.Errorf("读取视频文件失败：%w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("视频路径不是普通文件")
	}
	c.mu.RLock()
	maxUploadBytes := c.identity.MaxUploadBytes
	c.mu.RUnlock()
	if maxUploadBytes > 0 && info.Size() > maxUploadBytes {
		return 0, fmt.Errorf("视频大小 %d 字节超过当前 Bot 上传上限 %d 字节", info.Size(), maxUploadBytes)
	}
	request = prepareUploadRequest(request)

	select {
	case c.uploadSem <- struct{}{}:
		defer func() { <-c.uploadSem }()
	case <-requestCtx.Done():
		return 0, requestCtx.Err()
	}

	progress := newUploadProgressReporter(onProgress)
	uploadEngine := newUploadEngine(uploadAPI, progress, uploadConcurrency)
	var inputFile tg.InputFileClass
	if request.File != nil {
		inputFile, err = uploadEngine.FromFile(requestCtx, request.File)
	} else {
		inputFile, err = uploadEngine.FromPath(requestCtx, request.Path)
	}
	progress.Close()
	if err != nil {
		return 0, fmt.Errorf("%w：上传视频数据失败：%w", ErrUploadData, err)
	}
	if err := requestCtx.Err(); err != nil {
		return 0, err
	}
	if request.BeforeSend != nil {
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
	}
	// Cancellation can race with a durable BeforeSend callback. Re-check at the
	// exact submission boundary so a completed local fsync never causes a
	// deliberately cancelled job to enter messages.sendMedia.
	if err := requestCtx.Err(); err != nil {
		return 0, err
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

	if err := c.reserveSendSlot(requestCtx); err != nil {
		return 0, err
	}
	updatesResult, err := message.NewSender(api).
		WithUploader(uploadEngine).
		To(&tg.InputPeerChannel{ChannelID: request.Channel.ID, AccessHash: request.Channel.AccessHash}).
		RandomID(request.RandomID).
		Media(requestCtx, video)
	if err != nil {
		return 0, fmt.Errorf("%w：提交频道消息失败：%w", ErrSendOutcomeUnknown, err)
	}
	messageID, err := unpack.MessageID(updatesResult, nil)
	if err != nil {
		return 0, fmt.Errorf("%w：消息已发送但无法读取消息 ID：%w", ErrSendOutcomeUnknown, err)
	}
	return messageID, nil
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
