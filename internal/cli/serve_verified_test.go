package cli

import (
	"context"
	"database/sql"
	"io"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// rowVerified reads the raw `verified` column for a message (#169).
func rowVerified(t *testing.T, s *store.Store, publicID string) sql.NullInt64 {
	t.Helper()
	var v sql.NullInt64
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT verified FROM messages WHERE public_id = ?`, publicID).Scan(&v); err != nil {
		t.Fatalf("read verified %s: %v", publicID, err)
	}
	return v
}

// withUnverifiedDelivery drives the mailman down the ErrUnverifiedDelivery
// branch: the paste mechanics all succeed, but capture-pane never echoes the
// verify token, so it exhausts its retry budget and reports unverified.
func withUnverifiedDelivery(t *testing.T) {
	t.Helper()
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })
	prevRetry := tmuxio.SetRetrySchedule([]time.Duration{time.Microsecond})
	t.Cleanup(func() { tmuxio.SetRetrySchedule(prevRetry) })
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("\n"), nil // token never surfaces
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
}

func TestServe_VerifiedDelivery_WritesVerified1(t *testing.T) {
	withSuccessfulDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	r, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"})

	stop, wait, _ := runServeInBackground(t, s, fastOpts("bob"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
		if len(d) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	if v := rowVerified(t, s, r.PublicID); !v.Valid || v.Int64 != 1 {
		t.Errorf("verified delivery wrote verified=(%d, valid=%v), want 1", v.Int64, v.Valid)
	}
}

func TestServe_UnverifiedDelivery_WritesVerified0(t *testing.T) {
	withUnverifiedDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	r, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"})

	stop, wait, logbuf := runServeInBackground(t, s, fastOpts("bob"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
		if len(d) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	// State is delivered (the message IS in the pane), but the durable bit is 0.
	if v := rowVerified(t, s, r.PublicID); !v.Valid || v.Int64 != 0 {
		t.Errorf("unverified delivery wrote verified=(%d, valid=%v), want 0", v.Int64, v.Valid)
	}
	// The WARN journal line is preserved — healthscan still derives its count.
	if !strings.Contains(logbuf.String(), "WARN delivered_in_input_box") {
		t.Errorf("expected preserved WARN delivered_in_input_box line; got:\n%s", logbuf.String())
	}
}
