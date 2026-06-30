package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// Self-report round-trip: resolveMCPIdentity resolves the caller via $TMUX_PANE,
// setMetabolism stores it (with a stamp), and the clear path empties both.
func TestSetMetabolism_SelfRoundTrip(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%4")

	caller, err := resolveMCPIdentity(ctx, s)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	res, err := setMetabolism(ctx, s, caller, "saturating")
	if err != nil {
		t.Fatalf("setMetabolism: %v", err)
	}
	if !res.OK || res.Agent != "engineer" || res.Metabolism != "saturating" {
		t.Errorf("result = %+v", res)
	}
	if res.MetabolismSetAt == "" {
		t.Error("MetabolismSetAt empty, want a stamp")
	}

	res, err = setMetabolism(ctx, s, caller, "")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if res.Metabolism != "" || res.MetabolismSetAt != "" {
		t.Errorf("after clear: %+v", res)
	}
}

// Self-only: with two agents registered and the caller resolved to engineer (via
// $TMUX_PANE), pilot's metabolism is untouched. There is no target parameter, so
// a peer write is unexpressible; this pins that the write is keyed to the
// resolved self — #621 AC#2 (a third-party write would clobber pilot's real
// signal).
func TestSetMetabolism_SelfOnly_PeerUntouched(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.UpsertAgent(ctx, "pilot", "%5"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%4") // the caller IS engineer

	caller, err := resolveMCPIdentity(ctx, s)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if caller != "engineer" {
		t.Fatalf("resolved caller = %q, want engineer", caller)
	}
	if _, err := setMetabolism(ctx, s, caller, "compact-pending"); err != nil {
		t.Fatalf("setMetabolism: %v", err)
	}
	pilot, _ := s.GetAgent(ctx, "pilot")
	if pilot.Metabolism != "" {
		t.Errorf("peer pilot.metabolism = %q, want empty (self-only)", pilot.Metabolism)
	}
	eng, _ := s.GetAgent(ctx, "engineer")
	if eng.Metabolism != "compact-pending" {
		t.Errorf("self engineer.metabolism = %q, want compact-pending", eng.Metabolism)
	}
}

// agent_state surfaces the self-reported metabolism ALONGSIDE the observed state,
// on a distinct field (AC#5 orthogonality). A mailbox-only agent short-circuits
// the observed probe to "idle" (no tmux), isolating the two axes: observed=idle,
// self-reported=compact-pending, both present — and the self-report does NOT
// masquerade as an observed (paste-unsafe) state.
func TestResolveAgentState_SurfacesMetabolismOrthogonally(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SetDeliveryMode(ctx, "engineer", store.DeliveryModeMailboxOnly); err != nil {
		t.Fatalf("mode: %v", err)
	}
	if err := s.SetMetabolism(ctx, "engineer", store.MetabolismCompactPending); err != nil {
		t.Fatalf("set: %v", err)
	}

	res, err := resolveAgentState(ctx, s, "engineer")
	if err != nil {
		t.Fatalf("resolveAgentState: %v", err)
	}
	if res.State != tmuxio.StateIdle.String() {
		t.Errorf("observed State = %q, want idle (a self-report must NOT change the observed state)", res.State)
	}
	if res.Metabolism != store.MetabolismCompactPending {
		t.Errorf("Metabolism = %q, want compact-pending (surfaced alongside observed)", res.Metabolism)
	}
	if res.MetabolismSetAt == "" {
		t.Error("MetabolismSetAt empty in agent_state, want a stamp")
	}
	// The observed state is paste-SAFE — the self-report never enters the
	// delivery-gate predicate (IsPasteUnsafe reads the observed State only).
	if tmuxio.IsPasteUnsafe(tmuxio.StateIdle) {
		t.Fatal("precondition: idle must be paste-safe — self-report cannot gate delivery")
	}
}

// maybeAutoClearMetabolism (the serve observe-loop hook) clears compact-pending
// ONLY when the observed state is at-rest-in-compaction, and never clobbers
// warming/saturating. Mutation anchor for the observed-supersedes-self-report
// wiring: change the state compared (set_metabolism.go) or drop the store's
// compact-pending WHERE-guard, and an assert flips.
func TestMaybeAutoClearMetabolism(t *testing.T) {
	ctx := context.Background()
	allStates := []tmuxio.State{
		tmuxio.StateUnknown, tmuxio.StateIdle, tmuxio.StateWorking,
		tmuxio.StateAtRestInCompaction, tmuxio.StateAwaitingOperator,
		tmuxio.StateInCopyMode, tmuxio.StateRateLimited, tmuxio.StateUsageLimited,
	}
	for _, observed := range allStates {
		s := newCmdTestStore(t)
		if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := s.SetMetabolism(ctx, "engineer", store.MetabolismCompactPending); err != nil {
			t.Fatalf("set: %v", err)
		}
		if err := maybeAutoClearMetabolism(ctx, s, "engineer", observed); err != nil {
			t.Fatalf("autoclear(%v): %v", observed, err)
		}
		a, _ := s.GetAgent(ctx, "engineer")
		wantCleared := observed == tmuxio.StateAtRestInCompaction
		if wantCleared && a.Metabolism != "" {
			t.Errorf("observed=%v: compact-pending NOT cleared (got %q)", observed, a.Metabolism)
		}
		if !wantCleared && a.Metabolism != store.MetabolismCompactPending {
			t.Errorf("observed=%v: compact-pending wrongly cleared", observed)
		}
	}

	// warming is never superseded — only compact-pending auto-clears.
	s := newCmdTestStore(t)
	_ = s.UpsertAgent(ctx, "engineer", "%4")
	_ = s.SetMetabolism(ctx, "engineer", store.MetabolismWarming)
	if err := maybeAutoClearMetabolism(ctx, s, "engineer", tmuxio.StateAtRestInCompaction); err != nil {
		t.Fatalf("autoclear: %v", err)
	}
	a, _ := s.GetAgent(ctx, "engineer")
	if a.Metabolism != store.MetabolismWarming {
		t.Errorf("warming clobbered by at-rest observation: got %q", a.Metabolism)
	}
}

// MCP round-trip + invalid-rejection through the tool surface. The handler
// resolves self (no target field in the input) and shares the setMetabolism
// core, so the wire shape matches the CLI.
func TestMCP_SetMetabolism(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "engineer", "%4")
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%4")

	got := callMCPTool(t, s, "tmux-tell.set_metabolism", map[string]any{"value": "warming"})
	if got["_isError"] == true {
		t.Fatalf("unexpected MCP error: %v", got)
	}
	if got["ok"] != true || got["agent"] != "engineer" || got["metabolism"] != "warming" {
		t.Errorf("got %v", got)
	}
	a, _ := s.GetAgent(ctx, "engineer")
	if a.Metabolism != "warming" {
		t.Errorf("stored = %q, want warming", a.Metabolism)
	}

	// Invalid value rejected at the surface; the prior valid value is unchanged.
	bad := callMCPTool(t, s, "tmux-tell.set_metabolism", map[string]any{"value": "compacting"})
	if bad["_isError"] != true {
		t.Errorf("invalid value accepted: %v", bad)
	}
	a, _ = s.GetAgent(ctx, "engineer")
	if a.Metabolism != "warming" {
		t.Errorf("rejected write changed the row: %q", a.Metabolism)
	}
}

// The agents text listing carries a METABOLISM column with the legend emoji.
func TestAgentsListing_ShowsMetabolismColumn(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "engineer", "%4")
	_ = s.SetMetabolism(ctx, "engineer", store.MetabolismSaturating)

	var stdout, stderr bytes.Buffer
	if rc := runAgentsWithStore(ctx, s, map[string]bool{"%4": true}, false, "text", &stdout, &stderr); rc != exitOK {
		t.Fatalf("agents rc=%d stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !bytes.Contains(stdout.Bytes(), []byte("METABOLISM")) {
		t.Errorf("listing missing METABOLISM header:\n%s", out)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("saturating")) ||
		!bytes.Contains(stdout.Bytes(), []byte(store.MetabolismEmoji[store.MetabolismSaturating])) {
		t.Errorf("listing missing the metabolism value+emoji:\n%s", out)
	}
}

// The CLI runner: a positional value self-reports, and --clear retracts it. Uses
// a temp-file DB (the runner opens its own store) so the assertions read what the
// runner wrote.
func TestRunSetMetabolismCLI(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	seed, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	if err := seed.UpsertAgent(context.Background(), "engineer", "%4"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = seed.Close()
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%4")

	var stdout, stderr bytes.Buffer
	if rc := runSetMetabolismCLI([]string{"--db", dbPath, "warming"}, &stdout, &stderr); rc != exitOK {
		t.Fatalf("set rc=%d stderr=%s", rc, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("warming")) {
		t.Errorf("set output missing value: %s", stdout.String())
	}
	check, _ := store.Open(dbPath)
	a, _ := check.GetAgent(context.Background(), "engineer")
	if a.Metabolism != "warming" {
		t.Errorf("after CLI set: metabolism = %q, want warming", a.Metabolism)
	}
	_ = check.Close()

	stdout.Reset()
	if rc := runSetMetabolismCLI([]string{"--db", dbPath, "--clear"}, &stdout, &stderr); rc != exitOK {
		t.Fatalf("clear rc=%d stderr=%s", rc, stderr.String())
	}
	check2, _ := store.Open(dbPath)
	a, _ = check2.GetAgent(context.Background(), "engineer")
	if a.Metabolism != "" {
		t.Errorf("after CLI --clear: metabolism = %q, want empty", a.Metabolism)
	}
	_ = check2.Close()
}
