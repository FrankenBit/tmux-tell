package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

func TestStatus_EmptyRegistry(t *testing.T) {
	s := newCmdTestStore(t)
	var stdout, stderr bytes.Buffer
	exit := runStatusWithStore(context.Background(), s, "text", false, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "NAME\tPAUSED") {
		t.Errorf("missing header in %q", stdout.String())
	}
}

func TestStatus_ReflectsQueueDepth(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "2"})
	_ = s.SetPaused(ctx, "alice", true)

	var stdout, stderr bytes.Buffer
	exit := runStatusWithStore(ctx, s, "json", false, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []agentStatus
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	byName := map[string]agentStatus{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if !byName["alice"].Paused {
		t.Errorf("alice should be paused")
	}
	if byName["bob"].Queued != 2 {
		t.Errorf("bob queued = %d, want 2", byName["bob"].Queued)
	}
}
