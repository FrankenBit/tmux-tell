package identity_test

import (
	"context"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func openTestStore(t *testing.T, agents map[string]string) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	for name, pane := range agents {
		if err := s.UpsertAgent(ctx, name, pane); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return s
}

func TestResolve_ExplicitOverrideWins(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "envname")
	t.Setenv("TMUX_PANE", "%5")
	s := openTestStore(t, map[string]string{"panebound": "%5"})

	name, src, err := identity.Resolve(context.Background(), s, "explicit")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if name != "explicit" || src != identity.SourceExplicit {
		t.Errorf("got name=%q src=%q; want explicit/%q",
			name, src, identity.SourceExplicit)
	}
}

func TestResolve_EnvBeatsRegistry(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "envname")
	t.Setenv("TMUX_PANE", "%5")
	s := openTestStore(t, map[string]string{"panebound": "%5"})

	name, src, err := identity.Resolve(context.Background(), s, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if name != "envname" || src != identity.SourceEnv {
		t.Errorf("got name=%q src=%q; want envname/%q",
			name, src, identity.SourceEnv)
	}
}

func TestResolve_PaneWhenEnvEmpty(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%5")
	s := openTestStore(t, map[string]string{"panebound": "%5"})

	name, src, err := identity.Resolve(context.Background(), s, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if name != "panebound" || src != identity.SourcePane {
		t.Errorf("got name=%q src=%q; want panebound/%q",
			name, src, identity.SourcePane)
	}
}

func TestResolve_NoneWhenPaneUnregistered(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%99")
	s := openTestStore(t, map[string]string{"otherpane": "%1"})

	name, src, err := identity.Resolve(context.Background(), s, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if name != "" || src != identity.SourceNone {
		t.Errorf("got name=%q src=%q; want empty/none", name, src)
	}
}

func TestResolve_NoneWhenNothingAvailable(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	s := openTestStore(t, nil)

	name, src, err := identity.Resolve(context.Background(), s, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if name != "" || src != identity.SourceNone {
		t.Errorf("got name=%q src=%q; want empty/none", name, src)
	}
}
