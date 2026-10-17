package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/model"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

func waitForUploadRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Controller) uploadJobWithRetry(
	ctx context.Context,
	job model.Job,
	channel model.Channel,
	cancel context.CancelFunc,
) (int, error) {
	for retryIndex := 0; ; retryIndex++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if c.queuePaused() {
			return 0, errQueuePaused
		}
		if retryIndex == 0 && c.beforeUploadAttempt != nil {
			c.beforeUploadAttempt(job)
		}

		// Reopen and revalidate the source for every attempt. Telegram uploads
		// cannot resume a partially transferred file, and reusing an os.File at
		// EOF would make the next attempt incomplete.
		file, metadata, err := openVerifiedJob(job)
		if err != nil {
			return 0, err
		}
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return 0, err
		}

		now := time.Now()
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			_ = file.Close()
			return 0, err
		}
		// Pause may have been requested after the file check. Re-check while
		// holding the same mutex used by Pause so no new request starts between
		// files or retry attempts.
		if c.paused || c.pauseRequested {
			c.mu.Unlock()
			_ = file.Close()
			return 0, errQueuePaused
		}
		currentIndex := findJobIndexLocked(c.jobs, job.ID)
		if currentIndex < 0 {
			c.mu.Unlock()
			_ = file.Close()
			return 0, errQueueJobNotRunnable
		}
		current := &c.jobs[currentIndex]
		validState := retryIndex == 0 && (current.State == model.JobQueued || current.State == model.JobInterrupted) ||
			retryIndex > 0 && (current.State == model.JobUploading || current.State == model.JobSending)
		if !validState {
			if current.State == model.JobCancelled {
				c.mu.Unlock()
				_ = file.Close()
				return 0, errQueueJobCancelled
			}
			if retryIndex == 0 {
				c.mu.Unlock()
				_ = file.Close()
				return 0, errQueueJobNotRunnable
			}
			c.mu.Unlock()
			_ = file.Close()
			return 0, fmt.Errorf("任务状态已变为 %s，停止上传", current.State)
		}
		current.Metadata = metadata
		current.State = model.JobUploading
		current.Uploaded = 0
		current.BytesPerSecond = 0
		if retryIndex == 0 {
			current.StartedAt = &now
		}
		current.Error = ""
		c.activeID = job.ID
		c.activeCancel = cancel
		c.attemptRevision++
		attemptID := c.attemptRevision
		c.activeAttempt = attemptID
		c.retryWaitActive = false
		c.queueRevision++
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			_ = file.Close()
			return 0, fmt.Errorf("%w：保存上传状态失败：%v", errQueuePersistenceLost, err)
		}
		// Persist can overlap a Pause or CancelAll call. Treat this locked check
		// as the attempt's start boundary: an intent recorded before it prevents
		// a new Telegram request; an intent recorded afterwards applies the
		// existing soft-pause/cancel semantics to an already-started attempt.
		c.mu.Lock()
		ctxErr := ctx.Err()
		paused := c.paused || c.pauseRequested
		if ctxErr != nil || paused {
			if c.activeID == job.ID && c.activeAttempt == attemptID {
				c.activeAttempt = 0
			}
		}
		c.mu.Unlock()
		if ctxErr != nil {
			_ = file.Close()
			return 0, ctxErr
		}
		if paused {
			_ = file.Close()
			return 0, errQueuePaused
		}

		gateway := c.currentGateway()
		if gateway == nil {
			err = tgtransport.ErrNotConnected
		} else {
			messageID, uploadErr := gateway.UploadVideo(ctx, tgtransport.UploadRequest{
				Channel:  channel,
				Path:     job.Path,
				File:     file,
				Name:     job.Name,
				Caption:  captionFromFilename(job.Name),
				RandomID: job.RandomID,
				Metadata: metadata,
				BeforeSend: func() error {
					return c.prepareSendForAttempt(job.ID, attemptID)
				},
			}, func(progress model.Progress) {
				c.applyProgressForAttempt(job.ID, attemptID, progress)
			})
			err = uploadErr
			if err == nil {
				// A read-only file close failure after Telegram confirmed the send
				// must not turn a successful message into a retryable failure.
				_ = file.Close()
				return messageID, nil
			}
		}
		_ = file.Close()
		c.mu.Lock()
		if c.activeID == job.ID && c.activeAttempt == attemptID {
			c.activeAttempt = 0
		}
		c.mu.Unlock()

		if !isRetryableUploadError(ctx, err) {
			return 0, err
		}
		if retryIndex >= len(c.uploadRetryDelays) {
			if len(c.uploadRetryDelays) == 0 {
				return 0, err
			}
			return 0, fmt.Errorf("自动重试 %d 次后仍失败：%w", len(c.uploadRetryDelays), err)
		}

		delay := c.uploadRetryDelays[retryIndex]
		if err := c.waitBeforeUploadRetry(ctx, job.ID, err, delay, retryIndex+1); err != nil {
			return 0, err
		}
	}
}

func (c *Controller) waitBeforeUploadRetry(
	ctx context.Context,
	jobID string,
	uploadErr error,
	delay time.Duration,
	retryNumber int,
) error {
	retryMessage := fmt.Sprintf(
		"连接中断，%s 后自动重试（%d/%d）：%v",
		formatRetryDelay(delay),
		retryNumber,
		len(c.uploadRetryDelays),
		uploadErr,
	)

	c.mu.Lock()
	if c.paused || c.pauseRequested {
		c.mu.Unlock()
		return errQueuePaused
	}
	if findJobIndexLocked(c.jobs, jobID) < 0 {
		c.mu.Unlock()
		return errors.New("上传队列在重试期间发生了变化")
	}
	current := findJobIndexLocked(c.jobs, jobID)
	c.jobs[current].State = model.JobUploading
	c.jobs[current].Uploaded = 0
	c.jobs[current].BytesPerSecond = 0
	c.jobs[current].Error = retryMessage
	c.queueRevision++
	c.retryWaitActive = true
	c.notifyLocked()
	c.mu.Unlock()

	if err := c.persist(); err != nil {
		c.mu.Lock()
		c.retryWaitActive = false
		c.mu.Unlock()
		return fmt.Errorf("%w：保存重试状态失败：%v", errQueuePersistenceLost, err)
	}

	wait := c.uploadRetryWait
	if wait == nil {
		wait = waitForUploadRetry
	}
	waitErr := wait(ctx, delay)
	c.mu.Lock()
	c.retryWaitActive = false
	paused := c.paused || c.pauseRequested
	c.mu.Unlock()
	if waitErr != nil {
		return waitErr
	}
	if paused {
		return errQueuePaused
	}
	return nil
}

func formatRetryDelay(delay time.Duration) string {
	if delay%time.Second == 0 {
		return fmt.Sprintf("%d 秒", int(delay/time.Second))
	}
	return delay.String()
}

func isRetryableUploadError(ctx context.Context, err error) bool {
	if err == nil || errors.Is(err, tgtransport.ErrSendOutcomeUnknown) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		// A caller cancellation is deliberate. A transport-internal
		// cancellation while our job context is still live is safe to retry
		// because UploadVideo wraps every messages.sendMedia error as an
		// unknown outcome before it reaches this controller.
		return ctx.Err() == nil
	}
	if errors.Is(err, tgtransport.ErrNotConnected) || errors.Is(err, tgtransport.ErrUploadData) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}
