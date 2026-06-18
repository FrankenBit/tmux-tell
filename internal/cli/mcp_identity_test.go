package cli

import (
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

func TestResolveMCPIdentity_PrefersExplicitEnv(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "explicit")
	t.Setenv("TMUX_PANE", "%99")
	s := newCmdTestStore(t, "explicit") // pane_id=%99 in newCmdTestStore

	got, err := resolveMCPIdentity(context.Background(), s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "explicit" {
		t.Errorf("got %q, want explicit (env override)", got)
	}
}

func TestResolveMCPIdentity_TMUXPaneLookup(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%5")
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "pilot", "%5")
	_ = s.UpsertAgent(ctx, "bosun", "%7")

	got, err := resolveMCPIdentity(ctx, s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "pilot" {
		t.Errorf("got %q, want pilot (TMUX_PANE=%%5)", got)
	}
}

func TestResolveMCPIdentity_ParentTMUXPaneLookup(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	prevParentEnv := mcpParentEnvValue
	t.Cleanup(func() { mcpParentEnvValue = prevParentEnv })
	mcpParentEnvValue = func(key string) (string, bool) {
		if key == "TMUX_PANE" {
			return "%5", true
		}
		return "", false
	}
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "pilot", "%5")
	_ = s.UpsertAgent(ctx, "bosun", "%7")

	got, err := resolveMCPIdentity(ctx, s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "pilot" {
		t.Errorf("got %q, want pilot (parent TMUX_PANE=%%5)", got)
	}
}

func TestResolveMCPIdentity_ParentAgentEnv(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	prevParentEnv := mcpParentEnvValue
	t.Cleanup(func() { mcpParentEnvValue = prevParentEnv })
	mcpParentEnvValue = func(key string) (string, bool) {
		if key == "TMUX_AGENT_NAME" {
			return "carpenter", true
		}
		return "", false
	}
	s := newCmdTestStore(t)

	got, err := resolveMCPIdentity(context.Background(), s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "carpenter" {
		t.Errorf("got %q, want carpenter (parent TMUX_AGENT_NAME)", got)
	}
}

// TestResolveMCPIdentity_UnregisteredPaneReturnsError confirms the #355 fix:
// a pane that is set but not in the registry returns an error naming the pane,
// not a silent empty string.
func TestResolveMCPIdentity_UnregisteredPaneReturnsError(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%999") // not registered
	prevParentEnv := mcpParentEnvValue
	t.Cleanup(func() { mcpParentEnvValue = prevParentEnv })
	mcpParentEnvValue = func(string) (string, bool) { return "", false }
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })

	_, err := resolveMCPIdentity(context.Background(), s)
	if err == nil {
		t.Fatal("want error for unregistered pane, got nil")
	}
	if !strings.Contains(err.Error(), "%999") {
		t.Errorf("error should name the unregistered pane; got: %v", err)
	}
	if !strings.Contains(err.Error(), "register") {
		t.Errorf("error should suggest register; got: %v", err)
	}
}

// TestResolveMCPIdentity_NeitherEnvSet confirms the #355 fix: when both
// $TMUX_AGENT_NAME and $TMUX_PANE are empty (typical codex MCP child), an
// actionable error naming the missing env source is returned.
func TestResolveMCPIdentity_NeitherEnvSet(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	prevParentEnv := mcpParentEnvValue
	t.Cleanup(func() { mcpParentEnvValue = prevParentEnv })
	mcpParentEnvValue = func(string) (string, bool) { return "", false }
	s := newCmdTestStore(t, "alice")

	_, err := resolveMCPIdentity(context.Background(), s)
	if err == nil {
		t.Fatal("want error when no identity source, got nil")
	}
	// Error must hint at the missing identity source and MCP spawn context (#355/#553).
	if !strings.Contains(err.Error(), "TMUX_AGENT_NAME") {
		t.Errorf("error should mention TMUX_AGENT_NAME; got: %v", err)
	}
	if !strings.Contains(err.Error(), "MCP") {
		t.Errorf("error should mention MCP wrapper; got: %v", err)
	}
}
