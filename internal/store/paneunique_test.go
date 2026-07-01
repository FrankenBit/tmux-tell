package store

import (
	"context"
	"strings"
	"testing"
)

// #595: the UNIQUE(pane_id) index is the write-side backstop for
// one-pane-one-identity (#549). #564's tx-clear in UpsertAgent prevents
// duplicate pane_id rows in the normal register path; these tests pin that the
// schema-level constraint machine-enforces the invariant so a path BYPASSING
// that clear cannot silently create a duplicate-pane row — it fails at the DB
// layer instead of drifting (the #565 read-side warning's write-side sibling).

// The load-bearing pin. Mutation-verify: delete the `CREATE UNIQUE INDEX`
// migration in store.go and the raw UPDATE below succeeds (err == nil), so this
// test fails at the first assertion — exactly the silent duplicate the
// constraint exists to reject.
func TestUniquePaneID_RejectsBypassDuplicate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := s.UpsertAgent(ctx, "bob", ""); err != nil {
		t.Fatalf("seed bob pane-less: %v", err)
	}

	// Force a duplicate the #564 clear-then-insert would have prevented: bind
	// bob to %1 with a raw UPDATE that skips UpsertAgent's clear.
	_, err := s.DB().ExecContext(ctx, `UPDATE agents SET pane_id = '%1' WHERE name = 'bob'`)
	if err == nil {
		t.Fatal("raw UPDATE created a duplicate pane_id — UNIQUE(pane_id) index missing or ineffective")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("err = %v, want a UNIQUE constraint violation", err)
	}
}

// Multiple pane-less (NULL pane_id) rows must coexist: SQLite treats NULLs as
// distinct in a UNIQUE index, which is precisely what makes the constraint
// compatible with the displaced-to-NULL dormant-row pattern (#549 Fix-2a).
func TestUniquePaneID_MultipleNullsAllowed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, n := range []string{"a", "b", "c"} {
		if err := s.UpsertAgent(ctx, n, ""); err != nil {
			t.Fatalf("upsert pane-less %s: %v", n, err)
		}
	}
	// Displace a held pane too, so the displaced row joins the NULL set.
	if err := s.UpsertAgent(ctx, "d", "%9"); err != nil {
		t.Fatalf("seed d: %v", err)
	}
	if err := s.UpsertAgent(ctx, "e", "%9"); err != nil {
		t.Fatalf("displace d→NULL via e: %v", err)
	}
	// a, b, c, and now d are all NULL — four NULL rows coexisting, no collision.
	var nulls int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE pane_id IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nulls != 4 {
		t.Errorf("NULL pane_id rows = %d, want 4 (a,b,c,d)", nulls)
	}
}

// The #564 displaced-to-NULL register path coexists with the constraint:
// re-binding a held pane to a new name clears the old holder first, so the
// constraint never fires in the normal path.
func TestUniquePaneID_DisplacePathCoexists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := s.UpsertAgent(ctx, "bob", "%1"); err != nil {
		t.Fatalf("displace re-bind failed under constraint: %v", err)
	}
	a, err := s.GetAgent(ctx, "alice")
	if err != nil {
		t.Fatalf("get alice: %v", err)
	}
	if a.PaneID != "" {
		t.Errorf("alice pane = %q, want cleared to NULL (displaced)", a.PaneID)
	}
	b, err := s.GetAgent(ctx, "bob")
	if err != nil {
		t.Fatalf("get bob: %v", err)
	}
	if b.PaneID != "%1" {
		t.Errorf("bob pane = %q, want %%1", b.PaneID)
	}
}

// Migration self-heal: on a legacy DB that carried a pre-#564 duplicate, the
// cleanup UPDATE (which runs BEFORE the index creation in store.go) NULLs the
// displaced duplicates so CREATE UNIQUE INDEX succeeds instead of failing
// Open(). The SQL here mirrors the #595 migration pair in store.go.
func TestUniquePaneID_MigrationSelfHealsLegacyDuplicates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Reconstruct the pre-migration state: drop the index, then hand-seed a
	// duplicate-pane pair the constraint would now forbid.
	if _, err := s.DB().ExecContext(ctx, `DROP INDEX idx_agents_pane_id`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO agents (name, pane_id, updated_at)
		 VALUES ('bob', '%1', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`); err != nil {
		t.Fatalf("seed legacy duplicate: %v", err)
	}

	// Without cleanup, re-creating the unique index must fail — this is exactly
	// the Open() failure the cleanup step exists to prevent.
	if _, err := s.DB().ExecContext(ctx,
		`CREATE UNIQUE INDEX idx_agents_pane_id ON agents(pane_id)`); err == nil {
		t.Fatal("CREATE UNIQUE INDEX unexpectedly succeeded with a live duplicate present")
	}

	// The migration's cleanup step NULLs the displaced duplicates, keeping the
	// lowest-rowid survivor (mirrors the #595 cleanup migration).
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE agents SET pane_id = NULL
		 WHERE pane_id IS NOT NULL
		   AND rowid NOT IN (SELECT MIN(rowid) FROM agents WHERE pane_id IS NOT NULL GROUP BY pane_id)`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_pane_id ON agents(pane_id)`); err != nil {
		t.Fatalf("index creation after cleanup should succeed: %v", err)
	}

	// Exactly one survivor holds %1 (alice, the lowest-rowid row).
	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE pane_id = '%1'`).Scan(&n); err != nil {
		t.Fatalf("count survivors: %v", err)
	}
	if n != 1 {
		t.Errorf("survivors for %%1 = %d, want 1", n)
	}
	if a, _ := s.GetAgent(ctx, "alice"); a.PaneID != "%1" {
		t.Errorf("alice pane = %q, want %%1 (lowest-rowid survivor)", a.PaneID)
	}
}
