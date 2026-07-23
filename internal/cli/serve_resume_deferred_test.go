package cli

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// resumeDeferredRunner installs the shared deliverRunner fake plus a collapsed
// settle delay, so a test can drive BOTH a control row (send-keys, no verify)
// and a message row (load-buffer + capture-pane echo so the verify token is
// found) in one serve run without paying 500ms per delivery.
func resumeDeferredRunner(t *testing.T) {
	t.Helper()
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })

	var (
		bodyMu   sync.Mutex
		body     string
		paneSeen atomic.Value
	)
	prev := tmuxio.SetTmuxRunner(deliverRunner(&bodyMu, &body, &paneSeen))
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
}

// stateOf returns the current state of a single message by public id.
func stateOf(t *testing.T, s *store.Store, publicID string) store.State {
	t.Helper()
	ctx := context.Background()
	// #227 deferred rows are hidden from the default all-states view, so a
	// helper that must observe the deferred→queued→delivered transition has to
	// look in both views.
	for _, f := range []store.ListFilter{{Limit: 100}, {Deferred: true, Limit: 100}} {
		msgs, err := s.ListMessages(ctx, f)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, m := range msgs {
			if m.PublicID == publicID {
				return m.State
			}
		}
	}
	t.Fatalf("message %s not found", publicID)
	return ""
}

// waitForState polls until the given message reaches want, or fails on timeout.
func waitForState(t *testing.T, s *store.Store, publicID string, want store.State) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if stateOf(t, s, publicID) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("message %s state = %q, want %q (timed out)", publicID, stateOf(t, s, publicID), want)
}

// TestServe_ResumeDeferred_AutoFiresOnSessionReset is the #843 regression guard:
// a self-handoff staged with deliver_after=resume must be delivered after the
// mailman delivers a bus /compact — WITHOUT the chamber calling
// flush_deferred{resume}.
//
// This is the empirically-anchored failure: bus id b2f0 was staged at
// 2026-07-23T12:37:38 with deliver_after=resume, the chamber's own /compact
// control row (e98d) was delivered at 12:38:11, and b2f0 sat in `deferred`
// indefinitely because PromoteDeferred had no caller on the serve path — only
// the register trigger ever got an auto-fire (#258a). The staged row is
// inserted BEFORE the /compact so its created_at ordering also proves the
// handoff lands ahead of later traffic.
func TestServe_ResumeDeferred_AutoFiresOnSessionReset(t *testing.T) {
	resumeDeferredRunner(t)

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "quartermaster", "%1")

	staged, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "quartermaster", ToAgent: "quartermaster",
		Body: "post-/compact orientation: pick up the #843 thread", Kind: store.KindMessage,
		DeliverAfter: deferTriggerResume,
	})
	if err != nil {
		t.Fatalf("insert staged: %v", err)
	}
	if got := stateOf(t, s, staged.PublicID); got != store.StateDeferred {
		t.Fatalf("staged row state = %q, want %q (precondition: it must start deferred)",
			got, store.StateDeferred)
	}

	compact, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "quartermaster", ToAgent: "quartermaster",
		Body: "/compact", Kind: store.KindControl,
	})
	if err != nil {
		t.Fatalf("insert compact: %v", err)
	}

	stop, wait, logbuf := runServeInBackground(t, s, fastOpts("quartermaster"))
	waitForState(t, s, staged.PublicID, store.StateDelivered)
	stop()
	wait()

	if got := stateOf(t, s, compact.PublicID); got != store.StateDelivered {
		t.Errorf("/compact row state = %q, want delivered", got)
	}
	if !strings.Contains(logbuf.String(), "resume_deferred_promoted") {
		t.Errorf("expected resume_deferred_promoted log line; got %s", logbuf.String())
	}
}

// TestServe_ResumeDeferred_NotPromotedByOrdinaryDelivery is the over-fire guard:
// delivering a NORMAL message must NOT flush the chamber's resume-staged rows.
// Only a session reset (/compact or /clear) is the "chamber came back" edge — an
// ordinary bus arrival is exactly what woke the b2f0 chamber WITHOUT its staged
// handoff being due (external wake c4ec). Promoting here would dump the staged
// orientation into the pre-compaction context, which is the very thing
// deliver_after=resume exists to avoid.
func TestServe_ResumeDeferred_NotPromotedByOrdinaryDelivery(t *testing.T) {
	resumeDeferredRunner(t)

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%2")
	_ = s.UpsertAgent(ctx, "quartermaster", "%1")

	staged, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "quartermaster", ToAgent: "quartermaster",
		Body: "staged orientation", Kind: store.KindMessage,
		DeliverAfter: deferTriggerResume,
	})
	if err != nil {
		t.Fatalf("insert staged: %v", err)
	}

	ordinary, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "quartermaster", Body: "an ordinary bus message",
	})
	if err != nil {
		t.Fatalf("insert ordinary: %v", err)
	}

	stop, wait, _ := runServeInBackground(t, s, fastOpts("quartermaster"))
	waitForState(t, s, ordinary.PublicID, store.StateDelivered)
	// Give the loop several more iterations to (wrongly) promote before asserting.
	time.Sleep(50 * time.Millisecond)
	stop()
	wait()

	if got := stateOf(t, s, staged.PublicID); got != store.StateDeferred {
		t.Errorf("staged row state = %q, want %q — an ordinary delivery must NOT "+
			"promote resume-staged rows (#843 over-fire guard)", got, store.StateDeferred)
	}
}

// TestServe_ResumeDeferred_DoesNotPromoteRegisterTrigger pins the trigger
// scoping: a /compact fires the `resume` trigger only. A row staged for
// `register` (#258a's spawn-die session bridge) must stay deferred until its own
// trigger — the (re)register — fires. Guards against the promote being wired
// with the wrong trigger constant, which would both miss the resume rows and
// wrongly drain the register ones.
func TestServe_ResumeDeferred_DoesNotPromoteRegisterTrigger(t *testing.T) {
	resumeDeferredRunner(t)

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bosun", "%2")
	_ = s.UpsertAgent(ctx, "quartermaster", "%1")

	registerStaged, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "bosun", ToAgent: "quartermaster",
		Body: "remember this for your next dispatch", Kind: store.KindMessage,
		DeliverAfter: deferTriggerRegister,
	})
	if err != nil {
		t.Fatalf("insert register-staged: %v", err)
	}

	compact, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "quartermaster", ToAgent: "quartermaster",
		Body: "/compact", Kind: store.KindControl,
	})
	if err != nil {
		t.Fatalf("insert compact: %v", err)
	}

	stop, wait, _ := runServeInBackground(t, s, fastOpts("quartermaster"))
	waitForState(t, s, compact.PublicID, store.StateDelivered)
	time.Sleep(50 * time.Millisecond)
	stop()
	wait()

	if got := stateOf(t, s, registerStaged.PublicID); got != store.StateDeferred {
		t.Errorf("register-staged row state = %q, want %q — a /compact fires the "+
			"resume trigger only (#843 trigger-scoping guard)", got, store.StateDeferred)
	}
}
