package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

func TestPaneConflicts_DetectsDuplicateNonEmptyPane(t *testing.T) {
	agents := []store.Agent{
		{Name: "alice", PaneID: "%5"},
		{Name: "shipwright", PaneID: "%5"}, // collides with alice on %5
		{Name: "bosun", PaneID: "%0"},      // distinct — no conflict
	}
	got := paneConflicts(agents)
	if len(got) != 1 {
		t.Fatalf("want 1 conflicted pane, got %d: %v", len(got), got)
	}
	names, ok := got["%5"]
	if !ok {
		t.Fatalf("expected %%5 to be flagged; got %v", got)
	}
	// sorted, both participants named
	if strings.Join(names, ",") != "alice,shipwright" {
		t.Errorf("conflict names = %v, want [alice shipwright] sorted", names)
	}
}

// Distinct panes and empty/NULL panes must NOT register as conflicts — a
// dormant pane-less row (the expected post-Fix-2a rebind state) is normal, and
// two pane-less rows are not a collision.
func TestPaneConflicts_NoFalsePositives(t *testing.T) {
	agents := []store.Agent{
		{Name: "alice", PaneID: "%1"},
		{Name: "bob", PaneID: "%2"},
		{Name: "dormant1", PaneID: ""}, // rebound-to-NULL: not a conflict
		{Name: "dormant2", PaneID: ""}, // two empties don't collide
	}
	if got := paneConflicts(agents); len(got) != 0 {
		t.Errorf("expected no conflicts; got %v", got)
	}
}

// End-to-end through the listing: a colliding pane flags the row's PaneConflict,
// marks the PANE cell, and emits a warning naming both agents + a recovery hint.
func TestAgentsListing_FlagsAndWarnsOnPaneConflict(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	// Seed two rows on the same pane. UpsertAgent's #549 Fix-2a supersedes at
	// register time, and #595's UNIQUE(pane_id) index now forecloses even a raw
	// bypass write — so the only way a duplicate-pane row can exist is as a
	// LEGACY row predating the index (the migration's cleanup NULLs such rows at
	// the next Open(), but until then the read-side warning is what surfaces it).
	// Reconstruct that legacy state by dropping the index before the raw seed.
	if _, err := s.DB().ExecContext(ctx, `DROP INDEX idx_agents_pane_id`); err != nil {
		t.Fatalf("drop index to reconstruct legacy dup state: %v", err)
	}
	if err := s.UpsertAgent(ctx, "alice", "%5"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO agents (name, pane_id, updated_at) VALUES ('shipwright', '%5', strftime('%Y-%m-%dT%H:%M:%fZ','now'))`); err != nil {
		t.Fatalf("seed colliding row: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if rc := runAgentsWithStore(ctx, s, map[string]bool{"%5": true}, false, "text", &stdout, &stderr); rc != exitOK {
		t.Fatalf("agents rc=%d stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "⚠") {
		t.Errorf("expected a conflict marker in the listing:\n%s", out)
	}
	if !strings.Contains(out, "shared by 2 agents") || !strings.Contains(out, "alice") || !strings.Contains(out, "shipwright") {
		t.Errorf("warning should name both agents + the shared pane:\n%s", out)
	}
	if !strings.Contains(out, "%5") {
		t.Errorf("warning should name the shared pane:\n%s", out)
	}
}

// No conflict → clean listing, no warning noise.
func TestAgentsListing_NoWarningWhenClean(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if rc := runAgentsWithStore(ctx, s, map[string]bool{"%1": true}, false, "text", &stdout, &stderr); rc != exitOK {
		t.Fatalf("agents rc=%d", rc)
	}
	if strings.Contains(stdout.String(), "⚠") {
		t.Errorf("clean listing should carry no conflict marker:\n%s", stdout.String())
	}
}
