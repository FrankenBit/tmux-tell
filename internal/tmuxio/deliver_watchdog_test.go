package tmuxio

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// Coverage for the #883 watchdog ping on the deliver path.
//
// The bug: a bus-triggered /compact SIGABRTed its own mailman. The #865
// post-compact settle budget polls for ~104s; systemd's WatchdogSec is 30s; and
// deliver.go had no ping hook at all, though observe_gate.go has had one — and
// serve.go has wired it on both observe paths — since the sibling incident.
//
// The property under test:
//
//	progress  → pings   (a slow-but-working loop stays alive)
//	wedged    → SILENT  (a blocked capture still dies)  ← the load-bearing arm
//
// ⚠️ SCOPE, measured rather than assumed. An earlier version of this comment
// claimed the arms distinguish a ping placed after the capture from one placed
// in the retry sleep. THEY DO NOT — mutating the ping into the sleep leaves
// every arm here green. The loop strictly alternates sleep → capture, so a wedge
// in either prevents reaching the other and both placements go silent together.
//
// What these arms DO pin is the real property: the ping is inside the loop and
// stops when the loop stops. The mutation that reddens them is moving the ping
// OUT of the loop — onto a timer or goroutine — which would satisfy the watchdog
// regardless of whether delivery is alive. That is the placement that must never
// happen, and it is the one worth guarding.

// blockingRunner returns a tmux runner whose capture-pane blocks until ctx is
// cancelled — a mailman genuinely wedged on tmux, not merely slow. Every other
// subcommand (the paste sequence) succeeds normally so the delivery reaches the
// verify loop before it hangs.
func blockingRunner() func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return func(ctx context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "capture-pane" {
			<-ctx.Done() // wedged: never returns on its own
			return nil, ctx.Err()
		}
		return nil, nil
	}
}

func TestDeliver_PingsOncePerVerifyPoll(t *testing.T) {
	shortRetries(t)
	var pings int64

	// capture-pane returns a frame that never satisfies the verify token, so the
	// loop runs its full budget: a slow-but-progressing delivery.
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if len(args) > 0 && args[0] == "capture-pane" {
			return []byte("nothing that verifies\n"), nil
		}
		return nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = Deliver(ctx, DeliverParams{
		Pane:        "%1",
		Body:        "hello",
		VerifyToken: "id abc123",
		Ping:        func() { atomic.AddInt64(&pings, 1) },

		PrePasteRaceCheckDisabled: true,
	})

	if got := atomic.LoadInt64(&pings); got == 0 {
		t.Fatalf("progressing verify loop pinged %d times; want >0 — the watchdog would fire mid-delivery", got)
	}
}

// 🔴 THE NEGATIVE CONTROL: a mailman blocked in tmuxRun must go SILENT, so
// systemd still kills it. Without this arm, TestDeliver_PingsOncePerVerifyPoll
// is satisfied by a ping anywhere at all — including one on an independent
// timer, which would keep a wedged process alive indefinitely.
//
// (This arm does NOT distinguish after-capture from in-sleep placement; that
// was measured and the file-header comment records why. It distinguishes
// in-loop from out-of-loop, which is the distinction that matters.)
func TestDeliver_WedgedCaptureNeverPings(t *testing.T) {
	shortRetries(t)
	var pings int64

	prev := SetTmuxRunner(blockingRunner())
	t.Cleanup(func() { SetTmuxRunner(prev) })

	// Short deadline: the wedged capture holds until this fires.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Deliver(ctx, DeliverParams{
		Pane:        "%1",
		Body:        "hello",
		VerifyToken: "id abc123",
		Ping:        func() { atomic.AddInt64(&pings, 1) },

		PrePasteRaceCheckDisabled: true,
	})

	if got := atomic.LoadInt64(&pings); got != 0 {
		t.Fatalf("wedged capture pinged %d times; want 0 — a ping from a stuck loop DISABLES the watchdog for this path", got)
	}
	if err == nil {
		t.Fatal("wedged delivery returned nil error; want the ctx failure")
	}
	// Sanity: it really did block rather than fail fast, or the arm proves nothing.
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("delivery returned after %s — it failed fast instead of blocking, so this arm did not exercise a wedged capture", elapsed)
	}
}

func TestDeliver_NilPingIsSafe(t *testing.T) {
	shortRetries(t)
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if len(args) > 0 && args[0] == "capture-pane" {
			return []byte("nothing that verifies\n"), nil
		}
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Must not panic: every call site is guarded, and tests/other callers may
	// legitimately leave Ping nil.
	_ = Deliver(ctx, DeliverParams{
		Pane:        "%1",
		Body:        "hello",
		VerifyToken: "id abc123",

		PrePasteRaceCheckDisabled: true,
	})
}

// MaxVerifySpan is what lets the mailman log both budgets next to the watchdog
// interval instead of requiring a reader to sum defaultRetryDelays by hand.
// The post-compact span exceeding WatchdogSec is the whole incident, so the
// relationship is pinned rather than left to arithmetic in a comment.
func TestMaxVerifySpan_PostCompactExceedsWatchdog(t *testing.T) {
	general, postCompact := MaxVerifySpan()

	if general <= 0 || postCompact <= 0 {
		t.Fatalf("spans must be positive; got general=%s postCompact=%s", general, postCompact)
	}
	if postCompact <= general {
		t.Fatalf("post-compact span %s must exceed general %s — the #865 extension is the point", postCompact, general)
	}

	// Mirrors `WatchdogSec=30` in init/tmux-tell-*-mailman@.service. The Go
	// identifier deliberately drops the "Sec" suffix — it is a time.Duration and
	// carries its own unit (staticcheck ST1011) — while the messages below keep
	// the systemd spelling, because that is the string a reader greps the unit
	// file for.
	const watchdogTimeout = 30 * time.Second
	if general >= watchdogTimeout {
		t.Errorf("general verify span %s >= WatchdogSec %s — the general path would now trip the watchdog too", general, watchdogTimeout)
	}
	// This is the incident, asserted rather than described: the settle budget is
	// LONGER than the watchdog, and survivable only because Deliver pings.
	if postCompact <= watchdogTimeout {
		t.Skipf("post-compact span %s no longer exceeds WatchdogSec %s — budgets changed; re-check that the ping is still needed", postCompact, watchdogTimeout)
	}
}

func TestMaxVerifySpan_EmptyScheduleIsZero(t *testing.T) {
	prev := retryDelays
	retryDelays = nil
	t.Cleanup(func() { retryDelays = prev })

	g, p := MaxVerifySpan()
	if g != 0 || p != 0 {
		t.Fatalf("empty schedule must report zero spans, got general=%s postCompact=%s", g, p)
	}
}
