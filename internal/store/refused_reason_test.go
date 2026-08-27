package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCapRefusal_PersistsReason pins #921: a StateRefused row carries its
// cap-check diagnosis in the error column, as StateFailed rows already do.
//
// Why this exists. Measured across the live store on 2026-08-27:
//
//	failed    n=189    error text present on 189   ← the column CAN hold a value
//	refused   n=1030   error text present on    0   ← the substrate never wrote it
//
// The column already existed, so this was a write that was not happening
// rather than a schema change. Until it landed, a refusal's reason lived
// ONLY in the sending chamber's ephemeral ok:false receipt — one transcript,
// never shared — so the cause of 1030 rows was structurally unrecoverable by
// anyone but the original sender, and only while their session lasted.
//
// EVERY ARM ASSERTS THE DISCRIMINATING SUBSTRING, NOT MERELY NON-EMPTY.
// A non-empty assertion passes when the WRONG reason is written — it cannot
// tell a recipient-cap message from a backlog-cap one, which is the only
// thing a reader of the row needs it for.
func TestCapRefusal_PersistsReason(t *testing.T) {
	ctx := context.Background()

	// Control. A row that was NOT refused must carry no error text, or the
	// assertions below would pass against a store that writes text onto
	// everything. Without this the suite cannot fail in the world where a
	// blanket write is the bug.
	t.Run("control_accepted_row_has_no_error", func(t *testing.T) {
		s := newTestStore(t)
		res, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "fine",
			MaxRecipientQueue: 5,
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		msg, gerr := s.GetMessage(ctx, res.PublicID)
		if gerr != nil {
			t.Fatalf("GetMessage: %v", gerr)
		}
		if msg.Error.Valid && msg.Error.String != "" {
			t.Errorf("accepted row carries error %q, want none", msg.Error.String)
		}
	})

	t.Run("recipient_queue_full_reason_is_persisted", func(t *testing.T) {
		s := newTestStore(t)
		for i := 0; i < 2; i++ {
			if _, err := s.InsertMessage(ctx, InsertParams{
				FromAgent: "alice", ToAgent: "bob", Body: "m",
				MaxRecipientQueue: 2,
			}); err != nil {
				t.Fatalf("fill %d: %v", i, err)
			}
		}
		_, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "cap-hit",
			MaxRecipientQueue: 2,
		})
		var capErr *CapRejectionError
		if !errors.As(err, &capErr) {
			t.Fatalf("err type = %T, want *CapRejectionError", err)
		}
		msg, gerr := s.GetMessage(ctx, capErr.RefusedID)
		if gerr != nil {
			t.Fatalf("GetMessage(%s): %v", capErr.RefusedID, gerr)
		}
		if !msg.Error.Valid || msg.Error.String == "" {
			t.Fatal("refused row carries NO error text — the reason was dropped (#921)")
		}
		// DISCRIMINATING: this arm must not pass on a backlog-cap message.
		if !strings.Contains(msg.Error.String, "recipient queue full") {
			t.Errorf("error = %q, want it to name the recipient-queue cap", msg.Error.String)
		}
		if strings.Contains(msg.Error.String, "sender backlog") {
			t.Errorf("error = %q names the WRONG cap", msg.Error.String)
		}
		// THE PAYLOAD, not the label. Every assertion above names a CAP, and a
		// mutant routing each branch to its own constant label satisfies all of
		// them while dropping "bob (2/2, need 1 slot(s))" entirely — measured:
		// a two-constant mutant passed every arm. #921 wanted the DIAGNOSIS,
		// and a cap name is not one. (@surveyor, tt#931 review.)
		if !strings.Contains(msg.Error.String, "bob") {
			t.Errorf("error = %q does not name the saturated recipient", msg.Error.String)
		}
		if !strings.Contains(msg.Error.String, "(2/2") {
			t.Errorf("error = %q does not carry the occupancy figure", msg.Error.String)
		}
	})

	t.Run("sender_backlog_full_reason_is_persisted", func(t *testing.T) {
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
			FromAgent: "alice", ToAgent: "bob", Body: "cap-hit",
			MaxSenderBacklog: 2,
		})
		var capErr *CapRejectionError
		if !errors.As(err, &capErr) {
			t.Fatalf("err type = %T, want *CapRejectionError", err)
		}
		msg, gerr := s.GetMessage(ctx, capErr.RefusedID)
		if gerr != nil {
			t.Fatalf("GetMessage(%s): %v", capErr.RefusedID, gerr)
		}
		if !msg.Error.Valid || msg.Error.String == "" {
			t.Fatal("refused row carries NO error text — the reason was dropped (#921)")
		}
		// DISCRIMINATING in the opposite direction from the arm above. The two
		// arms together are what prove the reason is the ACTUAL cap that fired
		// rather than a constant string satisfying both.
		if !strings.Contains(msg.Error.String, "sender backlog") {
			t.Errorf("error = %q, want it to name the sender-backlog cap", msg.Error.String)
		}
		if strings.Contains(msg.Error.String, "recipient queue full") {
			t.Errorf("error = %q names the WRONG cap", msg.Error.String)
		}
		// Payload again, and on the pair axis this time: the backlog message
		// names BOTH ends of the pair, which a per-cap constant cannot.
		if !strings.Contains(msg.Error.String, "alice") || !strings.Contains(msg.Error.String, "bob") {
			t.Errorf("error = %q does not name the sender→recipient pair", msg.Error.String)
		}
		if !strings.Contains(msg.Error.String, "(2/2") {
			t.Errorf("error = %q does not carry the occupancy figure", msg.Error.String)
		}
	})

	// A pair rejection writes TWO refused rows. Both must carry the reason:
	// the second is written with `_ =` and its return value discarded, so
	// nothing at the call site would notice if it were dropped.
	t.Run("pair_rejection_both_rows_carry_the_reason", func(t *testing.T) {
		s := newTestStore(t)
		for i := 0; i < 2; i++ {
			if _, err := s.InsertMessage(ctx, InsertParams{
				FromAgent: "alice", ToAgent: "bob", Body: "m",
				MaxRecipientQueue: 2,
			}); err != nil {
				t.Fatalf("fill %d: %v", i, err)
			}
		}
		p1 := InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "p1", MaxRecipientQueue: 2}
		p2 := InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "p2", MaxRecipientQueue: 2}
		_, _, err := s.InsertMessagePair(ctx, p1, p2, false)
		var capErr *CapRejectionError
		if !errors.As(err, &capErr) {
			t.Fatalf("err type = %T, want *CapRejectionError", err)
		}
		rows, lerr := s.ListMessages(ctx, ListFilter{State: StateRefused})
		if lerr != nil {
			t.Fatalf("ListMessages: %v", lerr)
		}
		if len(rows) != 2 {
			t.Fatalf("refused rows = %d, want 2 (the pair is atomic)", len(rows))
		}
		for _, m := range rows {
			if !m.Error.Valid || !strings.Contains(m.Error.String, "recipient queue full") {
				t.Errorf("refused row %s carries error %q, want the recipient-queue cap",
					m.PublicID, m.Error.String)
			}
		}
	})
}
