package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// fakeSystemctl records every invocation and lets the test assert on it.
type fakeSystemctl struct {
	calls [][]string
	err   error
	out   []byte
}

func (f *fakeSystemctl) install(t *testing.T) {
	t.Helper()
	prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{}, args...))
		return f.out, f.err
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })
}

func TestMCP_Register_HappyPath(t *testing.T) {
	t.Setenv("TMUX_PANE", "%9")
	s := newCmdTestStore(t) // empty registry
	fs := &fakeSystemctl{}
	fs.install(t)

	got := callMCPTool(t, s, "semaphore.register", map[string]any{
		"name": "newone",
	})
	if got["ok"] != true {
		t.Errorf("ok = %v; got=%v", got["ok"], got)
	}
	if got["name"] != "newone" || got["pane"] != "%9" {
		t.Errorf("got %v", got)
	}
	if got["mailman"] != "active" {
		t.Errorf("mailman = %v, want active", got["mailman"])
	}

	a, err := s.GetAgent(context.Background(), "newone")
	if err != nil {
		t.Fatalf("agent not found in store: %v", err)
	}
	if a.PaneID != "%9" {
		t.Errorf("pane_id = %q, want %%9", a.PaneID)
	}
	// systemctl --user enable --now claude-mailman@newone.service
	if len(fs.calls) != 1 || fs.calls[0][2] != "claude-mailman@newone.service" {
		t.Errorf("systemctl calls = %v", fs.calls)
	}
}

func TestMCP_Register_ExplicitPaneArg(t *testing.T) {
	t.Setenv("TMUX_PANE", "%1") // should be ignored when pane is given
	s := newCmdTestStore(t)
	(&fakeSystemctl{}).install(t)

	got := callMCPTool(t, s, "semaphore.register", map[string]any{
		"name":          "explicit",
		"pane":          "%42",
		"start_mailman": false,
	})
	if got["pane"] != "%42" {
		t.Errorf("pane = %v, want %%42", got["pane"])
	}
	if got["mailman"] != "skipped" {
		t.Errorf("mailman = %v, want skipped", got["mailman"])
	}
}

func TestMCP_Register_CollisionWithoutForce(t *testing.T) {
	t.Setenv("TMUX_PANE", "%9")
	s := newCmdTestStore(t, "existing")
	(&fakeSystemctl{}).install(t)

	got := callMCPTool(t, s, "semaphore.register", map[string]any{
		"name": "existing",
	})
	if got["_isError"] != true {
		t.Errorf("should fail without force; got=%v", got)
	}
	// Store should be untouched (still pointing at %99 from newCmdTestStore).
	a, _ := s.GetAgent(context.Background(), "existing")
	if a.PaneID != "%99" {
		t.Errorf("collision overwrote without force: pane_id = %s", a.PaneID)
	}
}

func TestMCP_Register_CollisionWithForceOverwrites(t *testing.T) {
	t.Setenv("TMUX_PANE", "%42")
	s := newCmdTestStore(t, "existing")
	(&fakeSystemctl{}).install(t)

	got := callMCPTool(t, s, "semaphore.register", map[string]any{
		"name":  "existing",
		"force": true,
	})
	if got["ok"] != true {
		t.Fatalf("got %v", got)
	}
	a, _ := s.GetAgent(context.Background(), "existing")
	if a.PaneID != "%42" {
		t.Errorf("force=true didn't overwrite: %s", a.PaneID)
	}
}

func TestMCP_Register_NoPaneAvailable(t *testing.T) {
	t.Setenv("TMUX_PANE", "")
	s := newCmdTestStore(t)
	(&fakeSystemctl{}).install(t)

	got := callMCPTool(t, s, "semaphore.register", map[string]any{
		"name": "noenv",
	})
	if got["_isError"] != true {
		t.Errorf("should error without pane; got=%v", got)
	}
}

func TestMCP_Register_SystemctlFailureStillReportsRegistration(t *testing.T) {
	t.Setenv("TMUX_PANE", "%9")
	s := newCmdTestStore(t)
	(&fakeSystemctl{err: errors.New("exit 1"), out: []byte("nope")}).install(t)

	got := callMCPTool(t, s, "semaphore.register", map[string]any{
		"name": "registered-but-mailman-broken",
	})
	if got["ok"] != true {
		t.Errorf("ok should be true (registration succeeded): %v", got)
	}
	if got["mailman"] != "failed" {
		t.Errorf("mailman = %v, want failed", got["mailman"])
	}
	if !strings.Contains(got["mailman_error"].(string), "nope") {
		t.Errorf("error didn't surface: %v", got["mailman_error"])
	}
}

func TestMCP_Unregister_HappyPath(t *testing.T) {
	s := newCmdTestStore(t, "doomed", "keep")
	fs := &fakeSystemctl{}
	fs.install(t)

	got := callMCPTool(t, s, "semaphore.unregister", map[string]any{
		"name": "doomed",
	})
	if got["ok"] != true {
		t.Errorf("ok = %v", got["ok"])
	}
	if got["mailman"] != "stopped" {
		t.Errorf("mailman = %v, want stopped", got["mailman"])
	}
	// Row gone.
	_, err := s.GetAgent(context.Background(), "doomed")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	// "keep" untouched.
	if _, err := s.GetAgent(context.Background(), "keep"); err != nil {
		t.Errorf("keep should still exist: %v", err)
	}
	// systemctl disable --now claude-mailman@doomed.service
	if len(fs.calls) != 1 {
		t.Fatalf("calls = %d", len(fs.calls))
	}
	if fs.calls[0][2] != "claude-mailman@doomed.service" {
		t.Errorf("wrong unit: %v", fs.calls[0])
	}
}

func TestMCP_Unregister_PurgeMessagesAlso(t *testing.T) {
	s := newCmdTestStore(t, "alice", "doomed")
	(&fakeSystemctl{}).install(t)
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "doomed", Body: "1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "doomed", Body: "2"})

	got := callMCPTool(t, s, "semaphore.unregister", map[string]any{
		"name":           "doomed",
		"purge_messages": true,
	})
	if got["ok"] != true {
		t.Errorf("ok = %v", got["ok"])
	}
	if int(got["deleted"].(float64)) != 2 {
		t.Errorf("deleted = %v, want 2", got["deleted"])
	}
}

func TestMCP_Unregister_IdempotentOnMissingMailman(t *testing.T) {
	s := newCmdTestStore(t, "doomed")
	(&fakeSystemctl{
		err: errors.New("exit 1"),
		out: []byte("Unit claude-mailman@doomed.service not loaded."),
	}).install(t)

	got := callMCPTool(t, s, "semaphore.unregister", map[string]any{
		"name": "doomed",
	})
	if got["ok"] != true {
		t.Errorf("should succeed idempotently; got %v", got)
	}
}
