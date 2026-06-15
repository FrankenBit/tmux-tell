package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
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

// TestMCP_Register_SkipsMailmanWithNonDefaultDB pins #293 on the MCP path.
// When the MCP process is running against a non-default $CLAUDE_MSG_DB,
// `tmux-msg.register` with start_mailman defaulted (true) returns ok:true
// with the agent row written, but `mailman` is `skipped` and `mailman_error`
// names the divergence — the operator sees that they need to start `serve`
// as a foreground subprocess to deliver against the sandbox DB. The actual
// systemctl runner must never be reached.
func TestMCP_Register_SkipsMailmanWithNonDefaultDB(t *testing.T) {
	t.Setenv("CLAUDE_MSG_DB", "/tmp/some-sandbox.db")
	t.Setenv("TMUX_PANE", "%9")
	s := newCmdTestStore(t)
	fs := &fakeSystemctl{}
	fs.install(t)

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
		"name": "sandboxed",
	})
	if got["ok"] != true {
		t.Errorf("ok = %v, want true (registration itself succeeds); got=%v",
			got["ok"], got)
	}
	if got["mailman"] != "skipped" {
		t.Errorf("mailman = %v, want \"skipped\"", got["mailman"])
	}
	mmErr, _ := got["mailman_error"].(string)
	if !strings.Contains(mmErr, "non-default CLAUDE_MSG_DB") {
		t.Errorf("mailman_error missing 'non-default CLAUDE_MSG_DB' guidance: %q", mmErr)
	}
	if !strings.Contains(mmErr, "serve --agent sandboxed") {
		t.Errorf("mailman_error missing foreground-serve recovery hint: %q", mmErr)
	}
	if len(fs.calls) != 0 {
		t.Errorf("systemctl called %d times; should be 0 (mismatch detected before)", len(fs.calls))
	}
	// Registration itself succeeded — agent row exists.
	if _, err := s.GetAgent(context.Background(), "sandboxed"); err != nil {
		t.Errorf("agent row missing after MCP register: %v", err)
	}
}

// TestMCP_Register_SkipsMailmanWithMissingEnv pins #356 on the MCP path.
// When the MCP child's env lacks DBUS_SESSION_BUS_ADDRESS or XDG_RUNTIME_DIR,
// `tmux-msg.register` returns ok:true (the agent row is written), but
// `mailman` is `skipped` and `mailman_error` names the missing vars and the
// recovery path. The actual systemctl runner must never be reached.
func TestMCP_Register_SkipsMailmanWithMissingEnv(t *testing.T) {
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	s := newCmdTestStore(t)
	fs := &fakeSystemctl{}
	fs.install(t)

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
		"name": "codex-agent",
	})
	if got["ok"] != true {
		t.Errorf("ok = %v, want true (registration itself succeeds); got=%v",
			got["ok"], got)
	}
	if got["mailman"] != "skipped" {
		t.Errorf("mailman = %v, want \"skipped\"", got["mailman"])
	}
	mmErr, _ := got["mailman_error"].(string)
	if !strings.Contains(mmErr, "DBUS_SESSION_BUS_ADDRESS") {
		t.Errorf("mailman_error missing 'DBUS_SESSION_BUS_ADDRESS': %q", mmErr)
	}
	if !strings.Contains(mmErr, "XDG_RUNTIME_DIR") {
		t.Errorf("mailman_error missing 'XDG_RUNTIME_DIR': %q", mmErr)
	}
	if !strings.Contains(mmErr, "serve --agent codex-agent") {
		t.Errorf("mailman_error missing foreground-serve recovery hint: %q", mmErr)
	}
	if len(fs.calls) != 0 {
		t.Errorf("systemctl called %d times; should be 0 (env check fires before)", len(fs.calls))
	}
	// Registration itself succeeded — agent row exists.
	if _, err := s.GetAgent(context.Background(), "codex-agent"); err != nil {
		t.Errorf("agent row missing after MCP register: %v", err)
	}
}

func TestMCP_Register_HappyPath(t *testing.T) {
	t.Setenv("TMUX_PANE", "%9")
	s := newCmdTestStore(t) // empty registry
	fs := &fakeSystemctl{}
	fs.install(t)

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
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
	// systemctl --user enable --now tmux-tell-claude-mailman@newone.service
	if len(fs.calls) != 1 || fs.calls[0][2] != "tmux-tell-claude-mailman@newone.service" {
		t.Errorf("systemctl calls = %v", fs.calls)
	}
}

func TestMCP_Register_ExplicitPaneArg(t *testing.T) {
	t.Setenv("TMUX_PANE", "%1") // should be ignored when pane is given
	s := newCmdTestStore(t)
	(&fakeSystemctl{}).install(t)

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
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

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
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

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
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

// TestMCP_Register_ClearsStuckState pins #291: a chamber re-registering via
// the MCP register tool (force=true) with a corrected pane un-parks a stuck
// mailman. Without the clear, pane_id would update but stuck_reason would
// persist and the mailman would never resume — the load-bearing recovery path
// for the MCP (chamber-driven) register surface.
func TestMCP_Register_ClearsStuckState(t *testing.T) {
	t.Setenv("TMUX_PANE", "%42")
	s := newCmdTestStore(t, "existing")
	(&fakeSystemctl{}).install(t)
	if err := s.SetStuck(context.Background(), "existing", store.StuckReasonPaneNotFound); err != nil {
		t.Fatalf("seed stuck: %v", err)
	}

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
		"name":  "existing",
		"force": true,
	})
	if got["ok"] != true {
		t.Fatalf("got %v", got)
	}
	a, _ := s.GetAgent(context.Background(), "existing")
	if a.StuckReason != "" {
		t.Errorf("MCP register force=true did not clear stuck_reason: %q", a.StuckReason)
	}
}

// TestMCP_Register_ClearsAttentionState pins #298: the MCP register tool
// path mirrors the CLI's #224 attention auto-clear. A chamber re-registering
// via MCP (the spawn-die / self-recovery / ad-hoc reset path) must have its
// stale attention_state cleared so the operator's attention queue doesn't
// carry stale "awaiting_operator" signals across chamber restarts — same
// substrate-honest semantics as the CLI register surface.
func TestMCP_Register_ClearsAttentionState(t *testing.T) {
	t.Setenv("TMUX_PANE", "%42")
	s := newCmdTestStore(t, "existing")
	(&fakeSystemctl{}).install(t)
	if err := s.SetAttentionState(context.Background(), "existing",
		store.AttentionStateAwaitingOperator); err != nil {
		t.Fatalf("seed attention_state: %v", err)
	}

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
		"name":  "existing",
		"force": true,
	})
	if got["ok"] != true {
		t.Fatalf("got %v", got)
	}
	a, _ := s.GetAgent(context.Background(), "existing")
	if a.AttentionState != store.AttentionStateIdle {
		t.Errorf("MCP register did not clear attention_state: %q, want %q",
			a.AttentionState, store.AttentionStateIdle)
	}
}

// TestMCP_Register_PromotesRegisterDeferred pins the #258(a) wiring on the MCP
// path: a register-deferred message auto-promotes when the recipient registers
// via the MCP tool (the spawn-die path chambers actually use), and the response
// surfaces deferred_promoted. Resume-deferred rows stay staged (isolation).
func TestMCP_Register_PromotesRegisterDeferred(t *testing.T) {
	t.Setenv("TMUX_PANE", "%42")
	ctx := context.Background()
	s := newCmdTestStore(t, "dispatcher", "pilot")
	(&fakeSystemctl{}).install(t)

	reg, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "dispatcher", ToAgent: "pilot", Body: "your next dispatch",
		DeliverAfter: "register",
	})
	if err != nil {
		t.Fatalf("seed register-deferred: %v", err)
	}
	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "dispatcher", ToAgent: "pilot", Body: "resume note",
		DeliverAfter: "resume",
	}); err != nil {
		t.Fatalf("seed resume-deferred: %v", err)
	}

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
		"name":          "pilot",
		"force":         true,
		"start_mailman": false,
	})
	if got["ok"] != true {
		t.Fatalf("got %v", got)
	}
	if dp, _ := got["deferred_promoted"].(float64); int(dp) != 1 {
		t.Errorf("deferred_promoted = %v, want 1", got["deferred_promoted"])
	}

	queued, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "pilot", State: store.StateQueued})
	if len(queued) != 1 || queued[0].PublicID != reg.PublicID {
		t.Errorf("queued = %v, want only the promoted register row %s", queued, reg.PublicID)
	}
	deferred, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "pilot", Deferred: true})
	if len(deferred) != 1 || deferred[0].DeliverAfter.String != "resume" {
		t.Errorf("deferred = %v, want only the resume row still staged", deferred)
	}
}

func TestMCP_Register_NoPaneAvailable(t *testing.T) {
	t.Setenv("TMUX_PANE", "")
	s := newCmdTestStore(t)
	(&fakeSystemctl{}).install(t)

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
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

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
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

func TestMCP_Register_SurfacesQueuedBacklog(t *testing.T) {
	t.Setenv("TMUX_PANE", "%9")
	// "backlogged" was registered, received mail, then its pane died; it
	// now re-registers (force=true) and should learn it has backlog
	// without a separate inbox poll (#151).
	s := newCmdTestStore(t, "sender", "backlogged")
	(&fakeSystemctl{}).install(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: "sender", ToAgent: "backlogged", Body: "queued msg",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
		"name":  "backlogged",
		"force": true,
	})
	if got["ok"] != true {
		t.Fatalf("ok = %v; got=%v", got["ok"], got)
	}
	q, ok := got["queued"].(float64)
	if !ok {
		t.Fatalf("queued missing or wrong type; got=%v", got)
	}
	if int(q) != 3 {
		t.Errorf("queued = %v, want 3", q)
	}
	if _, present := got["queued_error"]; present {
		t.Errorf("unexpected queued_error: %v", got["queued_error"])
	}
}

func TestMCP_Register_ZeroQueuedWhenNoBacklog(t *testing.T) {
	t.Setenv("TMUX_PANE", "%9")
	s := newCmdTestStore(t) // empty registry, no queued mail
	(&fakeSystemctl{}).install(t)

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
		"name": "fresh",
	})
	if got["ok"] != true {
		t.Fatalf("got=%v", got)
	}
	q, ok := got["queued"].(float64)
	if !ok {
		t.Fatalf("queued missing or wrong type; got=%v", got)
	}
	if int(q) != 0 {
		t.Errorf("queued = %v, want 0 (present-and-zero, not omitted)", q)
	}
}

func TestMCP_Unregister_HappyPath(t *testing.T) {
	s := newCmdTestStore(t, "doomed", "keep")
	fs := &fakeSystemctl{}
	fs.install(t)

	got := callMCPTool(t, s, "tmux-msg.unregister", map[string]any{
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
	// systemctl disable --now tmux-tell-claude-mailman@doomed.service
	if len(fs.calls) != 1 {
		t.Fatalf("calls = %d", len(fs.calls))
	}
	if fs.calls[0][2] != "tmux-tell-claude-mailman@doomed.service" {
		t.Errorf("wrong unit: %v", fs.calls[0])
	}
}

func TestMCP_Unregister_PurgeQueueAlso(t *testing.T) {
	s := newCmdTestStore(t, "alice", "doomed")
	(&fakeSystemctl{}).install(t)
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "doomed", Body: "1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "doomed", Body: "2"})

	got := callMCPTool(t, s, "tmux-msg.unregister", map[string]any{
		"name":        "doomed",
		"purge_queue": true,
		"force":       true,
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
		out: []byte("Unit tmux-tell-claude-mailman@doomed.service not loaded."),
	}).install(t)

	got := callMCPTool(t, s, "tmux-msg.unregister", map[string]any{
		"name": "doomed",
	})
	if got["ok"] != true {
		t.Errorf("should succeed idempotently; got %v", got)
	}
}
