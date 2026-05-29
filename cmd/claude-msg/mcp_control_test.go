package main

import (
	"context"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

func TestMCP_Control_HappyPath(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")

	got := callMCPTool(t, s, "semaphore.control", map[string]any{
		"to":      "bob",
		"command": "compact",
	})
	if got["ok"] != true {
		t.Fatalf("ok = %v; got=%v", got["ok"], got)
	}
	if got["command"] != "/compact" {
		t.Errorf("command = %v, want /compact", got["command"])
	}
	id, _ := got["id"].(string)
	if len(id) != 4 {
		t.Errorf("id = %q, want 4 hex chars", id)
	}

	// Verify the message landed in the store as a control row.
	msgs, err := s.ListMessages(context.Background(), store.ListFilter{
		ToAgent: "bob", State: store.StateQueued, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("queued = %d, want 1", len(msgs))
	}
	if msgs[0].Kind != store.KindControl {
		t.Errorf("kind = %q, want %q", msgs[0].Kind, store.KindControl)
	}
	if msgs[0].Body != "/compact" {
		t.Errorf("body = %q, want /compact", msgs[0].Body)
	}
}

func TestMCP_Control_RejectsUnknownCommand(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")

	got := callMCPTool(t, s, "semaphore.control", map[string]any{
		"to":      "bob",
		"command": "clear",
	})
	if got["_isError"] != true {
		t.Errorf("expected error for unknown command; got=%v", got)
	}
}

func TestMCP_Control_RejectsUnknownRecipient(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice")

	got := callMCPTool(t, s, "semaphore.control", map[string]any{
		"to":      "ghost",
		"command": "compact",
	})
	if got["_isError"] != true {
		t.Errorf("expected error for unknown recipient; got=%v", got)
	}
}

func TestMCP_Control_RequiresIdentity(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "")
	s := newCmdTestStore(t, "bob")

	got := callMCPTool(t, s, "semaphore.control", map[string]any{
		"to":      "bob",
		"command": "compact",
	})
	if got["_isError"] != true {
		t.Errorf("expected error when identity cannot be resolved; got=%v", got)
	}
}
