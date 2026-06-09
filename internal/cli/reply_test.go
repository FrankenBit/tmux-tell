package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// askVia drives the ask CLI path and returns the ask_id.
func askVia(t *testing.T, s *store.Store, from, to, body string) string {
	t.Helper()
	r, err := s.InsertMessage(context.Background(), store.InsertParams{
		FromAgent: from, ToAgent: to, Body: body, ExpectsReply: true,
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	return r.PublicID
}

func replyVia(t *testing.T, s *store.Store, from, to, askID, body string) string {
	t.Helper()
	r, err := s.InsertMessage(context.Background(), store.InsertParams{
		FromAgent: from, ToAgent: to, ReplyTo: askID, Body: body,
	})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	return r.PublicID
}

// TestAsk_SetsExpectsReply pins that the ask path (send with ExpectsReply) marks
// the row + returns an ask_id. Exercises the shared send logic the ask CLI/MCP
// route through.
func TestAsk_SetsExpectsReply(t *testing.T) {
	s := resendStore(t) // alice + bob registered
	withReachability(t, map[string]bool{"%3": true}, true)

	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(context.Background(), s, sendParams{
		From: "alice", To: "bob", Body: "is CI green?", ExpectsReply: true,
		MaxBody: capBodyBytes, MaxRecipient: capRecipientQueue, MaxSender: capSenderBacklog,
		Format: "json",
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if !r.OK || r.ID == "" {
		t.Fatalf("ask resp = %+v, want ok + id", r)
	}
	var er int
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT expects_reply FROM messages WHERE public_id = ?`, r.ID).Scan(&er); err != nil {
		t.Fatalf("read expects_reply: %v", err)
	}
	if er != 1 {
		t.Errorf("ask message expects_reply = %d, want 1", er)
	}
}

// TestAsk_RejectsMultiRecipient: the comma-in-`--to` guard fires before any
// store work, so no store is needed.
func TestAsk_RejectsMultiRecipient(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := runAskCLI([]string{"--from", "alice", "--to", "bob,carol", "q?"}, &stdout, &stderr)
	if exit != exitUsage {
		t.Fatalf("exit = %d, want exitUsage for multi-recipient ask", exit)
	}
}

// TestWaitForReply_RoundTrip: ask → reply → doWaitForReply returns it.
func TestWaitForReply_RoundTrip(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	askID := askVia(t, s, "alice", "bob", "q?")
	rid := replyVia(t, s, "bob", "alice", askID, "yes, green")

	res := doWaitForReply(ctx, s, "alice", askID, time.Second)
	if !res.OK || res.TimedOut || res.Reply == nil {
		t.Fatalf("wait = %+v, want ok + a reply", res)
	}
	if res.Reply.ID != rid || res.Reply.Body != "yes, green" || res.Reply.From != "bob" {
		t.Errorf("reply = %+v, want %s from bob", res.Reply, rid)
	}
}

// TestWaitForReply_Timeout: no reply → timed_out:true, no reply block.
func TestWaitForReply_Timeout(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	askID := askVia(t, s, "alice", "bob", "q?")

	res := doWaitForReply(ctx, s, "alice", askID, 40*time.Millisecond)
	if !res.OK || !res.TimedOut || res.Reply != nil {
		t.Errorf("wait = %+v, want ok + timed_out + no reply", res)
	}
}

// TestWaitForReply_UnverifiedFlag pins Q4: a reply that's delivered_in_input_box
// (verified=0) is returned with unverified:true, not discarded.
func TestWaitForReply_UnverifiedFlag(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	askID := askVia(t, s, "alice", "bob", "q?")
	rid := replyVia(t, s, "bob", "alice", askID, "soft answer")
	// Drive the reply to delivered_in_input_box (verified=0).
	if _, err := s.ClaimNext(ctx, "alice"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.MarkDeliveredInInputBox(ctx, rid); err != nil {
		t.Fatalf("mark: %v", err)
	}

	res := doWaitForReply(ctx, s, "alice", askID, time.Second)
	if res.Reply == nil {
		t.Fatalf("wait = %+v, want the unverified reply returned (not discarded)", res)
	}
	if !res.Reply.Unverified || res.Reply.State != displayStateDeliveredInInputBox {
		t.Errorf("reply = %+v, want unverified:true + state delivered_in_input_box", res.Reply)
	}
}

// TestCheckReplies_NonBlocking: returns all replies, and honors since.
func TestCheckReplies_NonBlocking(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	askID := askVia(t, s, "alice", "bob", "q?")

	// No replies yet → empty, no error, no blocking.
	r0, err := doCheckReplies(ctx, s, "alice", askID, 0)
	if err != nil || !r0.OK || len(r0.Replies) != 0 {
		t.Fatalf("check (none) = %+v err=%v, want ok + empty", r0, err)
	}

	replyVia(t, s, "bob", "alice", askID, "a1")
	replyVia(t, s, "bob", "alice", askID, "a2")
	r1, _ := doCheckReplies(ctx, s, "alice", askID, 0)
	if len(r1.Replies) != 2 {
		t.Fatalf("check = %d replies, want 2", len(r1.Replies))
	}
	// since = first reply's numeric id → only the second.
	first, _ := s.GetMessage(ctx, r1.Replies[0].ID)
	r2, _ := doCheckReplies(ctx, s, "alice", askID, first.ID)
	if len(r2.Replies) != 1 || r2.Replies[0].Body != "a2" {
		t.Errorf("since-filtered = %+v, want only a2", r2.Replies)
	}
}
