package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// deliverRunner is the standard fake tmux runner the session tests share:
// records the paste-buffer target pane + echoes the loaded body back on
// capture-pane so verify-token passes.
func deliverRunner(bodyMu *sync.Mutex, body *string, paneSeen *atomic.Value) func(context.Context, io.Reader, ...string) ([]byte, error) {
	return func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "load-buffer":
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				bodyMu.Lock()
				*body = string(b)
				bodyMu.Unlock()
			}
		case "paste-buffer":
			for i, a := range args {
				if a == "-t" && i+1 < len(args) {
					paneSeen.Store(args[i+1])
				}
			}
		case "capture-pane":
			bodyMu.Lock()
			defer bodyMu.Unlock()
			return []byte(*body), nil
		case "display-message":
			// pane_current_command for the #761 gate (PaneAcceptsPaste). These
			// harnesses model panes hosting a LIVE adapter, so report a non-shell
			// foreground process — "node" is what both claude and codex show. A
			// test modelling a BARE SHELL must override this with "bash", which is
			// what makes its refusal fire for the right reason.
			return []byte("node\n"), nil
		}
		return nil, nil
	}
}

// TestServe_SessionID_ResolvesAndDelivers: the agent's row points at a stale
// pane (%4, now bare), but its self-discovered session-id lives in %7 — whose
// process carries the wrapper-injected TMUX_TELL_SESSION_ID but NOT `--resume
// surveyor` and has a generic title. So ONLY the session-id path can resolve it;
// the name path
// would find surveyor nowhere and block. Delivery to %7 proves the session-id
// primary resolution won, healed the registry, and skipped the name drift-check.
func TestServe_SessionID_ResolvesAndDelivers(t *testing.T) {
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%4\t400\tbash\tbash\n" +
			"%7\t700\tclaude\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			if pid == 700 {
				return "claude\x00--remote-control\x00", nil // NO --resume <name>
			}
			return "bash\x00", nil
		},
		ChildrenReader: func(int) []int { return nil },
		EnvironReader: func(pid int, key string) (string, bool) {
			if key == discover.NeutralSessionIDEnv && pid == 700 {
				return "SURV-uuid", true
			}
			return "", false
		},
		MaxDepth: 1,
	}

	var (
		bodyMu   sync.Mutex
		body     string
		paneSeen atomic.Value
	)
	prev := tmuxio.SetTmuxRunner(deliverRunner(&bodyMu, &body, &paneSeen))
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4")         // stale pane (now bare)
	_ = s.SetSessionID(ctx, "surveyor", "SURV-uuid") // the session is the addressee
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "surveyor", Body: "hello"})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	waitDelivered(t, s, "surveyor")
	stop()
	wait()

	d, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "surveyor", State: store.StateDelivered, Limit: 10})
	if len(d) != 1 {
		t.Fatalf("delivered = %d, want 1 (session-id should resolve %%7); log=%s", len(d), logbuf.String())
	}
	if seen := paneSeen.Load(); seen != "%7" {
		t.Errorf("paste pane = %v, want %%7 (session-id resolution beats the name path)", seen)
	}
	agent, _ := s.GetAgent(ctx, "surveyor")
	if agent == nil || agent.PaneID != "%7" {
		t.Errorf("registry pane = %v, want %%7 (healed by session-id)", agent)
	}
	if !strings.Contains(logbuf.String(), "session_resolved") {
		t.Errorf("expected session_resolved log; got %s", logbuf.String())
	}
}

// TestServe_SessionID_StaleFallsBackToName: the stored session-id resolves
// nowhere (the session ended or re-resumed under a new id), but %4 still runs
// `claude --resume surveyor`. The resolver falls back to the name path, which
// delivers to %4 — resume-resilience — and logs session_stale.
func TestServe_SessionID_StaleFallsBackToName(t *testing.T) {
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%4\t400\tSurveyor\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			if pid == 400 {
				return "claude\x00--resume\x00surveyor\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		EnvironReader: func(pid int, key string) (string, bool) {
			// %4 runs a DIFFERENT session (NEW-uuid), not the stored GONE-uuid.
			if key == discover.NeutralSessionIDEnv && pid == 400 {
				return "NEW-uuid", true
			}
			return "", false
		},
		MaxDepth: 1,
	}

	var (
		bodyMu   sync.Mutex
		body     string
		paneSeen atomic.Value
	)
	prev := tmuxio.SetTmuxRunner(deliverRunner(&bodyMu, &body, &paneSeen))
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4")
	_ = s.SetSessionID(ctx, "surveyor", "GONE-uuid") // stale: no pane hosts it
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "surveyor", Body: "hello"})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	waitDelivered(t, s, "surveyor")
	stop()
	wait()

	d, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "surveyor", State: store.StateDelivered, Limit: 10})
	if len(d) != 1 {
		t.Fatalf("delivered = %d, want 1 (name fallback should deliver to %%4); log=%s", len(d), logbuf.String())
	}
	if seen := paneSeen.Load(); seen != "%4" {
		t.Errorf("paste pane = %v, want %%4 (name fallback)", seen)
	}
	if !strings.Contains(logbuf.String(), "session_stale") {
		t.Errorf("expected session_stale log; got %s", logbuf.String())
	}
}
