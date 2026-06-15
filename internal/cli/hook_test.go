package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestValidDeliveryMode_HookContext pins that hook-context is a recognized mode.
func TestValidDeliveryMode_HookContext(t *testing.T) {
	if !store.ValidDeliveryMode(store.DeliveryModeHookContext) {
		t.Error("hook-context should be a valid delivery mode")
	}
}

// mustHookContext flips an agent to hook-context delivery mode, the mode under
// which doHookContext actually delivers. Production hook-context fires ONLY for
// such agents (a paste-served agent's hook no-ops, #443 Obs1); these fixtures set
// the mode explicitly so they model that real scenario rather than relying on the
// migration's paste-and-enter default.
func mustHookContext(t *testing.T, s *store.Store, agent string) {
	t.Helper()
	if err := s.SetDeliveryMode(context.Background(), agent, store.DeliveryModeHookContext); err != nil {
		t.Fatalf("set hook-context mode for %q: %v", agent, err)
	}
}

// TestDoHookContext_PasteServedAgent_NoOp pins #443 Obs1: when a codex agent's
// delivery_mode was flipped to paste-and-enter (the mailman pastes) but a stale
// ~/.codex/config.toml hook block still fires hook-context, the hook MUST NOT
// claim or deliver the message — the mailman's paste is the single delivery.
// Otherwise both paths consume the same queued message → duplicate arrival at the
// chamber (invisible to the bus DB, which records one clean delivered_at). The
// load-bearing assertion is that the message stays QUEUED (unconsumed by the hook)
// plus a greppable WARN surfaces the stale-toml condition to the substrate.
func TestDoHookContext_PasteServedAgent_NoOp(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3") // default delivery_mode = paste-and-enter
	r, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "should stay queued"})

	var stderr bytes.Buffer
	out, presented, err := doHookContext(ctx, s, "bob", "UserPromptSubmit", &stderr)
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if presented != 0 || out.HookSpecificOutput != nil {
		t.Errorf("paste-served agent = presented %d / out %+v, want 0 + no hookSpecificOutput (no-op)", presented, out)
	}
	// The message must remain unconsumed by the hook so the paste path delivers it.
	m, _ := s.GetMessage(ctx, r.PublicID)
	if m.State != store.StateQueued {
		t.Errorf("message state = %s, want %s (hook must not consume a paste-served agent's message)", m.State, store.StateQueued)
	}
	// Substrate-observable WARN (user-silent, journal-greppable).
	if !strings.Contains(stderr.String(), "hook_context_skipped_paste_mode") {
		t.Errorf("expected WARN hook_context_skipped_paste_mode; got stderr:\n%s", stderr.String())
	}
}

// TestDoHookContext_PresentsAndMarksDelivered is the #249 round-trip core: a
// hook-context agent's pending messages are presented as additionalContext and
// marked delivered (= presented, ADR-0009 3b).
func TestDoHookContext_PresentsAndMarksDelivered(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	mustHookContext(t, s, "bob")
	r1, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "first question"})
	r2, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "carol", ToAgent: "bob", Body: "second question"})

	out, presented, err := doHookContext(ctx, s, "bob", "UserPromptSubmit", io.Discard)
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
	mustHookContext(t, s, "bob")

	out, presented, err := doHookContext(ctx, s, "bob", "SessionStart", io.Discard)
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
	mustHookContext(t, s, "bob")
	r, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "stuck"})
	// Simulate a crashed hook: claim (→ delivering) but never mark.
	if _, err := s.ClaimNext(ctx, "bob"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	out, presented, err := doHookContext(ctx, s, "bob", "UserPromptSubmit", io.Discard)
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
	mustHookContext(t, s, "bob")
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "bob", ToAgent: "bob", Body: "staged", DeliverAfter: "resume"})

	_, presented, err := doHookContext(ctx, s, "bob", "UserPromptSubmit", io.Discard)
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

// TestRunHookContextCLI_UnregisteredFromAgentErrors pins #361: passing --from
// with an agent name that is not in the registry must fail with a non-zero exit
// and a JSON error body, not silently no-op (which would mask misconfiguration).
func TestRunHookContextCLI_UnregisteredFromAgentErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "messages.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Deliberately do NOT register "ghost" — it must be treated as unregistered.
	s.Close()

	var stdout, stderr bytes.Buffer
	exit := runHookContextCLI(
		[]string{"--db", db, "--from", "ghost"},
		nil, &stdout, &stderr)
	if exit == exitOK {
		t.Fatalf("expected non-zero exit for unregistered --from agent; stdout=%s", stdout.String())
	}
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(out.Error, "ghost") || !strings.Contains(out.Error, "not registered") {
		t.Errorf("error message should mention the agent name and 'not registered'; got %q", out.Error)
	}
}

// TestRunHookContextCLI_EventNameOverride pins that --event-name overrides the
// stdin-derived hook_event_name in the emitted hookSpecificOutput.hookEventName.
// This is the deterministic-event-name seam (#248): some CLIs (Codex) require
// the output's hookEventName to match the firing event but don't document their
// stdin schema, so the operator pins it in the hook command. The override wins
// even when stdin carries a different event name.
func TestRunHookContextCLI_EventNameOverride(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "messages.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_ = s.UpsertAgent(ctx, "bob", "%3")
	mustHookContext(t, s, "bob")
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "ping"}); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	s.Close()

	var stdout, stderr bytes.Buffer
	// stdin says UserPromptSubmit; --event-name pins SessionStart — the override wins.
	exit := runHookContextCLI(
		[]string{"--db", db, "--from", "bob", "--event-name", "SessionStart"},
		strings.NewReader(`{"hook_event_name":"UserPromptSubmit"}`),
		&stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", exit, exitOK, stderr.String())
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, stdout.String())
	}
	if got := out.HookSpecificOutput.HookEventName; got != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart (--event-name overrides stdin)", got)
	}
	if !strings.Contains(out.HookSpecificOutput.AdditionalContext, "ping") {
		t.Errorf("additionalContext missing the message body: %q", out.HookSpecificOutput.AdditionalContext)
	}
}
