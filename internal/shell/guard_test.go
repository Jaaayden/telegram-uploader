package shell

import (
	"errors"
	"testing"
)

func TestCallGuardTurnsReportedFailureIntoError(t *testing.T) {
	var guard callGuard
	refused := errors.New("refused")
	reached := false
	err := guard.run(func() {
		guard.fail(refused)
		reached = true // stands for the binding's os.Exit
	})
	if !errors.Is(err, refused) {
		t.Fatalf("run returned %v, want the reported error", err)
	}
	if reached {
		t.Fatal("the code after the error callback ran; the process would have exited")
	}
	if guard.depth != 0 {
		t.Fatalf("depth = %d after run, want 0", guard.depth)
	}
}

func TestCallGuardNestedRunsReportToTheInnermost(t *testing.T) {
	var guard callGuard
	refused := errors.New("refused")
	var inner error
	outer := guard.run(func() {
		inner = guard.run(func() { guard.fail(refused) })
	})
	if !errors.Is(inner, refused) || outer != nil {
		t.Fatalf("inner = %v, outer = %v", inner, outer)
	}
}

func TestCallGuardLeavesOtherCasesAlone(t *testing.T) {
	var guard callGuard
	if err := guard.run(func() {}); err != nil {
		t.Fatalf("run without a failure returned %v", err)
	}
	// Outside run the callback returns, so the caller can record a fatal error.
	guard.fail(errors.New("fatal"))

	defer func() {
		if recover() == nil {
			t.Fatal("an unrelated panic was swallowed")
		}
		if guard.depth != 0 {
			t.Fatalf("depth = %d after a panic, want 0", guard.depth)
		}
	}()
	_ = guard.run(func() { panic("unrelated") })
}
