package identity_test

import (
	"context"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
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
	t.Setenv("TMUX_AGENT_NAME", "envname")
	t.Setenv("CLAUDE_AGENT_NAME", "")
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

// TestResolve_RegisteredPaneBeatsEnv pins the #549 Fix-1b precedence flip: when
// $TMUX_AGENT_NAME (a name pin) and a REGISTERED $TMUX_PANE disagree, the
// registered pane wins — it is the re-register-reachable truth, whereas the pin
// can be a baked/stale codex global-config value. (Pre-#549 the pin won here,
// which let a stale pin shadow a fresh re-registration until process restart.)
func TestResolve_RegisteredPaneBeatsEnv(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "envname")
	t.Setenv("CLAUDE_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%5")
	s := openTestStore(t, map[string]string{"panebound": "%5"})

	name, src, err := identity.Resolve(context.Background(), s, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if name != "panebound" || src != identity.SourcePane {
		t.Errorf("got name=%q src=%q; want panebound/%q (registered pane beats stale pin)",
			name, src, identity.SourcePane)
	}
}

// TestResolve_EnvWhenPaneUnregistered pins the pin's surviving bootstrap role:
// when $TMUX_PANE is present but NOT in the registry, the name pin is still the
// best signal (the flip only demotes the pin below a *registered* pane).
func TestResolve_EnvWhenPaneUnregistered(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "envname")
	t.Setenv("CLAUDE_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%99") // not registered
	s := openTestStore(t, map[string]string{"panebound": "%5"})

	name, src, err := identity.Resolve(context.Background(), s, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if name != "envname" || src != identity.SourceEnv {
		t.Errorf("got name=%q src=%q; want envname/%q (pin is the fallback for an unregistered pane)",
			name, src, identity.SourceEnv)
	}
}

func TestResolve_PaneWhenEnvEmpty(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
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
	t.Setenv("TMUX_AGENT_NAME", "")
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
	t.Setenv("TMUX_AGENT_NAME", "")
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
