package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

type stagedTransfer struct {
	bytes   int64
	elapsed time.Duration
}

func albumSendingNote(count int) string {
	if count > 1 {
		return "正在合并发送…"
	}
	return "正在提交频道消息…"
}

func collectRunnableGroupLocked(jobs []model.Job, groupID string) []model.Job {
	if groupID == "" {
		return nil
	}
	var members []model.Job
	for i := range jobs {
		job := jobs[i]
		if job.GroupID != groupID {
			continue
		}
		if job.State == model.JobQueued || job.State == model.JobInterrupted {
			members = append(members, job)
			if len(members) == model.MaxAlbumSize {
				break
			}
		}
	}
	return members
}

func (c *Controller) runSendAlbum(
	ctx context.Context,
	groupID string,
	channel model.Channel,
	stats *uploadStats,
	stagedMetrics map[string]stagedTransfer,
	restaged map[string]bool,
) bool {
	for retryIndex := 0; ; retryIndex++ {
		if ctx.Err() != nil || c.queuePaused() {
			return false
		}
		gateway := c.currentGateway()
		albumGateway, ok := gateway.(tgtransport.AlbumGateway)
		if !ok {
			return true
		}

		c.mu.Lock()
		if c.paused || c.pauseRequested || c.cancelAllRequested {
			c.clearActiveBatchLocked()
			c.notifyLocked()
			c.mu.Unlock()
			return false
		}
		members := collectRunnableGroupLocked(c.jobs, groupID)
		if len(members) == 0 {
			c.clearActiveBatchLocked()
			c.notifyLocked()
			c.mu.Unlock()
			return true
		}
		for _, member := range members {
			if member.Staged == nil {
				c.clearActiveBatchLocked()
				c.notifyLocked()
				c.mu.Unlock()
				return true
			}
		}

		batchIDs := make([]string, len(members))
		items := make([]tgtransport.AlbumItem, len(members))
		note := albumSendingNote(len(members))
		for i, member := range members {
			batchIDs[i] = member.ID
			items[i] = tgtransport.AlbumItem{
				Media:    *cloneStagedMedia(member.Staged),
				Caption:  captionFromFilename(member.Name),
				RandomID: member.RandomID,
			}
			if idx := findJobIndexLocked(c.jobs, member.ID); idx >= 0 {
				c.jobs[idx].State = model.JobSending
				c.jobs[idx].Uploaded = c.jobs[idx].Size
				c.jobs[idx].BytesPerSecond = 0
				c.jobs[idx].Error = ""
				c.jobs[idx].Note = note
			}
		}
		albumCtx, albumCancel := context.WithCancel(ctx)
		c.activeID = batchIDs[0]
		c.activeBatch = append([]string(nil), batchIDs...)
		c.activeCancel = albumCancel
		c.activeAttempt = 0
		c.retryWaitActive = false
		c.queueRevision++
		c.notifyLocked()
		c.mu.Unlock()

		// Persist JobSending for all album members before messages.sendMultiMedia
		// so a crash during submission recovers every member as JobConfirming.
		if persistErr := c.persist(); persistErr != nil {
			albumCancel()
			c.mu.Lock()
			for _, id := range batchIDs {
				if idx := findJobIndexLocked(c.jobs, id); idx >= 0 && c.jobs[idx].State == model.JobSending {
					c.jobs[idx].State = model.JobQueued
					c.jobs[idx].Note = ""
				}
			}
			c.clearActiveBatchLocked()
			c.queueRevision++
			c.mu.Unlock()
			c.setPersistenceError(persistErr)
			return false
		}

		c.mu.Lock()
		ctxErr := ctx.Err()
		paused := c.paused || c.pauseRequested
		cancelAll := c.cancelAllRequested
		c.mu.Unlock()
		if cancelAll || paused || ctxErr != nil {
			albumCancel()
			c.revertBatchBeforeSubmit(batchIDs, cancelAll, paused, ctxErr != nil)
			return false
		}

		_ = diagnostics.Logf("album_send_start group=%s count=%d", groupID, len(items))
		sendBegan := time.Now()
		messageIDs, err := albumGateway.SendAlbum(albumCtx, channel, items)
		sendElapsed := time.Since(sendBegan)
		albumCancel()

		if err == nil {
			completed := time.Now()
			share := sendElapsed / time.Duration(len(batchIDs))
			for i, member := range members {
				msgID := 0
				if i < len(messageIDs) {
					msgID = messageIDs[i]
				}
				var sentBytes int64
				var jobElapsed time.Duration
				if metric, hasMetric := stagedMetrics[member.ID]; hasMetric {
					sentBytes = metric.bytes
					jobElapsed = metric.elapsed + share
					delete(stagedMetrics, member.ID)
				}
				stats.add(sentBytes, jobElapsed)
				_ = diagnostics.Logf(
					"job_sent id=%s name=%q message_id=%d size=%d sent_bytes=%d finished_at=%q elapsed=%s avg_speed=%s",
					member.ID,
					member.Name,
					msgID,
					member.Size,
					sentBytes,
					formatLocalTime(completed),
					formatElapsed(jobElapsed),
					formatRate(transferRate(sentBytes, jobElapsed)),
				)
			}
			_ = diagnostics.Logf("album_sent group=%s count=%d elapsed=%s", groupID, len(batchIDs), formatElapsed(sendElapsed))

			c.mu.Lock()
			for i, id := range batchIDs {
				idx := findJobIndexLocked(c.jobs, id)
				if idx < 0 {
					continue
				}
				msgID := 0
				if i < len(messageIDs) {
					msgID = messageIDs[i]
				}
				c.jobs[idx].State = model.JobSent
				c.jobs[idx].Uploaded = c.jobs[idx].Size
				c.jobs[idx].BytesPerSecond = 0
				c.jobs[idx].MessageID = msgID
				c.jobs[idx].ChannelID = channel.ID
				c.jobs[idx].CompletedAt = &completed
				c.jobs[idx].Error = ""
				c.jobs[idx].Note = ""
				c.jobs[idx].Staged = nil
				clearResume(&c.jobs[idx])
			}
			c.queueRevision++
			c.notifyLocked()
			c.mu.Unlock()
			if persistErr := c.persist(); persistErr != nil {
				c.clearPendingRemovalsForIDs(batchIDs)
				c.mu.Lock()
				c.clearActiveBatchLocked()
				c.lastError = "消息已发送，但保存本地状态失败：" + persistErr.Error()
				c.notifyLocked()
				c.mu.Unlock()
				return false
			}
			return c.finalizeActiveBatch(batchIDs)
		}

		_ = diagnostics.Logf(
			"album_send_end group=%s count=%d success=false elapsed=%s error_type=%T outcome_unknown=%t",
			groupID,
			len(batchIDs),
			formatElapsed(sendElapsed),
			err,
			errors.Is(err, tgtransport.ErrSendOutcomeUnknown),
		)

		if errors.Is(err, tgtransport.ErrSendOutcomeUnknown) {
			confirmMsg := "合并消息可能已经送达；请先检查频道，再选择“已送达”或“重新上传”"
			c.mu.Lock()
			for _, id := range batchIDs {
				if idx := findJobIndexLocked(c.jobs, id); idx >= 0 {
					c.jobs[idx].State = model.JobConfirming
					c.jobs[idx].BytesPerSecond = 0
					c.jobs[idx].Note = ""
					c.jobs[idx].Error = confirmMsg
				}
				delete(c.pendingRemoval, id)
			}
			c.lastError = confirmMsg
			c.clearActiveBatchLocked()
			c.queueRevision++
			c.notifyLocked()
			c.mu.Unlock()
			if persistErr := c.persist(); persistErr != nil {
				c.setPersistenceError(persistErr)
			}
			return false
		}

		if staleIdx, stale := tgtransport.StaleAlbumMediaIndex(err); stale {
			expiredIDs := batchIDs
			if staleIdx >= 0 && staleIdx < len(batchIDs) {
				expiredIDs = []string{batchIDs[staleIdx]}
			}
			alreadyRestaged := false
			for _, id := range expiredIDs {
				if restaged[id] {
					alreadyRestaged = true
					break
				}
			}
			if !alreadyRestaged && ctx.Err() == nil && !c.cancelAllRequestedLocked() {
				for _, id := range expiredIDs {
					restaged[id] = true
				}
				_ = diagnostics.Logf("album_restage group=%s expired=%d", groupID, len(expiredIDs))
				expiredSet := make(map[string]struct{}, len(expiredIDs))
				for _, id := range expiredIDs {
					expiredSet[id] = struct{}{}
				}
				c.mu.Lock()
				for _, id := range batchIDs {
					idx := findJobIndexLocked(c.jobs, id)
					if idx < 0 {
						continue
					}
					c.jobs[idx].State = model.JobQueued
					c.jobs[idx].BytesPerSecond = 0
					c.jobs[idx].Note = ""
					c.jobs[idx].Error = ""
					if _, isExpired := expiredSet[id]; isExpired {
						c.jobs[idx].Staged = nil
						c.jobs[idx].Uploaded = 0
						clearResume(&c.jobs[idx])
					}
				}
				c.queueRevision++
				c.notifyLocked()
				c.mu.Unlock()
				if persistErr := c.persist(); persistErr != nil {
					c.clearPendingRemovalsForIDs(batchIDs)
					c.finishActiveBatch()
					c.setPersistenceError(persistErr)
					return false
				}
				return c.finalizeActiveBatch(batchIDs)
			}
		}

		if errors.Is(err, tgtransport.ErrAlbumRejected) {
			return c.failAlbumBatchAndContinue(batchIDs, err)
		}

		c.mu.Lock()
		cancelAll = c.cancelAllRequested
		paused = c.paused || c.pauseRequested
		hasPending := c.hasAnyPendingLocked(batchIDs)
		c.mu.Unlock()

		if !cancelAll && !paused && !hasPending && ctx.Err() == nil && isRetryableUploadError(ctx, err) && retryIndex < len(c.uploadRetryDelays) {
			delay := c.uploadRetryDelays[retryIndex]
			_ = diagnostics.Logf(
				"album_retry_wait group=%s retry=%d max_retries=%d delay_ms=%d error_type=%T",
				groupID,
				retryIndex+1,
				len(c.uploadRetryDelays),
				delay.Milliseconds(),
				err,
			)
			waitErr := c.waitBeforeAlbumRetry(ctx, batchIDs, err, delay, retryIndex+1)
			if waitErr == nil {
				continue
			}
			err = waitErr
		}

		if errors.Is(err, errQueuePersistenceLost) {
			c.clearPendingRemovalsForIDs(batchIDs)
			_ = c.failAlbumBatchAndContinue(batchIDs, err)
			return false
		}

		c.mu.Lock()
		cancelAll = c.cancelAllRequested
		paused = c.paused || c.pauseRequested
		hasPending = c.hasAnyPendingLocked(batchIDs)
		c.mu.Unlock()

		if cancelAll || ctx.Err() != nil || paused || errors.Is(err, errQueuePaused) {
			c.revertBatchBeforeSubmit(batchIDs, cancelAll, paused || errors.Is(err, errQueuePaused), ctx.Err() != nil)
			return false
		}

		if hasPending {
			c.mu.Lock()
			for _, id := range batchIDs {
				if idx := findJobIndexLocked(c.jobs, id); idx >= 0 {
					c.jobs[idx].State = model.JobQueued
					c.jobs[idx].Error = ""
					c.jobs[idx].Note = ""
					c.jobs[idx].BytesPerSecond = 0
				}
			}
			c.queueRevision++
			c.notifyLocked()
			c.mu.Unlock()
			if persistErr := c.persist(); persistErr != nil {
				c.clearPendingRemovalsForIDs(batchIDs)
				c.finishActiveBatch()
				c.setPersistenceError(persistErr)
				return false
			}
			return c.finalizeActiveBatch(batchIDs)
		}

		if isRetryableUploadError(ctx, err) && len(c.uploadRetryDelays) > 0 && retryIndex >= len(c.uploadRetryDelays) {
			err = fmt.Errorf("自动重试 %d 次后仍失败：%w", len(c.uploadRetryDelays), err)
		}
		return c.failAlbumBatchAndContinue(batchIDs, err)
	}
}

func (c *Controller) revertBatchBeforeSubmit(batchIDs []string, cancelAll, paused, ctxCancelled bool) {
	c.mu.Lock()
	for _, id := range batchIDs {
		idx := findJobIndexLocked(c.jobs, id)
		if idx < 0 {
			continue
		}
		c.jobs[idx].Note = ""
		c.jobs[idx].BytesPerSecond = 0
		switch {
		case cancelAll:
			c.jobs[idx].State = model.JobCancelled
			c.jobs[idx].Error = ""
			c.jobs[idx].Staged = nil
			c.jobs[idx].Uploaded = 0
		case ctxCancelled:
			c.jobs[idx].State = model.JobInterrupted
			if paused {
				c.jobs[idx].Error = "队列已暂停，可重新开始当前任务"
			} else {
				c.jobs[idx].Error = "程序关闭或连接中断，可重新开始上传"
			}
		default:
			c.jobs[idx].State = model.JobQueued
			c.jobs[idx].Error = ""
		}
	}
	c.queueRevision++
	c.notifyLocked()
	c.mu.Unlock()
	if persistErr := c.persist(); persistErr != nil {
		c.clearPendingRemovalsForIDs(batchIDs)
		c.finishActiveBatch()
		c.setPersistenceError(persistErr)
		return
	}
	_ = c.finalizeActiveBatch(batchIDs)
}

func (c *Controller) waitBeforeAlbumRetry(
	ctx context.Context,
	batchIDs []string,
	sendErr error,
	delay time.Duration,
	retryNumber int,
) error {
	retryMessage := fmt.Sprintf(
		"连接中断，%s 后自动重试（%d/%d）：%v",
		formatRetryDelay(delay),
		retryNumber,
		len(c.uploadRetryDelays),
		sendErr,
	)
	waitCtx, waitCancel := context.WithCancel(ctx)
	defer waitCancel()

	c.mu.Lock()
	if c.paused || c.pauseRequested {
		c.mu.Unlock()
		return errQueuePaused
	}
	if c.cancelAllRequested {
		c.mu.Unlock()
		return context.Canceled
	}
	for _, id := range batchIDs {
		if idx := findJobIndexLocked(c.jobs, id); idx >= 0 {
			c.jobs[idx].State = model.JobQueued
			c.jobs[idx].Note = ""
			c.jobs[idx].BytesPerSecond = 0
			c.jobs[idx].Error = retryMessage
		}
	}
	c.activeCancel = waitCancel
	c.retryWaitActive = true
	c.queueRevision++
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
	waitErr := wait(waitCtx, delay)
	c.mu.Lock()
	c.retryWaitActive = false
	c.activeCancel = nil
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

func (c *Controller) failAlbumBatchAndContinue(batchIDs []string, err error) bool {
	c.mu.Lock()
	for _, id := range batchIDs {
		idx := findJobIndexLocked(c.jobs, id)
		if idx < 0 {
			continue
		}
		cancelled := c.jobs[idx].State == model.JobCancelled || c.cancelAllRequested
		if cancelled {
			c.jobs[idx].State = model.JobCancelled
			c.jobs[idx].Error = ""
		} else {
			c.jobs[idx].State = model.JobFailed
			c.jobs[idx].Error = err.Error()
		}
		c.jobs[idx].Note = ""
		c.jobs[idx].Staged = nil
		c.jobs[idx].Uploaded = 0
		c.jobs[idx].BytesPerSecond = 0
	}
	if !c.cancelAllRequested {
		c.lastError = err.Error()
	} else {
		c.lastError = ""
	}
	c.queueRevision++
	c.notifyLocked()
	c.mu.Unlock()
	if persistErr := c.persist(); persistErr != nil {
		c.clearPendingRemovalsForIDs(batchIDs)
		c.finishActiveBatch()
		c.setPersistenceError(persistErr)
		return false
	}
	return c.finalizeActiveBatch(batchIDs)
}

func (c *Controller) finalizeActiveBatch(batchIDs []string) bool {
	for {
		if !c.finishActiveBatchUnlessPending(batchIDs) {
			return true
		}
		if removeErr := c.removeBatchAfterTerminalPersist(batchIDs); removeErr != nil {
			c.setPersistenceError(removeErr)
			c.finishActiveBatch()
			return false
		}
	}
}

func (c *Controller) finishActiveBatchUnlessPending(ids []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if _, pending := c.pendingRemoval[id]; pending {
			return true
		}
	}
	c.clearActiveBatchLocked()
	c.notifyLocked()
	return false
}

func (c *Controller) finishActiveBatch() {
	c.mu.Lock()
	c.clearActiveBatchLocked()
	c.notifyLocked()
	c.mu.Unlock()
}

func (c *Controller) clearActiveBatchLocked() {
	c.activeID = ""
	c.activeBatch = nil
	c.activeCancel = nil
	c.activeAttempt = 0
	c.retryWaitActive = false
}

func (c *Controller) clearPendingRemovalsForIDs(ids []string) {
	c.mu.Lock()
	changed := false
	for _, id := range ids {
		if _, ok := c.pendingRemoval[id]; ok {
			delete(c.pendingRemoval, id)
			changed = true
		}
	}
	if changed {
		c.notifyLocked()
	}
	c.mu.Unlock()
}

func (c *Controller) hasAnyPendingLocked(ids []string) bool {
	for _, id := range ids {
		if _, ok := c.pendingRemoval[id]; ok {
			return true
		}
	}
	return false
}

func (c *Controller) cancelAllRequestedLocked() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cancelAllRequested
}

func (c *Controller) removeBatchAfterTerminalPersist(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			idSet[id] = struct{}{}
		}
	}
	for {
		c.mu.RLock()
		toRemove := make(map[string]struct{}, len(idSet))
		for id := range idSet {
			if _, pending := c.pendingRemoval[id]; pending {
				toRemove[id] = struct{}{}
			}
		}
		if len(toRemove) == 0 {
			c.mu.RUnlock()
			return nil
		}
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		c.mu.RUnlock()

		remaining, removed := filteredJobs(current, func(job model.Job) bool {
			_, ok := toRemove[job.ID]
			return ok
		})
		if removed == 0 {
			c.clearPendingRemovalsForIDs(ids)
			return nil
		}
		if err := c.persistCandidate(remaining, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.clearPendingRemovalsForIDs(ids)
			return err
		}
		c.mu.Lock()
		if c.queueRevision != revision {
			c.mu.Unlock()
			continue
		}
		c.jobs = cloneJobsForPersistence(remaining)
		for id := range toRemove {
			delete(c.pendingRemoval, id)
		}
		c.folder = folderIfJobs(c.jobs, folder)
		c.queueRevision++
		c.lastError = ""
		c.notifyLocked()
		c.mu.Unlock()
		return nil
	}
}
