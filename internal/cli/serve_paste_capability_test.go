package cli

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// withActiveProfile swaps the process-global adapter Profile for the duration
// of a test and restores it on cleanup. The serve loop's paste-capability gate
// (#323) keys on active.PasteCapable, which production sets from the launching
// binary (tmux-tell-claude vs tmux-tell-codex); in-package tests simulate a
// non-Claude adapter by swapping active here.
func withActiveProfile(t *testing.T, p Profile) {
	t.Helper()
	prev := active
	active = p
	t.Cleanup(func() { active = prev })
}

// codexLikeProfile is a paste-INcapable adapter profile mirroring
// cmd/tmux-tell-codex/main.go's Profile — the case #323 protects against.
var codexLikeProfile = Profile{
	BinaryName:   "tmux-tell-codex",
	DisplayLabel: "Codex",
	PasteCapable: false,
}

// TestServe_PasteIncapableAdapter_ForceDefers pins the #323 fix: when the
// serving adapter is paste-incapable (Codex) AND the agent's delivery_mode is
// paste-and-enter, the mailman refuses the paste loop, exits cleanly, and the
// queued message is left untouched (force-defer). This is the substrate-honest
// defense that prevents the observe-gate from clobbering operator input on a
// pane it can't classify.
func TestServe_PasteIncapableAdapter_ForceDefers(t *testing.T) {
	withActiveProfile(t, codexLikeProfile)
	// Install a fake runner that would SUCCEED delivery. With the gate working,
	// it is never reached (the short-circuit returns first). Its purpose is
	// mutation hygiene: if the gate ever stops firing, the fall-through reaches
	// this runner and the message becomes delivered — making the "stays queued"
	// assertion below fail fast and cleanly rather than hanging on real tmux.
	withSuccessfulDelivery(t)

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "lookout", "%7")
	// The misconfiguration the gate defends against: a Codex agent in
	// paste-and-enter mode (the register-time default, which is adapter-blind).
	if err := s.SetDeliveryMode(ctx, "lookout", store.DeliveryModePasteAndEnter); err != nil {
		t.Fatalf("setup delivery_mode: %v", err)
	}
	// A queued message that MUST survive untouched — force-defer, not deliver,
	// not fail.
	ins, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "quartermaster", ToAgent: "lookout", Body: "test ping",
	})
	if err != nil {
		t.Fatalf("setup message: %v", err)
	}

	opts := fastOpts("lookout")
	logbuf := &bytes.Buffer{}
	logger := log.New(logbuf, "[mailman/test] ", 0)
	// Bounded stop context: the gate makes runServeWithStore return immediately
	// (well under the bound). The bound only matters under mutation — if the
	// gate stops firing, the loop would otherwise idle-poll forever; the timeout
	// lets it return so the "stays queued" assertion can fail cleanly.
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	exit := runServeWithStore(stopCtx, s, opts, logger,
		io.Discard, io.Discard)

	if exit != exitOK {
		t.Errorf("exit = %d, want exitOK; log=%s", exit, logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "paste_incapable_adapter") {
		t.Errorf("expected paste_incapable_adapter WARN; got %s", logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "refusing paste-and-enter") {
		t.Errorf("expected refusing-paste log; got %s", logbuf.String())
	}
	// The corrective command must name the migration path so the operator isn't
	// left guessing.
	if !strings.Contains(logbuf.String(), "--delivery-mode hook-context") {
		t.Errorf("expected migration command in log; got %s", logbuf.String())
	}

	// The message stays queued — not delivered (would mean a clobbering paste
	// happened) and not failed (would mean it was dropped).
	queued, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "lookout", State: store.StateQueued, Limit: 10,
	})
	if len(queued) != 1 || queued[0].PublicID != ins.PublicID {
		t.Errorf("message %s should remain queued (force-defer); got queued=%d %+v",
			ins.PublicID, len(queued), queued)
	}
}

// TestServe_PasteIncapableAdapter_HookContextUsesExistingShortCircuit pins the
// gate's specificity: a paste-incapable adapter whose delivery_mode is already
// a non-paste mode (hook-context) does NOT trip the #323 gate — it falls
// through to the pre-existing no-paste short-circuit (#116/#249). Both exit
// cleanly, but the distinct log lines prove the new gate fires ONLY for the
// paste-and-enter misconfiguration, not for a correctly-migrated Codex agent.
func TestServe_PasteIncapableAdapter_HookContextUsesExistingShortCircuit(t *testing.T) {
	withActiveProfile(t, codexLikeProfile)

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "lookout", "%7")
	if err := s.SetDeliveryMode(ctx, "lookout", store.DeliveryModeHookContext); err != nil {
		t.Fatalf("setup delivery_mode: %v", err)
	}

	opts := fastOpts("lookout")
	logbuf := &bytes.Buffer{}
	logger := log.New(logbuf, "[mailman/test] ", 0)
	exit := runServeWithStore(context.Background(), s, opts, logger,
		io.Discard, io.Discard)

	if exit != exitOK {
		t.Errorf("exit = %d, want exitOK; log=%s", exit, logbuf.String())
	}
	// The #323 gate must NOT fire — the agent is correctly in hook-context.
	if strings.Contains(logbuf.String(), "paste_incapable_adapter") {
		t.Errorf("paste_incapable gate should NOT fire for hook-context; got %s", logbuf.String())
	}
	// The pre-existing no-paste short-circuit handles it instead.
	if !strings.Contains(logbuf.String(), "mailman does not paste") {
		t.Errorf("expected the existing no-paste short-circuit; got %s", logbuf.String())
	}
}

// TestServe_PasteCapableAdapter_GateDoesNotFire pins the negative control: the
// default (Claude) paste-capable adapter in paste-and-enter mode does NOT trip
// the #323 gate — delivery proceeds normally. Guards against the gate
// over-firing and force-deferring Claude deliveries.
func TestServe_PasteCapableAdapter_GateDoesNotFire(t *testing.T) {
	withSuccessfulDelivery(t)
	// active stays the default Claude profile (PasteCapable=true).

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	// bob defaults to paste-and-enter from schema migration.
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello",
	})

	opts := fastOpts("bob")
	stop, wait, logbuf := runServeInBackground(t, s, opts)
	waitFor(t, 2*time.Second, func() bool {
		d, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		return len(d) == 1
	}, "message should deliver for paste-capable adapter")
	stop()
	wait()

	if strings.Contains(logbuf.String(), "paste_incapable_adapter") {
		t.Errorf("paste_incapable gate must NOT fire for paste-capable Claude; got %s",
			logbuf.String())
	}
}
