package discover

import (
	"context"
	"errors"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestPaneHostsLiveClaude is the #761 delivery-path liveness primitive. It must
// distinguish a LIVE claude process from a BARE SHELL — and specifically must NOT
// be fooled by a stale tmux title/window-name, which outlives the dead process
// and is exactly what let the mailman paste bus content into bash.
//
// The axis the bug lives on is "a bare shell that still RESOLVES to the agent"
// (via title), not merely "a bare shell" — so the stale-title arm is the load-
// bearing one; a title-less bare shell was already caught by the #626 block.
func TestPaneHostsLiveClaude(t *testing.T) {
	cases := []struct {
		name     string
		panes    string // list-panes -F output: id\tpid\ttitle\tcurrent_cmd
		cmdline  map[int]string
		environ  map[int]string // pid → TMUX_TELL_SESSION_ID
		pane     string
		want     bool
		whyFalse string
	}{
		{
			name:    "live claude --resume → true",
			panes:   "%4\t400\tsurveyor\tclaude\n",
			cmdline: map[int]string{400: "claude\x00--resume\x00surveyor\x00"},
			pane:    "%4",
			want:    true,
		},
		{
			name:    "bare shell with STALE AGENT-NAMED TITLE → false (the #761 exploit)",
			panes:   "%4\t400\tsurveyor\tbash\n", // title lies; claude is gone
			cmdline: map[int]string{400: "bash\x00"},
			pane:    "%4",
			want:    false,
		},
		{
			name:    "bare shell, no title → false",
			panes:   "%4\t400\t\tbash\n",
			cmdline: map[int]string{400: "bash\x00"},
			pane:    "%4",
			want:    false,
		},
		{
			name:    "fresh chamber: no --resume but session-id env → true",
			panes:   "%4\t400\tsurveyor\tclaude\n",
			cmdline: map[int]string{400: "claude\x00"},
			environ: map[int]string{400: "FRESH-uuid"},
			pane:    "%4",
			want:    true,
		},
		{
			name:    "live claude for a DIFFERENT agent → true (agent-agnostic; attribution is the drift block's job)",
			panes:   "%4\t400\tPilot\tclaude\n",
			cmdline: map[int]string{400: "claude\x00--resume\x00Pilot\x00"},
			pane:    "%4",
			want:    true,
		},
		{
			name:    "pane absent from the walk → false (fail closed)",
			panes:   "%9\t900\tother\tclaude\n",
			cmdline: map[int]string{900: "claude\x00--resume\x00other\x00"},
			pane:    "%4",
			want:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prev := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
				return []byte(c.panes), nil
			})
			t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prev) })

			w := &Walker{
				CmdlineReader: func(pid int) (string, error) {
					if v, ok := c.cmdline[pid]; ok {
						return v, nil
					}
					return "", errors.New("no fake")
				},
				ChildrenReader: func(int) []int { return nil },
				MaxDepth:       1,
			}
			if c.environ != nil {
				w.EnvironReader = func(pid int, key string) (string, bool) {
					if key != NeutralSessionIDEnv {
						return "", false
					}
					v, ok := c.environ[pid]
					return v, ok
				}
			}

			if got := w.PaneHostsLiveClaude(context.Background(), c.pane); got != c.want {
				t.Errorf("PaneHostsLiveClaude(%s) = %v, want %v", c.pane, got, c.want)
			}
		})
	}
}

// TestPaneHostsLiveClaude_WalkErrorFailsClosed: when the pane list cannot be read
// at all, the liveness question is UNANSWERABLE — and a check that cannot verify
// must never authorize a paste-and-enter. Returns false (block), not true.
// Mutation anchor: flipping the error arm to `true` reds this.
func TestPaneHostsLiveClaude_WalkErrorFailsClosed(t *testing.T) {
	prev := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return nil, errors.New("tmux is down")
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prev) })

	w := &Walker{
		CmdlineReader:  func(int) (string, error) { return "claude\x00--resume\x00surveyor\x00", nil },
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}
	if w.PaneHostsLiveClaude(context.Background(), "%4") {
		t.Error("PaneHostsLiveClaude = true on a walk error; want false (fail closed — cannot verify ⇒ must not authorize a paste)")
	}
}
