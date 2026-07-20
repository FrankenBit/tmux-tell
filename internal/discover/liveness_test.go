package discover

import (
	"context"
	"errors"
	"io"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestPaneAcceptsPaste is the #761 delivery-path gate. It keys on the hazard's
// actual mechanism — paste-and-enter executes the body as shell commands only
// when the pane's FOREGROUND process is an interactive shell — and must NOT key
// on which adapter is running, nor on tmux metadata that outlives a dead process.
//
// The codex row is the load-bearing one: a LIVE chamber that the earlier
// claude-specific gate REFUSED (denial of service, lookout 2026-07-20), because
// codex's argv is `codex … resume` (positional, no `--resume`) and its pane
// carried no wrapper-injected session-id. Adapter-neutrality is what it buys.
func TestPaneAcceptsPaste(t *testing.T) {
	cases := []struct {
		name string
		cmd  string // what pane_current_command reports
		want bool
	}{
		{"live claude (node) → accept", "node", true},
		{"live CODEX (node) → accept — the regression this fixes", "node", true},
		{"bare bash → REFUSE (the #761 exploit surface)", "bash", false},
		{"login shell '-bash' → REFUSE (dash-stripped)", "-bash", false},
		{"zsh → REFUSE", "zsh", false},
		{"fish → REFUSE", "fish", false},
		{"non-shell TUI (less) → accept (#719 territory, not #761)", "less", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, _ ...string) ([]byte, error) {
				return []byte(c.cmd + "\n"), nil
			})
			t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

			w := &Walker{ChildrenReader: func(int) []int { return nil }, MaxDepth: 1}
			if got := w.PaneAcceptsPaste(context.Background(), "%4"); got != c.want {
				t.Errorf("PaneAcceptsPaste(cmd=%q) = %v, want %v", c.cmd, got, c.want)
			}
		})
	}
}

// TestPaneAcceptsPaste_FailsClosed: when the pane's current command cannot be
// read the question is unanswerable, and a gate that cannot verify must not
// authorize a paste-and-enter. Mutation anchor: returning true on the error arm
// reds this.
func TestPaneAcceptsPaste_FailsClosed(t *testing.T) {
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, _ ...string) ([]byte, error) {
		return nil, errors.New("tmux is down")
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	w := &Walker{ChildrenReader: func(int) []int { return nil }, MaxDepth: 1}
	if w.PaneAcceptsPaste(context.Background(), "%4") {
		t.Error("PaneAcceptsPaste = true when the pane command could not be read; want false (fail closed)")
	}
}

// TestPaneAcceptsPaste_EmptyCommandFailsClosed: an empty pane_current_command is
// could-not-determine, not "safe". Same third-state discipline as the error arm.
func TestPaneAcceptsPaste_EmptyCommandFailsClosed(t *testing.T) {
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, _ ...string) ([]byte, error) {
		return []byte("\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	w := &Walker{ChildrenReader: func(int) []int { return nil }, MaxDepth: 1}
	if w.PaneAcceptsPaste(context.Background(), "%4") {
		t.Error("PaneAcceptsPaste = true on an empty pane command; want false (could-not-determine ⇒ refuse)")
	}
}
