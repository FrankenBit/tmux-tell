package store

import (
	"context"
	"testing"
	"time"
)

// ask inserts an ask from→to and returns its public_id.
func ask(t *testing.T, s *Store, from, to, body string) string {
	t.Helper()
	r, err := s.InsertMessage(context.Background(), InsertParams{
		FromAgent: from, ToAgent: to, Body: body, ExpectsReply: true,
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	return r.PublicID
}

// reply inserts a reply from→to threaded under askID and returns its public_id.
func reply(t *testing.T, s *Store, from, to, askID, body string) string {
	t.Helper()
	r, err := s.InsertMessage(context.Background(), InsertParams{
		FromAgent: from, ToAgent: to, ReplyTo: askID, Body: body,
	})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	return r.PublicID
}

// TestExpectsReply_Column pins the Q1 marker: ask sets expects_reply=1, a plain
// send leaves it 0.
func TestExpectsReply_Column(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	askID := ask(t, s, "alice", "bob", "q?")
	plain, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "fyi"})

	read := func(id string) int {
		var v int
		if err := s.DB().QueryRowContext(ctx, `SELECT expects_reply FROM messages WHERE public_id = ?`, id).Scan(&v); err != nil {
			t.Fatalf("read expects_reply %s: %v", id, err)
		}
		return v
	}
	if read(askID) != 1 {
		t.Errorf("ask expects_reply = %d, want 1", read(askID))
	}
	if read(plain.PublicID) != 0 {
		t.Errorf("plain send expects_reply = %d, want 0", read(plain.PublicID))
	}
}

// TestListReplies pins reply scoping + the since filter: a reply is reply_to=ask
// AND to_agent=asker; unrelated messages are excluded.
func TestListReplies(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	askID := ask(t, s, "alice", "bob", "q?")
	r1 := reply(t, s, "bob", "alice", askID, "a1")
	r2 := reply(t, s, "bob", "alice", askID, "a2")
	// Noise: a reply to a different ask, and a non-reply to alice.
	other := ask(t, s, "alice", "carol", "q2?")
	reply(t, s, "carol", "alice", other, "different thread")
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "bob", ToAgent: "alice", Body: "unthreaded"})

	got, err := s.ListReplies(ctx, "alice", askID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].PublicID != r1 || got[1].PublicID != r2 {
		t.Fatalf("replies = %v, want [%s %s] in order", ids(got), r1, r2)
	}

	// since filter: only replies after r1's id.
	after, _ := s.ListReplies(ctx, "alice", askID, got[0].ID)
	if len(after) != 1 || after[0].PublicID != r2 {
		t.Errorf("since-filtered = %v, want only %s", ids(after), r2)
	}
}

func ids(ms []Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.PublicID
	}
	return out
}

// TestWaitForReply_FindsExisting: a reply already present returns immediately.
func TestWaitForReply_FindsExisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	askID := ask(t, s, "alice", "bob", "q?")
	rid := reply(t, s, "bob", "alice", askID, "answer")

	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	m, err := s.WaitForReply(ctx, "alice", askID, 0, time.Millisecond)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if m == nil || m.PublicID != rid {
		t.Errorf("wait = %v, want reply %s", m, rid)
	}
}

// TestWaitForReply_Timeout: no reply → returns ctx error (caller maps to
// timed_out).
func TestWaitForReply_Timeout(t *testing.T) {
	s := newTestStore(t)
	askID := ask(t, s, "alice", "bob", "q?")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	m, err := s.WaitForReply(ctx, "alice", askID, 0, 5*time.Millisecond)
	if m != nil {
		t.Errorf("wait returned a reply %v, want nil on timeout", m)
	}
	if err == nil {
		t.Error("wait should return ctx error on timeout")
	}
}

// TestWaitForReply_ArrivesDuringWait: the blocking wait returns the reply that
// lands while it's polling — the push-shaped behavior.
func TestWaitForReply_ArrivesDuringWait(t *testing.T) {
	s := newTestStore(t)
	askID := ask(t, s, "alice", "bob", "q?")

	go func() {
		time.Sleep(20 * time.Millisecond)
		reply(t, s, "bob", "alice", askID, "late answer")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m, err := s.WaitForReply(ctx, "alice", askID, 0, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if m == nil || m.Body != "late answer" {
		t.Errorf("wait = %v, want the late-arriving reply", m)
	}
}
