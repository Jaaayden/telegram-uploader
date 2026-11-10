package telegram

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// fakeClock advances only when the limiter sleeps, so the tests measure the
// exact simulated transfer time without depending on the scheduler.
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.at = c.at.Add(d)
	return nil
}

func newFakeLimiter(rate int64) (*RateLimiter, *fakeClock) {
	clock := &fakeClock{at: time.Unix(1_700_000_000, 0)}
	limiter := NewRateLimiter(rate)
	limiter.now = clock.now
	limiter.sleep = clock.sleep
	return limiter, clock
}

func TestRateLimiterPacesToConfiguredRate(t *testing.T) {
	const rate = 1 << 20 // 1 MiB/s
	limiter, clock := newFakeLimiter(rate)
	start := clock.at
	total := 0
	for total < 10<<20 {
		if err := limiter.Wait(context.Background(), rateLimitChunk); err != nil {
			t.Fatal(err)
		}
		total += rateLimitChunk
	}
	elapsed := clock.at.Sub(start)
	// 10 MiB at 1 MiB/s takes ten seconds, minus the small initial burst.
	if elapsed < 9*time.Second || elapsed > 10500*time.Millisecond {
		t.Fatalf("10 MiB at 1 MiB/s took %v of simulated time", elapsed)
	}
}

func TestRateLimiterUnlimitedNeverSleeps(t *testing.T) {
	limiter, clock := newFakeLimiter(0)
	start := clock.at
	for range 1000 {
		if err := limiter.Wait(context.Background(), 512<<10); err != nil {
			t.Fatal(err)
		}
	}
	if !clock.at.Equal(start) {
		t.Fatalf("unlimited limiter slept for %v", clock.at.Sub(start))
	}
	var nilLimiter *RateLimiter
	if err := nilLimiter.Wait(context.Background(), 1); err != nil || nilLimiter.Limit() != 0 {
		t.Fatal("nil limiter must behave as unlimited")
	}
}

func TestRateLimiterNewLimitAppliesImmediately(t *testing.T) {
	limiter, clock := newFakeLimiter(1024) // 1 KiB/s: one chunk owes 64 seconds
	if err := limiter.Wait(context.Background(), rateLimitChunk); err != nil {
		t.Fatal(err)
	}
	limiter.SetLimit(0)
	start := clock.at
	if err := limiter.Wait(context.Background(), rateLimitChunk); err != nil {
		t.Fatal(err)
	}
	if !clock.at.Equal(start) {
		t.Fatal("removing the limit must drop the debt of the old limit")
	}
	limiter.SetLimit(-5)
	if limiter.Limit() != 0 {
		t.Fatal("negative limits must be normalized to unlimited")
	}
}

func TestRateLimiterDoesNotAccumulateIdleCredit(t *testing.T) {
	const rate = 1 << 20
	limiter, clock := newFakeLimiter(rate)
	if err := limiter.Wait(context.Background(), rateLimitChunk); err != nil {
		t.Fatal(err)
	}
	clock.at = clock.at.Add(time.Hour) // a long stall must not buy an hour of burst
	start := clock.at
	sent := 0
	for sent < 4<<20 {
		if err := limiter.Wait(context.Background(), rateLimitChunk); err != nil {
			t.Fatal(err)
		}
		sent += rateLimitChunk
	}
	if elapsed := clock.at.Sub(start); elapsed < 3*time.Second {
		t.Fatalf("4 MiB after an idle hour took only %v", elapsed)
	}
}

func TestRateLimiterWaitHonorsCancellation(t *testing.T) {
	limiter := NewRateLimiter(1)
	ctx, cancel := context.WithCancel(context.Background())
	if err := limiter.Wait(ctx, 10); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := limiter.Wait(ctx, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after cancel = %v, want context.Canceled", err)
	}
}

func TestThrottledReaderDeliversAllBytesInSmallReads(t *testing.T) {
	limiter, clock := newFakeLimiter(256 << 10)
	payload := bytes.Repeat([]byte{0xAB}, 1<<20)
	reader := &throttledReader{ctx: context.Background(), reader: bytes.NewReader(payload), limiter: limiter}
	start := clock.at
	buffer := make([]byte, 512<<10)
	n, err := io.ReadFull(reader, buffer)
	if err != nil || n != len(buffer) {
		t.Fatalf("ReadFull = %d, %v", n, err)
	}
	rest, err := io.ReadAll(reader)
	if err != nil || len(rest) != len(payload)-len(buffer) {
		t.Fatalf("ReadAll = %d bytes, %v", len(rest), err)
	}
	// 1 MiB at 256 KiB/s is four seconds.
	if elapsed := clock.at.Sub(start); elapsed < 3*time.Second || elapsed > 4500*time.Millisecond {
		t.Fatalf("1 MiB at 256 KiB/s took %v of simulated time", elapsed)
	}
}
