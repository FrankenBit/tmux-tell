package store

import (
	"context"
	"errors"
	"testing"
)

// A freshly-registered agent has empty metabolism + NULL metabolism_set_at (the
// migration default). SetMetabolism round-trips each of the three vocabulary
// states through GetAgent and ListAgents, and stamps metabolism_set_at.
func TestSetMetabolism_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	a, err := s.GetAgent(ctx, "engineer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.Metabolism != "" || a.MetabolismSetAt != "" {
		t.Errorf("fresh agent metabolism=%q set_at=%q, want both empty", a.Metabolism, a.MetabolismSetAt)
	}

	for _, want := range []string{MetabolismWarming, MetabolismSaturating, MetabolismCompactPending} {
		if err := s.SetMetabolism(ctx, "engineer", want); err != nil {
			t.Fatalf("SetMetabolism(%q): %v", want, err)
		}
		a, err := s.GetAgent(ctx, "engineer")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if a.Metabolism != want {
			t.Errorf("metabolism = %q, want %q", a.Metabolism, want)
		}
		// A non-empty self-report stamps metabolism_set_at (the staleness key).
		if a.MetabolismSetAt == "" {
			t.Errorf("metabolism_set_at empty after SetMetabolism(%q), want a stamp", want)
		}
	}

	// The listing path carries it too.
	list, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, ag := range list {
		if ag.Name == "engineer" {
			found = true
			if ag.Metabolism != MetabolismCompactPending {
				t.Errorf("ListAgents metabolism = %q, want %q", ag.Metabolism, MetabolismCompactPending)
			}
			if ag.MetabolismSetAt == "" {
				t.Errorf("ListAgents metabolism_set_at empty, want a stamp")
			}
		}
	}
	if !found {
		t.Fatal("engineer not in ListAgents")
	}
}

// Clearing (value "") empties metabolism AND nulls metabolism_set_at, holding
// the invariant: metabolism == "" ⟺ metabolism_set_at IS NULL (surfaced as "").
func TestSetMetabolism_ClearNullsStamp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetMetabolism(ctx, "engineer", MetabolismSaturating); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.SetMetabolism(ctx, "engineer", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	a, err := s.GetAgent(ctx, "engineer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.Metabolism != "" {
		t.Errorf("metabolism = %q after clear, want empty", a.Metabolism)
	}
	if a.MetabolismSetAt != "" {
		t.Errorf("metabolism_set_at = %q after clear, want empty (NULL)", a.MetabolismSetAt)
	}
}

// Invalid values are rejected (the validation gate). Mutation anchor for
// ValidMetabolism: widening it to accept arbitrary strings flips this red.
func TestSetMetabolism_RejectsInvalid(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, bad := range []string{"warm", "at-rest", "compacting", "idle", "WARMING", "saturated"} {
		if err := s.SetMetabolism(ctx, "engineer", bad); err == nil {
			t.Errorf("SetMetabolism(%q) accepted, want rejected", bad)
		}
	}
	// And the row was never written.
	a, err := s.GetAgent(ctx, "engineer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.Metabolism != "" {
		t.Errorf("metabolism = %q after rejected writes, want empty", a.Metabolism)
	}
}

func TestSetMetabolism_UnknownAgent(t *testing.T) {
	s := newTestStore(t)
	err := s.SetMetabolism(context.Background(), "ghost", MetabolismWarming)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// ClearMetabolismIfPending clears ONLY compact-pending — the observed-truth-
// supersedes-self-report mechanism. It must NOT clobber a warming/saturating
// self-report (those are not superseded by observing at-rest), and is a no-op
// (not an error) against an empty value or a missing agent.
//
// Mutation anchor: dropping the `AND metabolism = 'compact-pending'` guard from
// ClearMetabolismIfPending flips the warming/saturating sub-asserts red.
func TestClearMetabolismIfPending(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// compact-pending IS cleared.
	if err := s.SetMetabolism(ctx, "engineer", MetabolismCompactPending); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.ClearMetabolismIfPending(ctx, "engineer"); err != nil {
		t.Fatalf("clear-if-pending: %v", err)
	}
	a, _ := s.GetAgent(ctx, "engineer")
	if a.Metabolism != "" {
		t.Errorf("compact-pending not cleared: metabolism = %q", a.Metabolism)
	}
	if a.MetabolismSetAt != "" {
		t.Errorf("stamp not nulled on auto-clear: set_at = %q", a.MetabolismSetAt)
	}

	// warming/saturating are NOT clobbered (not superseded by observed at-rest).
	for _, keep := range []string{MetabolismWarming, MetabolismSaturating} {
		if err := s.SetMetabolism(ctx, "engineer", keep); err != nil {
			t.Fatalf("set %q: %v", keep, err)
		}
		if err := s.ClearMetabolismIfPending(ctx, "engineer"); err != nil {
			t.Fatalf("clear-if-pending: %v", err)
		}
		a, _ := s.GetAgent(ctx, "engineer")
		if a.Metabolism != keep {
			t.Errorf("ClearMetabolismIfPending clobbered %q -> %q, want preserved", keep, a.Metabolism)
		}
	}

	// No-op (no error) against a missing agent.
	if err := s.ClearMetabolismIfPending(ctx, "ghost"); err != nil {
		t.Errorf("clear-if-pending on missing agent = %v, want nil (no-op)", err)
	}
}
