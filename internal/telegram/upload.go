package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/jayden/telegram-video-uploader/internal/model"
)

type progressFunc func(context.Context, uploader.ProgressState) error

func (f progressFunc) Chunk(ctx context.Context, state uploader.ProgressState) error {
	return f(ctx, state)
}

func (c *Client) UploadVideo(ctx context.Context, request UploadRequest, onProgress func(model.Progress)) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.RandomID == 0 {
		return 0, errors.New("random_id 不能为空")
	}
	api, err := c.connectedAPI()
	if err != nil {
		return 0, err
	}
	validatedChannel, err := c.ValidateChannel(ctx, request.Channel)
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
	if request.Name == "" {
		request.Name = filepath.Base(request.Path)
	}
	if request.Caption == "" {
		request.Caption = request.Name
	}

	select {
	case c.uploadSem <- struct{}{}:
		defer func() { <-c.uploadSem }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	started := time.Now()
	progress := progressFunc(func(progressCtx context.Context, state uploader.ProgressState) error {
		if err := progressCtx.Err(); err != nil {
			return err
		}
		if onProgress != nil {
			elapsed := time.Since(started).Seconds()
			var speed float64
			if elapsed > 0 {
				speed = float64(state.Uploaded) / elapsed
			}
			onProgress(model.Progress{
				BytesDone:      state.Uploaded,
				BytesTotal:     state.Total,
				BytesPerSecond: speed,
				At:             time.Now(),
			})
		}
		return nil
	})
	uploadEngine := uploader.NewUploader(api).WithThreads(4).WithProgress(progress)
	var inputFile tg.InputFileClass
	if request.File != nil {
		inputFile, err = uploadEngine.FromFile(ctx, request.File)
	} else {
		inputFile, err = uploadEngine.FromPath(ctx, request.Path)
	}
	if err != nil {
		return 0, fmt.Errorf("上传视频数据失败：%w", err)
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

	if err := c.reserveSendSlot(ctx); err != nil {
		return 0, err
	}
	updatesResult, err := message.NewSender(api).
		WithUploader(uploadEngine).
		To(&tg.InputPeerChannel{ChannelID: request.Channel.ID, AccessHash: request.Channel.AccessHash}).
		RandomID(request.RandomID).
		Media(ctx, video)
	if err != nil {
		return 0, fmt.Errorf("%w：提交频道消息失败：%w", ErrSendOutcomeUnknown, err)
	}
	messageID, err := unpack.MessageID(updatesResult, nil)
	if err != nil {
		return 0, fmt.Errorf("%w：消息已发送但无法读取消息 ID：%w", ErrSendOutcomeUnknown, err)
	}
	return messageID, nil
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
