package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/media"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

func (c *Controller) Start(parent context.Context) error {
	if !c.opMu.TryLock() {
		return errors.New("另一个队列操作正在准备，请稍后重试")
	}
	defer c.opMu.Unlock()
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("上传队列已经在运行")
	}
	if c.gateway == nil {
		c.mu.Unlock()
		return errors.New("请先连接 Telegram Bot")
	}
	if c.channel.ID == 0 {
		c.mu.Unlock()
		return errors.New("请先绑定目标频道")
	}
	if !hasRunnableJob(c.jobs) {
		c.mu.Unlock()
		return errors.New("队列中没有待上传视频")
	}
	wasPaused := c.paused || c.pauseRequested
	if wasPaused {
		c.paused = false
		c.pauseRequested = false
		c.pauseRevision++
		c.queueRevision++
	}
	startPauseRevision := c.pauseRevision
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	c.running = true
	c.allCancel = cancel
	c.cancelJobID = ""
	c.cancelAllRequested = false
	c.lastError = ""
	c.notifyLocked()
	c.mu.Unlock()

	// Persist every random_id before the first send can happen.
	if err := c.persist(); err != nil {
		cancel()
		c.mu.Lock()
		c.running = false
		c.allCancel = nil
		if wasPaused && c.pauseRevision == startPauseRevision {
			c.paused = true
			c.pauseRevision++
			c.queueRevision++
		}
		// A pause request is transient and only meaningful while Running is
		// true. Preserve any newer paused intent, but normalize its view.
		c.pauseRequested = false
		c.lastError = err.Error()
		c.notifyLocked()
		c.mu.Unlock()
		return fmt.Errorf("保存上传队列失败：%w", err)
	}
	go c.runQueue(ctx)
	return nil
}

func hasRunnableJob(jobs []model.Job) bool {
	for _, job := range jobs {
		if job.State == model.JobQueued || job.State == model.JobInterrupted {
			return true
		}
	}
	return false
}

func openVerifiedJob(job model.Job) (*os.File, model.VideoMetadata, error) {
	pathInfo, err := os.Lstat(job.Path)
	if err != nil {
		return nil, model.VideoMetadata{}, fmt.Errorf("上传前检查失败：%w", err)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Size() != job.Size || !pathInfo.ModTime().Equal(job.ModTime) {
		return nil, model.VideoMetadata{}, errors.New("上传前检查失败：文件已被替换或修改")
	}
	file, err := os.Open(job.Path)
	if err != nil {
		return nil, model.VideoMetadata{}, fmt.Errorf("打开视频文件失败：%w", err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, model.VideoMetadata{}, fmt.Errorf("读取视频文件状态失败：%w", err)
	}
	if !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) || fileInfo.Size() != job.Size || !fileInfo.ModTime().Equal(job.ModTime) {
		_ = file.Close()
		return nil, model.VideoMetadata{}, errors.New("上传前检查失败：文件在扫描后发生了变化")
	}
	metadata, err := media.ParseMP4MetadataFile(file)
	if err != nil {
		_ = file.Close()
		return nil, model.VideoMetadata{}, err
	}
	return file, metadata, nil
}

func (c *Controller) runQueue(ctx context.Context) {
	defer func() {
		c.mu.Lock()
		c.running = false
		if c.pauseRequested {
			c.pauseRevision++
		}
		c.pauseRequested = false
		c.activeID = ""
		c.activeCancel = nil
		c.activeAttempt = 0
		c.retryWaitActive = false
		c.allCancel = nil
		c.cancelJobID = ""
		c.cancelAllRequested = false
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			c.setPersistenceError(err)
		}
	}()

	for {
		job, channel, ok := c.nextJob()
		if !ok {
			return
		}
		if err := ctx.Err(); err != nil {
			return
		}

		jobCtx, jobCancel := context.WithCancel(ctx)
		messageID, err := c.uploadJobWithRetry(jobCtx, job, channel, jobCancel)
		jobContextCancelled := jobCtx.Err() != nil

		c.mu.Lock()
		c.activeCancel = nil
		c.activeAttempt = 0
		c.retryWaitActive = false
		cancelCurrent := c.cancelJobID == job.ID
		if cancelCurrent {
			c.cancelJobID = ""
		}
		cancelAll := c.cancelAllRequested
		c.mu.Unlock()
		jobCancel()

		if err != nil {
			if errors.Is(err, errQueueJobCancelled) || errors.Is(err, errQueueJobNotRunnable) {
				c.finishActiveJob(job.ID)
				continue
			}
			if errors.Is(err, tgtransport.ErrSendOutcomeUnknown) {
				c.mu.Lock()
				if current := findJobIndexLocked(c.jobs, job.ID); current >= 0 {
					c.jobs[current].State = model.JobConfirming
					c.jobs[current].BytesPerSecond = 0
					c.jobs[current].Error = "消息可能已经送达；请先检查频道，再选择“已发送”或“重试”"
					c.lastError = c.jobs[current].Error
				}
				// An unknown outcome must stay visible for manual confirmation;
				// never delete it merely because the user clicked Remove.
				delete(c.pendingRemoval, job.ID)
				c.queueRevision++
				c.notifyLocked()
				c.mu.Unlock()
				if persistErr := c.persist(); persistErr != nil {
					c.setPersistenceError(persistErr)
				}
				return
			}
			if errors.Is(err, errQueuePaused) {
				jobContextCancelled = true
			}
			if errors.Is(err, errQueuePersistenceLost) {
				c.clearPendingRemoval(job.ID)
				_ = c.failJob(job.ID, err)
				return
			}
			if jobContextCancelled || cancelCurrent || cancelAll || ctx.Err() != nil {
				c.mu.Lock()
				current := findJobIndexLocked(c.jobs, job.ID)
				if current >= 0 {
					if cancelCurrent || cancelAll {
						c.jobs[current].State = model.JobCancelled
						c.jobs[current].Error = ""
					} else {
						c.jobs[current].State = model.JobInterrupted
						if c.paused || c.pauseRequested {
							c.jobs[current].Error = "队列已暂停，可重新开始当前任务"
						} else {
							c.jobs[current].Error = "程序关闭或连接中断，可重新开始上传"
						}
					}
					c.jobs[current].BytesPerSecond = 0
					c.queueRevision++
					c.notifyLocked()
				}
				c.mu.Unlock()
				if persistErr := c.persist(); persistErr != nil {
					c.clearPendingRemoval(job.ID)
					c.setPersistenceError(persistErr)
					c.finishActiveJob(job.ID)
					return
				} else if c.finishActiveJobUnlessPending(job.ID) {
					if removeErr := c.removeAfterTerminalPersist(job.ID); removeErr != nil {
						c.setPersistenceError(removeErr)
					}
					c.finishActiveJob(job.ID)
				}
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if persistErr := c.failJob(job.ID, err); persistErr != nil {
				c.clearPendingRemoval(job.ID)
				c.finishActiveJob(job.ID)
				return
			}
			if c.finishActiveJobUnlessPending(job.ID) {
				if removeErr := c.removeAfterTerminalPersist(job.ID); removeErr != nil {
					c.setPersistenceError(removeErr)
					c.finishActiveJob(job.ID)
					return
				}
				c.finishActiveJob(job.ID)
			}
			continue
		}

		completed := time.Now()
		c.mu.Lock()
		current := findJobIndexLocked(c.jobs, job.ID)
		if current < 0 {
			c.mu.Unlock()
			continue
		}
		c.jobs[current].State = model.JobSent
		c.jobs[current].Uploaded = c.jobs[current].Size
		c.jobs[current].MessageID = messageID
		c.jobs[current].ChannelID = channel.ID
		c.jobs[current].CompletedAt = &completed
		c.jobs[current].Error = ""
		c.queueRevision++
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			c.clearPendingRemoval(job.ID)
			c.mu.Lock()
			c.lastError = "消息已发送，但保存本地状态失败：" + err.Error()
			c.notifyLocked()
			c.mu.Unlock()
			return
		}
		if c.finishActiveJobUnlessPending(job.ID) {
			if removeErr := c.removeAfterTerminalPersist(job.ID); removeErr != nil {
				c.setPersistenceError(removeErr)
				c.finishActiveJob(job.ID)
				return
			}
			c.finishActiveJob(job.ID)
		}
	}
}

// finishActiveJobUnlessPending atomically closes an active job only when a
// concurrent removal request has not arrived. If it returns true, the caller
// keeps the active identity until removeAfterTerminalPersist finishes.
func (c *Controller) finishActiveJobUnlessPending(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeID != id {
		return false
	}
	if _, pending := c.pendingRemoval[id]; pending {
		return true
	}
	c.clearActiveJobLocked(id)
	c.notifyLocked()
	return false
}

func (c *Controller) finishActiveJob(id string) {
	c.mu.Lock()
	if c.activeID == id {
		c.clearActiveJobLocked(id)
		c.notifyLocked()
	}
	c.mu.Unlock()
}

func (c *Controller) clearActiveJobLocked(id string) {
	c.activeID = ""
	c.activeCancel = nil
	c.activeAttempt = 0
	c.retryWaitActive = false
	if c.cancelJobID == id {
		c.cancelJobID = ""
	}
}

func (c *Controller) clearPendingRemoval(id string) {
	c.mu.Lock()
	if _, ok := c.pendingRemoval[id]; ok {
		delete(c.pendingRemoval, id)
		c.notifyLocked()
	}
	c.mu.Unlock()
}

// removeAfterTerminalPersist performs the destructive local queue edit only
// after the terminal state (Sent, Cancelled, Interrupted, or Failed) has been durably saved. The
// filtered candidate is persisted before it is published, so a failed write
// leaves the terminal record visible and recoverable.
func (c *Controller) removeAfterTerminalPersist(id string) error {
	for {
		c.mu.RLock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		index := findJobIndexLocked(current, id)
		c.mu.RUnlock()
		if index < 0 {
			c.clearPendingRemoval(id)
			return nil
		}
		remaining, _ := filteredJobs(current, func(job model.Job) bool { return job.ID == id })
		if err := c.persistCandidate(remaining, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.clearPendingRemoval(id)
			return err
		}
		c.mu.Lock()
		if c.queueRevision != revision {
			c.mu.Unlock()
			continue
		}
		c.jobs = cloneJobsForPersistence(remaining)
		delete(c.pendingRemoval, id)
		if len(c.jobs) == 0 {
			c.folder = ""
		} else {
			c.folder = folder
		}
		c.queueRevision++
		c.lastError = ""
		c.notifyLocked()
		c.mu.Unlock()
		return nil
	}
}

func (c *Controller) currentGateway() tgtransport.Gateway {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gateway
}

func (c *Controller) nextJob() (model.Job, model.Channel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.paused || c.pauseRequested {
		return model.Job{}, model.Channel{}, false
	}
	for _, job := range c.jobs {
		if job.State == model.JobQueued || job.State == model.JobInterrupted {
			return job, c.channel, true
		}
	}
	return model.Job{}, model.Channel{}, false
}

func captionFromFilename(name string) string {
	extension := filepath.Ext(name)
	if strings.EqualFold(extension, ".mp4") {
		return strings.TrimSuffix(name, extension)
	}
	return name
}

func (c *Controller) applyProgress(jobID string, progress model.Progress) {
	c.applyProgressForAttempt(jobID, 0, progress)
}

func (c *Controller) applyProgressForAttempt(jobID string, attemptID uint64, progress model.Progress) {
	c.mu.Lock()
	currentIndex := findJobIndexLocked(c.jobs, jobID)
	if currentIndex < 0 {
		c.mu.Unlock()
		return
	}
	if attemptID != 0 && (c.activeID != jobID || c.activeAttempt != attemptID) {
		c.mu.Unlock()
		return
	}
	job := &c.jobs[currentIndex]
	if job.State != model.JobUploading && job.State != model.JobSending {
		c.mu.Unlock()
		return
	}
	// Several gotd part workers can finish almost simultaneously. A callback
	// that was prepared first may acquire the controller after a newer one; do
	// not let it overwrite newer byte or speed information.
	if progress.BytesDone < job.Uploaded {
		c.mu.Unlock()
		return
	}
	job.Uploaded = progress.BytesDone
	job.BytesPerSecond = progress.BytesPerSecond
	c.notifyLocked()
	c.mu.Unlock()
}

func (c *Controller) prepareSendForAttempt(jobID string, attemptID uint64) error {
	c.mu.Lock()
	currentIndex := findJobIndexLocked(c.jobs, jobID)
	if currentIndex < 0 {
		c.mu.Unlock()
		return errors.New("消息提交前上传队列发生了变化")
	}
	if c.activeID != jobID || c.activeAttempt != attemptID {
		c.mu.Unlock()
		return errors.New("消息提交前上传任务已失效")
	}
	job := &c.jobs[currentIndex]
	if job.State != model.JobUploading && job.State != model.JobSending {
		state := job.State
		c.mu.Unlock()
		return fmt.Errorf("消息提交前任务状态已变为 %s", state)
	}
	if job.State == model.JobSending {
		c.mu.Unlock()
		return nil
	}
	job.State = model.JobSending
	job.Uploaded = job.Size
	job.BytesPerSecond = 0
	c.queueRevision++
	c.notifyLocked()
	c.mu.Unlock()

	// Partial file parts cannot resume, so intermediate byte counters stay in
	// memory. This one transition is different: it is the durable boundary that
	// makes a crash during messages.sendMedia recover as Confirming rather than
	// blindly retrying a possibly delivered message.
	if err := c.persist(); err != nil {
		c.setPersistenceError(err)
		return fmt.Errorf("%w：保存消息提交状态失败：%v", errQueuePersistenceLost, err)
	}
	return nil
}

func (c *Controller) failJob(jobID string, err error) error {
	slog.Warn("upload job failed", "job", jobID, "error", err)
	c.mu.Lock()
	if currentIndex := findJobIndexLocked(c.jobs, jobID); currentIndex >= 0 {
		c.jobs[currentIndex].State = model.JobFailed
		c.jobs[currentIndex].BytesPerSecond = 0
		c.jobs[currentIndex].Error = err.Error()
		c.queueRevision++
	}
	c.lastError = err.Error()
	c.notifyLocked()
	c.mu.Unlock()
	if persistErr := c.persist(); persistErr != nil {
		c.setPersistenceError(persistErr)
		return persistErr
	}
	return nil
}
