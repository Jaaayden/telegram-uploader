package telegram

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tgerr"
)

type testInvoker func(context.Context, bin.Encoder, bin.Decoder) error

func (f testInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	return f(ctx, input, output)
}

func TestFloodGateWaitIsCancelableAndDoesNotBlockOtherInvocations(t *testing.T) {
	waitObserved := make(chan struct{}, 1)
	gate := &floodGate{
		onWait: func(time.Duration) {
			waitObserved <- struct{}{}
		},
	}

	first := gate.middleware().Handle(testInvoker(func(context.Context, bin.Encoder, bin.Decoder) error {
		return tgerr.New(420, "FLOOD_WAIT_1")
	}))
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first(firstCtx, nil, nil)
	}()

	select {
	case <-waitObserved:
	case <-time.After(time.Second):
		t.Fatal("flood wait callback was not called")
	}

	// A flood wait in one invocation must not serialize unrelated calls behind
	// the timer. The second call should return immediately.
	second := gate.middleware().Handle(testInvoker(func(context.Context, bin.Encoder, bin.Decoder) error {
		return nil
	}))
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancelSecond()
	if err := second(secondCtx, nil, nil); err != nil {
		t.Fatalf("unrelated invocation returned error = %v", err)
	}

	cancelFirst()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("flood-wait invocation error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("flood-wait invocation did not observe context cancellation")
	}
}

func TestFloodGateReturnsNonFloodErrors(t *testing.T) {
	want := errors.New("transport failed")
	gate := &floodGate{}
	invoke := gate.middleware().Handle(testInvoker(func(context.Context, bin.Encoder, bin.Decoder) error {
		return want
	}))

	if err := invoke(context.Background(), nil, nil); !errors.Is(err, want) {
		t.Fatalf("invoke() error = %v, want %v", err, want)
	}
}
