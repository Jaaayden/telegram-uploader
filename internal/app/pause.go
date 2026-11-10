package app

import (
	"context"
	"errors"

	"github.com/Jaaayden/telegram-uploader/internal/model"
)

// Pause requests a soft pause. It never cancels the active Telegram request:
// the current file is allowed to finish, then runQueue stops before selecting
// another runnable job. The upload goroutine does not hold opMu for its whole
// lifetime, so queue edits can proceed while an upload is active.
func (c *Controller) Pause() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		if !c.opMu.TryLock() {
			return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
		}
		defer c.opMu.Unlock()
		c.mu.Lock()
	}
	wasPaused := c.paused
	wasRequested := c.pauseRequested
	var cancelRetryWait context.CancelFunc
	c.paused = true
	if c.running {
		c.pauseRequested = true
		if c.retryWaitActive {
			cancelRetryWait = c.activeCancel
		}
	}
	stateChanged := !wasPaused || wasRequested != c.pauseRequested
	if stateChanged {
		c.pauseRevision++
		c.queueRevision++
		c.notifyLocked()
	}
	revision := c.pauseRevision
	c.mu.Unlock()
	if !stateChanged {
		return nil
	}
	if err := c.persist(); err != nil {
		c.mu.Lock()
		if c.pauseRevision == revision {
			c.paused = wasPaused
			c.pauseRequested = wasRequested && c.running
			c.pauseRevision++
			c.notifyLocked()
		}
		c.mu.Unlock()
		c.setPersistenceError(err)
		return err
	}
	// A normal upload remains a soft pause and is allowed to finish. Waiting
	// after a failed pre-send attempt is different: no Telegram request is in
	// flight, so interrupt the backoff immediately and leave the job
	// recoverable instead of making Pause wait for the longest retry delay.
	if cancelRetryWait != nil {
		cancelRetryWait()
	}
	return nil
}

// PauseNow pauses the queue and stops the file that is uploading right away
// instead of letting it finish. The file stays runnable ("interrupted") and
// keeps its resume state, so Start continues it — from where it stopped if
// Telegram still has the uploaded parts, otherwise from the beginning.
//
// A file whose message is already being submitted is never interrupted: the
// outcome of an aborted submission would be unknown. It finishes, as with
// Pause.
func (c *Controller) PauseNow() error {
	if err := c.Pause(); err != nil {
		return err
	}
	c.mu.Lock()
	var cancel context.CancelFunc
	if c.running && c.activeID != "" && c.activeAttempt != 0 {
		if index := findJobIndexLocked(c.jobs, c.activeID); index >= 0 && c.jobs[index].State == model.JobUploading {
			// Recorded under the same lock prepareSendForAttempt takes, so the
			// attempt either sees this and refuses to send, or has already
			// become JobSending and is left alone.
			c.interruptAttempt = c.activeAttempt
			cancel = c.activeCancel
		}
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Resume clears a soft pause without starting a queue by itself. Callers can
// then use Start, which also accepts a paused queue and resumes it. If the
// queue is still finishing its active file, clearing the request lets it
// continue with the next file as soon as that operation returns.
func (c *Controller) Resume() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		if !c.opMu.TryLock() {
			return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
		}
		defer c.opMu.Unlock()
		c.mu.Lock()
	}
	if !c.paused && !c.pauseRequested {
		c.mu.Unlock()
		return nil
	}
	wasPaused := c.paused
	wasRequested := c.pauseRequested
	c.paused = false
	c.pauseRequested = false
	c.pauseRevision++
	c.queueRevision++
	revision := c.pauseRevision
	c.notifyLocked()
	c.mu.Unlock()
	if err := c.persist(); err != nil {
		c.mu.Lock()
		if c.pauseRevision == revision {
			c.paused = wasPaused
			c.pauseRequested = wasRequested && c.running
			c.pauseRevision++
			c.notifyLocked()
		}
		c.mu.Unlock()
		c.setPersistenceError(err)
		return err
	}
	return nil
}

// IsPaused reports an explicit pause intent. It is safe to call while an
// upload is active and is equivalent to Snapshot().Paused.
func (c *Controller) IsPaused() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paused
}

func (c *Controller) queuePaused() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paused || c.pauseRequested
}
