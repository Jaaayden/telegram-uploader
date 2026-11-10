package ui

import (
	"sync"
	"time"
)

// scheduleCoordinator owns one optional in-memory timer for the UI's
// scheduled queue start.  A schedule remains marked as due after its timer
// fires so that the caller can retry a start which was temporarily blocked
// (for example, while Telegram is reconnecting).
//
// The generation counter makes callbacks from timers that were stopped while
// being replaced harmless.  Callbacks are always invoked after releasing mu;
// a callback may therefore safely call Set, Cancel, or RetryDue again.
type scheduleCoordinator struct {
	mu sync.Mutex

	callback func()
	timer    *time.Timer

	generation uint64
	at         time.Time
	set        bool
	due        bool
	stopped    bool
}

// newScheduleCoordinator creates a coordinator whose timers use the local
// process clock and time.AfterFunc.
func newScheduleCoordinator(callback func()) *scheduleCoordinator {
	return &scheduleCoordinator{callback: callback}
}

// Set replaces any existing schedule.  A time in the past (or exactly now)
// becomes due immediately and invokes the callback once after the state has
// been updated.
func (c *scheduleCoordinator) Set(at time.Time) {
	if c == nil {
		return
	}

	var callback func()
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}

	c.stopTimerLocked()
	c.generation++
	generation := c.generation
	c.at = at
	c.set = true
	c.due = false

	if !at.After(time.Now()) {
		c.due = true
		callback = c.callback
	} else {
		c.timer = time.AfterFunc(time.Until(at), func() {
			c.fire(generation)
		})
	}
	c.mu.Unlock()

	invokeScheduleCallback(callback)
}

// Cancel removes the current schedule and prevents any stopped timer from
// invoking the callback.
func (c *scheduleCoordinator) Cancel() {
	if c == nil {
		return
	}

	c.mu.Lock()
	c.stopTimerLocked()
	c.generation++
	c.at = time.Time{}
	c.set = false
	c.due = false
	c.mu.Unlock()
}

// HoldDue marks a schedule as due without invoking the callback.  This is
// useful when the scheduled start has arrived but cannot begin yet; the
// caller can later call RetryDue after the blocking condition is resolved.
func (c *scheduleCoordinator) HoldDue(at time.Time) {
	if c == nil {
		return
	}

	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}

	c.stopTimerLocked()
	c.generation++
	c.at = at
	c.set = true
	c.due = true
	c.mu.Unlock()
}

// RetryDue invokes the callback once when the current schedule is already
// due.  The due state is retained, so a later retry remains possible if the
// callback reports another temporary failure.
func (c *scheduleCoordinator) RetryDue() {
	if c == nil {
		return
	}

	c.mu.Lock()
	if c.stopped || !c.set || !c.due {
		c.mu.Unlock()
		return
	}
	callback := c.callback
	c.mu.Unlock()

	invokeScheduleCallback(callback)
}

// RetryDueAfter schedules one later callback for a schedule which is already
// due. Repeated calls replace the previous delayed retry, so connection error
// paths can safely apply backoff without accumulating timers.
func (c *scheduleCoordinator) RetryDueAfter(delay time.Duration) {
	if c == nil {
		return
	}

	var callback func()
	c.mu.Lock()
	if c.stopped || !c.set || !c.due {
		c.mu.Unlock()
		return
	}

	c.stopTimerLocked()
	c.generation++
	generation := c.generation
	if delay <= 0 {
		callback = c.callback
	} else {
		c.timer = time.AfterFunc(delay, func() {
			c.fireRetry(generation)
		})
	}
	c.mu.Unlock()

	invokeScheduleCallback(callback)
}

// CancelRetry removes a delayed retry while retaining the scheduled time and
// due state. It is used after Telegram reconnects successfully.
func (c *scheduleCoordinator) CancelRetry() {
	if c == nil {
		return
	}

	c.mu.Lock()
	if !c.set || !c.due {
		c.mu.Unlock()
		return
	}
	c.stopTimerLocked()
	c.generation++
	c.mu.Unlock()
}

// State returns the scheduled time, whether a schedule exists, and whether
// that schedule has become due.  The returned time is zero when no schedule
// exists.
func (c *scheduleCoordinator) State() (at time.Time, set, due bool) {
	if c == nil {
		return time.Time{}, false, false
	}

	c.mu.Lock()
	at, set, due = c.at, c.set, c.due
	c.mu.Unlock()
	return at, set, due
}

// Stop stops the in-memory timer and permanently closes this coordinator.
// Existing state is retained for inspection, but no later operation can
// schedule or retry a callback.  A new coordinator should be created if the
// owning window is rebuilt.
func (c *scheduleCoordinator) Stop() {
	if c == nil {
		return
	}

	c.mu.Lock()
	c.stopTimerLocked()
	c.generation++
	c.stopped = true
	c.mu.Unlock()
}

func (c *scheduleCoordinator) fire(generation uint64) {
	c.mu.Lock()
	if c.stopped || generation != c.generation || !c.set || c.due {
		c.mu.Unlock()
		return
	}

	c.timer = nil
	c.due = true
	callback := c.callback
	c.mu.Unlock()

	invokeScheduleCallback(callback)
}

func (c *scheduleCoordinator) fireRetry(generation uint64) {
	c.mu.Lock()
	if c.stopped || generation != c.generation || !c.set || !c.due {
		c.mu.Unlock()
		return
	}

	c.timer = nil
	callback := c.callback
	c.mu.Unlock()

	invokeScheduleCallback(callback)
}

func (c *scheduleCoordinator) stopTimerLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

func invokeScheduleCallback(callback func()) {
	if callback != nil {
		callback()
	}
}
