package store

import (
	"context"
	"path/filepath"
	"testing"
)

// #721: agent identity is keyed on the CANONICAL (lower+trim) form of the name,
// so a chamber that (re)registers under a different casing than a prior run does
// not spawn a phantom second identity that shadows the live one and silently
// misroutes deferred/addressed messages. These tests pin both facets: the
// routing-key canonicalization at the store boundary (facet 1) and the one-time
// normalization migration that heals pre-existing mixed-case rows (facet 2).

// reopenableStore returns a FILE-BACKED store (and its path) so a test can Close
// and re-Open it — the way to re-run the idempotent migrations against rows a
// test seeded, which the shared :memory: newTestStore cannot do.
func reopenableStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "canon.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestCanonicalName(t *testing.T) {
	cases := map[string]string{
		"quartermaster":     "quartermaster",
		"Quartermaster":     "quartermaster",
		"QUARTERMASTER":     "quartermaster",
		"  Quartermaster  ": "quartermaster",
		"QM":                "qm",
		"":                  "",
		"   ":               "",
	}
	for in, want := range cases {
		if got := CanonicalName(in); got != want {
			t.Errorf("CanonicalName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUpsertAgent_CanonicalizesName: a register under any casing stores + is
// resolvable under the canonical key, and a lookup under any casing finds it.
// Mutation-verify: drop the `name = CanonicalName(name)` line in GetAgent and
// the mixed-case GetAgent below returns ErrNotFound instead of the row.
func TestUpsertAgent_CanonicalizesName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "Quartermaster", "%10"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, lookup := range []string{"quartermaster", "Quartermaster", "QUARTERMASTER", "  quartermaster "} {
		a, err := s.GetAgent(ctx, lookup)
		if err != nil {
			t.Fatalf("GetAgent(%q): %v", lookup, err)
		}
		if a.Name != "quartermaster" {
			t.Errorf("GetAgent(%q).Name = %q, want canonical %q", lookup, a.Name, "quartermaster")
		}
	}
	all, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("agent count = %d, want exactly one canonical row", len(all))
	}
}

// TestUpsertAgent_CaseVariantReregisterCollapsesToOneRow is the incident repro
// (2026-07-06): a chamber registered under "quartermaster" (%13, stale) then
// re-registered under "Quartermaster" (%10, live), and the two coexisted as
// separate identities — the live one shadowed by the stale one. With
// canonicalization the re-register targets the SAME primary key, so the pane
// simply moves and exactly one identity survives, pointing at the live pane.
func TestUpsertAgent_CaseVariantReregisterCollapsesToOneRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "quartermaster", "%13"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := s.UpsertAgent(ctx, "Quartermaster", "%10"); err != nil {
		t.Fatalf("re-register under different casing: %v", err)
	}
	all, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("agent count = %d, want exactly one (no case-variant shadow)", len(all))
	}
	if all[0].Name != "quartermaster" || all[0].PaneID != "%10" {
		t.Errorf("survivor = %q@%q, want the live canonical quartermaster@%%10", all[0].Name, all[0].PaneID)
	}
}

// TestReservedRoutingName_CanonicalBlocksCaseVariant: the reserved-name guard is
// case-insensitive, so "Operator" / " operator " cannot slip past it and shadow
// the operator routing primitive.
func TestReservedRoutingName_CanonicalBlocksCaseVariant(t *testing.T) {
	for _, n := range []string{"operator", "Operator", "OPERATOR", "  operator  "} {
		if !ReservedRoutingName(n) {
			t.Errorf("ReservedRoutingName(%q) = false, want true", n)
		}
	}
	s := newTestStore(t)
	if err := s.UpsertAgent(context.Background(), "Operator", "%1"); err == nil {
		t.Error("UpsertAgent(\"Operator\") succeeded, want ErrReservedRoutingName")
	}
}

// TestPromoteDeferred_CaseInsensitive is the deferred-layer anchor: a message
// staged under one casing promotes when its recipient (re)registers/flushes
// under any casing — the exact path deliver_after=register exists for. Before
// the fix, PromoteDeferred's `to_agent = ?` exact match missed the case variant
// and the staged row sat in `deferred` forever.
// Mutation-verify: drop canonicalization in PromoteDeferred and/or InsertMessage
// and the cross-case promote returns 0.
func TestPromoteDeferred_CaseInsensitive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Staged (deferred) under mixed-case recipient — stored canonical at insert.
	if _, err := s.InsertMessage(ctx, InsertParams{
		FromAgent:    "surveyor",
		ToAgent:      "Quartermaster",
		Body:         "review verdict",
		DeliverAfter: "register",
	}); err != nil {
		t.Fatalf("staged insert: %v", err)
	}
	// Promote under a DIFFERENT casing than was staged.
	n, err := s.PromoteDeferred(ctx, "quartermaster", "register")
	if err != nil {
		t.Fatalf("PromoteDeferred: %v", err)
	}
	if n != 1 {
		t.Fatalf("promoted %d, want 1 (case-variant deferred row must promote)", n)
	}
	depth, err := s.RecipientQueueDepth(ctx, "QUARTERMASTER")
	if err != nil {
		t.Fatalf("RecipientQueueDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("queue depth = %d, want 1 (promoted row is now deliverable under any casing)", depth)
	}
}

// TestRouting_CaseInsensitive_ImmediateSendThenClaim is the immediate-delivery
// anchor: a message sent to one casing is claimable by the mailman running under
// the recipient's canonical name. Before the fix a send to "Quartermaster" and a
// mailman claiming as "quartermaster" never met.
func TestRouting_CaseInsensitive_ImmediateSendThenClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "quartermaster", "%10"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.InsertMessage(ctx, InsertParams{FromAgent: "Bosun", ToAgent: "Quartermaster", Body: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	m, err := s.ClaimNext(ctx, "quartermaster")
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if m == nil {
		t.Fatal("ClaimNext returned nil — a message sent to a case-variant recipient was not routed")
	}
	if m.ToAgent != "quartermaster" || m.FromAgent != "bosun" {
		t.Errorf("claimed msg from/to = %q/%q, want canonical bosun/quartermaster", m.FromAgent, m.ToAgent)
	}
}

// TestMigration_CanonicalizesLegacyMixedCaseRows pins facet 2 on a DB that
// already holds a pre-#721 mixed-case row: reopening runs the normalization
// migration, which renames the agent row to its canonical key and re-points its
// PENDING messages so nothing is stranded under the old casing.
// Mutation-verify: delete the `UPDATE agents SET name = trim(lower(name))`
// migration statement and the post-reopen GetAgent("admin") returns ErrNotFound.
func TestMigration_CanonicalizesLegacyMixedCaseRows(t *testing.T) {
	s, path := reopenableStore(t)
	ctx := context.Background()

	// Seed a pre-fix state RAW so the canonicalizing write path doesn't touch it:
	// an agent stored as "Admin" plus a queued message addressed to "Admin".
	if err := s.UpsertAgent(ctx, "admin", "%11"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE agents SET name = 'Admin' WHERE name = 'admin'`); err != nil {
		t.Fatalf("de-canonicalize agent row: %v", err)
	}
	res, err := s.InsertMessage(ctx, InsertParams{FromAgent: "bosun", ToAgent: "admin", Body: "queued for admin"})
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE messages SET to_agent = 'Admin' WHERE public_id = ?`, res.PublicID); err != nil {
		t.Fatalf("de-canonicalize message: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen → the #721 normalization migration runs against the legacy rows.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	if _, err := s2.GetAgent(ctx, "admin"); err != nil {
		t.Errorf("GetAgent(\"admin\") after migration: %v — legacy row not renamed to canonical", err)
	}
	var mixed int
	if err := s2.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE name = 'Admin'`).Scan(&mixed); err != nil {
		t.Fatalf("count mixed-case rows: %v", err)
	}
	if mixed != 0 {
		t.Errorf("legacy 'Admin' agent row survived migration (count=%d)", mixed)
	}
	depth, err := s2.RecipientQueueDepth(ctx, "admin")
	if err != nil {
		t.Fatalf("RecipientQueueDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("pending message to 'Admin' not re-pointed to canonical 'admin' (depth=%d)", depth)
	}
}

// TestMigration_CollapsesCaseVariantDuplicateRows pins the collapse-before-rename
// ordering: a legacy DB carrying BOTH "Admin" and "admin" rows must not crash on
// reopen (the bare `SET name = lower(name)` would hit a PRIMARY KEY collision).
// The collapse keeps the newest (live) row and drops the stale one.
// Mutation-verify: delete the collapse `DELETE FROM agents ...` migration
// statement and reopen fails with a UNIQUE/PRIMARY KEY constraint error.
func TestMigration_CollapsesCaseVariantDuplicateRows(t *testing.T) {
	s, path := reopenableStore(t)
	ctx := context.Background()

	// Live canonical row (fresh updated_at via UpsertAgent).
	if err := s.UpsertAgent(ctx, "admin", "%10"); err != nil {
		t.Fatalf("seed live admin: %v", err)
	}
	// Stale case-variant row with an OLDER updated_at + a distinct pane (so the
	// #595 UNIQUE(pane_id) index doesn't reject the seed itself).
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO agents (name, pane_id, updated_at) VALUES ('Admin', '%13', '2020-01-01T00:00:00.000Z')`); err != nil {
		t.Fatalf("seed stale Admin: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen must succeed (collapse runs before the rename → no PK collision).
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen failed — collapse-before-rename ordering broken: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	all, err := s2.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("agent count after collapse = %d, want 1", len(all))
	}
	if all[0].Name != "admin" || all[0].PaneID != "%10" {
		t.Errorf("survivor = %q@%q, want the live admin@%%10 (newest updated_at)", all[0].Name, all[0].PaneID)
	}
}
