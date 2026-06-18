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

// withMismatchCapture redirects the #549 Fix-1b identity_mismatch WARN to a
// buffer and resets its once-guard, restoring both on cleanup. White-box so it
// can touch the unexported package vars.
func withMismatchCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevW, prevOnce := mismatchWarnWriter, mismatchWarnOnce
	mismatchWarnWriter = &buf
	mismatchWarnOnce = &sync.Once{}
	t.Cleanup(func() { mismatchWarnWriter = prevW; mismatchWarnOnce = prevOnce })
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

// TestResolve_StalePinWarnsAndPaneWins pins the #549 Fix-1b mismatch signal:
// when a $TMUX_AGENT_NAME pin disagrees with a registered pane, the pane wins
// AND a loud identity_mismatch WARN names the stale pin.
func TestResolve_StalePinWarnsAndPaneWins(t *testing.T) {
	buf := withMismatchCapture(t)
	setAgentEnv(t, "stalepin", "")
	t.Setenv("TMUX_PANE", "%5")
	s := memStore(t)
	if err := s.UpsertAgent(context.Background(), "realname", "%5"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	name, src, err := Resolve(context.Background(), s, "")
	if err != nil || name != "realname" || src != SourcePane {
		t.Fatalf("got (%q,%q,%v), want (realname,pane,nil)", name, src, err)
	}
	got := buf.String()
	for _, want := range []string{"WARN identity_mismatch", "stalepin", "realname"} {
		if !strings.Contains(got, want) {
			t.Errorf("mismatch warn missing %q; got %q", want, got)
		}
	}
}

// TestResolve_AgreeingPinNoWarn: a pin that AGREES with the registered pane is
// not a conflict — no warning.
func TestResolve_AgreeingPinNoWarn(t *testing.T) {
	buf := withMismatchCapture(t)
	setAgentEnv(t, "samename", "")
	t.Setenv("TMUX_PANE", "%5")
	s := memStore(t)
	if err := s.UpsertAgent(context.Background(), "samename", "%5"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, src, _ := Resolve(context.Background(), s, ""); src != SourcePane {
		t.Errorf("src = %q, want pane", src)
	}
	if buf.Len() != 0 {
		t.Errorf("agreeing pin must not warn; got %q", buf.String())
	}
}

// TestResolve_MismatchWarnsOncePerProcess: the long-lived MCP server's env is
// fixed, so a persistent mismatch must warn once, not on every resolve.
func TestResolve_MismatchWarnsOncePerProcess(t *testing.T) {
	buf := withMismatchCapture(t)
	setAgentEnv(t, "stalepin", "")
	t.Setenv("TMUX_PANE", "%5")
	s := memStore(t)
	if err := s.UpsertAgent(context.Background(), "realname", "%5"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := Resolve(context.Background(), s, ""); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if n := strings.Count(buf.String(), "WARN identity_mismatch"); n != 1 {
		t.Errorf("mismatch warn emitted %d times across 3 resolves, want once", n)
	}
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
