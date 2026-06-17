package store

import (
	"context"
	"testing"
)

// TestClaimNextWithStrategy_PriorityRouting is the #449 first-worked-instance at
// the store layer: under load, a high-priority channel's head is claimed before
// an older normal-priority channel's head (cross-channel routing), while
// within-channel FIFO is preserved.
func TestClaimNextWithStrategy_PriorityRouting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// alice sends a normal message FIRST (lower id), bob a high-priority one
	// SECOND (higher id), both to R. Pure FIFO would deliver alice first;
	// priority routing delivers bob's (high) channel first.
	a, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "r", Body: "fyi", Priority: PriorityNormal})
	b, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "bob", ToAgent: "r", Body: "urgent", Priority: PriorityHigh})

	got, err := s.ClaimNextWithStrategy(ctx, "r", StrategyMaxPriority)
	if err != nil || got == nil {
		t.Fatalf("claim: %v / %v", got, err)
	}
	if got.PublicID != b.PublicID {
		t.Errorf("claimed %s, want bob's high-priority %s (priority routing should beat the older normal message)", got.PublicID, b.PublicID)
	}
	if got.Priority != PriorityHigh {
		t.Errorf("claimed priority = %d, want %d", got.Priority, PriorityHigh)
	}

	// Next claim takes alice's normal message (the remaining channel).
	got2, _ := s.ClaimNextWithStrategy(ctx, "r", StrategyMaxPriority)
	if got2 == nil || got2.PublicID != a.PublicID {
		t.Errorf("second claim = %v, want alice's %s", got2, a.PublicID)
	}
}

// TestClaimNextWithStrategy_WithinChannelFIFO confirms priority never reorders
// within a single sender-channel: alice sends low then high to R; the LOW
// (head) delivers first despite the high being more urgent.
func TestClaimNextWithStrategy_WithinChannelFIFO(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "r", Body: "low-first", Priority: PriorityLow})
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "r", Body: "high-second", Priority: PriorityHigh})

	got, _ := s.ClaimNextWithStrategy(ctx, "r", StrategyMaxPriority)
	if got == nil || got.PublicID != first.PublicID {
		t.Errorf("claimed %v, want the channel HEAD %s (within-channel FIFO — priority must not reorder within a sender)", got, first.PublicID)
	}
}

// TestClaimNext_DefaultIsFIFO confirms the plain ClaimNext (default strategy)
// is pure FIFO when priorities are uniform — the property that keeps all
// existing behavior + the 67 existing tests unchanged.
func TestClaimNext_DefaultIsFIFO(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "bob", ToAgent: "r", Body: "1"}) // normal (default)
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "r", Body: "2"})    // normal
	got, _ := s.ClaimNext(ctx, "r")
	if got == nil || got.PublicID != first.PublicID {
		t.Errorf("default ClaimNext claimed %v, want the oldest %s (uniform priority → FIFO)", got, first.PublicID)
	}
}
