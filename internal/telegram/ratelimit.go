package telegram

import (
	"context"
	"io"
	"sync"
	"time"
)

const (
	// rateLimitChunk caps a single throttled read. Smaller reads spread the
	// allowance evenly instead of releasing one 512 KiB part at a time.
	rateLimitChunk = 64 << 10
	// rateLimitPoll bounds one sleep so a changed or removed limit takes effect
	// quickly even when the previous limit was very low.
	rateLimitPoll = 100 * time.Millisecond
	// rateLimitBurst is how much unused allowance may be carried over after an
	// idle period (for example while a part is waiting for Telegram).
	rateLimitBurst = 250 * time.Millisecond
)

// RateLimiter caps the average upload rate. The zero limit means unlimited.
// One limiter can be shared by successive clients, so changing the limit does
// not require reconnecting and applies to the video that is already uploading.
type RateLimiter struct {
	mu    sync.Mutex
	rate  int64 // bytes per second; <= 0 disables the limit
	next  time.Time
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewRateLimiter creates a limiter with the given limit in bytes per second.
func NewRateLimiter(bytesPerSecond int64) *RateLimiter {
	l := &RateLimiter{now: time.Now, sleep: sleepContext}
	l.SetLimit(bytesPerSecond)
	return l
}

// SetLimit changes the limit. Zero or a negative value removes it.
func (l *RateLimiter) SetLimit(bytesPerSecond int64) {
	if l == nil {
		return
	}
	if bytesPerSecond < 0 {
		bytesPerSecond = 0
	}
	l.mu.Lock()
	if l.rate != bytesPerSecond {
		l.rate = bytesPerSecond
		// Debt accumulated under the previous limit must not delay the new one.
		l.next = time.Time{}
	}
	l.mu.Unlock()
}

// Limit returns the current limit in bytes per second (zero when unlimited).
func (l *RateLimiter) Limit() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

// Wait blocks until n more bytes may be sent, or until ctx is done.
func (l *RateLimiter) Wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 {
		return nil
	}
	for {
		l.mu.Lock()
		rate := l.rate
		if rate <= 0 {
			l.mu.Unlock()
			return nil
		}
		now := l.now()
		if floor := now.Add(-rateLimitBurst); l.next.Before(floor) {
			l.next = floor
		}
		if !l.next.After(now) {
			l.next = l.next.Add(time.Duration(float64(n) / float64(rate) * float64(time.Second)))
			l.mu.Unlock()
			return nil
		}
		wait := l.next.Sub(now)
		sleep := l.sleep
		l.mu.Unlock()
		if wait > rateLimitPoll {
			wait = rateLimitPoll
		}
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// throttledReader paces the single goroutine that reads file parts for gotd's
// uploader. Because that reader feeds a channel no larger than the part
// concurrency, pacing the reads paces the network traffic as well.
type throttledReader struct {
	ctx     context.Context
	reader  io.Reader
	limiter *RateLimiter
}

func (t *throttledReader) Read(p []byte) (int, error) {
	if len(p) > rateLimitChunk && t.limiter.Limit() > 0 {
		p = p[:rateLimitChunk]
	}
	n, err := t.reader.Read(p)
	if n > 0 {
		if waitErr := t.limiter.Wait(t.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}
