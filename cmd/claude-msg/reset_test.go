package main

import (
	"bytes"
	"context"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

func TestReset_DefaultWipesQueuedAndDelivering(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "2"})
	m, _ := s.ClaimNext(ctx, "bob")
	_ = s.MarkDelivered(ctx, m.PublicID)

	var stdout, stderr bytes.Buffer
	exit := runResetWithStore(ctx, s, "", false, "json", &stdout, &stderr)
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
	exit := runResetWithStore(ctx, s, "", true, "json", &stdout, &stderr)
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
	exit := runResetWithStore(ctx, s, "bob", false, "json", &stdout, &stderr)
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
	_ = runResetWithStore(context.Background(), s, "", true, "json", &stdout, &stderr)
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
