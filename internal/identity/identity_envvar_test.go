package identity

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// withWarnCapture redirects the deprecation WARN to a buffer and resets the
// once-guard, restoring both on cleanup. White-box (package identity) so it can
// touch the unexported package vars.
func withWarnCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevW, prevOnce := deprecationWarnWriter, deprecationWarnOnce
	deprecationWarnWriter = &buf
	deprecationWarnOnce = &sync.Once{}
	t.Cleanup(func() { deprecationWarnWriter = prevW; deprecationWarnOnce = prevOnce })
	return &buf
}

func memStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// setAgentEnv pins both agent-name vars (the chamber process itself may have
// CLAUDE_AGENT_NAME set, so every case must control both). "" = effectively
// unset for the name != "" checks.
func setAgentEnv(t *testing.T, newVal, oldVal string) {
	t.Helper()
	t.Setenv(envAgentName, newVal)
	t.Setenv(legacyEnvAgentName, oldVal)
}

func TestEnvFallback_NewVarWins_NoWarn(t *testing.T) {
	buf := withWarnCapture(t)
	setAgentEnv(t, "engineer", "")
	name, src, err := Resolve(context.Background(), memStore(t), "")
	if err != nil || name != "engineer" || src != SourceEnv {
		t.Fatalf("got (%q,%q,%v), want (engineer,env,nil)", name, src, err)
	}
	if buf.Len() != 0 {
		t.Errorf("canonical var should not warn; got %q", buf.String())
	}
}

func TestEnvFallback_LegacyVar_Warns(t *testing.T) {
	buf := withWarnCapture(t)
	setAgentEnv(t, "", "engineer")
	name, src, err := Resolve(context.Background(), memStore(t), "")
	if err != nil || name != "engineer" || src != SourceEnv {
		t.Fatalf("got (%q,%q,%v), want (engineer,env,nil)", name, src, err)
	}
	got := buf.String()
	for _, want := range []string{
		"WARN deprecated_surface_used",
		"name=CLAUDE_AGENT_NAME",
		"removal=v1.0",
		"TMUX_AGENT_NAME", // points at the replacement
	} {
		if !strings.Contains(got, want) {
			t.Errorf("legacy warn missing %q; got %q", want, got)
		}
	}
}

func TestEnvFallback_BothSet_NewWins_NoWarn(t *testing.T) {
	buf := withWarnCapture(t)
	setAgentEnv(t, "engineer", "stale-old")
	name, _, _ := Resolve(context.Background(), memStore(t), "")
	if name != "engineer" {
		t.Errorf("both set → name = %q, want engineer (canonical wins)", name)
	}
	if buf.Len() != 0 {
		t.Errorf("canonical present → no warn even if legacy also set; got %q", buf.String())
	}
}

func TestEnvFallback_OverrideShortCircuits_NoWarn(t *testing.T) {
	buf := withWarnCapture(t)
	setAgentEnv(t, "", "engineer") // legacy set, but override precedes env
	name, src, _ := Resolve(context.Background(), memStore(t), "explicit-name")
	if name != "explicit-name" || src != SourceExplicit {
		t.Errorf("got (%q,%q), want (explicit-name,explicit)", name, src)
	}
	if buf.Len() != 0 {
		t.Errorf("override short-circuits env → no legacy read, no warn; got %q", buf.String())
	}
}

func TestEnvFallback_LegacyWarnsOncePerProcess(t *testing.T) {
	buf := withWarnCapture(t)
	setAgentEnv(t, "", "engineer")
	for i := 0; i < 3; i++ {
		if _, _, err := Resolve(context.Background(), memStore(t), ""); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if n := strings.Count(buf.String(), "WARN deprecated_surface_used"); n != 1 {
		t.Errorf("legacy warn emitted %d times across 3 resolves, want once-per-process", n)
	}
}
