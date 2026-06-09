package cli

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// TestValidDeliveryMode_HookContext pins that hook-context is a recognized mode.
func TestValidDeliveryMode_HookContext(t *testing.T) {
	if !store.ValidDeliveryMode(store.DeliveryModeHookContext) {
		t.Error("hook-context should be a valid delivery mode")
	}
}

// TestDoHookContext_PresentsAndMarksDelivered is the #249 round-trip core: a
// hook-context agent's pending messages are presented as additionalContext and
// marked delivered (= presented, ADR-0009 3b).
func TestDoHookContext_PresentsAndMarksDelivered(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	r1, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "first question"})
	r2, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "carol", ToAgent: "bob", Body: "second question"})

	out, presented, err := doHookContext(ctx, s, "bob", "UserPromptSubmit")
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if presented != 2 {
		t.Fatalf("presented = %d, want 2", presented)
	}
	if out.HookSpecificOutput == nil {
		t.Fatal("expected hookSpecificOutput")
	}
	ac := out.HookSpecificOutput.AdditionalContext
	// Senders render title-cased (chrome convention, #249 N3): Alice / Carol.
	for _, want := range []string{"first question", "second question", "Alice", "Carol", "2 messages"} {
		if !strings.Contains(ac, want) {
			t.Errorf("additionalContext missing %q; got:\n%s", want, ac)
		}
	}
	if out.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want UserPromptSubmit", out.HookSpecificOutput.HookEventName)
	}
	// Both marked delivered + verified (presented = confirmed).
	for _, id := range []string{r1.PublicID, r2.PublicID} {
		m, _ := s.GetMessage(ctx, id)
		if m.State != store.StateDelivered || !m.Verified.Valid || m.Verified.Int64 != 1 {
			t.Errorf("message %s = state %s verified %+v, want delivered + verified=1", id, m.State, m.Verified)
		}
	}
}

// TestDoHookContext_NoMessages_NoOp: an empty inbox yields an empty hookOutput
// (no hookSpecificOutput) — a clean no-op hook fire.
func TestDoHookContext_NoMessages_NoOp(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")

	out, presented, err := doHookContext(ctx, s, "bob", "SessionStart")
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if presented != 0 || out.HookSpecificOutput != nil {
		t.Errorf("empty inbox = presented %d / out %+v, want 0 + no hookSpecificOutput", presented, out)
	}
}

// TestDoHookContext_RecoversStuckDelivering: a message left in `delivering` by a
// crashed prior hook (no mailman runs for hook-context) is recovered + presented.
func TestDoHookContext_RecoversStuckDelivering(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	r, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "stuck"})
	// Simulate a crashed hook: claim (→ delivering) but never mark.
	if _, err := s.ClaimNext(ctx, "bob"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	out, presented, err := doHookContext(ctx, s, "bob", "UserPromptSubmit")
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if presented != 1 || out.HookSpecificOutput == nil ||
		!strings.Contains(out.HookSpecificOutput.AdditionalContext, "stuck") {
		t.Errorf("stuck message not recovered+presented; presented=%d out=%+v", presented, out)
	}
	m, _ := s.GetMessage(ctx, r.PublicID)
	if m.State != store.StateDelivered {
		t.Errorf("recovered message state = %s, want delivered", m.State)
	}
}

// TestDoHookContext_HonorsDeferred: a deferred message (#227) is NOT presented —
// the hook honors the same staging rules as the pane path (ClaimNext excludes
// deferred).
func TestDoHookContext_HonorsDeferred(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "bob", ToAgent: "bob", Body: "staged", DeliverAfter: "resume"})

	_, presented, err := doHookContext(ctx, s, "bob", "UserPromptSubmit")
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if presented != 0 {
		t.Errorf("deferred message should not be presented; presented=%d", presented)
	}
}

// TestServe_HookContextShortCircuits: the mailman exits cleanly (no paste loop)
// for a hook-context agent, mirroring mailbox-only.
func TestServe_HookContextShortCircuits(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if err := s.SetDeliveryMode(ctx, "bob", store.DeliveryModeHookContext); err != nil {
		t.Fatalf("set mode: %v", err)
	}

	var logbuf bytes.Buffer
	logger := log.New(&logbuf, "[mailman/test] ", 0)
	exit := runServeWithStore(context.Background(), s, fastOpts("bob"), logger, &bytes.Buffer{}, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d, want exitOK (clean short-circuit)", exit)
	}
	if !strings.Contains(logbuf.String(), "delivery_mode=hook-context — mailman does not paste") {
		t.Errorf("expected hook-context short-circuit log; got %s", logbuf.String())
	}
}
