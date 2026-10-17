package app

import (
	"context"
	"errors"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/model"
)

func (c *Controller) persistJobMutation(id string, before model.Job, revision uint64) error {
	if err := c.persist(); err != nil {
		c.mu.Lock()
		if c.queueRevision == revision {
			if index := findJobIndexLocked(c.jobs, id); index >= 0 {
				c.jobs[index] = before
				c.queueRevision++
				c.notifyLocked()
			}
		}
		c.mu.Unlock()
		c.setPersistenceError(err)
		return err
	}
	return nil
}

func (c *Controller) CancelJob(id string) error {
	if id == "" {
		return errors.New("任务 ID 不能为空")
	}
	var cancel context.CancelFunc
	c.mu.Lock()
	index := findJobIndexLocked(c.jobs, id)
	if index < 0 {
		c.mu.Unlock()
		return errors.New("没有找到上传任务")
	}
	if _, pending := c.pendingRemoval[id]; pending {
		c.mu.Unlock()
		return errors.New("该任务正在取消并删除")
	}
	if (c.jobs[index].State == model.JobUploading || c.jobs[index].State == model.JobSending) && c.activeID == id {
		if c.activeCancel == nil {
			c.mu.Unlock()
			return errors.New("该任务当前无法取消")
		}
		c.cancelJobID = id
		cancel = c.activeCancel
		c.notifyLocked()
		c.mu.Unlock()
		cancel()
		return nil
	}
	switch c.jobs[index].State {
	case model.JobQueued, model.JobInterrupted, model.JobFailed, model.JobSkipped:
		before := c.jobs[index]
		c.jobs[index].State = model.JobCancelled
		c.jobs[index].Error = ""
		c.queueRevision++
		revision := c.queueRevision
		c.notifyLocked()
		c.mu.Unlock()
		return c.persistJobMutation(id, before, revision)
	default:
		c.mu.Unlock()
		return errors.New("该任务已经结束或等待人工确认")
	}
}

func (c *Controller) CancelAll() error {
	// If Start is still durably saving the queue (or the runner is between
	// jobs), there is no active Telegram request to protect. Cancel the queue
	// context immediately so the runner cannot begin a new file in the window
	// before the revision-safe cancellation save below completes.
	c.mu.RLock()
	var cancelBeforeActive context.CancelFunc
	if c.running && c.activeID == "" {
		cancelBeforeActive = c.allCancel
	}
	c.mu.RUnlock()
	if cancelBeforeActive != nil {
		cancelBeforeActive()
	}

	for {
		c.mu.RLock()
		jobs := cloneJobsForPersistence(c.jobs)
		channel := c.channel
		folder := c.folder
		revision := c.queueRevision
		c.mu.RUnlock()

		for i := range jobs {
			switch jobs[i].State {
			case model.JobQueued, model.JobInterrupted, model.JobFailed, model.JobSkipped:
				jobs[i].State = model.JobCancelled
				jobs[i].Error = ""
			}
		}

		// Save the non-active cancellation states before publishing them or
		// interrupting the current transfer. If durable storage is unavailable,
		// the caller gets an error and both memory and queue.json retain the
		// pre-cancellation state instead of disagreeing after a restart.
		if err := c.persistCandidate(jobs, channel, false, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return err
		}

		var cancel context.CancelFunc
		c.mu.Lock()
		if c.queueRevision != revision {
			c.mu.Unlock()
			continue
		}
		c.preserveActiveProgressLocked(jobs)
		c.jobs = cloneJobsForPersistence(jobs)
		if folder != "" || len(jobs) == 0 {
			c.folder = folder
		}
		c.pauseRevision++
		c.paused = false
		c.pauseRequested = false
		c.cancelAllRequested = true
		cancel = c.allCancel
		c.queueRevision++
		c.lastError = ""
		c.notifyLocked()
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
}

func (c *Controller) Retry(id string) error {
	c.mu.Lock()
	index := findJobIndexLocked(c.jobs, id)
	if index < 0 {
		c.mu.Unlock()
		return errors.New("没有找到上传任务")
	}
	if _, pending := c.pendingRemoval[id]; pending {
		c.mu.Unlock()
		return errors.New("该任务正在取消并删除")
	}
	if c.jobs[index].ID == c.activeID && (c.jobs[index].State == model.JobUploading || c.jobs[index].State == model.JobSending) {
		c.mu.Unlock()
		return errors.New("当前上传任务不能重试，请先取消")
	}
	switch c.jobs[index].State {
	case model.JobFailed, model.JobInterrupted, model.JobCancelled, model.JobSkipped, model.JobConfirming:
		before := c.jobs[index]
		resetPendingJobProgress(&c.jobs[index])
		c.jobs[index].State = model.JobQueued
		c.queueRevision++
		revision := c.queueRevision
		c.lastError = ""
		c.notifyLocked()
		c.mu.Unlock()
		return c.persistJobMutation(id, before, revision)
	default:
		c.mu.Unlock()
		return errors.New("该任务当前不能重试")
	}
}

func (c *Controller) Skip(id string) error {
	c.mu.Lock()
	index := findJobIndexLocked(c.jobs, id)
	if _, pending := c.pendingRemoval[id]; pending {
		c.mu.Unlock()
		return errors.New("该任务正在取消并删除")
	}
	if index >= 0 && c.jobs[index].ID != c.activeID && (c.jobs[index].State == model.JobFailed || c.jobs[index].State == model.JobConfirming) {
		before := c.jobs[index]
		c.jobs[index].State = model.JobSkipped
		c.jobs[index].Error = ""
		c.queueRevision++
		revision := c.queueRevision
		c.notifyLocked()
		c.mu.Unlock()
		return c.persistJobMutation(id, before, revision)
	}
	c.mu.Unlock()
	return errors.New("该任务当前不能跳过")
}

func (c *Controller) MarkSent(id string) error {
	c.mu.Lock()
	index := findJobIndexLocked(c.jobs, id)
	if _, pending := c.pendingRemoval[id]; pending {
		c.mu.Unlock()
		return errors.New("该任务正在取消并删除")
	}
	if index >= 0 && c.jobs[index].State == model.JobConfirming {
		before := c.jobs[index]
		now := time.Now()
		c.jobs[index].State = model.JobSent
		c.jobs[index].Uploaded = c.jobs[index].Size
		c.jobs[index].CompletedAt = &now
		c.jobs[index].Error = ""
		c.queueRevision++
		revision := c.queueRevision
		c.notifyLocked()
		c.mu.Unlock()
		return c.persistJobMutation(id, before, revision)
	}
	c.mu.Unlock()
	return errors.New("该任务不处于待确认状态")
}
