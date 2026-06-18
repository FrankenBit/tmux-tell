package cli

import (
	"bytes"
	"context"
	"io"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// captureTitle installs a fake tmux runner that records select-pane calls so
// the set-pane-name tests never touch a live tmux server. Restored on cleanup.
func captureTitle(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	restore := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(restore) })
	return &calls
}

// Self-assert via $TMUX_PANE (the claude / operator-shell path): the title
// lands on the pane the caller is actually in, and the result reports the
// resolved agent.
func TestSetPaneName_SelfViaPane(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "lookout", "%6"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("TMUX_PANE", "%6")
	calls := captureTitle(t)

	res, err := setPaneName(ctx, s, "", "Lookout")
	if err != nil {
		t.Fatalf("setPaneName: %v", err)
	}
	if res.Pane != "%6" || res.Title != "Lookout" || res.Agent != "lookout" {
		t.Errorf("result = %+v", res)
	}
	if len(*calls) != 1 {
		t.Fatalf("tmux calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got[0] != "select-pane" || got[2] != "%6" || got[4] != "Lookout" {
		t.Errorf("tmux args = %v", got)
	}
}

// Codex MCP children don't inherit $TMUX_PANE (#355); they resolve a name via
// $TMUX_AGENT_NAME, and the pane is recovered from that agent's registry row.
func TestSetPaneName_CodexFallbackViaAgentName(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "carpenter", "%9"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("TMUX_PANE", "") // no pane inherited
	t.Setenv("TMUX_AGENT_NAME", "carpenter")
	calls := captureTitle(t)

	res, err := setPaneName(ctx, s, "", "Carpenter")
	if err != nil {
		t.Fatalf("setPaneName: %v", err)
	}
	if res.Pane != "%9" {
		t.Errorf("pane = %q, want %%9 (recovered from agent row)", res.Pane)
	}
	if (*calls)[0][2] != "%9" {
		t.Errorf("title set on wrong pane: %v", (*calls)[0])
	}
}

// An explicit --as override targets the NAMED agent's registered pane, NOT the
// caller's own $TMUX_PANE — the operator-script retitle path.
func TestSetPaneName_ExplicitAsTargetsNamedPane(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "carpenter", "%9"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("TMUX_PANE", "%0") // operator's own pane — must be ignored
	t.Setenv("TMUX_AGENT_NAME", "")
	calls := captureTitle(t)

	res, err := setPaneName(ctx, s, "carpenter", "Carpenter")
	if err != nil {
		t.Fatalf("setPaneName: %v", err)
	}
	if res.Pane != "%9" {
		t.Errorf("explicit --as must target the named pane; got %q", res.Pane)
	}
	if (*calls)[0][2] != "%9" {
		t.Errorf("title set on wrong pane: %v", (*calls)[0])
	}
}

// Re-asserting the same name is idempotent — both calls succeed and each hits
// tmux (no spurious dedupe). #556 AC: "idempotent re-set".
func TestSetPaneName_IdempotentReset(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "lookout", "%6"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("TMUX_AGENT_NAME", "")
	calls := captureTitle(t)

	for i := 0; i < 2; i++ {
		if _, err := setPaneName(ctx, s, "", "Lookout"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if len(*calls) != 2 {
		t.Errorf("want 2 idempotent tmux calls, got %d", len(*calls))
	}
}

func TestSetPaneName_RejectsBlankName(t *testing.T) {
	s := newCmdTestStore(t)
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("TMUX_AGENT_NAME", "")
	captureTitle(t)
	if _, err := setPaneName(context.Background(), s, "", "   "); err == nil {
		t.Error("blank name should error")
	}
}

// With neither $TMUX_PANE nor a resolvable agent, the caller's pane can't be
// determined — fail loud rather than silently no-op.
func TestSetPaneName_NoPaneNoAgentErrors(t *testing.T) {
	s := newCmdTestStore(t)
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_AGENT_NAME", "")
	captureTitle(t)
	if _, err := setPaneName(context.Background(), s, "", "X"); err == nil {
		t.Error("should error when no pane is resolvable")
	}
}

// setPaneName persists the display name on the agent row (#556 final commit)
// and the agents listing surfaces it in the trailing DISPLAY column.
func TestSetPaneName_PersistsDisplayName(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "lookout", "%6"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("TMUX_AGENT_NAME", "")
	captureTitle(t)

	res, err := setPaneName(ctx, s, "", "Lookout")
	if err != nil {
		t.Fatalf("setPaneName: %v", err)
	}
	if !res.DisplayPersisted {
		t.Error("DisplayPersisted = false, want true")
	}
	a, err := s.GetAgent(ctx, "lookout")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.DisplayName != "Lookout" {
		t.Errorf("persisted display_name = %q, want Lookout", a.DisplayName)
	}

	// The text listing carries it in the DISPLAY column.
	var stdout, stderr bytes.Buffer
	if rc := runAgentsWithStore(ctx, s, map[string]bool{"%6": true}, false, "text", &stdout, &stderr); rc != exitOK {
		t.Fatalf("agents rc=%d stderr=%s", rc, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("DISPLAY")) || !bytes.Contains(stdout.Bytes(), []byte("Lookout")) {
		t.Errorf("agents listing missing DISPLAY/Lookout:\n%s", stdout.String())
	}
}

// When the resolved name has no agent row, persistence can't land — but the
// title is still set and the failure is SURFACED (not swallowed) via
// DisplayError (Surveyor #563 observability nit). Induced via $TMUX_AGENT_NAME
// pointing at an unregistered name while $TMUX_PANE provides the pane.
func TestSetPaneName_SurfacesPersistFailure(t *testing.T) {
	s := newCmdTestStore(t)
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("TMUX_AGENT_NAME", "ghost") // resolves a name with no agent row
	calls := captureTitle(t)

	res, err := setPaneName(context.Background(), s, "", "Ghost")
	if err != nil {
		t.Fatalf("title-set must still succeed: %v", err)
	}
	if !res.OK || len(*calls) != 1 {
		t.Errorf("title should be set despite persist failure; res=%+v calls=%d", res, len(*calls))
	}
	if res.DisplayPersisted {
		t.Error("DisplayPersisted should be false (no agent row)")
	}
	if res.DisplayError == "" {
		t.Error("persist failure should be surfaced in DisplayError, not swallowed")
	}
}

// MCP round-trip: the tool resolves self (no override) and emits the shared
// setPaneNameResult shape.
func TestMCP_SetPaneName(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "lookout", "%6"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("TMUX_AGENT_NAME", "")
	captureTitle(t)

	got := callMCPTool(t, s, "tmux-tell.set_pane_name", map[string]any{"name": "Lookout"})
	if got["_isError"] == true {
		t.Fatalf("unexpected MCP error: %v", got)
	}
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if got["pane"] != "%6" || got["title"] != "Lookout" {
		t.Errorf("got %v", got)
	}
}
