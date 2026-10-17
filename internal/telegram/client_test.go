package telegram

import (
	"context"
	"errors"
	"testing"
	"time"
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

func TestClientUploadConcurrencyDefaultsAndCanChangeWithoutReconnect(t *testing.T) {
	client := newOfflineClient(t)
	if got := client.currentUploadConcurrency(); got != DefaultUploadConcurrency {
		t.Fatalf("default upload concurrency = %d, want %d", got, DefaultUploadConcurrency)
	}
	if got := client.SetUploadConcurrency(UploadConcurrencyFast); got != UploadConcurrencyFast {
		t.Fatalf("SetUploadConcurrency(fast) = %d, want %d", got, UploadConcurrencyFast)
	}
	if got := client.currentUploadConcurrency(); got != UploadConcurrencyFast {
		t.Fatalf("current upload concurrency = %d, want %d", got, UploadConcurrencyFast)
	}
	if got := client.SetUploadConcurrency(123); got != DefaultUploadConcurrency {
		t.Fatalf("SetUploadConcurrency(invalid) = %d, want default %d", got, DefaultUploadConcurrency)
	}
}

func TestClientUsesConfiguredUploadConcurrency(t *testing.T) {
	client, err := NewClient(Config{
		AppID:             12345,
		APIHash:           "test-api-hash",
		BotToken:          "12345:test-bot-token",
		UploadConcurrency: UploadConcurrencyFast,
	}, Events{})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if got := client.currentUploadConcurrency(); got != UploadConcurrencyFast {
		t.Fatalf("configured upload concurrency = %d, want %d", got, UploadConcurrencyFast)
	}
}

func TestBindingEventSendUnblocksWhenRunEnds(t *testing.T) {
	client := newOfflineClient(t)
	for i := 0; i < cap(client.binding); i++ {
		client.binding <- BindingEvent{}
	}

	done := make(chan struct{})
	go func() {
		client.sendBindingEvent(BindingEvent{})
		close(done)
	}()

	close(client.runDone)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sendBindingEvent() remained blocked after Run ended")
	}
}

func TestBindingValidationIsDeduplicatedPerGeneration(t *testing.T) {
	client := newOfflineClient(t)
	client.bindingMu.Lock()
	client.bindingCode = "binding-code"
	client.bindingGeneration = 7
	client.bindingMu.Unlock()

	code, generation, claimed := client.claimBindingValidation("binding-code")
	if !claimed || code != "binding-code" || generation != 7 {
		t.Fatalf("first validation claim = (%q, %d, %t), want binding-code, 7, true", code, generation, claimed)
	}
	if _, _, claimed := client.claimBindingValidation("binding-code"); claimed {
		t.Fatal("duplicate validation claim for one generation succeeded")
	}

	client.releaseBindingValidation(generation)
	if _, _, claimed := client.claimBindingValidation("binding-code"); !claimed {
		t.Fatal("validation claim did not become available after the task finished")
	}
}

func TestBindingValidationContextFollowsRunContext(t *testing.T) {
	client := newOfflineClient(t)
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	client.mu.Lock()
	client.runCtx = runCtx
	client.mu.Unlock()

	validationCtx, cancelValidation, ok := client.bindingValidationContext()
	if !ok {
		t.Fatal("bindingValidationContext() reported no active Run")
	}
	defer cancelValidation()

	cancelRun()
	select {
	case <-validationCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("binding validation context did not follow Run cancellation")
	}
}

func TestRequestLifecycleStopCancelsAndWaitsForActiveRequest(t *testing.T) {
	client := newOfflineClient(t)
	client.startRequestLifecycle(context.Background())

	requestCtx, release, err := client.beginRequest(context.Background())
	if err != nil {
		t.Fatalf("beginRequest() error = %v", err)
	}

	cancelObserved := make(chan struct{})
	allowRelease := make(chan struct{})
	go func() {
		<-requestCtx.Done()
		close(cancelObserved)
		<-allowRelease
		release()
	}()

	stopDone := make(chan struct{})
	go func() {
		client.stopRequestLifecycle()
		close(stopDone)
	}()
	secondStopDone := make(chan struct{})
	go func() {
		client.stopRequestLifecycle()
		close(secondStopDone)
	}()

	select {
	case <-cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("active request was not cancelled when lifecycle stopped")
	}
	select {
	case <-stopDone:
		t.Fatal("stopRequestLifecycle() returned before active request released")
	default:
	}
	select {
	case <-secondStopDone:
		t.Fatal("concurrent stopRequestLifecycle() returned before active request released")
	default:
	}

	close(allowRelease)
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("stopRequestLifecycle() did not wait for active request")
	}
	select {
	case <-secondStopDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent stopRequestLifecycle() did not complete")
	}
}

func TestRequestLifecycleRejectsRequestsAfterStopAndIsIdempotent(t *testing.T) {
	client := newOfflineClient(t)
	client.startRequestLifecycle(context.Background())
	client.stopRequestLifecycle()
	client.stopRequestLifecycle()

	if _, release, err := client.beginRequest(context.Background()); !errors.Is(err, ErrNotConnected) {
		if release != nil {
			release()
		}
		t.Fatalf("beginRequest() after stop error = %v, want ErrNotConnected", err)
	}
}

func TestRequestLifecycleFollowsCallerCancellation(t *testing.T) {
	client := newOfflineClient(t)
	client.startRequestLifecycle(context.Background())

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	requestCtx, release, err := client.beginRequest(callerCtx)
	if err != nil {
		t.Fatalf("beginRequest() error = %v", err)
	}
	defer func() {
		release()
		client.stopRequestLifecycle()
	}()

	cancelCaller()
	select {
	case <-requestCtx.Done():
		if !errors.Is(requestCtx.Err(), context.Canceled) {
			t.Fatalf("request context error = %v, want context.Canceled", requestCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("request context did not follow caller cancellation")
	}
}
