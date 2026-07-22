package cli

import (
	"bytes"
	"context"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestReapCLI_RequiresDryRunOrConfirm pins the gate: exactly one of --dry-run or
// --confirm is required. Both cases return before any store is opened.
func TestReapCLI_RequiresDryRunOrConfirm(t *testing.T) {
	for _, args := range [][]string{
		{"--db", ":memory:"},                           // neither
		{"--db", ":memory:", "--dry-run", "--confirm"}, // both
	} {
		var stdout, stderr bytes.Buffer
		if exit := runReapCLI(args, &stdout, &stderr); exit != exitUsage {
			t.Errorf("args %v: exit = %d, want %d", args, exit, exitUsage)
		}
		got := parseJSONResult(t, stdout.Bytes())
		if got["ok"] != false {
			t.Errorf("args %v: ok = %v, want false", args, got["ok"])
		}
	}
}

// TestReap_DryRunDoesNotMutate: dry-run lists the fossils and touches nothing.
func TestReap_DryRunDoesNotMutate(t *testing.T) {
	s := newCmdTestStore(t, "alice", "live") // "live" has pane %99 → protected
	ctx := context.Background()
	// Two fossils to an UNREGISTERED recipient (dead), one to the live recipient.
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "deadhost", Body: "f1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "deadhost", Body: "f2"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "live", Body: "protected"})

	var stdout, stderr bytes.Buffer
	exit := runReapWithStore(ctx, s, "", "7d", true /*dryRun*/, "json", future, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", got["dry_run"])
	}
	if int(got["count"].(float64)) != 2 {
		t.Errorf("count = %v, want 2 (deadhost×2; live protected)", got["count"])
	}
	// Nothing mutated: all three rows still queued.
	q, _ := s.ListMessages(ctx, store.ListFilter{State: store.StateQueued, Limit: 10})
	if len(q) != 3 {
		t.Errorf("queued after dry-run = %d, want 3 (no mutation)", len(q))
	}
}

// TestReap_ConfirmDeadLetters: --confirm dead-letters the dead-recipient fossils
// (state→failed) and leaves the live recipient's queue intact.
func TestReap_ConfirmDeadLetters(t *testing.T) {
	s := newCmdTestStore(t, "alice", "live")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "deadhost", Body: "f1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "deadhost", Body: "f2"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "live", Body: "protected"})

	var stdout, stderr bytes.Buffer
	exit := runReapWithStore(ctx, s, "", "7d", false /*confirm*/, "json", future, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if int(got["reaped"].(float64)) != 2 {
		t.Errorf("reaped = %v, want 2", got["reaped"])
	}
	// deadhost rows are now failed (dead-lettered, not deleted).
	failed, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "deadhost", State: store.StateFailed})
	if len(failed) != 2 {
		t.Errorf("deadhost failed = %d, want 2", len(failed))
	}
	// live recipient's row is untouched (still queued).
	liveQ, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "live", State: store.StateQueued})
	if len(liveQ) != 1 {
		t.Errorf("live queued = %d, want 1 (protected)", len(liveQ))
	}
}
