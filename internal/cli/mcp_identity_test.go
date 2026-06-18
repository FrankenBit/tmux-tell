package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// stubProc models one process in a synthetic tree for the MCP ancestor walk:
// its environment (the keys the walk reads) and its parent PID.
type stubProc struct {
	env  map[string]string
	ppid int
}

// stubProcTree replaces the /proc readers with lookups into a synthetic process
// tree so the bounded PPid walk can be exercised without a real /proc. A pid not
// in the tree reads as "no env / no parent", which terminates the walk — so an
// empty tree isolates a test from the real parent process environment.
func stubProcTree(t *testing.T, tree map[int]stubProc) {
	t.Helper()
	prevEnv, prevPPID := procEnvForPID, procPPIDForPID
	t.Cleanup(func() { procEnvForPID = prevEnv; procPPIDForPID = prevPPID })
	procEnvForPID = func(pid int, key string) (string, bool) {
		p, ok := tree[pid]
		if !ok {
			return "", false
		}
		v, ok := p.env[key]
		return v, ok
	}
	procPPIDForPID = func(pid int) (int, bool) {
		p, ok := tree[pid]
		if !ok {
			return 0, false
		}
		return p.ppid, true
	}
}

// TestResolveMCPIdentity_OwnRegisteredPaneBeatsPin: the calling process's own
// registered $TMUX_PANE outranks a $TMUX_AGENT_NAME pin (the #549 Fix-1b flip).
func TestResolveMCPIdentity_OwnRegisteredPaneBeatsPin(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "stalepin")
	t.Setenv("TMUX_PANE", "%99")
	s := newCmdTestStore(t, "realname") // realname → %99

	got, err := resolveMCPIdentity(context.Background(), s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "realname" {
		t.Errorf("got %q, want realname (own registered pane beats stale pin)", got)
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

// TestResolveMCPIdentity_AncestorPaneLookup: the codex MCP child has no own
// TMUX_PANE, but its immediate parent does — the walk finds it (#553).
func TestResolveMCPIdentity_AncestorPaneLookup(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	ppid := os.Getppid()
	stubProcTree(t, map[int]stubProc{
		ppid: {env: map[string]string{"TMUX_PANE": "%5"}, ppid: 1},
	})
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
		t.Errorf("got %q, want pilot (ancestor TMUX_PANE=%%5)", got)
	}
}

// TestResolveMCPIdentity_AncestorPaneAcrossWrapperDepth: a shim/wrapper sits
// between Codex and the MCP server and drops TMUX_PANE; the bounded walk climbs
// past it to the Codex process that still has it. This is the gap #562's
// immediate-parent-only read could not cover (Lookout finding 2).
func TestResolveMCPIdentity_AncestorPaneAcrossWrapperDepth(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	ppid := os.Getppid()
	const codexPID = 999001
	stubProcTree(t, map[int]stubProc{
		ppid:     {env: nil, ppid: codexPID},                           // shim: no tmux env
		codexPID: {env: map[string]string{"TMUX_PANE": "%5"}, ppid: 1}, // codex two levels up
	})
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "pilot", "%5")

	got, err := resolveMCPIdentity(ctx, s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "pilot" {
		t.Errorf("got %q, want pilot (walked past wrapper to codex pane)", got)
	}
}

// TestResolveMCPIdentity_AncestorPaneBeatsOwnStalePin is the Fix-1b thesis at the
// MCP layer: the child carries a STALE $TMUX_AGENT_NAME pin but no own pane; an
// ancestor's registered pane must win over the pin (the pin is the global-config
// value #562 left winning before the parent pane was consulted).
func TestResolveMCPIdentity_AncestorPaneBeatsOwnStalePin(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "stalepin")
	t.Setenv("TMUX_PANE", "")
	ppid := os.Getppid()
	stubProcTree(t, map[int]stubProc{
		ppid: {env: map[string]string{"TMUX_PANE": "%5"}, ppid: 1},
	})
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "realname", "%5")

	got, err := resolveMCPIdentity(ctx, s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "realname" {
		t.Errorf("got %q, want realname (ancestor pane beats own stale pin)", got)
	}
}

// TestResolveMCPIdentity_AncestorNamePinFallback: no resolvable pane anywhere, so
// an ancestor's name pin is the last-resort signal.
func TestResolveMCPIdentity_AncestorNamePinFallback(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	ppid := os.Getppid()
	stubProcTree(t, map[int]stubProc{
		ppid: {env: map[string]string{"TMUX_AGENT_NAME": "carpenter"}, ppid: 1},
	})
	s := newCmdTestStore(t)

	got, err := resolveMCPIdentity(context.Background(), s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "carpenter" {
		t.Errorf("got %q, want carpenter (ancestor name pin fallback)", got)
	}
}

// TestResolveMCPIdentity_AncestorUnregisteredPaneFailsLoud: the first pane-bearing
// ancestor carries an UNregistered pane — fail loud with register advice rather
// than silently climbing to a higher ancestor's name pin (Lookout finding 1).
func TestResolveMCPIdentity_AncestorUnregisteredPaneFailsLoud(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	ppid := os.Getppid()
	const codexPID = 999002
	stubProcTree(t, map[int]stubProc{
		// First pane-bearing ancestor is unregistered; a higher one has a name
		// pin that must NOT be reached.
		ppid:     {env: map[string]string{"TMUX_PANE": "%888"}, ppid: codexPID},
		codexPID: {env: map[string]string{"TMUX_AGENT_NAME": "should-not-win"}, ppid: 1},
	})
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })

	_, err := resolveMCPIdentity(context.Background(), s)
	if err == nil {
		t.Fatal("want fail-loud error for unregistered ancestor pane, got nil")
	}
	if !strings.Contains(err.Error(), "%888") || !strings.Contains(err.Error(), "register") {
		t.Errorf("error should name %%888 and suggest register; got: %v", err)
	}
	if strings.Contains(err.Error(), "should-not-win") {
		t.Errorf("walk must stop at the first pane-bearing ancestor, not climb to a name pin; got: %v", err)
	}
}

// TestResolveMCPIdentity_OwnUnregisteredPaneReturnsError (#355): an own pane that
// is set but not registered returns an error naming the pane.
func TestResolveMCPIdentity_OwnUnregisteredPaneReturnsError(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%999")       // not registered
	stubProcTree(t, map[int]stubProc{}) // isolate from real /proc
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

// TestResolveMCPIdentity_NeitherEnvSet (#355/#553): no own env, no resolvable
// ancestor — an actionable error naming the missing source and MCP context.
func TestResolveMCPIdentity_NeitherEnvSet(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	stubProcTree(t, map[int]stubProc{}) // isolate from real /proc
	s := newCmdTestStore(t, "alice")

	_, err := resolveMCPIdentity(context.Background(), s)
	if err == nil {
		t.Fatal("want error when no identity source, got nil")
	}
	if !strings.Contains(err.Error(), "TMUX_AGENT_NAME") {
		t.Errorf("error should mention TMUX_AGENT_NAME; got: %v", err)
	}
	if !strings.Contains(err.Error(), "MCP") {
		t.Errorf("error should mention MCP wrapper; got: %v", err)
	}
}

// TestResolveMCPIdentity_WalkCycleGuardTerminates: a cyclic PPid chain (corrupt
// /proc) must not hang — the visited-set breaks the cycle and the walk falls
// through to the no-identity error.
func TestResolveMCPIdentity_WalkCycleGuardTerminates(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "")
	ppid := os.Getppid()
	const a, b = 999010, 999011
	stubProcTree(t, map[int]stubProc{
		ppid: {ppid: a},
		a:    {ppid: b},
		b:    {ppid: a}, // cycle a → b → a
	})
	s := newCmdTestStore(t, "alice")

	_, err := resolveMCPIdentity(context.Background(), s)
	if err == nil {
		t.Fatal("want no-identity error (cycle yields no pane), got nil")
	}
}
