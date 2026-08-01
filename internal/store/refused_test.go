package store

import (
	"context"
	"errors"
	"testing"
)

// TestCapRefusal_WritesDurableRefusedRow pins the #881 invariant at the store
// layer: when a cap check fires (ErrRecipientQueueFull or
// ErrSenderBacklogFull), InsertMessage returns a *CapRejectionError whose
// RefusedID is the public_id of a StateRefused row written to the store.
// The refused row is durable — it can be retrieved via ListMessages and
// GetMessage — and is distinct from the refused sentinel error.
//
// Mutation check: remove the insertRefusedRow call and RefusedID becomes "",
// and ListMessages(state=refused) returns zero rows.
func TestCapRefusal_WritesDurableRefusedRow(t *testing.T) {
	ctx := context.Background()

	t.Run("recipient_queue_full", func(t *testing.T) {
		s := newTestStore(t)

		// Fill alice→bob to the cap.
		for i := 0; i < 2; i++ {
			if _, err := s.InsertMessage(ctx, InsertParams{
				FromAgent: "alice", ToAgent: "bob", Body: "m",
				MaxRecipientQueue: 2,
			}); err != nil {
				t.Fatalf("fill %d: %v", i, err)
			}
		}

		// This send hits the recipient cap.
		_, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "cap-hit",
			MaxRecipientQueue: 2,
		})
		if !errors.Is(err, ErrRecipientQueueFull) {
			t.Fatalf("err = %v, want ErrRecipientQueueFull", err)
		}

		var capErr *CapRejectionError
		if !errors.As(err, &capErr) {
			t.Fatalf("err type = %T, want *CapRejectionError", err)
		}
		if capErr.RefusedID == "" {
			t.Fatal("CapRejectionError.RefusedID is empty — refused row was not written")
		}

		// The refused row must be durable: retrievable by ID.
		msg, gerr := s.GetMessage(ctx, capErr.RefusedID)
		if gerr != nil {
			t.Fatalf("GetMessage(%s): %v", capErr.RefusedID, gerr)
		}
		if msg.State != StateRefused {
			t.Errorf("state = %q, want %q", msg.State, StateRefused)
		}
		if msg.Body != "cap-hit" {
			t.Errorf("body = %q, want %q", msg.Body, "cap-hit")
		}
	})

	t.Run("sender_backlog_full", func(t *testing.T) {
		s := newTestStore(t)

		for i := 0; i < 2; i++ {
			if _, err := s.InsertMessage(ctx, InsertParams{
				FromAgent: "alice", ToAgent: "bob", Body: "m",
				MaxSenderBacklog: 2,
			}); err != nil {
				t.Fatalf("fill %d: %v", i, err)
			}
		}

		_, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "backlog-hit",
			MaxSenderBacklog: 2,
		})
		if !errors.Is(err, ErrSenderBacklogFull) {
			t.Fatalf("err = %v, want ErrSenderBacklogFull", err)
		}

		var capErr *CapRejectionError
		if !errors.As(err, &capErr) {
			t.Fatalf("err type = %T, want *CapRejectionError", err)
		}
		if capErr.RefusedID == "" {
			t.Fatal("CapRejectionError.RefusedID is empty — refused row was not written")
		}

		msg, gerr := s.GetMessage(ctx, capErr.RefusedID)
		if gerr != nil {
			t.Fatalf("GetMessage(%s): %v", capErr.RefusedID, gerr)
		}
		if msg.State != StateRefused {
			t.Errorf("state = %q, want %q", msg.State, StateRefused)
		}
	})
}

// TestListMessages_StateRefused pins that ListMessages with State=StateRefused
// returns refused rows and does not return queued rows. Mutation check: remove
// the StateRefused constant or change its value and the filter mismatches.
func TestListMessages_StateRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Enqueue one normal message from alice→carol (different pair; no cap clash).
	_, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: "alice", ToAgent: "carol", Body: "queued-msg",
	})
	if err != nil {
		t.Fatalf("queued insert: %v", err)
	}

	// Force a refused row via a cap refusal on alice→bob (cap=2).
	for i := 0; i < 2; i++ {
		if _, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "m",
			MaxRecipientQueue: 2,
		}); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	_, capErr := s.InsertMessage(ctx, InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "refused-msg",
		MaxRecipientQueue: 2,
	})
	if !errors.Is(capErr, ErrRecipientQueueFull) {
		t.Fatalf("expected cap refusal, got: %v", capErr)
	}

	refused, err := s.ListMessages(ctx, ListFilter{State: StateRefused})
	if err != nil {
		t.Fatalf("list refused: %v", err)
	}
	if len(refused) != 1 {
		t.Fatalf("refused count = %d, want 1", len(refused))
	}
	if refused[0].State != StateRefused {
		t.Errorf("state = %q, want %q", refused[0].State, StateRefused)
	}
	if refused[0].Body != "refused-msg" {
		t.Errorf("body = %q, want %q", refused[0].Body, "refused-msg")
	}

	// State=queued must not surface refused rows.
	queued, err := s.ListMessages(ctx, ListFilter{State: StateQueued})
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	for _, m := range queued {
		if m.State == StateRefused {
			t.Errorf("refused row %s leaked into queued listing", m.PublicID)
		}
	}
}

// TestInsertMessagePair_CapRefusal_WritesTwoRefusedRows pins that a pair
// refusal writes a refused row for p1 (the primary) and another for p2.
// InsertMessagePair requires p1 and p2 to share the same (from, to) pair —
// the pair models two messages in the same exchange (e.g. send + chrome marker).
// The RefusedID in the returned error is p1's row.
func TestInsertMessagePair_CapRefusal_WritesTwoRefusedRows(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Fill alice→bob to the cap before the pair insert.
	for i := 0; i < 2; i++ {
		if _, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "m",
			MaxRecipientQueue: 2,
		}); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}

	// Both messages in the pair must share the same (from, to).
	_, _, err := s.InsertMessagePair(ctx,
		InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "p1", MaxRecipientQueue: 2},
		InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "p2"},
		false,
	)
	if !errors.Is(err, ErrRecipientQueueFull) {
		t.Fatalf("pair err = %v, want ErrRecipientQueueFull", err)
	}

	var capErr *CapRejectionError
	if !errors.As(err, &capErr) {
		t.Fatalf("err type = %T, want *CapRejectionError", err)
	}
	if capErr.RefusedID == "" {
		t.Fatal("CapRejectionError.RefusedID empty — p1 refused row not written")
	}

	// p1 row exists.
	p1, gerr := s.GetMessage(ctx, capErr.RefusedID)
	if gerr != nil {
		t.Fatalf("GetMessage(p1 %s): %v", capErr.RefusedID, gerr)
	}
	if p1.State != StateRefused {
		t.Errorf("p1 state = %q, want %q", p1.State, StateRefused)
	}

	// Two refused rows total (p1 + p2).
	all, err := s.ListMessages(ctx, ListFilter{State: StateRefused})
	if err != nil {
		t.Fatalf("list refused: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("refused count = %d, want 2 (one per pair member)", len(all))
	}
}

// TestCapRejectionError_ErrorsIs confirms that errors.Is works through
// CapRejectionError.Unwrap() — the caller's switch on ErrRecipientQueueFull /
// ErrSenderBacklogFull continues to fire even after wrapping in CapRejectionError.
// Mutation check: remove Unwrap() and errors.Is returns false.
func TestCapRejectionError_ErrorsIs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for i := 0; i < 2; i++ {
		if _, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "a", ToAgent: "b", Body: "m", MaxRecipientQueue: 2,
		}); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	_, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: "a", ToAgent: "b", Body: "x", MaxRecipientQueue: 2,
	})

	if !errors.Is(err, ErrRecipientQueueFull) {
		t.Errorf("errors.Is(err, ErrRecipientQueueFull) = false, want true — Unwrap broken")
	}
	var capErr *CapRejectionError
	if !errors.As(err, &capErr) {
		t.Errorf("errors.As *CapRejectionError = false, want true")
	}
}
