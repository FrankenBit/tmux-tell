package store

import (
	"context"
	"strings"
	"testing"
)

// Fixed cutoffs that bracket every row created during the test, so the age
// clause (created_at < cutoff) is isolated from the liveness / state axes.
const (
	reapFutureCutoff = "2999-01-01T00:00:00.000Z" // every row is "older" → age clause always true
	reapPastCutoff   = "2000-01-01T00:00:00.000Z" // no row is older → age clause always false
)

// TestReapUndeliverable pins the #726 reap primitives. The load-bearing
// invariant is the LIVENESS clause: a recipient holding a live-pane registration
// (an intentional not-yet-live placeholder) is NEVER reaped, while an
// unreachable recipient (no registration, or registered without a live pane) is.
// Removing the NOT EXISTS liveness clause from reapableUndeliverablePredicate
// makes `live` reapable and flips the count 2→3 — this test then fails, which is
// the mutation-verification of the clause.
func TestReapUndeliverable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// live: registered WITH a live pane — the cabinboy case. Must be protected.
	if err := s.UpsertAgent(ctx, "live", "%2"); err != nil {
		t.Fatalf("upsert live: %v", err)
	}
	// ghost: registered but NO pane — a dead registration. Reapable.
	if err := s.UpsertAgent(ctx, "ghost", ""); err != nil {
		t.Fatalf("upsert ghost: %v", err)
	}
	// deadunreg: never registered at all (no agents row). Reapable.

	mustInsert := func(from, to, body string) {
		t.Helper()
		if _, err := s.InsertMessage(ctx, InsertParams{FromAgent: from, ToAgent: to, Body: body}); err != nil {
			t.Fatalf("insert %s→%s: %v", from, to, err)
		}
	}
	mustInsert("bosun", "live", "protected-1")      // live pane → must survive
	mustInsert("bosun", "ghost", "fossil-ghost")    // no pane → reap
	mustInsert("bosun", "deadunreg", "fossil-dead") // unregistered → reap
	mustInsert("bosun", "deadunreg", "fossil-dead2")

	// A deferred row for ghost (state=deferred) and a promoted-deferred row
	// (state=queued WITH deliver_after set) — both excluded by the state /
	// deliver_after IS NULL clauses, exactly as staleQueued excludes them.
	if _, err := s.InsertMessage(ctx, InsertParams{FromAgent: "bosun", ToAgent: "ghost", Body: "deferred", DeliverAfter: "resume"}); err != nil {
		t.Fatalf("insert deferred: %v", err)
	}
	if _, err := s.PromoteDeferred(ctx, "ghost", "resume"); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// --- dry-run list (all agents), future cutoff: age clause admits everything ---
	cands, err := s.ListReapableUndeliverable(ctx, "", reapFutureCutoff)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("ListReapableUndeliverable = %d rows, want 3 (ghost×1 + deadunreg×2; live protected, deferred+promoted excluded); got %+v", len(cands), cands)
	}
	for _, c := range cands {
		if c.ToAgent == "live" {
			t.Fatalf("LIVENESS BREACH: live-pane recipient %q in reap set (row %s) — the cabinboy hazard", c.ToAgent, c.PublicID)
		}
	}

	// --- age clause: past cutoff admits nothing ---
	old, err := s.ListReapableUndeliverable(ctx, "", reapPastCutoff)
	if err != nil {
		t.Fatalf("list past: %v", err)
	}
	if len(old) != 0 {
		t.Fatalf("past-cutoff list = %d, want 0 (age clause must exclude all)", len(old))
	}

	// --- agent scope ---
	scoped, err := s.ListReapableUndeliverable(ctx, "ghost", reapFutureCutoff)
	if err != nil {
		t.Fatalf("list scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ToAgent != "ghost" {
		t.Fatalf("agent-scoped list = %+v, want exactly ghost×1", scoped)
	}

	// --- the mutate: dead-letter, not delete ---
	const reason = "dead-letter-reap: test (#726)"
	n, err := s.ReapUndeliverable(ctx, "", reapFutureCutoff, reason)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 3 {
		t.Fatalf("ReapUndeliverable = %d, want 3", n)
	}

	// live's row must SURVIVE as queued (protected).
	liveQ, err := s.ListMessages(ctx, ListFilter{ToAgent: "live", State: StateQueued})
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	if len(liveQ) != 1 {
		t.Fatalf("live queued rows after reap = %d, want 1 (protected)", len(liveQ))
	}

	// Reaped rows are FAILED (dead-lettered), not deleted, with the reason stamped.
	failed, err := s.ListMessages(ctx, ListFilter{ToAgent: "deadunreg", State: StateFailed})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(failed) != 2 {
		t.Fatalf("deadunreg failed rows after reap = %d, want 2 (dead-lettered, not deleted)", len(failed))
	}
	for _, m := range failed {
		if !m.Error.Valid || !strings.Contains(m.Error.String, "dead-letter-reap") {
			t.Errorf("reaped row %s error = %q, want dead-letter-reap reason stamped", m.PublicID, m.Error.String)
		}
	}

	// The promoted-deferred row for ghost must survive queued (deliver_after set).
	ghostQ, err := s.ListMessages(ctx, ListFilter{ToAgent: "ghost", State: StateQueued})
	if err != nil {
		t.Fatalf("list ghost queued: %v", err)
	}
	if len(ghostQ) != 1 || !ghostQ[0].DeliverAfter.Valid {
		t.Errorf("expected 1 surviving promoted-deferred queued row for ghost, got %d: %+v", len(ghostQ), ghostQ)
	}

	// Idempotent: a second reap finds nothing (rows already left 'queued').
	n2, err := s.ReapUndeliverable(ctx, "", reapFutureCutoff, reason)
	if err != nil {
		t.Fatalf("reap 2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second ReapUndeliverable = %d, want 0 (idempotent)", n2)
	}
}
