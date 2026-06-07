package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// TestServe_PingDeliveredWithoutPaste is the end-to-end branch-wiring
// test: a kind=ping row to a live-pane agent transitions to delivered
// through the real mailman loop, and — the load-bearing invariant —
// without ever pasting into the recipient's pane (#144).
func TestServe_PingDeliveredWithoutPaste(t *testing.T) {
	// bob's pane %3 is live.
	prevLP := tmuxio.SetListPanesRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%1\n%3\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesRunner(prevLP) })

	// A ping must NOT paste — fail if any delivery command runs.
	var pasted atomic.Bool
	prevRun := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if len(args) > 0 {
			switch args[0] {
			case "load-buffer", "paste-buffer", "send-keys":
				pasted.Store(true)
			}
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prevRun) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	id, err := insertPing(ctx, s, "alice", "bob")
	if err != nil {
		t.Fatalf("insertPing: %v", err)
	}

	stop, wait, _ := runServeInBackground(t, s, fastOpts("bob"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m, _ := s.GetMessage(ctx, id); m != nil && m.State == store.StateDelivered {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	m, _ := s.GetMessage(ctx, id)
	if m.State != store.StateDelivered {
		t.Fatalf("state = %s, want delivered", m.State)
	}
	if pasted.Load() {
		t.Error("ping pasted into the pane — it must be substrate-only, no paste-and-Enter")
	}
}

// TestServe_PingFailedOnDeadPane: a ping to a registered agent whose pane
// is no longer live transitions to failed with a reachability reason.
func TestServe_PingFailedOnDeadPane(t *testing.T) {
	// bob's pane %3 is NOT among the live panes.
	prevLP := tmuxio.SetListPanesRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%1\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesRunner(prevLP) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	id, _ := insertPing(ctx, s, "alice", "bob")

	stop, wait, _ := runServeInBackground(t, s, fastOpts("bob"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m, _ := s.GetMessage(ctx, id); m != nil && m.State == store.StateFailed {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	m, _ := s.GetMessage(ctx, id)
	if m.State != store.StateFailed {
		t.Fatalf("state = %s, want failed", m.State)
	}
	if !m.Error.Valid || !strings.Contains(m.Error.String, "not live") {
		t.Errorf("error = %v, want mention of not live", m.Error)
	}
}

// TestMCPPingHandler_Wiring confirms the tmux-msg.ping MCP handler
// resolves identity, runs the shared probe, and returns the pingResult
// shape. With no mailman running it lands in the timeout state on a
// short budget — enough to prove the wiring without flakiness.
func TestMCPPingHandler_Wiring(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")

	h := mcpPingHandler(s)
	raw := json.RawMessage(`{"agent":"bob","timeout_seconds":0.05}`)
	out, err := h(ctx, raw)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	res, ok := out.(pingResult)
	if !ok {
		t.Fatalf("return type = %T, want pingResult", out)
	}
	if res.Agent != "bob" {
		t.Errorf("agent = %s, want bob", res.Agent)
	}
	if res.State != pingStateTimeout {
		t.Errorf("state = %s, want %s (no mailman running)", res.State, pingStateTimeout)
	}
	if res.OK {
		t.Error("ok = true, want false on timeout")
	}
}
