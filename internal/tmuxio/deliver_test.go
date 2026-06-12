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
		Pane:        "%3",
		Body:        "rendered body with id 7f3a marker",
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

// TestDeliverySubmitted exercises the #336 input-emptied verify predicate
// directly across both adapter profiles and the sentinel-less fallback.
// The load-bearing cases: (1) input-emptied verifies even with the token
// absent (collapse-robustness); (2) a paste still sitting in the input row
// reads as not-submitted even when its token is literally visible there
// (the false-positive token-match would hit); (3) codex's bottom-most
// anchoring ignores the same `› ` glyph on transcript turns.
func TestDeliverySubmitted(t *testing.T) {
	claude := ClaudePaneProfile()
	codex := CodexPaneProfile()
	cases := []struct {
		name    string
		profile PaneProfile
		capture string
		token   string
		want    bool
	}{
		{"claude cleared input verifies without token", claude, "a submitted turn\n[Pasted Content 1800 chars]\n" + PromptSentinel, "id 7f3a", true},
		{"claude paste in input rejects despite token visible", claude, "transcript\n" + PromptSentinel + "rendered body id 7f3a marker", "id 7f3a", false},
		{"codex bottom-most empty ignores transcript sentinel", codex, CodexPromptSentinel + "[Pasted Content 1800 chars]\n\n" + CodexPromptSentinel, "id 7f3a", true},
		{"codex collapsed paste in bottom input rejects", codex, "some codex output\n" + CodexPromptSentinel + "[Pasted Content 1800 chars]", "id 7f3a", false},
		{"no sentinel falls back to token-match hit", claude, "plain pane with id 7f3a somewhere", "id 7f3a", true},
		{"no sentinel falls back to token-match miss", claude, "plain pane, nothing here", "id 7f3a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := ActivePaneProfile()
			SetActivePaneProfile(tc.profile)
			defer SetActivePaneProfile(prev)
			if got := deliverySubmitted(tc.capture, tc.token); got != tc.want {
				t.Errorf("deliverySubmitted(%q) = %v, want %v", tc.capture, got, tc.want)
			}
		})
	}
}

// TestDeliver_InputEmptied_VerifiesOnClearedInput pins the end-to-end loop
// wiring: with the Claude profile active, a post-Enter capture whose
// bottom-most `❯ ` row is empty verifies the delivery even though the
// verify token never appears in the pane (the collapse case).
func TestDeliver_InputEmptied_VerifiesOnClearedInput(t *testing.T) {
	shortRetries(t)
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("> earlier submitted turn\n[Pasted Content 1800 chars]\n" + PromptSentinel), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id 7f3a",
	})
	if err != nil {
		t.Fatalf("expected verified via input-emptied (token absent), got %v", err)
	}
}

// TestDeliver_InputEmptied_RejectsPasteStillInInput pins the mid-turn /
// queued-Enter honesty: the paste is still in the bottom input row (token
// literally visible there), so the delivery reads as unverified rather than
// false-positive-confirmed.
func TestDeliver_InputEmptied_RejectsPasteStillInInput(t *testing.T) {
	shortRetries(t)
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("some transcript\n" + PromptSentinel + "rendered body with id 7f3a marker"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id 7f3a",
	})
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Fatalf("paste still in input row must read as unverified; got %v", err)
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

// TestDeriveRetrySchedule_DefaultPreservesBaseline pins the load-bearing
// invariant: the default budget (5s) reproduces the historical
// 100ms/250ms/500ms/1s/1.5s/1.65s schedule exactly. Operators upgrading
// without setting the verify-retry-budget knob (#153) see zero behavior
// change.
func TestDeriveRetrySchedule_DefaultPreservesBaseline(t *testing.T) {
	got := DeriveRetrySchedule(DefaultRetryBudget)
	want := []time.Duration{
		100 * time.Millisecond,
		250 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		1500 * time.Millisecond,
		1650 * time.Millisecond,
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: got %v, want %v", i, got[i], want[i])
		}
	}
	var sum time.Duration
	for _, d := range got {
		sum += d
	}
	if sum != DefaultRetryBudget {
		t.Errorf("default schedule sum: got %v, want %v", sum, DefaultRetryBudget)
	}
}

// TestDeriveRetrySchedule_ScalesProportionally pins the scaling rule:
// a doubled budget doubles every step, a halved budget halves every
// step, and the schedule sum equals the budget across both directions.
func TestDeriveRetrySchedule_ScalesProportionally(t *testing.T) {
	cases := []struct {
		name   string
		budget time.Duration
	}{
		{"half (2.5s)", 2500 * time.Millisecond},
		{"double (10s)", 10 * time.Second},
		{"triple (15s)", 15 * time.Second},
		{"large hub (30s)", 30 * time.Second},
	}
	baseline := DeriveRetrySchedule(DefaultRetryBudget)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveRetrySchedule(tc.budget)
			if len(got) != len(baseline) {
				t.Fatalf("len: got %d, want %d", len(got), len(baseline))
			}
			scale := float64(tc.budget) / float64(DefaultRetryBudget)
			for i, d := range baseline {
				want := time.Duration(float64(d) * scale)
				if got[i] != want {
					t.Errorf("step %d: got %v, want %v (scale %.2f)",
						i, got[i], want, scale)
				}
			}
			var sum time.Duration
			for _, d := range got {
				sum += d
			}
			// Allow 1ns of rounding drift from the float64 conversion.
			diff := sum - tc.budget
			if diff < -time.Nanosecond || diff > time.Nanosecond {
				t.Errorf("schedule sum: got %v, want %v (diff %v)",
					sum, tc.budget, diff)
			}
		})
	}
}

// TestDeriveRetrySchedule_NonPositiveBudgetFallsBack pins the defensive
// guard: a zero or negative budget produces the default schedule (not
// an empty schedule that would skip verify entirely).
func TestDeriveRetrySchedule_NonPositiveBudgetFallsBack(t *testing.T) {
	for _, budget := range []time.Duration{0, -1 * time.Second} {
		got := DeriveRetrySchedule(budget)
		want := DeriveRetrySchedule(DefaultRetryBudget)
		if len(got) != len(want) {
			t.Errorf("budget %v: len got %d, want %d", budget, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("budget %v step %d: got %v, want %v",
					budget, i, got[i], want[i])
			}
		}
	}
}

// TestSetRetrySchedule_RoundTripsPrevious pins the cleanup contract:
// SetRetrySchedule returns the prior schedule so callers (mailman
// startup, tests) can restore it. The legacy SetRetryDelaysForTest
// alias still works.
func TestSetRetrySchedule_RoundTripsPrevious(t *testing.T) {
	original := append([]time.Duration(nil), retryDelays...)
	t.Cleanup(func() { retryDelays = original })

	fresh := []time.Duration{1 * time.Second, 2 * time.Second}
	prev := SetRetrySchedule(fresh)
	if len(prev) != len(original) {
		t.Fatalf("prev len: got %d, want %d", len(prev), len(original))
	}
	if len(retryDelays) != len(fresh) {
		t.Errorf("retryDelays not replaced; len got %d, want %d",
			len(retryDelays), len(fresh))
	}

	// Legacy alias still works
	prev2 := SetRetryDelaysForTest(original)
	if len(prev2) != len(fresh) {
		t.Errorf("legacy alias: prev len got %d, want %d", len(prev2), len(fresh))
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
