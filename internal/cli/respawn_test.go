package cli

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// recordingOps is a scripted respawnOps: agentState returns `states` in order
// (the last entry repeats once exhausted); sendExit records its call count;
// awaitExit returns the scripted `exited`; relaunch records its call count AND
// captures the command it was asked to send-keys (the #285/#730 repaired
// primitive — the retired respawn-pane -k took no command, the root-cause bug).
type recordingOps struct {
	states      []tmuxio.State
	stateErr    error
	stateIdx    int
	sendExitN   int
	sendExitErr error
	exited      bool // what awaitExit returns (did the adapter reach a bare shell?)
	awaitExitN  int
	relaunchN   int
	relaunchCmd string // the command relaunch was asked to send-keys
	relaunchErr error
	// sessionLive is what sessionForPane returns: true = the pane hosts a
	// resolvable TMUX_TELL_SESSION_ID after the relaunch (the chamber came back);
	// false = a bare shell (relaunch failed to start a chamber → #761 unregister).
	sessionLive     bool
	sessionForPaneN int
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
		awaitExit: func(_ context.Context, _ string, _ time.Duration) bool {
			r.awaitExitN++
			return r.exited
		},
		relaunch: func(_ context.Context, _ string, cmd string) error {
			r.relaunchN++
			r.relaunchCmd = cmd
			return r.relaunchErr
		},
		sessionForPane: func(_ context.Context, _ string) (string, bool) {
			r.sessionForPaneN++
			if r.sessionLive {
				return "fresh-session-uuid", true
			}
			return "", false
		},
	}
}

// fastRespawnTunables shrinks the second-scale respawn waits so pathway tests
// run in milliseconds. Restores on cleanup.
func fastRespawnTunables(t *testing.T) {
	t.Helper()
	pe, prt, ppe, arw := respawnExitGrace, respawnReadyTimeout, respawnPollEvery, autoRestartExitWindow
	respawnExitGrace = 1 * time.Millisecond
	respawnReadyTimeout = 30 * time.Millisecond
	respawnPollEvery = 1 * time.Millisecond
	autoRestartExitWindow = 1 * time.Millisecond
	t.Cleanup(func() {
		respawnExitGrace, respawnReadyTimeout, respawnPollEvery, autoRestartExitWindow = pe, prt, ppe, arw
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

const testRelaunchCmd = "chamber-claude.sh Pilot"

// Happy path: idle pane → relaunch_cmd present → /exit sent → bare shell observed
// → relaunch send-keys the REGISTERED command → stale session-id cleared (#626)
// → shrink counter reset → returns not-stopped. Mutation anchor: if relaunch
// send-keys the wrong string (or nothing — the retired respawn-pane -k behaviour),
// the relaunchCmd assert reds.
func TestRespawnChamber_HappyPath(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	// sessionLive: the relaunched chamber re-established a resolvable session-id
	// (the ground-truth liveness gate passes → agent kept, counter reset).
	ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}, exited: true, sessionLive: true}

	stopped := respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", testRelaunchCmd, 0)
	if stopped {
		t.Fatal("stopped = true, want false on the happy path")
	}
	if ops.sendExitN != 1 {
		t.Errorf("sendExit called %d times, want 1 (graceful /exit)", ops.sendExitN)
	}
	if ops.relaunchN != 1 {
		t.Errorf("relaunch called %d times, want 1", ops.relaunchN)
	}
	if ops.relaunchCmd != testRelaunchCmd {
		t.Errorf("relaunch cmd = %q, want %q (must send-keys the REGISTERED command, not respawn-pane -k)",
			ops.relaunchCmd, testRelaunchCmd)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.SessionID != "" {
		t.Errorf("SessionID = %q after relaunch, want cleared (#626 re-establishment)", a.SessionID)
	}
	if a.RespawnShrinkCount != 0 {
		t.Errorf("RespawnShrinkCount = %d after relaunch, want 0 (reset)", a.RespawnShrinkCount)
	}
}

// Idle-gate: a non-idle pane (operator working/typing) SKIPS the respawn — no
// /exit, no awaitExit, no relaunch, and neither the session-id nor the counter is
// touched. Mutation anchor: dropping the `state != StateIdle` guard fires under an
// open turn and flips the call-count asserts.
func TestRespawnChamber_SkipsWhenNotIdle(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	ops := &recordingOps{states: []tmuxio.State{tmuxio.StateWorking}, exited: true}

	stopped := respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", testRelaunchCmd, 0)
	if stopped {
		t.Fatal("stopped = true, want false")
	}
	if ops.sendExitN != 0 || ops.relaunchN != 0 {
		t.Errorf("sendExit=%d relaunch=%d, want 0/0 (skipped under a non-idle pane)", ops.sendExitN, ops.relaunchN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.SessionID != "old-session-uuid" {
		t.Errorf("SessionID = %q, want preserved (no respawn happened)", a.SessionID)
	}
	if a.RespawnShrinkCount != 2 {
		t.Errorf("RespawnShrinkCount = %d, want 2 preserved (counter retained for a later trigger)", a.RespawnShrinkCount)
	}
}

// relaunch_cmd guard: threshold reached + idle pane, but NO registered relaunch
// command → the pathway must NOT send /exit (killing a chamber it can't relaunch
// strands a bare shell — the exact bug this replaces). No /exit, no awaitExit, no
// relaunch; counter + session preserved. Mutation anchor: dropping the empty-cmd
// guard fires /exit (sendExitN==1) and strands the chamber.
func TestRespawnChamber_SkipsWhenNoRelaunchCmd(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}, exited: true}

	stopped := respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", "" /*no relaunch_cmd*/, 0)
	if stopped {
		t.Fatal("stopped = true, want false")
	}
	if ops.sendExitN != 0 || ops.awaitExitN != 0 || ops.relaunchN != 0 {
		t.Errorf("sendExit=%d awaitExit=%d relaunch=%d, want 0/0/0 (never /exit a chamber we cannot relaunch)",
			ops.sendExitN, ops.awaitExitN, ops.relaunchN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.SessionID != "old-session-uuid" || a.RespawnShrinkCount != 2 {
		t.Errorf("state mutated: session=%q count=%d, want preserved/2", a.SessionID, a.RespawnShrinkCount)
	}
}

// No bare shell: /exit is sent but the adapter never exits to a shell within the
// grace window (awaitExit=false). The chamber is left running — NO relaunch, and
// the session-id + counter are preserved so a later cycle retries. Mutation anchor:
// relaunching without confirming the bare shell would type the launch command into
// a live adapter.
func TestRespawnChamber_NoBareShellSkipsRelaunch(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}, exited: false}

	stopped := respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", testRelaunchCmd, 0)
	if stopped {
		t.Fatal("stopped = true, want false")
	}
	if ops.sendExitN != 1 {
		t.Errorf("sendExit=%d, want 1 (we DID try a graceful exit)", ops.sendExitN)
	}
	if ops.relaunchN != 0 {
		t.Errorf("relaunch=%d, want 0 (no bare shell observed → never send-keys into a live adapter)", ops.relaunchN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.SessionID != "old-session-uuid" || a.RespawnShrinkCount != 2 {
		t.Errorf("state mutated: session=%q count=%d, want preserved/2 (chamber left running)", a.SessionID, a.RespawnShrinkCount)
	}
}

// relaunch failure: the send-keys relaunch errors → the counter is NOT reset (the
// reset lives AFTER a successful relaunch), so a later cycle retries.
func TestRespawnChamber_RelaunchFailureRetainsCounter(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	ops := &recordingOps{
		states:      []tmuxio.State{tmuxio.StateIdle},
		exited:      true,
		relaunchErr: context.DeadlineExceeded, // any non-nil error
	}

	_ = respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", testRelaunchCmd, 0)
	if ops.relaunchN != 1 {
		t.Errorf("relaunch attempted %d times, want 1", ops.relaunchN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.RespawnShrinkCount != 2 {
		t.Errorf("RespawnShrinkCount = %d after failed relaunch, want 2 retained", a.RespawnShrinkCount)
	}
}

// Ready-timeout, but the chamber IS alive: the restarted chamber never reaches
// idle within the ready window (slow to settle), so the pathway TIMES OUT — but
// the ground-truth liveness gate finds a resolvable session-id in the pane, so the
// chamber is coming up and the pathway completes (session-id cleared for the fresh
// self-register, counter reset). This is the "slow-but-alive" case that must NOT
// unregister; the failed-relaunch (bare-shell) case is TestRelaunchAfterExit_
// FailedRelaunchUnregisters.
func TestRespawnChamber_ReadyTimeoutStillCompletes(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	// idle for the gate, then never idle → ready wait times out; sessionLive → the
	// chamber is up (just slow), so the liveness gate keeps it registered.
	ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle, tmuxio.StateWorking}, exited: true, sessionLive: true}

	stopped := respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", testRelaunchCmd, 0)
	if stopped {
		t.Fatal("stopped = true, want false (timeout proceeds, not stops)")
	}
	if ops.relaunchN != 1 {
		t.Errorf("relaunch called %d times, want 1", ops.relaunchN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.SessionID != "" {
		t.Errorf("SessionID = %q, want cleared even on ready-timeout", a.SessionID)
	}
	if a.RespawnShrinkCount != 0 {
		t.Errorf("RespawnShrinkCount = %d, want 0 (reset even on ready-timeout)", a.RespawnShrinkCount)
	}
}

// relaunchAfterExit is the shared tail used by BOTH the #285 threshold respawn and
// the #730 co-trigger. Directly exercise it: a bare shell observed → relaunch with
// the registered command + session cleared + relaunched=true; NO bare shell →
// relaunched=false, no relaunch, session preserved (the #730 "chamber didn't exit,
// normal /compact" no-op).
func TestRelaunchAfterExit(t *testing.T) {
	fastRespawnTunables(t)

	t.Run("bare shell → relaunch", func(t *testing.T) {
		s := respawnTestStore(t, "old-session-uuid")
		ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}, exited: true, sessionLive: true}
		relaunched, stopped := relaunchAfterExit(context.Background(), s, ops.toOps(), discardLogger(),
			"pilot", "%6", testRelaunchCmd, autoRestartExitWindow, 0)
		if !relaunched || stopped {
			t.Fatalf("relaunched=%v stopped=%v, want true/false", relaunched, stopped)
		}
		if ops.relaunchN != 1 || ops.relaunchCmd != testRelaunchCmd {
			t.Errorf("relaunchN=%d cmd=%q, want 1 / %q", ops.relaunchN, ops.relaunchCmd, testRelaunchCmd)
		}
		a, _ := s.GetAgent(context.Background(), "pilot")
		if a.SessionID != "" {
			t.Errorf("SessionID=%q, want cleared", a.SessionID)
		}
	})

	t.Run("no bare shell → no-op", func(t *testing.T) {
		s := respawnTestStore(t, "old-session-uuid")
		ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}, exited: false}
		relaunched, stopped := relaunchAfterExit(context.Background(), s, ops.toOps(), discardLogger(),
			"pilot", "%6", testRelaunchCmd, autoRestartExitWindow, 0)
		if relaunched || stopped {
			t.Fatalf("relaunched=%v stopped=%v, want false/false", relaunched, stopped)
		}
		if ops.relaunchN != 0 {
			t.Errorf("relaunchN=%d, want 0 (chamber still running)", ops.relaunchN)
		}
		a, _ := s.GetAgent(context.Background(), "pilot")
		if a.SessionID != "old-session-uuid" {
			t.Errorf("SessionID=%q, want preserved (no relaunch)", a.SessionID)
		}
	})
}

// #761 — a relaunch that does NOT bring the chamber back (relaunch_cmd is a bare
// basename not on the mailman's PATH: send-keys "succeeds" typing it, the shell
// prints command-not-found and stays a bare shell) must UNREGISTER the agent, so
// the mailman never delivers bus content into the stranded bare shell. The
// ground-truth signal is the ABSENCE of a resolvable session-id in the pane
// (sessionLive=false); AgentState is deliberately NOT trusted here — a residual
// adapter TUI frame reads StateIdle on a dead pane. Mutation anchor: dropping the
// sessionForPane gate / DeleteAgent leaves the agent registered → the GetAgent
// ErrNotFound assert reds. Both the StateIdle-reading and the timed-out bare shell
// must unregister.
func TestRelaunchAfterExit_FailedRelaunchUnregisters(t *testing.T) {
	fastRespawnTunables(t)

	t.Run("idle-reading bare shell (residual frame) unregisters", func(t *testing.T) {
		s := respawnTestStore(t, "old-session-uuid")
		// exited: bare shell observed; relaunch send-keys succeeds (no error);
		// StateIdle would fool AgentState — but sessionLive=false is the truth.
		ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}, exited: true, sessionLive: false}
		relaunched, stopped := relaunchAfterExit(context.Background(), s, ops.toOps(), discardLogger(),
			"pilot", "%6", testRelaunchCmd, autoRestartExitWindow, 0)
		if relaunched || stopped {
			t.Fatalf("relaunched=%v stopped=%v, want false/false (failed relaunch)", relaunched, stopped)
		}
		if ops.relaunchN != 1 {
			t.Errorf("relaunchN=%d, want 1 (send-keys DID fire — it just didn't start a chamber)", ops.relaunchN)
		}
		if ops.sessionForPaneN == 0 {
			t.Error("sessionForPane never consulted — the liveness gate must run")
		}
		if _, err := s.GetAgent(context.Background(), "pilot"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("agent still registered after failed relaunch (err=%v), want ErrNotFound (unregistered #761)", err)
		}
	})

	t.Run("timed-out bare shell unregisters", func(t *testing.T) {
		s := respawnTestStore(t, "old-session-uuid")
		// idle for the gate, then never idle → ready wait times out; sessionLive=false
		// → bare shell, relaunch failed → unregister (NOT the old "proceed anyway").
		ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle, tmuxio.StateWorking}, exited: true, sessionLive: false}
		relaunched, _ := relaunchAfterExit(context.Background(), s, ops.toOps(), discardLogger(),
			"pilot", "%6", testRelaunchCmd, autoRestartExitWindow, 0)
		if relaunched {
			t.Error("relaunched=true, want false (timed-out bare shell = failed relaunch)")
		}
		if _, err := s.GetAgent(context.Background(), "pilot"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("agent still registered (err=%v), want ErrNotFound", err)
		}
	})
}

// #761 at the full respawnChamber level: a threshold respawn whose relaunch fails
// to start a chamber unregisters the agent (fail-closed) rather than resetting the
// counter on a registered-and-dead chamber. Mutation anchor: without the gate the
// agent survives (counter reset) and the GetAgent ErrNotFound assert reds.
func TestRespawnChamber_FailedRelaunchUnregisters(t *testing.T) {
	fastRespawnTunables(t)
	s := respawnTestStore(t, "old-session-uuid")
	ops := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}, exited: true, sessionLive: false}

	stopped := respawnChamber(context.Background(), s, ops.toOps(), discardLogger(), "pilot", "%6", testRelaunchCmd, 0)
	if stopped {
		t.Fatal("stopped = true, want false")
	}
	if ops.relaunchN != 1 {
		t.Errorf("relaunchN=%d, want 1 (relaunch attempted)", ops.relaunchN)
	}
	if _, err := s.GetAgent(context.Background(), "pilot"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("agent still registered after failed relaunch (err=%v), want ErrNotFound (unregistered #761)", err)
	}
}

// respawnIfThresholdReached is the shared tail of both shrink triggers (PR1 clear
// + PR2 self-compact). It fires the respawn ONLY when count >= threshold. Below
// threshold it must not touch the pane at all. Mutation anchor: flipping the
// `count >= threshold` comparison fires a respawn on the sub-threshold case.
func TestRespawnIfThresholdReached_Routing(t *testing.T) {
	fastRespawnTunables(t)

	// Below threshold: no respawn, no pane touch, returns not-stopped.
	s := respawnTestStore(t, "old-session-uuid") // counter seeded at 2
	below := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}, exited: true}
	if stopped := respawnIfThresholdReached(context.Background(), s, below.toOps(), discardLogger(),
		"pilot", "%6", testRelaunchCmd, "self-compact", 2 /*count*/, 3 /*threshold*/, 0); stopped {
		t.Fatal("stopped = true below threshold, want false")
	}
	if below.sendExitN != 0 || below.relaunchN != 0 {
		t.Errorf("below threshold: sendExit=%d relaunch=%d, want 0/0", below.sendExitN, below.relaunchN)
	}
	a, _ := s.GetAgent(context.Background(), "pilot")
	if a.RespawnShrinkCount != 2 || a.SessionID != "old-session-uuid" {
		t.Errorf("below threshold mutated state: count=%d session=%q, want 2 / preserved",
			a.RespawnShrinkCount, a.SessionID)
	}

	// At threshold: fires the respawn (idle pane → full pathway → counter reset).
	// sessionLive → the relaunched chamber came back (liveness gate passes).
	s2 := respawnTestStore(t, "old-session-uuid")
	at := &recordingOps{states: []tmuxio.State{tmuxio.StateIdle}, exited: true, sessionLive: true}
	if stopped := respawnIfThresholdReached(context.Background(), s2, at.toOps(), discardLogger(),
		"pilot", "%6", testRelaunchCmd, "self-compact", 3 /*count*/, 3 /*threshold*/, 0); stopped {
		t.Fatal("stopped = true at threshold happy path, want false")
	}
	if at.sendExitN != 1 || at.relaunchN != 1 {
		t.Errorf("at threshold: sendExit=%d relaunch=%d, want 1/1 (respawn fired)", at.sendExitN, at.relaunchN)
	}
	a2, _ := s2.GetAgent(context.Background(), "pilot")
	if a2.RespawnShrinkCount != 0 {
		t.Errorf("at threshold: RespawnShrinkCount = %d after respawn, want 0 (reset)", a2.RespawnShrinkCount)
	}
}

// isClearControl matches ONLY a bare `/clear` control row (the PR1 trigger).
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

// isCompactControl matches ONLY a bare `/compact` control row (the #730 co-trigger
// discriminator). /clear is excluded (its respawn path is #285 PR1), and a
// message-kind /compact is excluded (not a substrate-triggered control).
func TestIsCompactControl(t *testing.T) {
	cases := []struct {
		kind store.Kind
		body string
		want bool
	}{
		{store.KindControl, "/compact", true},
		{store.KindControl, " /compact ", true}, // trimmed
		{store.KindControl, "/clear", false},
		{store.KindControl, "/rename Pilot task", false},
		{store.KindMessage, "/compact", false}, // operator-typed, not a control row
	}
	for _, c := range cases {
		got := isCompactControl(&store.Message{Kind: c.kind, Body: c.body})
		if got != c.want {
			t.Errorf("isCompactControl(kind=%v body=%q) = %v, want %v", c.kind, c.body, got, c.want)
		}
	}
}
