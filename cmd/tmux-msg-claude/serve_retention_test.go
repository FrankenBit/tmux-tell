package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// seedDelivered inserts a delivered message with an explicit created_at timestamp
// so retention tests can place rows on either side of a cutoff without waiting.
func seedDelivered(t *testing.T, s *store.Store, from, to, createdAt string) string {
	t.Helper()
	ctx := context.Background()
	res, err := s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		from+"-to-"+to+"-"+createdAt, from, to, "body", "message", string(store.StateDelivered), createdAt)
	if err != nil {
		t.Fatalf("seedDelivered: %v", err)
	}
	id, _ := res.LastInsertId()
	_ = id
	return from + "-to-" + to + "-" + createdAt
}

// TestRetentionSweep_InfiniteSkips pins that a mailman configured with
// retention="infinite" does NOT start a sweep goroutine, so no rows are
// deleted over its lifetime.
func TestRetentionSweep_InfiniteSkips(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")

	// Seed an old delivered message using the current time so it would be
	// deleted by any finite window — but infinite must not touch it.
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "keep me"})
	m, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkDelivered(ctx, m.PublicID)

	opts := fastOpts("bob")
	opts.Retention = "infinite"

	stop, wait, _ := runServeInBackground(t, s, opts)
	time.Sleep(20 * time.Millisecond)
	stop()
	wait()

	rest, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob"})
	if len(rest) != 1 {
		t.Errorf("retention=infinite: want 1 row untouched, got %d", len(rest))
	}
}

// TestRetentionSweep_EmptyRetentionSkips pins that an empty Retention field
// (the zero-value / unconfigured case) also skips the sweep goroutine.
func TestRetentionSweep_EmptyRetentionSkips(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "keep me"})
	m, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkDelivered(ctx, m.PublicID)

	opts := fastOpts("bob")
	opts.Retention = "" // zero value = infinite

	stop, wait, _ := runServeInBackground(t, s, opts)
	time.Sleep(20 * time.Millisecond)
	stop()
	wait()

	rest, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob"})
	if len(rest) != 1 {
		t.Errorf("retention='': want 1 row untouched, got %d", len(rest))
	}
}

// TestRetentionSweep_DeletesOldDelivered pins that runRetentionSweep deletes
// delivered rows older than the window and leaves newer rows intact.
func TestRetentionSweep_DeletesOldDelivered(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	base := time.Now().UTC()
	old := base.Add(-48 * time.Hour).Format(strandedTimeFormat)
	newish := base.Add(time.Hour).Format(strandedTimeFormat)

	// Seed directly with known timestamps.
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		"old1", "a", "bob", "x", "message", string(store.StateDelivered), old)
	if err != nil {
		t.Fatalf("seed old: %v", err)
	}
	_, err = s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		"new1", "a", "bob", "y", "message", string(store.StateDelivered), newish)
	if err != nil {
		t.Fatalf("seed new: %v", err)
	}

	stopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logbuf := &bytes.Buffer{}
	logger := log.New(logbuf, "[test] ", 0)

	// Use a very short interval so the sweep fires quickly in the test.
	go runRetentionSweep(stopCtx, s, logger, "bob", "1d", 5*time.Millisecond)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		rest, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob"})
		if len(rest) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	rest, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob"})
	if len(rest) != 1 {
		t.Errorf("after sweep: want 1 row, got %d", len(rest))
	}
	if len(rest) == 1 && rest[0].PublicID != "new1" {
		t.Errorf("want new1 to survive, got %s", rest[0].PublicID)
	}
	if !bytes.Contains(logbuf.Bytes(), []byte("retention_sweep_deleted")) {
		t.Errorf("expected retention_sweep_deleted log; got %s", logbuf.String())
	}
}

// TestRetentionSweep_OnlyDeletesTerminalStates pins that queued messages are
// never touched by the retention sweep (only delivered+failed are eligible).
func TestRetentionSweep_OnlyDeletesTerminalStates(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	old := time.Now().UTC().Add(-48 * time.Hour).Format(strandedTimeFormat)

	// Old delivered row — eligible for sweep.
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		"old-delivered", "a", "bob", "x", "message", string(store.StateDelivered), old)
	if err != nil {
		t.Fatalf("seed delivered: %v", err)
	}
	// Old queued row — NOT eligible; in-flight states are never swept.
	_, err = s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		"old-queued", "a", "bob", "z", "message", string(store.StateQueued), old)
	if err != nil {
		t.Fatalf("seed queued: %v", err)
	}

	stopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.New(io.Discard, "", 0)
	go runRetentionSweep(stopCtx, s, logger, "bob", "1d", 5*time.Millisecond)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		delivered, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered})
		if len(delivered) == 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	rest, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob"})
	if len(rest) != 1 {
		t.Errorf("want 1 row (queued), got %d", len(rest))
	}
	if len(rest) == 1 && rest[0].PublicID != "old-queued" {
		t.Errorf("want old-queued to survive, got %s", rest[0].PublicID)
	}
}

// TestRetentionSweep_AgentScoped pins that the sweep only deletes rows for the
// configured agent, leaving other agents' rows untouched.
func TestRetentionSweep_AgentScoped(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	old := time.Now().UTC().Add(-48 * time.Hour).Format(strandedTimeFormat)

	// Old delivered row for bob — eligible.
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		"bob-old", "a", "bob", "x", "message", string(store.StateDelivered), old)
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	// Old delivered row for carol — should NOT be deleted by bob's mailman.
	_, err = s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		"carol-old", "a", "carol", "y", "message", string(store.StateDelivered), old)
	if err != nil {
		t.Fatalf("seed carol: %v", err)
	}

	stopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.New(io.Discard, "", 0)
	// Sweep runs for bob only.
	go runRetentionSweep(stopCtx, s, logger, "bob", "1d", 5*time.Millisecond)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		bobMsgs, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob"})
		if len(bobMsgs) == 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	carolMsgs, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "carol"})
	if len(carolMsgs) != 1 {
		t.Errorf("carol's rows should be untouched: want 1, got %d", len(carolMsgs))
	}
}
