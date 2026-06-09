package cli

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// seedDeliveredUnverified inserts a delivered_in_input_box row (state=delivered,
// verified=0) with an explicit created_at so dedupe tests can control window placement.
func seedDeliveredUnverified(t *testing.T, s *store.Store, publicID, fromAgent, toAgent, body, createdAt string) {
	t.Helper()
	ctx := context.Background()
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, verified, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		publicID, fromAgent, toAgent, body, "message", string(store.StateDelivered), 0, createdAt)
	if err != nil {
		t.Fatalf("seedDeliveredUnverified: %v", err)
	}
}

// withDedupeRunner installs a fake tmux runner that:
//   - On capture-pane: returns captureOutput (caller controls token visibility)
//   - On all other commands: no-ops (load-buffer / paste-buffer / send-keys)
//
// Returns a cleanup function that restores the prior runner.
func withDedupeRunner(t *testing.T, captureOutput string) {
	t.Helper()
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })
	prevRetry := tmuxio.SetRetryDelaysForTest([]time.Duration{time.Microsecond})
	t.Cleanup(func() { tmuxio.SetRetryDelaysForTest(prevRetry) })
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte(captureOutput), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
}

// TestDedupeWindow_ZeroDisables pins that DedupeWindow=0 skips the dedupe
// check and delivers the message normally (no FindDedupeMatch call, replay
// lands as delivered).
func TestDedupeWindow_ZeroDisables(t *testing.T) {
	withSuccessfulDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	// Seed a delivered_unverified original.
	original := time.Now().UTC().Add(-5 * time.Second).Format("2006-01-02T15:04:05.000Z")
	seedDeliveredUnverified(t, s, "orig-z", "alice", "bob", "dedupe-body", original)

	// Send a replay with the same body.
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "dedupe-body"})

	opts := fastOpts("bob")
	opts.DedupeWindow = 0 // disabled

	stop, wait, _ := runServeInBackground(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		delivered, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered})
		// 2 delivered: original + newly delivered replay
		if len(delivered) == 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	delivered, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered})
	// original (verified=0) + replay (verified=1) both delivered
	if len(delivered) != 2 {
		t.Errorf("dedupe disabled: want 2 delivered rows, got %d", len(delivered))
	}
}

// TestDedupeWindow_OriginalConfirmed pins that when the original token is
// visible in scrollback: original gets verified=1, duplicate gets state=failed,
// a dedupe_notice is inserted back to the sender, and the log records
// "dedupe_absorbed".
func TestDedupeWindow_OriginalConfirmed(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	original := time.Now().UTC().Add(-5 * time.Second).Format("2006-01-02T15:04:05.000Z")
	seedDeliveredUnverified(t, s, "orig-c", "alice", "bob", "confirm-body", original)

	replay, _ := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "confirm-body",
		ReplayOf: "orig-c", ReplayOfAt: original,
	})

	// Runner returns the original's verify token in capture-pane — simulates that
	// "id orig-c" is now visible in the recipient's scrollback.
	withDedupeRunner(t, "id orig-c\nsome other content")

	opts := fastOpts("bob")
	opts.DedupeWindow = 60 * time.Second

	logbuf := &bytes.Buffer{}
	logger := log.New(logbuf, "[test] ", 0)
	stopCtx, stop := context.WithCancel(context.Background())

	done := make(chan int, 1)
	go func() {
		done <- runServeWithStore(stopCtx, s, opts, logger, io.Discard, io.Discard)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		failed, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateFailed})
		if len(failed) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	<-done

	// Original should now be verified=1.
	orig, _ := s.GetMessage(ctx, "orig-c")
	if orig == nil || !orig.Verified.Valid || orig.Verified.Int64 != 1 {
		v := int64(-1)
		if orig != nil && orig.Verified.Valid {
			v = orig.Verified.Int64
		}
		t.Errorf("original: want verified=1, got %d", v)
	}

	// Replay (duplicate) should be failed.
	replayMsg, _ := s.GetMessage(ctx, replay.PublicID)
	if replayMsg == nil || replayMsg.State != store.StateFailed {
		state := "<nil>"
		if replayMsg != nil {
			state = string(replayMsg.State)
		}
		t.Errorf("replay: want state=failed, got %s", state)
	}

	// A dedupe_notice should have been inserted to alice.
	notices, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "alice", Kind: store.KindDedupeNotice})
	if len(notices) == 0 {
		t.Error("want dedupe_notice inserted to alice, got none")
	}

	// Log should record dedupe_absorbed.
	if !strings.Contains(logbuf.String(), "dedupe_absorbed") {
		t.Errorf("expected dedupe_absorbed in log; got:\n%s", logbuf.String())
	}
}

// TestDedupeWindow_OriginalGone pins that when the original token is NOT visible
// in scrollback, the replay is delivered normally (original stays unverified).
func TestDedupeWindow_OriginalGone(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	original := time.Now().UTC().Add(-5 * time.Second).Format("2006-01-02T15:04:05.000Z")
	seedDeliveredUnverified(t, s, "orig-g", "alice", "bob", "gone-body", original)

	// Send a replay.
	replay, _ := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "gone-body",
		ReplayOf: "orig-g", ReplayOfAt: original,
	})

	// Runner: capture-pane returns empty (token not visible); delivery ops no-op.
	// For the delivery of the replay itself, we need load-buffer to record the body
	// and capture-pane to return it (so the verify token is found). We do this by
	// having capture-pane first return empty (for CheckTokenVisible), then return
	// the body (for deliverOne verify). We track call count.
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })
	prevRetry := tmuxio.SetRetryDelaysForTest([]time.Duration{time.Microsecond})
	t.Cleanup(func() { tmuxio.SetRetryDelaysForTest(prevRetry) })
	var callCount int
	var lastBody string
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "load-buffer":
			b, _ := io.ReadAll(stdin)
			lastBody = string(b)
		case "capture-pane":
			callCount++
			if callCount == 1 {
				return []byte("no token here"), nil // CheckTokenVisible: gone
			}
			return []byte(lastBody), nil // deliverOne verify: body echoed
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	opts := fastOpts("bob")
	opts.DedupeWindow = 60 * time.Second

	stop, wait, logbuf := runServeInBackground(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msg, _ := s.GetMessage(ctx, replay.PublicID)
		if msg != nil && msg.State == store.StateDelivered {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	// Replay should be delivered (not absorbed).
	replayMsg, _ := s.GetMessage(ctx, replay.PublicID)
	if replayMsg == nil || replayMsg.State != store.StateDelivered {
		state := "<nil>"
		if replayMsg != nil {
			state = string(replayMsg.State)
		}
		t.Errorf("replay: want state=delivered, got %s", state)
	}

	// Original should still be unverified.
	orig, _ := s.GetMessage(ctx, "orig-g")
	if orig == nil || !orig.Verified.Valid || orig.Verified.Int64 != 0 {
		t.Error("original should remain verified=0 when token not visible")
	}

	// Log should record dedupe_original_gone.
	if !strings.Contains(logbuf.String(), "dedupe_original_gone") {
		t.Errorf("expected dedupe_original_gone in log; got:\n%s", logbuf.String())
	}
}

// TestDedupeWindow_WindowBoundary pins that the dedupe only fires for rows
// newer than the cutoff: a row just inside the window matches, one just
// outside does not.
func TestDedupeWindow_WindowBoundary(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	now := time.Now().UTC()
	window := 5 * time.Second

	// Just inside the window (4s ago).
	inside := now.Add(-4 * time.Second).Format("2006-01-02T15:04:05.000Z")
	// Just outside the window (6s ago).
	outside := now.Add(-6 * time.Second).Format("2006-01-02T15:04:05.000Z")

	cutoff := now.Add(-window).Format("2006-01-02T15:04:05.000Z")

	seedDeliveredUnverified(t, s, "inside-orig", "alice", "bob", "boundary-body", inside)
	seedDeliveredUnverified(t, s, "outside-orig", "alice", "bob", "outside-body", outside)

	got, _ := s.FindDedupeMatch(ctx, "alice", "bob", "boundary-body", cutoff)
	if got == nil || got.PublicID != "inside-orig" {
		id := "<nil>"
		if got != nil {
			id = got.PublicID
		}
		t.Errorf("inside window: want inside-orig, got %s", id)
	}

	got, _ = s.FindDedupeMatch(ctx, "alice", "bob", "outside-body", cutoff)
	if got != nil {
		t.Errorf("outside window: want nil, got %s", got.PublicID)
	}
}
