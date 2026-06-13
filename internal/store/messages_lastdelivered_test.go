package store

import (
	"context"
	"testing"
)

// TestRecipientLastDelivered exercises the #348 derive: MAX(delivered_at) over
// state=delivered rows, ok=false when none, failed rows excluded, and the
// delivered_in_input_box soft-fail (state=delivered, verified=0) counted.
func TestRecipientLastDelivered(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, a := range []string{"alice", "bob", "carol", "dave"} {
		if err := s.UpsertAgent(ctx, a, "%9"); err != nil {
			t.Fatalf("seed %s: %v", a, err)
		}
	}

	claim := func(to string) string {
		t.Helper()
		if _, err := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: to, Body: "x", Kind: KindMessage}); err != nil {
			t.Fatalf("insert→%s: %v", to, err)
		}
		m, err := s.ClaimNext(ctx, to)
		if err != nil || m == nil {
			t.Fatalf("claim→%s: m=%v err=%v", to, m, err)
		}
		return m.PublicID
	}

	// No delivery yet → ok=false, empty ts.
	if ts, ok, err := s.RecipientLastDelivered(ctx, "bob"); err != nil || ok || ts != "" {
		t.Errorf("no-delivery: ts=%q ok=%v err=%v; want \"\"/false/nil", ts, ok, err)
	}

	// One confirmed delivery → ok=true, non-empty ts.
	if err := s.MarkDelivered(ctx, claim("bob")); err != nil {
		t.Fatal(err)
	}
	if ts, ok, err := s.RecipientLastDelivered(ctx, "bob"); err != nil || !ok || ts == "" {
		t.Fatalf("after delivery: ts=%q ok=%v err=%v; want a timestamp", ts, ok, err)
	}

	// Failed delivery (pane gone) must NOT count as a delivery.
	if err := s.MarkFailed(ctx, claim("carol"), "pane gone"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.RecipientLastDelivered(ctx, "carol"); ok {
		t.Error("failed delivery counted as last_delivered; want excluded")
	}

	// delivered_in_input_box soft-fail (state=delivered, verified=0) MUST count.
	if err := s.MarkDeliveredInInputBox(ctx, claim("dave")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.RecipientLastDelivered(ctx, "dave"); !ok {
		t.Error("delivered_in_input_box not counted as last_delivered; want counted")
	}
}
