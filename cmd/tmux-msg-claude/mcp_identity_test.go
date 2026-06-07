package main

import (
	"context"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
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

func TestResolveMCPIdentity_NoMatchReturnsEmpty(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%999") // not registered
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })

	got, err := resolveMCPIdentity(context.Background(), s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty for unregistered pane", got)
	}
}

func TestResolveMCPIdentity_NeitherEnvSet(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	s := newCmdTestStore(t, "alice")

	got, err := resolveMCPIdentity(context.Background(), s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty when no identity source", got)
	}
}
