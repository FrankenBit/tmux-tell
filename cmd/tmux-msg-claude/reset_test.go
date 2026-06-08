package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func TestReset_DefaultWipesQueuedAndDelivering(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "2"})
	m, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkDelivered(ctx, m.PublicID)

	var stdout, stderr bytes.Buffer
	exit := runResetWithStore(ctx, s, "", false, "", "", "json", time.Time{}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	// 1 queued before delete; delivered stays because hard=false.
	if int(got["deleted"].(float64)) != 1 {
		t.Errorf("deleted = %v, want 1", got["deleted"])
	}
	delivered, _ := s.ListMessages(ctx, store.ListFilter{State: store.StateDelivered, Limit: 10})
	if len(delivered) != 1 {
		t.Errorf("delivered preserved? got %d, want 1", len(delivered))
	}
}

func TestReset_HardWipesAll(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	m, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkDelivered(ctx, m.PublicID)
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "2"})

	var stdout, stderr bytes.Buffer
	exit := runResetWithStore(ctx, s, "", true, "", "", "json", time.Time{}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if int(got["deleted"].(float64)) != 2 {
		t.Errorf("deleted = %v, want 2", got["deleted"])
	}
	all, _ := s.ListMessages(ctx, store.ListFilter{Limit: 10})
	if len(all) != 0 {
		t.Errorf("remaining = %d, want 0 after --hard", len(all))
	}
}

func TestReset_ScopedToOneAgent(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "carol", Body: "2"})

	var stdout, stderr bytes.Buffer
	exit := runResetWithStore(ctx, s, "bob", false, "", "", "json", time.Time{}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if int(got["deleted"].(float64)) != 1 {
		t.Errorf("deleted = %v, want 1", got["deleted"])
	}
	carol, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "carol"})
	if len(carol) != 1 {
		t.Errorf("carol's messages = %d, want 1 (untouched)", len(carol))
	}
}

func TestReset_LeavesAgentsTableAlone(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	var stdout, stderr bytes.Buffer
	_ = runResetWithStore(context.Background(), s, "", true, "", "", "json", time.Time{}, &stdout, &stderr)
	list, _ := s.ListAgents(context.Background())
	if len(list) != 2 {
		t.Errorf("agents = %d, want 2", len(list))
	}
}

func TestResetCLI_RequiresConfirm(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := runResetCLI([]string{"--db", ":memory:"}, &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d", exit, exitUsage)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != false {
		t.Errorf("ok = %v, want false", got["ok"])
	}
}

// future is far enough ahead that all messages inserted during a test are
// "older than" any reasonable duration window.
var future = time.Now().Add(365 * 24 * time.Hour)

func TestReset_OlderThan_DefaultDeletesDeliveredAndFailed(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	// Two delivered, one failed, one queued.
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "d1"})
	m1, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkDelivered(ctx, m1.PublicID)
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "d2"})
	m2, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkDelivered(ctx, m2.PublicID)
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "f1"})
	m3, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkFailed(ctx, m3.PublicID, "timeout")
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "q1"})

	var stdout, stderr bytes.Buffer
	exit := runResetWithStore(ctx, s, "", false, "1h", "", "json", future, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s stdout=%s", exit, stderr.String(), stdout.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if int(got["deleted"].(float64)) != 3 {
		t.Errorf("deleted = %v, want 3 (2 delivered + 1 failed)", got["deleted"])
	}
	queued, _ := s.ListMessages(ctx, store.ListFilter{State: store.StateQueued, Limit: 10})
	if len(queued) != 1 {
		t.Errorf("queued remaining = %d, want 1 (in-flight must survive)", len(queued))
	}
}

func TestReset_OlderThan_StateFilter(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "d"})
	md, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkDelivered(ctx, md.PublicID)
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "f"})
	mf, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkFailed(ctx, mf.PublicID, "err")

	var stdout, stderr bytes.Buffer
	exit := runResetWithStore(ctx, s, "", false, "1h", "delivered", "json", future, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if int(got["deleted"].(float64)) != 1 {
		t.Errorf("deleted = %v, want 1 (only delivered)", got["deleted"])
	}
	failed, _ := s.ListMessages(ctx, store.ListFilter{State: store.StateFailed, Limit: 10})
	if len(failed) != 1 {
		t.Errorf("failed remaining = %d, want 1 (should not be touched)", len(failed))
	}
}

func TestReset_OlderThan_ScopedToAgent(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "b"})
	mb, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkDelivered(ctx, mb.PublicID)
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "carol", Body: "c"})
	mc, _ := s.ClaimNext(ctx, "carol")
	_ = s.MarkDelivered(ctx, mc.PublicID)

	var stdout, stderr bytes.Buffer
	exit := runResetWithStore(ctx, s, "bob", false, "1h", "", "json", future, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if int(got["deleted"].(float64)) != 1 {
		t.Errorf("deleted = %v, want 1 (bob only)", got["deleted"])
	}
	carolMsgs, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "carol", State: store.StateDelivered, Limit: 10})
	if len(carolMsgs) != 1 {
		t.Errorf("carol's delivered = %d, want 1 (untouched)", len(carolMsgs))
	}
}

func TestReset_OlderThan_RefusesHardFlag(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	var stdout, stderr bytes.Buffer
	exit := runResetWithStore(context.Background(), s, "", true, "1h", "", "json", time.Now(), &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d (--older-than+--hard must be rejected)", exit, exitUsage)
	}
}

func TestReset_OlderThan_RejectsInvalidState(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	var stdout, stderr bytes.Buffer
	exit := runResetWithStore(context.Background(), s, "", false, "1h", "queued", "json", time.Now(), &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d (queued not allowed with --older-than)", exit, exitUsage)
	}
}
