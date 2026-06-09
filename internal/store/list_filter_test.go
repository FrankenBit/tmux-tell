package store

import (
	"context"
	"strings"
	"testing"
)

// TestExpectsReply_RoundTrip: Message.ExpectsReply is populated by all
// read paths (GetMessage, ListMessages, scanMessages via TailRows / MessagesByIDs).
func TestExpectsReply_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "q?", ExpectsReply: true})
	plain, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "fyi"})

	// GetMessage round-trip.
	m, err := s.GetMessage(ctx, r.PublicID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if !m.ExpectsReply {
		t.Errorf("ask: ExpectsReply = false, want true")
	}
	p, _ := s.GetMessage(ctx, plain.PublicID)
	if p.ExpectsReply {
		t.Errorf("plain: ExpectsReply = true, want false")
	}

	// ListMessages round-trip.
	msgs, err := s.ListMessages(ctx, ListFilter{ToAgent: "bob", State: StateQueued})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	byID := map[string]Message{}
	for _, m := range msgs {
		byID[m.PublicID] = m
	}
	if !byID[r.PublicID].ExpectsReply {
		t.Errorf("ListMessages ask: ExpectsReply = false, want true")
	}
	if byID[plain.PublicID].ExpectsReply {
		t.Errorf("ListMessages plain: ExpectsReply = true, want false")
	}
}

// TestListFilter_Unanswered: filter returns only expects_reply=1 messages
// that have not received a reply from ToAgent.
func TestListFilter_Unanswered(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Two asks from alice to bob.
	askAnswered, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "q1?", ExpectsReply: true})
	askOpen, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "q2?", ExpectsReply: true})
	// Plain send (not an ask) — must not appear.
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "fyi"})

	// Bob replies to the first ask.
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "bob", ToAgent: "alice", ReplyTo: askAnswered.PublicID, Body: "a1"})

	msgs, err := s.ListMessages(ctx, ListFilter{ToAgent: "bob", State: StateQueued, Unanswered: true})
	if err != nil {
		t.Fatalf("ListMessages unanswered: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("unanswered = %d, want 1; ids: %v", len(msgs), func() []string {
			var s []string
			for _, m := range msgs {
				s = append(s, m.PublicID)
			}
			return s
		}())
	}
	if msgs[0].PublicID != askOpen.PublicID {
		t.Errorf("unanswered id = %s, want %s", msgs[0].PublicID, askOpen.PublicID)
	}
}

// TestListFilter_AwaitingReply: filter returns only expects_reply=1 messages
// sent by FromAgent that have not received a reply from the recipient.
func TestListFilter_AwaitingReply(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Alice sends two asks to bob.
	askAnswered, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "q1?", ExpectsReply: true})
	askOpen, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "q2?", ExpectsReply: true})
	// Plain send — must not appear.
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "fyi"})

	// Bob replies to the first ask.
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "bob", ToAgent: "alice", ReplyTo: askAnswered.PublicID, Body: "a1"})

	msgs, err := s.ListMessages(ctx, ListFilter{FromAgent: "alice", State: StateQueued, AwaitingReply: true})
	if err != nil {
		t.Fatalf("ListMessages awaiting_reply: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("awaiting_reply = %d, want 1", len(msgs))
	}
	if msgs[0].PublicID != askOpen.PublicID {
		t.Errorf("awaiting_reply id = %s, want %s", msgs[0].PublicID, askOpen.PublicID)
	}
}

// TestListFilter_UnansweredRequiresToAgent mirrors the Unverified validation
// pattern: Unanswered=true without ToAgent is a caller bug (the NOT EXISTS
// subquery references ToAgent; an empty arg makes the filter vacuously true).
func TestListFilter_UnansweredRequiresToAgent(t *testing.T) {
	s := newTestStore(t)
	_, err := s.ListMessages(context.Background(), ListFilter{Unanswered: true})
	if err == nil {
		t.Fatal("Unanswered=true without ToAgent should error; got nil")
	}
	if !strings.Contains(err.Error(), "Unanswered=true") {
		t.Errorf("error should mention Unanswered=true; got %v", err)
	}
}

// TestListMessages_UnverifiedValidation pins the fail-loud check added in
// #220: Unverified=true is only valid when State is empty or StateDelivered.
func TestListMessages_UnverifiedValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Unverified alone (State="") → valid.
	if _, err := s.ListMessages(ctx, ListFilter{Unverified: true}); err != nil {
		t.Errorf("Unverified alone should be valid; got %v", err)
	}

	// Unverified + State=StateDelivered → valid.
	if _, err := s.ListMessages(ctx, ListFilter{
		Unverified: true,
		State:      StateDelivered,
	}); err != nil {
		t.Errorf("Unverified + State=delivered should be valid; got %v", err)
	}

	// Unverified + State=StateQueued → error.
	_, err := s.ListMessages(ctx, ListFilter{Unverified: true, State: StateQueued})
	if err == nil {
		t.Fatal("Unverified + State=queued should error; got nil")
	}
	if !strings.Contains(err.Error(), "Unverified=true") {
		t.Errorf("error should mention Unverified=true; got %v", err)
	}
}
