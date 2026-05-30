package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// recordedCall captures one invocation of tmuxRun for assertions.
type recordedCall struct {
	args  []string
	stdin string
}

// fakeRunner returns a tmuxRunner closure plus a slice that records every
// invocation. The provided handler can decide what each call returns; if
// nil, returns ("", nil) for every call.
func fakeRunner(handler func(args []string, stdin string) ([]byte, error)) (
	runner func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error),
	calls *[]recordedCall,
) {
	calls = &[]recordedCall{}
	runner = func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		var in string
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			in = string(b)
		}
		*calls = append(*calls, recordedCall{args: append([]string{}, args...), stdin: in})
		if handler == nil {
			return nil, nil
		}
		return handler(args, in)
	}
	return runner, calls
}

func withFakeRunner(t *testing.T, h func(args []string, stdin string) ([]byte, error)) *[]recordedCall {
	t.Helper()
	runner, calls := fakeRunner(h)
	prev := SetTmuxRunner(runner)
	t.Cleanup(func() { SetTmuxRunner(prev) })
	return calls
}

// shortRetries replaces the default retry backoff with near-zero waits so
// tests don't sleep 250 ms when they're meant to verify failure paths.
// Also collapses the settle delay so paste→Enter tests don't pay the
// production 500ms.
func shortRetries(t *testing.T) {
	t.Helper()
	prev := retryDelays
	retryDelays = []time.Duration{time.Microsecond, time.Microsecond}
	prevSettle := settleDelay
	settleDelay = time.Microsecond
	t.Cleanup(func() {
		retryDelays = prev
		settleDelay = prevSettle
	})
}

func TestDeliver_HappyPath_PastesAndVerifies(t *testing.T) {
	shortRetries(t)
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("...some preceding line...\nrendered body with id 7f3a marker\n"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3",
		Body: "rendered body with id 7f3a marker",
		VerifyToken: "id 7f3a",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Expect: load-buffer, paste-buffer, send-keys, capture-pane, [delete-buffer in defer].
	wantCmds := []string{"load-buffer", "paste-buffer", "send-keys", "capture-pane", "delete-buffer"}
	if len(*calls) < len(wantCmds) {
		t.Fatalf("got %d calls, want at least %d: %v", len(*calls), len(wantCmds), *calls)
	}
	for i, want := range wantCmds {
		if (*calls)[i].args[0] != want {
			t.Errorf("call %d = %q, want %q", i, (*calls)[i].args[0], want)
		}
	}
	// load-buffer stdin should carry the body.
	if (*calls)[0].stdin != "rendered body with id 7f3a marker" {
		t.Errorf("load-buffer stdin = %q", (*calls)[0].stdin)
	}
	// paste-buffer should target the right pane and use -d for buffer cleanup.
	pasteArgs := (*calls)[1].args
	if !contains(pasteArgs, "-t") || !contains(pasteArgs, "%3") {
		t.Errorf("paste-buffer not targeting %%3: %v", pasteArgs)
	}
	if !contains(pasteArgs, "-d") {
		t.Errorf("paste-buffer missing -d: %v", pasteArgs)
	}
}

func TestDeliver_UniqueBufferPerCall(t *testing.T) {
	shortRetries(t)
	calls := withFakeRunner(t, nil)
	_ = Deliver(context.Background(), DeliverParams{Pane: "%1", Body: "x"})
	_ = Deliver(context.Background(), DeliverParams{Pane: "%1", Body: "x"})

	var bufNames []string
	for _, c := range *calls {
		if c.args[0] == "load-buffer" {
			for i, a := range c.args {
				if a == "-b" && i+1 < len(c.args) {
					bufNames = append(bufNames, c.args[i+1])
				}
			}
		}
	}
	if len(bufNames) != 2 {
		t.Fatalf("expected 2 load-buffer calls, got %d", len(bufNames))
	}
	if bufNames[0] == bufNames[1] {
		t.Errorf("buffer names collided: %s", bufNames[0])
	}
	if !strings.HasPrefix(bufNames[0], "inject-") {
		t.Errorf("buffer name should start with inject-, got %q", bufNames[0])
	}
}

func TestDeliver_VerifyRetriesOnMiss(t *testing.T) {
	shortRetries(t)
	captureCount := 0
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] != "capture-pane" {
			return nil, nil
		}
		captureCount++
		if captureCount < 3 {
			return []byte("nothing here yet"), nil
		}
		return []byte("eventually the token id 7f3a appears"), nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id 7f3a",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if captureCount != 3 {
		t.Errorf("captureCount = %d, want 3 (1 initial + 2 retries)", captureCount)
	}
}

func TestDeliver_ReturnsUnverifiedSentinelAfterRetriesExhausted(t *testing.T) {
	shortRetries(t)
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("token never lands"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "missing",
	})
	if err == nil {
		t.Fatal("want error after retries exhausted")
	}
	// Soft-fail sentinel: paste/Enter completed mechanically, just
	// couldn't confirm the token surfaced. Caller treats this as a
	// soft success (mark delivered + WARN) rather than hard failure
	// (drop message).
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Errorf("error should wrap ErrUnverifiedDelivery; got %v", err)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should name the missing token; got %v", err)
	}
}

func TestDeliver_LoadBufferFailure(t *testing.T) {
	shortRetries(t)
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "load-buffer" {
			return []byte("can't grab a lock on the buffer"), errors.New("exit 1")
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{Pane: "%3", Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "load-buffer") {
		t.Errorf("err = %v, want load-buffer error", err)
	}
}

func TestDeliver_SkipsVerifyWhenTokenEmpty(t *testing.T) {
	shortRetries(t)
	calls := withFakeRunner(t, nil)
	err := Deliver(context.Background(), DeliverParams{Pane: "%3", Body: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for _, c := range *calls {
		if c.args[0] == "capture-pane" {
			t.Errorf("capture-pane should be skipped when VerifyToken=='', got call %v", c.args)
		}
	}
}

// TestDeliver_SettleDelay_ObservedBetweenPasteAndEnter pins the
// 2026-05-30 fix: a sleep between paste-buffer and send-keys Enter so
// Claude Code's TUI has time to ingest the paste before the submit
// keystroke arrives. The test sets a measurable delay, runs Deliver,
// and asserts the elapsed time covers it.
func TestDeliver_SettleDelay_ObservedBetweenPasteAndEnter(t *testing.T) {
	shortRetries(t)
	// Override just the settle delay (retryDelays stays at micros from
	// shortRetries) so the only meaningful wall clock cost is the settle.
	prev := SetSettleDelayForTest(40 * time.Millisecond)
	t.Cleanup(func() { SetSettleDelayForTest(prev) })

	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("verified id 1\n"), nil
		}
		return nil, nil
	})
	start := time.Now()
	if err := Deliver(context.Background(), DeliverParams{
		Pane: "%1", Body: "x", VerifyToken: "id 1",
	}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("elapsed = %s, want >= 40ms (settle delay should have fired)", elapsed)
	}
}

// Context cancellation during the settle delay should propagate
// without sending Enter.
func TestDeliver_SettleDelay_RespectsContextCancellation(t *testing.T) {
	shortRetries(t)
	prev := SetSettleDelayForTest(50 * time.Millisecond)
	t.Cleanup(func() { SetSettleDelayForTest(prev) })

	calls := withFakeRunner(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	err := Deliver(ctx, DeliverParams{Pane: "%1", Body: "x"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	// Confirm we never reached send-keys Enter (the cancellation
	// fired during the settle).
	for _, c := range *calls {
		if c.args[0] == "send-keys" {
			t.Errorf("send-keys should NOT run when ctx fires during settle; got %v", c.args)
		}
	}
}

func TestDeliver_RequiresPaneAndBody(t *testing.T) {
	withFakeRunner(t, nil)
	if err := Deliver(context.Background(), DeliverParams{Body: "x"}); err == nil {
		t.Error("want error for empty pane")
	}
	if err := Deliver(context.Background(), DeliverParams{Pane: "%1"}); err == nil {
		t.Error("want error for empty body")
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// Silence unused warnings when nothing else in this file uses fmt.
var _ = fmt.Sprintf
