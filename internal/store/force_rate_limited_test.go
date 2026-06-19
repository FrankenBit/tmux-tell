package store

import (
	"context"
	"testing"
)

// TestInsertMessage_ForceRateLimitedRoundTrip pins the #558 column plumbing: the
// flag set at insert survives to the claim read path the mailman uses, and
// defaults to false when unset (so every existing send is unchanged).
func TestInsertMessage_ForceRateLimitedRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	if _, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: "alice", ToAgent: "forced", Body: "x", ForceRateLimited: true,
	}); err != nil {
		t.Fatalf("insert forced: %v", err)
	}
	m, err := s.ClaimNext(ctx, "forced")
	if err != nil {
		t.Fatalf("claim forced: %v", err)
	}
	if m == nil || !m.ForceRateLimited {
		t.Fatalf("claimed ForceRateLimited = %v, want true", m)
	}

	if _, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: "alice", ToAgent: "normal", Body: "y",
	}); err != nil {
		t.Fatalf("insert default: %v", err)
	}
	m2, err := s.ClaimNext(ctx, "normal")
	if err != nil {
		t.Fatalf("claim default: %v", err)
	}
	if m2 == nil || m2.ForceRateLimited {
		t.Errorf("default ForceRateLimited = %v, want false", m2)
	}
}
