package telegram

import (
	"context"
	"errors"
	"testing"
)

func TestBotIDFromToken(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		wantID  int64
		wantErr bool
	}{
		{name: "valid", token: "123456:ABC-def", wantID: 123456},
		{name: "trim surrounding whitespace", token: " 42:token ", wantID: 42},
		{name: "missing separator", token: "123456", wantErr: true},
		{name: "empty suffix", token: "123456:", wantErr: true},
		{name: "non numeric id", token: "abc:token", wantErr: true},
		{name: "zero id", token: "0:token", wantErr: true},
		{name: "negative id", token: "-1:token", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotID, err := botIDFromToken(test.token)
			if test.wantErr {
				if err == nil {
					t.Fatalf("botIDFromToken() = %d, nil error; want an error", gotID)
				}
				return
			}
			if err != nil {
				t.Fatalf("botIDFromToken() error = %v", err)
			}
			if gotID != test.wantID {
				t.Fatalf("botIDFromToken() = %d, want %d", gotID, test.wantID)
			}
		})
	}
}

func newOfflineClient(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(Config{
		AppID:          12345,
		APIHash:        "test-api-hash",
		BotToken:       "12345:test-bot-token",
		SessionStorage: nil,
	}, Events{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func TestClientRunIsOneShot(t *testing.T) {
	client := newOfflineClient(t)
	client.mu.Lock()
	client.started = true
	client.mu.Unlock()

	if err := client.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Run() error = %v, want ErrAlreadyRunning", err)
	}
}

func TestWaitReadyHonorsContextCancellation(t *testing.T) {
	client := newOfflineClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.WaitReady(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitReady() error = %v, want context.Canceled", err)
	}
}

func TestWaitReadyReportsRunFailure(t *testing.T) {
	client := newOfflineClient(t)
	want := errors.New("run failed")
	client.mu.Lock()
	client.runErr = want
	client.mu.Unlock()
	close(client.runDone)

	_, err := client.WaitReady(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("WaitReady() error = %v, want %v", err, want)
	}
}
