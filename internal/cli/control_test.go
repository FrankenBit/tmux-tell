package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// runControlCLI uses store.Open(":memory:") via --db, so each test gets
// a separate connection inside the shared-cache process-wide DB. The
// existing newCmdTestStore is good for direct doControl/MCP calls; CLI
// tests need to drive runControlCLI itself.

func TestControlCLI_HappyPath_PlainCommand(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	t.Setenv("TMUX_AGENT_NAME", "alice")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "alice", "--command", "compact"},
		&stdout, &stderr,
	)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true || got["command"] != "/compact" {
		t.Errorf("got %v", got)
	}

	msgs, _ := s.ListMessages(context.Background(), store.ListFilter{
		ToAgent: "alice", State: store.StateQueued, Limit: 10,
	})
	if len(msgs) != 1 {
		t.Fatalf("queued = %d, want 1", len(msgs))
	}
	if msgs[0].Kind != store.KindControl || msgs[0].Body != "/compact" {
		t.Errorf("row = %+v", msgs[0])
	}
}

func TestControlCLI_RestartMacro_QueuesBothRows(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	t.Setenv("TMUX_AGENT_NAME", "alice")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "bob", "--command", "mcp-restart-tmux-tell"},
		&stdout, &stderr,
	)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["macro"] != "restart" {
		t.Errorf("macro = %v, want restart", got["macro"])
	}
	if got["enable_id"] == nil {
		t.Errorf("enable_id missing: %v", got)
	}

	msgs, _ := s.ListMessages(context.Background(), store.ListFilter{
		ToAgent: "bob", State: store.StateQueued, Limit: 10,
	})
	if len(msgs) != 2 {
		t.Fatalf("queued = %d, want 2", len(msgs))
	}
	wantBodies := []string{"/mcp disable tmux-tell", "/mcp enable tmux-tell"}
	for i, want := range wantBodies {
		if msgs[i].Body != want {
			t.Errorf("row[%d].Body = %q, want %q", i, msgs[i].Body, want)
		}
	}
}

// TestControlCLI_DeprecatedAlias_StillRestarts pins #480's backward-compat at the
// CLI surface: invoking the legacy `mcp-restart-tmux-msg` name still triggers the
// restart macro (2 rows, tmux-tell text), surfaces a `deprecated` field naming the
// canonical form, and emits a greppable WARN deprecated_control_macro to stderr.
func TestControlCLI_DeprecatedAlias_StillRestarts(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	t.Setenv("TMUX_AGENT_NAME", "alice")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "bob", "--command", "mcp-restart-tmux-msg"}, // legacy alias
		&stdout, &stderr,
	)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["macro"] != "restart" {
		t.Errorf("macro = %v, want restart (alias must still trigger the macro)", got["macro"])
	}
	if dep, _ := got["deprecated"].(string); !strings.Contains(dep, "mcp-restart-tmux-tell") {
		t.Errorf("deprecated field = %q, want it to name the canonical mcp-restart-tmux-tell", dep)
	}
	if !strings.Contains(stderr.String(), "WARN deprecated_control_macro") {
		t.Errorf("missing WARN deprecated_control_macro on stderr; got: %s", stderr.String())
	}
	msgs, _ := s.ListMessages(context.Background(), store.ListFilter{ToAgent: "bob", State: store.StateQueued, Limit: 10})
	if len(msgs) != 2 || msgs[0].Body != "/mcp disable tmux-tell" || msgs[1].Body != "/mcp enable tmux-tell" {
		t.Errorf("rows = %+v, want disable+enable tmux-tell", msgs)
	}
}

// TestControlCLI_SleepAlias_StillCompacts pins #646's backward-compat at the CLI
// surface: the deprecated `sleep` verb still resolves to the unchanged /compact
// CLI primitive, surfaces a `deprecated` field naming the canonical `compact`, and
// emits the greppable WARN deprecated_control_macro. The retained deprecated-path
// assertion at the IO boundary (the canonical `compact` path is covered by the
// other tests above). #646 reversed the #509 `compact`→`sleep` rename, so `sleep`
// is now the alias and `compact` the canonical verb.
func TestControlCLI_SleepAlias_StillCompacts(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	t.Setenv("TMUX_AGENT_NAME", "alice")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "alice", "--command", "sleep"}, // legacy alias, self-only
		&stdout, &stderr,
	)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["command"] != "/compact" {
		t.Errorf("command = %v, want /compact (alias must still emit the unchanged primitive)", got["command"])
	}
	if dep, _ := got["deprecated"].(string); !strings.Contains(dep, "compact") {
		t.Errorf("deprecated field = %q, want it to name the canonical compact", dep)
	}
	if !strings.Contains(stderr.String(), "WARN deprecated_control_macro") {
		t.Errorf("missing WARN deprecated_control_macro on stderr; got: %s", stderr.String())
	}
	msgs, _ := s.ListMessages(context.Background(), store.ListFilter{ToAgent: "alice", State: store.StateQueued, Limit: 10})
	if len(msgs) != 1 || msgs[0].Kind != store.KindControl || msgs[0].Body != "/compact" {
		t.Errorf("rows = %+v, want one /compact control row", msgs)
	}
}

func TestControlCLI_ResumeWith_QueuesBothRows(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	t.Setenv("TMUX_AGENT_NAME", "alice")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{
			"--to", "alice", "--command", "compact",
			"--resume-with", "carry on with #26",
		},
		&stdout, &stderr,
	)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["resume_id"] == nil || got["command"] != "/compact" {
		t.Errorf("got %v", got)
	}

	msgs, _ := s.ListMessages(context.Background(), store.ListFilter{
		ToAgent: "alice", State: store.StateQueued, Limit: 10,
	})
	if len(msgs) != 2 {
		t.Fatalf("queued = %d, want 2", len(msgs))
	}
	if msgs[0].Kind != store.KindControl || msgs[1].Kind != store.KindMessage {
		t.Errorf("kinds = %q/%q; want control/message", msgs[0].Kind, msgs[1].Kind)
	}
}

func TestControlCLI_ScopeRejected(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	t.Setenv("TMUX_AGENT_NAME", "alice")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "bob", "--command", "compact"},
		&stdout, &stderr,
	)
	if exit != exitUsage {
		t.Errorf("exit = %d, want usage", exit)
	}
	if !strings.Contains(stderr.String(), "self-only") {
		t.Errorf("stderr should mention self-only; got %q", stderr.String())
	}
}

func TestControlCLI_UnknownCommand(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	t.Setenv("TMUX_AGENT_NAME", "alice")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "alice", "--command", "clear"},
		&stdout, &stderr,
	)
	if exit != exitUsage {
		t.Errorf("exit = %d, want usage", exit)
	}
	if !strings.Contains(stderr.String(), "whitelist") &&
		!strings.Contains(stderr.String(), "invokable") {
		t.Errorf("stderr should hint at allowed list; got %q", stderr.String())
	}
}

func TestControlCLI_MissingFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no --to", []string{"--command", "compact"}},
		{"no --command", []string{"--to", "alice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX_AGENT_NAME", "alice")
			t.Setenv("CLAUDE_MSG_DB", ":memory:")
			var stdout, stderr bytes.Buffer
			exit := runControlCLI(tc.args, &stdout, &stderr)
			if exit != exitUsage {
				t.Errorf("exit = %d, want usage", exit)
			}
		})
	}
}

// TestControlCLI_ClearForTask_QueuesClearThenRename pins the #286 macro: a
// bosun→pilot clear with --for-task synthesises exactly two control rows in
// the operator-ratified order — /clear FIRST, then /rename "<Chamber> <task>"
// — and reports macro=clear with a rename_id. The rename body is the
// load-bearing invariant (mutation anchor): flip the order, drop the chamber
// prefix, or mis-template the label and this assertion fails.
func TestControlCLI_ClearForTask_QueuesClearThenRename(t *testing.T) {
	s := newCmdTestStore(t, "bosun", "pilot")
	t.Setenv("TMUX_AGENT_NAME", "bosun")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "pilot", "--command", "clear", "--for-task", "tmux-tell#286"},
		&stdout, &stderr,
	)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["macro"] != "clear" {
		t.Errorf("macro = %v, want clear", got["macro"])
	}
	if got["rename_id"] == nil {
		t.Errorf("rename_id missing: %v", got)
	}
	if got["command"] != "/clear" {
		t.Errorf("command = %v, want /clear", got["command"])
	}

	msgs, _ := s.ListMessages(context.Background(), store.ListFilter{
		ToAgent: "pilot", State: store.StateQueued, Limit: 10,
	})
	if len(msgs) != 2 {
		t.Fatalf("queued = %d, want 2", len(msgs))
	}
	if msgs[0].Kind != store.KindControl || msgs[0].Body != "/clear" {
		t.Errorf("row[0] = %+v, want control /clear FIRST", msgs[0])
	}
	if msgs[1].Kind != store.KindControl || msgs[1].Body != "/rename Pilot tmux-tell#286" {
		t.Errorf("row[1].Body = %q, want %q (rename SECOND, chamber-prefixed)", msgs[1].Body, "/rename Pilot tmux-tell#286")
	}
}

// TestControlCLI_ClearWithoutForTask_Rejected pins the required-arg contract:
// a clear with no --for-task is rejected (forward-only, no plain-/clear path).
func TestControlCLI_ClearWithoutForTask_Rejected(t *testing.T) {
	s := newCmdTestStore(t, "bosun", "pilot")
	t.Setenv("TMUX_AGENT_NAME", "bosun")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "pilot", "--command", "clear"},
		&stdout, &stderr,
	)
	if exit == exitOK {
		t.Fatalf("clear without --for-task should fail; got exitOK, stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires for_task") {
		t.Errorf("stderr should explain the required for_task; got %q", stderr.String())
	}
	// No half-actioned state: nothing queued.
	msgs, _ := s.ListMessages(context.Background(), store.ListFilter{ToAgent: "pilot", State: store.StateQueued, Limit: 10})
	if len(msgs) != 0 {
		t.Errorf("queued = %d, want 0 (no /clear without the relabel)", len(msgs))
	}
}

// TestControlCLI_ForTaskOnNonClear_Rejected pins the out-of-scope-flag fail-loud
// discipline: --for-task on any command other than clear is rejected, never
// silently dropped (the #558 escape-hatch lesson).
func TestControlCLI_ForTaskOnNonClear_Rejected(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	t.Setenv("TMUX_AGENT_NAME", "alice")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "alice", "--command", "compact", "--for-task", "tmux-tell#286"},
		&stdout, &stderr,
	)
	if exit == exitOK {
		t.Fatalf("for_task on a non-clear command should fail; got exitOK")
	}
	if !strings.Contains(stderr.String(), "only valid with command=clear") {
		t.Errorf("stderr should fail loud about scope; got %q", stderr.String())
	}
	msgs, _ := s.ListMessages(context.Background(), store.ListFilter{ToAgent: "alice", State: store.StateQueued, Limit: 10})
	if len(msgs) != 0 {
		t.Errorf("queued = %d, want 0 (rejected, not accepted-then-dropped)", len(msgs))
	}
}

// TestControlCLI_ClearForTask_InvalidLabel_Rejected pins that a label which
// could smuggle a second pasted command (here an embedded newline) is rejected
// by the constrained-charset validator before anything is queued.
func TestControlCLI_ClearForTask_InvalidLabel_Rejected(t *testing.T) {
	s := newCmdTestStore(t, "bosun", "pilot")
	t.Setenv("TMUX_AGENT_NAME", "bosun")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "pilot", "--command", "clear", "--for-task", "tmux-tell#286\n/clear"},
		&stdout, &stderr,
	)
	if exit == exitOK {
		t.Fatalf("injection-shaped label should be rejected; got exitOK")
	}
	if !strings.Contains(stderr.String(), "for_task") {
		t.Errorf("stderr should name the invalid for_task; got %q", stderr.String())
	}
	msgs, _ := s.ListMessages(context.Background(), store.ListFilter{ToAgent: "pilot", State: store.StateQueued, Limit: 10})
	if len(msgs) != 0 {
		t.Errorf("queued = %d, want 0 (validation precedes insert)", len(msgs))
	}
}

func TestControlCLI_AutoIdentity_FromPane(t *testing.T) {
	// Pane-derived identity (no CLAUDE_AGENT_NAME) — proves #27's
	// shared resolver flows into the new subcommand for free.
	s := newCmdTestStore(t, "alice")
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%99") // matches the pane upserted by newCmdTestStore
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "alice", "--command", "compact"},
		&stdout, &stderr,
	)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true {
		t.Errorf("got %v", got)
	}
}
