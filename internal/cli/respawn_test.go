package cli

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// recordingOps is a scripted respawnOps: agentState returns `states` in order
// (the last entry repeats once exhausted), and sendExit/respawn record their
// call counts + return the configured errors.
type recordingOps struct {
	states      []tmuxio.State
	stateErr    error
	stateIdx    int
	sendExitN   int
	sendExitErr error
	respawnN    int
	respawnErr  error
}

func (r *recordingOps) toOps() respawnOps {
	return respawnOps{
		agentState: func(_ context.Context, _ string) (tmuxio.State, error) {
			if r.stateErr != nil {
				return tmuxio.StateUnknown, r.stateErr
			}
			st := tmuxio.StateUnknown
			switch {
			case r.stateIdx < len(r.states):
				st = r.states[r.stateIdx]
			case len(r.states) > 0:
				st = r.states[len(r.states)-1]
			}
			r.stateIdx++
			return st, nil
		},
		sendExit: func(_ context.Context, _ string) error { r.sendExitN++; return r.sendExitErr },
		respawn:  func(_ context.Context, _ string) error { r.respawnN++; return r.respawnErr },
	}
}

// fastRespawnTunables shrinks the second-scale respawn waits so pathway tests
// run in milliseconds. Restores on cleanup.
func fastRespawnTunables(t *testing.T) {
	t.Helper()
	pe, prt, ppe := respawnExitGrace, respawnReadyTimeout, respawnPollEvery
	respawnExitGrace = 1 * time.Millisecond
	respawnReadyTimeout = 30 * time.Millisecond
	respawnPollEvery = 1 * time.Millisecond
	t.Cleanup(func() {
		respawnExitGrace, respawnReadyTimeout, respawnPollEvery = pe, prt, ppe
	})
}

func respawnTestStore(t *testing.T, sessionID string) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "pilot", "%6"); err != nil {
		t.Fatal(err)
	}
	if sessionID != "" {
		if err := s.SetSessionID(ctx, "pilot", sessionID); err != nil {
			t.Fatal(err)
		}
	}
	// Seed the shrink counter at 2 (as if threshold N=2 was just reached).
	for i := 0; i < 2; i++ {
		if _, err := s.IncrementRespawnShrinkCount(ctx, "pilot"); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// Happy path: idle pane → /exit sent → respawn-pane called → stale session-id
// cleared (#626 re-establishment) → shrink counter reset → returns not-stopped.
func TestRespawnChamber_HappyPath(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}}

	stopped := respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", 0)
	if stopped {
		t.Fatal("stopped = true, want false on the happy path")
	}
	if ops.sendExitN != 1 {
		t.Errorf("sendExit called %d times, want 1 (graceful /exit)", ops.sendExitN)
	}
	if ops.respawnN != 1 {
		t.Errorf("respawn called %d times, want 1", ops.respawnN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.SessionID != "" {
		t.Errorf("SessionID = %q after respawn, want cleared (#626 re-establishment)", a.SessionID)
	}
	if a.RespawnShrinkCount != 0 {
		t.Errorf("RespawnShrinkCount = %d after respawn, want 0 (reset)", a.RespawnShrinkCount)
	}
}

// Idle-gate: a non-idle pane (operator working/typing) SKIPS the respawn — no
// /exit, no respawn-pane, and neither the session-id nor the counter is touched,
// so a later clear retries. Mutation anchor: dropping the `state != StateIdle`
// guard fires the respawn under an open turn and flips the sendExit/respawn
// asserts.
func TestRespawnChamber_SkipsWhenNotIdle(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	ops := &recordingOps{states: []tmuxio.State{tmuxio.StateWorking}}

	stopped := respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", 0)
	if stopped {
		t.Fatal("stopped = true, want false")
	}
	if ops.sendExitN != 0 || ops.respawnN != 0 {
		t.Errorf("sendExit=%d respawn=%d, want 0/0 (respawn skipped under a non-idle pane)", ops.sendExitN, ops.respawnN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.SessionID != "old-session-uuid" {
		t.Errorf("SessionID = %q, want preserved (no respawn happened)", a.SessionID)
	}
	if a.RespawnShrinkCount != 2 {
		t.Errorf("RespawnShrinkCount = %d, want 2 preserved (counter retained for a later clear)", a.RespawnShrinkCount)
	}
}

// respawn-pane failure: the counter is NOT reset and the session-id is NOT
// cleared (both mutations live AFTER a successful respawn), so a later clear
// retries the whole pathway.
func TestRespawnChamber_RespawnFailureRetainsState(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	ops := &recordingOps{
		states:     []tmuxio.State{tmuxio.StateIdle},
		respawnErr: context.DeadlineExceeded, // any non-nil error
	}

	_ = respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", 0)
	if ops.respawnN != 1 {
		t.Errorf("respawn attempted %d times, want 1", ops.respawnN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.SessionID != "old-session-uuid" {
		t.Errorf("SessionID = %q after failed respawn, want preserved", a.SessionID)
	}
	if a.RespawnShrinkCount != 2 {
		t.Errorf("RespawnShrinkCount = %d after failed respawn, want 2 retained", a.RespawnShrinkCount)
	}
}

// Ready-timeout: the restarted claude never reaches idle within the ready
// window, so the pathway TIMES OUT but still proceeds (respawn already fired) —
// clearing the session-id and resetting the counter. The bare-shell guard +
// name-resolution backstop delivery until the chamber actually comes up.
func TestRespawnChamber_ReadyTimeoutStillCompletes(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	// idle for the gate, then never idle → ready wait times out.
	ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle, tmuxio.StateWorking}}

	stopped := respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", 0)
	if stopped {
		t.Fatal("stopped = true, want false (timeout proceeds, not stops)")
	}
	if ops.respawnN != 1 {
		t.Errorf("respawn called %d times, want 1", ops.respawnN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.SessionID != "" {
		t.Errorf("SessionID = %q, want cleared even on ready-timeout", a.SessionID)
	}
	if a.RespawnShrinkCount != 0 {
		t.Errorf("RespawnShrinkCount = %d, want 0 (reset even on ready-timeout)", a.RespawnShrinkCount)
	}
}

// respawnIfThresholdReached is the shared tail of both shrink triggers (PR1 clear
// + PR2 self-compact). It fires the respawn ONLY when count >= threshold. Below
// threshold it must not touch the pane at all (no /exit, no respawn-pane) — a
// counted-but-not-yet-triggering shrink. Mutation anchor: flipping the `count >=
// threshold` comparison fires a respawn on the sub-threshold case and flips the
// call-count asserts.
func TestRespawnIfThresholdReached_Routing(t *testing.T) {
	fastRespawnTunables(t)

	// Below threshold: no respawn, no pane touch, returns not-stopped.
	s := respawnTestStore(t, "old-session-uuid") // counter seeded at 2
	below := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}}
	if stopped := respawnIfThresholdReached(context.Background(), s, below.toOps(), discardLogger(),
		"pilot", "%6", "self-compact", 2 /*count*/, 3 /*threshold*/, 0); stopped {
		t.Fatal("stopped = true below threshold, want false")
	}
	if below.sendExitN != 0 || below.respawnN != 0 {
		t.Errorf("below threshold: sendExit=%d respawn=%d, want 0/0", below.sendExitN, below.respawnN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.RespawnShrinkCount != 2 || a.SessionID != "old-session-uuid" {
		t.Errorf("below threshold mutated state: count=%d session=%q, want 2 / preserved",
			a.RespawnShrinkCount, a.SessionID)
	}

	// At threshold: fires the respawn (idle pane → full pathway → counter reset).
	s2 := respawnTestStore(t, "old-session-uuid")
	at := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}}
	if stopped := respawnIfThresholdReached(context.Background(), s2, at.toOps(), discardLogger(),
		"pilot", "%6", "self-compact", 3 /*count*/, 3 /*threshold*/, 0); stopped {
		t.Fatal("stopped = true at threshold happy path, want false")
	}
	if at.sendExitN != 1 || at.respawnN != 1 {
		t.Errorf("at threshold: sendExit=%d respawn=%d, want 1/1 (respawn fired)", at.sendExitN, at.respawnN)
	}
	a2, _ := s2.GetAgent(context.Background(), "pilot")
	if a2.RespawnShrinkCount != 0 {
		t.Errorf("at threshold: RespawnShrinkCount = %d after respawn, want 0 (reset)", a2.RespawnShrinkCount)
	}
}

// isClearControl matches ONLY a bare `/clear` control row (the PR1 trigger).
// /compact is excluded (self-compact detection is PR2); the clear macro's
// `/rename` second row and ordinary messages are excluded (one count per clear).
func TestIsClearControl(t *testing.T) {
	cases := []struct {
		kind store.Kind
		body string
		want bool
	}{
		{store.KindControl, "/clear", true},
		{store.KindControl, " /clear ", true}, // trimmed
		{store.KindControl, "/compact", false},
		{store.KindControl, "/rename Pilot task", false},
		{store.KindControl, "/mcp disable tmux-tell", false},
		{store.KindMessage, "/clear", false}, // not a control row
	}
	for _, c := range cases {
		got := isClearControl(&store.Message{Kind: c.kind, Body: c.body})
		if got != c.want {
			t.Errorf("isClearControl(kind=%v body=%q) = %v, want %v", c.kind, c.body, got, c.want)
		}
	}
}
