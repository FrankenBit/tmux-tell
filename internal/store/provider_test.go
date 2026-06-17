package store

import (
	"context"
	"testing"
	"time"
)

// TestCountWorkingOnProvider pins the #448 cap-count primitive: it counts only
// same-provider agents whose observed_state is "working" AND whose state write
// is fresh within the TTL — so a crashed mailman's stale "working" ages out and
// stops pinning a slot. `now` is injected for deterministic TTL boundaries.
func TestCountWorkingOnProvider(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)

	// Register agents across two providers + states. newTestStore-style upsert:
	for _, a := range []string{"a1", "a2", "a3", "b1", "stale1", "idle1"} {
		if err := s.UpsertAgent(ctx, a, "%1"); err != nil {
			t.Fatalf("upsert %s: %v", a, err)
		}
	}
	set := func(agent, provider, state string, at time.Time) {
		if err := s.SetProvider(ctx, agent, provider); err != nil {
			t.Fatalf("set provider %s: %v", agent, err)
		}
		if err := s.SetObservedState(ctx, agent, state, at); err != nil {
			t.Fatalf("set state %s: %v", agent, err)
		}
	}
	// anthropic: a1,a2 fresh-working; idle1 fresh-idle; stale1 working-but-old.
	set("a1", "anthropic", "working", base)
	set("a2", "anthropic", "working", base)
	set("idle1", "anthropic", "idle", base)
	set("stale1", "anthropic", "working", base.Add(-30*time.Second)) // older than TTL
	// openai: b1 fresh-working (must not bleed into anthropic's count).
	set("b1", "openai", "working", base)

	ttl := 6 * time.Second
	got, err := s.CountWorkingOnProvider(ctx, "anthropic", ttl, base)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 2 {
		t.Errorf("anthropic working count = %d, want 2 (a1,a2; idle1 not working, stale1 aged out, b1 other provider)", got)
	}

	// openai sees only b1.
	if got, _ := s.CountWorkingOnProvider(ctx, "openai", ttl, base); got != 1 {
		t.Errorf("openai working count = %d, want 1", got)
	}

	// As `now` advances past the TTL for a1/a2, they too age out.
	if got, _ := s.CountWorkingOnProvider(ctx, "anthropic", ttl, base.Add(10*time.Second)); got != 0 {
		t.Errorf("anthropic count after TTL = %d, want 0 (all writes now stale)", got)
	}

	// Empty provider opts out → always 0.
	if got, _ := s.CountWorkingOnProvider(ctx, "", ttl, base); got != 0 {
		t.Errorf("empty-provider count = %d, want 0", got)
	}
}
