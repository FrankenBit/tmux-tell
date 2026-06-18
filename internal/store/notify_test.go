package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recordNotifier installs a notifier that records every notified recipient and
// resets the package global on cleanup, so tests don't bleed notifier state into
// one another.
func recordNotifier(t *testing.T) *notifyRecorder {
	t.Helper()
	r := &notifyRecorder{}
	SetNotifier(r.fire)
	t.Cleanup(func() { SetNotifier(nil) })
	return r
}

type notifyRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *notifyRecorder) fire(toAgent string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, toAgent)
}

func (r *notifyRecorder) recipients() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestInsertMessageFiresNotifierPostCommit pins #515 D4: the notifier fires
// strictly AFTER the insert commits, so the queued row is already visible to a
// fresh read when the doorbell rings. The notifier reads the DB (a different
// pool acquisition than the insert's tx) and asserts the row is present.
//
// Mutation anchor (verified): move fireNotify above tx.Commit() in InsertMessage
// and this fails — under the single-conn pool the in-tx read can't acquire the
// connection the open tx holds, so the bounded read returns nothing.
func TestInsertMessageFiresNotifierPostCommit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	var (
		mu         sync.Mutex
		fired      bool
		gotAgent   string
		rowVisible bool
	)
	SetNotifier(func(toAgent string) {
		mu.Lock()
		defer mu.Unlock()
		fired = true
		gotAgent = toAgent
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var n int
		_ = s.DB().QueryRowContext(rctx,
			`SELECT COUNT(*) FROM messages WHERE to_agent = ? AND state = ?`,
			toAgent, StateQueued).Scan(&n)
		rowVisible = n >= 1
	})
	t.Cleanup(func() { SetNotifier(nil) })

	if _, err := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !fired {
		t.Fatal("notifier did not fire on a queued insert")
	}
	if gotAgent != "bob" {
		t.Fatalf("notified %q, want bob", gotAgent)
	}
	if !rowVisible {
		t.Fatal("row not visible when notifier fired — notify fired pre-commit (D4 violated)")
	}
}

// TestInsertNoticeFiresNotifier: a notice (always immediate) rings its recipient.
func TestInsertNoticeFiresNotifier(t *testing.T) {
	s := newTestStore(t)
	r := recordNotifier(t)
	if _, err := s.InsertNotice(context.Background(), InsertParams{
		FromAgent: "sys", ToAgent: "carol", Body: "delivery failed", Kind: KindDeliveryFailureNotice,
	}); err != nil {
		t.Fatalf("insert notice: %v", err)
	}
	if got := r.recipients(); len(got) != 1 || got[0] != "carol" {
		t.Fatalf("notice notified %v, want [carol]", got)
	}
}

// TestDeferredInsertDoesNotFireNotifier pins the queued-only rule: a deferred
// (staged) insert is not deliverable, so it must NOT ring — it rings later, at
// promotion. PromoteDeferred then rings exactly once.
func TestDeferredInsertDoesNotFireNotifier(t *testing.T) {
	s := newTestStore(t)
	r := recordNotifier(t)
	ctx := context.Background()

	if _, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "later", DeliverAfter: "resume",
	}); err != nil {
		t.Fatalf("deferred insert: %v", err)
	}
	if got := r.recipients(); len(got) != 0 {
		t.Fatalf("deferred insert rang %v, want no ring until promotion", got)
	}

	n, err := s.PromoteDeferred(ctx, "bob", "resume")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if n != 1 {
		t.Fatalf("promoted %d, want 1", n)
	}
	if got := r.recipients(); len(got) != 1 || got[0] != "bob" {
		t.Fatalf("promotion rang %v, want [bob]", got)
	}
}

// TestPromoteDeferredNoRowsNoRing: a promote that matches nothing must not ring
// (no row became deliverable).
func TestPromoteDeferredNoRowsNoRing(t *testing.T) {
	s := newTestStore(t)
	r := recordNotifier(t)
	n, err := s.PromoteDeferred(context.Background(), "nobody", "resume")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if n != 0 {
		t.Fatalf("promoted %d, want 0", n)
	}
	if got := r.recipients(); len(got) != 0 {
		t.Fatalf("empty promote rang %v, want none", got)
	}
}

// TestNilNotifierSafe: with no notifier installed, inserts work (the hook is a
// no-op). Also covers the read-only / test default.
func TestNilNotifierSafe(t *testing.T) {
	SetNotifier(nil)
	s := newTestStore(t)
	if _, err := s.InsertMessage(context.Background(), InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hi",
	}); err != nil {
		t.Fatalf("insert with nil notifier: %v", err)
	}
}

// TestNotifierPanicSwallowed pins Surveyor #515 note 1: a panicking notifier can
// never propagate into a just-committed write's return path.
func TestNotifierPanicSwallowed(t *testing.T) {
	SetNotifier(func(string) { panic("boom") })
	t.Cleanup(func() { SetNotifier(nil) })
	s := newTestStore(t)
	if _, err := s.InsertMessage(context.Background(), InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hi",
	}); err != nil {
		t.Fatalf("insert returned error after notifier panic (should be swallowed): %v", err)
	}
}

// TestWaitForReplySelfHealsWithoutRing pins the #515 load-bearing insight: notify
// is an optimization over a poll, NEVER a replacement. Even with a watcher whose
// channel never fires (a permanently dropped doorbell), WaitForReply still
// completes via its poll — the answer always still comes from SQLite.
//
// Mutation anchor: delete the `case <-time.After(pollInterval):` arm from
// WaitForReply's select and this hangs to the test deadline (the never-firing
// ring can't carry it) — proving the poll is the correctness path.
func TestWaitForReplySelfHealsWithoutRing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// A watcher that hands back a channel which never fires.
	SetWatcher(func(context.Context, string) <-chan struct{} { return make(chan struct{}) })
	t.Cleanup(func() { SetWatcher(nil) })

	askID := ask(t, s, "alice", "bob", "q?")

	// Insert the reply mid-wait, directly (no t.Fatalf off the test goroutine).
	errc := make(chan error, 1)
	go func() {
		time.Sleep(40 * time.Millisecond)
		_, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "bob", ToAgent: "alice", ReplyTo: askID, Body: "a!",
		})
		errc <- err
	}()

	// Small poll interval so the poll — not the never-firing ring — drives this.
	m, err := s.WaitForReply(ctx, "alice", askID, 0, 15*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForReply: %v", err)
	}
	if m == nil || m.Body != "a!" {
		t.Fatalf("expected reply surfaced by poll, got %+v", m)
	}
	if err := <-errc; err != nil {
		t.Fatalf("reply insert: %v", err)
	}
}

// TestWaitForReplyWakesOnRing is the complement: when the watcher DOES fire,
// WaitForReply returns promptly without waiting out its (here very long) poll.
func TestWaitForReplyWakesOnRing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ring := make(chan struct{}, 1)
	SetWatcher(func(context.Context, string) <-chan struct{} { return ring })
	t.Cleanup(func() { SetWatcher(nil) })

	askID := ask(t, s, "alice", "bob", "q?")

	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = s.InsertMessage(ctx, InsertParams{
			FromAgent: "bob", ToAgent: "alice", ReplyTo: askID, Body: "a!",
		})
		ring <- struct{}{} // wake the waiter
	}()

	// Poll interval is 10s — only the ring can return this within the deadline.
	done := make(chan *Message, 1)
	go func() {
		m, _ := s.WaitForReply(ctx, "alice", askID, 0, 10*time.Second)
		done <- m
	}()
	select {
	case m := <-done:
		if m == nil || m.Body != "a!" {
			t.Fatalf("expected reply via ring, got %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitForReply did not wake on the ring within 3s (notify wake not wired)")
	}
}
