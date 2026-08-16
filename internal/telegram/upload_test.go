package telegram

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReserveSendSlotUpdatesTimestampBeforeFirstSend(t *testing.T) {
	client := &Client{}
	before := client.lastSend

	if err := client.reserveSendSlot(context.Background()); err != nil {
		t.Fatalf("reserveSendSlot() error = %v", err)
	}
	if client.lastSend.IsZero() || !client.lastSend.After(before) {
		t.Fatalf("lastSend = %v, want a timestamp newer than %v", client.lastSend, before)
	}
}

func TestReserveSendSlotCancellationDoesNotReserveSlot(t *testing.T) {
	client := &Client{lastSend: time.Now()}
	before := client.lastSend

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.reserveSendSlot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("reserveSendSlot() error = %v, want context.Canceled", err)
	}
	if !client.lastSend.Equal(before) {
		t.Fatalf("lastSend changed to %v after canceled wait; want %v", client.lastSend, before)
	}
}
