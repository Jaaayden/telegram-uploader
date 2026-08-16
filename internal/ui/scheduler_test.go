package ui

import (
	"sync"
	"testing"
	"time"
)

func TestScheduleCoordinatorFiresFutureSchedule(t *testing.T) {
	called := make(chan struct{}, 1)
	coordinator := newScheduleCoordinator(func() { called <- struct{}{} })
	defer coordinator.Stop()

	at := time.Now().Add(30 * time.Millisecond)
	coordinator.Set(at)

	stateAt, set, due := coordinator.State()
	if !set || due || !stateAt.Equal(at) {
		t.Fatalf("state immediately after Set = (%v, %v, %v), want scheduled and not due", stateAt, set, due)
	}

	waitForScheduleCallback(t, called)
	stateAt, set, due = coordinator.State()
	if !set || !due || !stateAt.Equal(at) {
		t.Fatalf("state after timer = (%v, %v, %v), want scheduled and due", stateAt, set, due)
	}
}

func TestScheduleCoordinatorPastScheduleFiresOnceImmediately(t *testing.T) {
	var mu sync.Mutex
	count := 0
	coordinator := newScheduleCoordinator(func() {
		mu.Lock()
		count++
		mu.Unlock()
	})
	defer coordinator.Stop()

	at := time.Now().Add(-time.Second)
	coordinator.Set(at)
	coordinator.Set(at)

	mu.Lock()
	got := count
	mu.Unlock()
	if got != 2 {
		t.Fatalf("past Set callback count = %d, want 2 (one per Set)", got)
	}

	_, set, due := coordinator.State()
	if !set || !due {
		t.Fatalf("past Set state = (%v, %v), want set and due", set, due)
	}
}

func TestScheduleCoordinatorResetIgnoresOldTimer(t *testing.T) {
	called := make(chan struct{}, 4)
	coordinator := newScheduleCoordinator(func() { called <- struct{}{} })
	defer coordinator.Stop()

	coordinator.Set(time.Now().Add(40 * time.Millisecond))
	coordinator.Set(time.Now().Add(140 * time.Millisecond))

	select {
	case <-called:
		t.Fatal("old timer invoked callback after Set replaced it")
	case <-time.After(80 * time.Millisecond):
	}
	waitForScheduleCallback(t, called)
}

func TestScheduleCoordinatorCancelPreventsCallback(t *testing.T) {
	called := make(chan struct{}, 1)
	coordinator := newScheduleCoordinator(func() { called <- struct{}{} })
	defer coordinator.Stop()

	coordinator.Set(time.Now().Add(40 * time.Millisecond))
	coordinator.Cancel()

	_, set, due := coordinator.State()
	if set || due {
		t.Fatalf("state after Cancel = (%v, %v), want unset and not due", set, due)
	}

	select {
	case <-called:
		t.Fatal("Cancel did not prevent the scheduled callback")
	case <-time.After(80 * time.Millisecond):
	}
}

func TestScheduleCoordinatorHoldDueAndRetryDue(t *testing.T) {
	called := make(chan struct{}, 2)
	coordinator := newScheduleCoordinator(func() { called <- struct{}{} })
	defer coordinator.Stop()

	at := time.Now().Add(2 * time.Hour)
	coordinator.Set(at)
	coordinator.HoldDue(at)

	stateAt, set, due := coordinator.State()
	if !set || !due || !stateAt.Equal(at) {
		t.Fatalf("state after HoldDue = (%v, %v, %v), want scheduled and due", stateAt, set, due)
	}
	select {
	case <-called:
		t.Fatal("HoldDue invoked the callback")
	default:
	}

	coordinator.RetryDue()
	waitForScheduleCallback(t, called)
	coordinator.RetryDue()
	waitForScheduleCallback(t, called)
}

func TestScheduleCoordinatorDelayedRetryReplacesPreviousTimer(t *testing.T) {
	called := make(chan struct{}, 2)
	coordinator := newScheduleCoordinator(func() { called <- struct{}{} })
	defer coordinator.Stop()

	coordinator.HoldDue(time.Now().Add(-time.Minute))
	coordinator.RetryDueAfter(30 * time.Millisecond)
	coordinator.RetryDueAfter(100 * time.Millisecond)

	select {
	case <-called:
		t.Fatal("replaced delayed retry invoked callback")
	case <-time.After(60 * time.Millisecond):
	}
	waitForScheduleCallback(t, called)
}

func TestScheduleCoordinatorCancelRetryRetainsDueSchedule(t *testing.T) {
	called := make(chan struct{}, 1)
	coordinator := newScheduleCoordinator(func() { called <- struct{}{} })
	defer coordinator.Stop()

	at := time.Now().Add(-time.Minute)
	coordinator.HoldDue(at)
	coordinator.RetryDueAfter(30 * time.Millisecond)
	coordinator.CancelRetry()

	stateAt, set, due := coordinator.State()
	if !set || !due || !stateAt.Equal(at) {
		t.Fatalf("state after CancelRetry = (%v, %v, %v), want due schedule retained", stateAt, set, due)
	}
	select {
	case <-called:
		t.Fatal("CancelRetry did not stop delayed callback")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestScheduleCoordinatorRetryDueRequiresDueState(t *testing.T) {
	called := make(chan struct{}, 1)
	coordinator := newScheduleCoordinator(func() { called <- struct{}{} })
	defer coordinator.Stop()

	coordinator.RetryDue()
	select {
	case <-called:
		t.Fatal("RetryDue invoked callback without a due schedule")
	default:
	}

	coordinator.Set(time.Now().Add(100 * time.Millisecond))
	coordinator.RetryDue()
	select {
	case <-called:
		t.Fatal("RetryDue invoked callback before the schedule became due")
	default:
	}
}

func TestScheduleCoordinatorStopPreventsTimerAndRetry(t *testing.T) {
	called := make(chan struct{}, 1)
	coordinator := newScheduleCoordinator(func() { called <- struct{}{} })

	coordinator.Set(time.Now().Add(40 * time.Millisecond))
	coordinator.Stop()
	coordinator.RetryDue()
	coordinator.Set(time.Now())

	select {
	case <-called:
		t.Fatal("Stop did not prevent later callbacks")
	case <-time.After(80 * time.Millisecond):
	}
}

func waitForScheduleCallback(t *testing.T, called <-chan struct{}) {
	t.Helper()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for schedule callback")
	}
}
