package main

import (
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// Self-invocation of a self-only command (compact) is the canonical
// "agent quietly trims its own context" path.
func TestMCP_Control_SelfInvocation_SelfOnlyCommand(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice")

	got := callMCPTool(t, s, "semaphore.control", map[string]any{
		"to":      "alice",
		"command": "compact",
	})
	if got["ok"] != true {
		t.Fatalf("ok = %v; got=%v", got["ok"], got)
	}
	if got["command"] != "/compact" {
		t.Errorf("command = %v, want /compact", got["command"])
	}

	msgs, err := s.ListMessages(context.Background(), store.ListFilter{
		ToAgent: "alice", State: store.StateQueued, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("queued = %d, want 1", len(msgs))
	}
	if msgs[0].Kind != store.KindControl || msgs[0].Body != "/compact" {
		t.Errorf("row = %+v", msgs[0])
	}
	if msgs[0].FromAgent != "alice" || msgs[0].ToAgent != "alice" {
		t.Errorf("self-invocation should round-trip: from=%q to=%q",
			msgs[0].FromAgent, msgs[0].ToAgent)
	}
}

// Peer-invoking a peer-allowed command (rename) succeeds.
func TestMCP_Control_PeerInvocation_PeerAllowedCommand(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")

	got := callMCPTool(t, s, "semaphore.control", map[string]any{
		"to":      "bob",
		"command": "rename",
	})
	if got["ok"] != true {
		t.Fatalf("rename peer-invoke should succeed; got=%v", got)
	}
	if got["command"] != "/rename" {
		t.Errorf("command = %v, want /rename", got["command"])
	}
}

// Peer-invoking a self-only command (compact) is blocked at the MCP
// boundary — the regression this scope split exists to prevent.
func TestMCP_Control_PeerInvocation_BlockedForSelfOnlyCommand(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")

	got := callMCPTool(t, s, "semaphore.control", map[string]any{
		"to":      "bob",
		"command": "compact",
	})
	if got["_isError"] != true {
		t.Fatalf("compact must be peer-denied; got=%v", got)
	}
	text, _ := got["_text"].(string)
	if !strings.Contains(text, "self-only") {
		t.Errorf("error text should mention self-only; got %q", text)
	}

	// No row should be queued.
	msgs, _ := s.ListMessages(context.Background(), store.ListFilter{
		ToAgent: "bob", State: store.StateQueued, Limit: 10,
	})
	if len(msgs) != 0 {
		t.Errorf("denied peer-control must not queue a row; got %d", len(msgs))
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
