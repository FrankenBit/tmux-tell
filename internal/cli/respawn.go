package cli

import (
	"context"
	"log"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/sdnotify"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// #285 respawn-pathway tunables. Package vars so tests can shrink them below the
// real second-scale waits (mirrors the stuck/stability-gate tunable pattern).
var (
	// respawnExitGrace bounds the poll for the adapter to exit to a bare shell
	// after /exit, before the relaunch send-keys. Unlike the retired respawn-pane
	// -k (which force-killed but re-ran the resurrect restore → a bare shell, the
	// #285 root-cause bug), the relaunch types the registered command into the
	// post-exit shell, so we must positively observe that shell first
	// (awaitAdapterExit) rather than blind-sleep. If the adapter doesn't exit
	// within this window the chamber is left running (no bare shell stranded).
	respawnExitGrace = 8 * time.Second
	// respawnReadyTimeout bounds the wait for the restarted claude to reach a
	// live idle prompt. On timeout the pathway proceeds anyway — the bare-shell
	// guard (#638) + name-resolution fallback backstop delivery until the
	// chamber is up, so no message is mis-delivered in the meantime.
	respawnReadyTimeout = 90 * time.Second
	// respawnPollEvery is the poll cadence for the ready wait.
	respawnPollEvery = 1 * time.Second
	// autoRestartExitWindow bounds the #730 co-trigger's wait for a bare shell
	// after a tmux-tell-triggered /compact that did NOT settle to idle. The
	// preceding post-compact stability wait (waitForStableIdle, up to
	// PostCompactPause) has already elapsed, so a chamber that exited is already at
	// a bare shell — this short window only tolerates a slightly-late exit before
	// concluding "chamber still running, no relaunch".
	autoRestartExitWindow = 5 * time.Second
)

// respawnReadyResult is the outcome of the post-respawn readiness wait.
type respawnReadyResult int

const (
	respawnReady    respawnReadyResult = iota // reached a live idle prompt
	respawnTimedOut                           // deadline elapsed (proceed anyway)
	respawnStopped                            // stopCtx cancelled mid-wait
)

// respawnOps are the tmux-touching operations the respawn pathway performs,
// injected so the pathway's decision logic (idle-gate, sequencing, session-id
// clear, counter reset, ready/timeout) is unit-testable without a live tmux and
// pane-profile. defaultRespawnOps wires the real tmuxio calls; tests supply a
// scripted fake. Mirrors the codebase's other seam points (tmuxRun/SetTmuxRunner,
// discover's EnvironReader/CmdlineReader).
type respawnOps struct {
	agentState func(ctx context.Context, pane string) (tmuxio.State, error)
	sendExit   func(ctx context.Context, pane string) error
	// awaitExit polls until the pane's adapter has exited to a bare shell (ready
	// for a send-keys relaunch) or window elapses; returns true iff a shell was
	// observed. Injected so the pathway's sequencing is unit-testable without a
	// live tmux.
	awaitExit func(ctx context.Context, pane string, window time.Duration) bool
	// relaunch send-keys the registered relaunch command into the (bare-shell)
	// pane to restart the chamber. Replaces the retired respawn-pane -k, which
	// re-ran pane_start_command — under tmux-resurrect the resurrect restore, i.e.
	// a bare shell, never the chamber (root cause, #285).
	relaunch func(ctx context.Context, pane, cmd string) error
}

// defaultRespawnOps wires the production tmuxio operations: a read-only
// AgentState probe, a graceful `/exit` via send-keys, a pane_current_command poll
// for the post-exit bare shell, and a send-keys of the registered relaunch
// command (NOT respawn-pane -k — see the respawn field doc / #285 root-cause).
func defaultRespawnOps() respawnOps {
	return respawnOps{
		agentState: func(ctx context.Context, pane string) (tmuxio.State, error) {
			st, _, err := tmuxio.AgentState(ctx, pane)
			return st, err
		},
		sendExit: func(ctx context.Context, pane string) error {
			return tmuxio.SendKeys(ctx, pane, "/exit")
		},
		awaitExit: func(ctx context.Context, pane string, window time.Duration) bool {
			return awaitAdapterExit(ctx, pane, window)
		},
		relaunch: func(ctx context.Context, pane, cmd string) error {
			return tmuxio.SendKeys(ctx, pane, cmd)
		},
	}
}

// respawnChamber performs the #285 bounded post-shrink respawn of agent's
// chamber process in the pane it currently occupies. The caller guarantees the
// preconditions: the shrink counter has reached the configured threshold, and a
// bus-delivered clear (or observed self-compact) just settled to a stable idle (a
// safe, non-mid-turn moment). The pathway:
//
//  1. Idle gate — confirm the pane is idle right now (not working, not
//     operator-typing). If not, SKIP (return without resetting the counter) so a
//     later trigger retries; NEVER respawn under an open operator turn.
//  2. relaunch_cmd guard — without a registered relaunch command the substrate
//     cannot restart the chamber (it can't infer the launch command; under tmux-
//     resurrect pane_start_command is the resurrect restore — see #285). Do NOT
//     send /exit: killing a chamber we can't relaunch strands a bare shell, the
//     exact bug this replaces. Retain the counter so a later relaunch_cmd fires.
//  3. Graceful /exit — ask the adapter to shut down cleanly so it flushes its
//     transcript before dying.
//     4-7. relaunchAfterExit — await the post-exit bare shell, send-keys the
//     relaunch command, clear the dead session-id (#626), and wait for the
//     restarted chamber to reach a live idle prompt. Shared with the #730
//     control-verb co-trigger.
//  8. Reset the shrink counter — the cycle restarts (only on a completed
//     relaunch).
//
// Returns stopped=true when stopCtx was cancelled mid-pathway (the caller should
// return from the serve loop). The pane-id does not change: send-keys relaunches
// into the same pane, so the stored pane_id stays valid throughout.
func respawnChamber(stopCtx context.Context, s *store.Store, ops respawnOps,
	logger *log.Logger, agent, pane, relaunchCmd string, watchdogPing time.Duration) (stopped bool) {

	// 1. Idle gate. A single AgentState read suffices: the caller only invokes
	// this right after waitForStableIdle confirmed stable idle, so a non-idle
	// read here means the operator (or a fresh turn) grabbed the pane in the
	// interim — defer rather than respawn under them.
	probeCtx, cancel := context.WithTimeout(stopCtx, 2*time.Second)
	state, err := ops.agentState(probeCtx, pane)
	cancel()
	if err != nil || state != tmuxio.StateIdle {
		logger.Printf("respawn_skip agent=%s pane=%s state=%v err=%v - pane not idle; deferring respawn (counter retained for a later trigger)",
			agent, pane, state, err)
		return stopCtx.Err() != nil
	}

	// 2. relaunch_cmd guard — never /exit a chamber we cannot relaunch.
	if relaunchCmd == "" {
		logger.Printf("respawn_skip_no_relaunch_cmd agent=%s pane=%s - respawn_after_shrinks>0 but relaunch_cmd empty; not exiting a chamber that cannot be relaunched (set via register --relaunch-cmd / set-relaunch-cmd)",
			agent, pane)
		return stopCtx.Err() != nil
	}

	logger.Printf("respawn_begin agent=%s pane=%s - shrink threshold reached; graceful /exit then send-keys relaunch",
		agent, pane)

	// 3. Graceful /exit — flush the transcript before the adapter dies.
	if err := ops.sendExit(stopCtx, pane); err != nil {
		logger.Printf("respawn_exit_send_err agent=%s pane=%s err=%v - proceeding to await exit anyway",
			agent, pane, err)
	}

	// 4-7. Await bare shell, relaunch, clear session, wait ready (shared tail).
	relaunched, stopped := relaunchAfterExit(stopCtx, s, ops, logger, agent, pane, relaunchCmd, respawnExitGrace, watchdogPing)
	if stopped {
		return true
	}
	if !relaunched {
		// Adapter didn't exit within the grace window, or the relaunch send failed
		// — the chamber is (probably) still running; no bare shell stranded. Retain
		// the counter so a later cycle retries.
		return false
	}

	// 8. Reset the counter — the cycle restarts.
	if err := s.ResetRespawnShrinkCount(stopCtx, agent); err != nil {
		logger.Printf("respawn_counter_reset_err agent=%s err=%v", agent, err)
	}
	logger.Printf("respawn_done agent=%s pane=%s - chamber relaunched, shrink counter reset",
		agent, pane)
	return false
}

// relaunchAfterExit is the shared tail of the #285 threshold respawn and the #730
// control-verb co-trigger. It (a) waits up to window for the adapter to have
// exited to a bare shell (awaitExit), (b) send-keys the relaunch command, (c)
// clears the now-dead session-id so delivery falls back to name-resolution
// (bare-shell-guarded during boot, #626 — the restarted chamber re-establishes a
// fresh session-id on self-register, dodging the #643 launch-era-id latching),
// and (d) waits (bounded) for the restarted chamber to reach a live idle prompt.
//
// relaunched=true iff a bare shell was observed AND the relaunch send-keys
// succeeded (the ready-wait outcome is logged but never flips this — the
// bare-shell guard + name-resolution backstop delivery until the chamber is up).
// stopped reflects stopCtx cancellation mid-pathway.
func relaunchAfterExit(stopCtx context.Context, s *store.Store, ops respawnOps,
	logger *log.Logger, agent, pane, relaunchCmd string, window, watchdogPing time.Duration) (relaunched, stopped bool) {

	if !ops.awaitExit(stopCtx, pane, window) {
		if stopCtx.Err() != nil {
			return false, true
		}
		logger.Printf("respawn_exit_timeout agent=%s pane=%s within=%s - adapter did not exit to a bare shell; skipping relaunch (chamber left running)",
			agent, pane, window)
		return false, false
	}

	if err := ops.relaunch(stopCtx, pane, relaunchCmd); err != nil {
		logger.Printf("respawn_relaunch_err agent=%s pane=%s err=%v - send-keys relaunch failed; chamber not restarted",
			agent, pane, err)
		return false, stopCtx.Err() != nil
	}

	if err := s.SetSessionID(stopCtx, agent, ""); err != nil {
		logger.Printf("respawn_session_clear_err agent=%s err=%v - stale session-id retained; name-resolution still delivers",
			agent, err)
	}

	ready := respawnWaitReady(stopCtx, ops, pane, watchdogPing)
	if ready == respawnStopped {
		return true, true
	}
	if ready == respawnTimedOut {
		logger.Printf("respawn_ready_timeout agent=%s pane=%s within=%s - proceeding; bare-shell guard + name-resolution backstop delivery until the chamber is up",
			agent, pane, respawnReadyTimeout)
	}
	return true, false
}

// awaitAdapterExit polls the pane's current command until it is an interactive
// shell (the adapter has exited to a bare prompt) or window elapses; returns true
// iff a shell was observed. The pane_current_command probe is adapter-agnostic
// and needs no host-PS1 parsing (see tmuxio.IsShellProcess) — the crisp "has the
// adapter exited?" signal that discriminates a bare shell from an unrecognized
// adapter UI (which a raw StateUnknown read would conflate, notably on codex).
func awaitAdapterExit(stopCtx context.Context, pane string, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for {
		probeCtx, cancel := context.WithTimeout(stopCtx, 2*time.Second)
		cmd, err := tmuxio.PaneCurrentCommand(probeCtx, pane)
		cancel()
		if err == nil && tmuxio.IsShellProcess(cmd) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-stopCtx.Done():
			return false
		case <-time.After(respawnPollEvery):
		}
		_ = sdnotify.Watchdog()
	}
}

// respawnIfThresholdReached fires the respawn pathway when a freshly-incremented
// shrink count has reached the agent's threshold, and logs either the
// threshold-hit respawn or the sub-threshold count. It is the shared tail of both
// #285 shrink triggers so they stay in lockstep: the inline bus-delivered clear
// (PR1) and the observe-path self-compact detection (PR2) each do their own
// counting, then hand the (count, threshold) here. trigger is a short label
// ("clear" | "self-compact") for log observability. Returns stopped=true when
// respawnChamber saw stopCtx cancelled (the caller should return from the loop).
func respawnIfThresholdReached(stopCtx context.Context, s *store.Store, ops respawnOps, logger *log.Logger,
	agent, pane, relaunchCmd, trigger string, count, threshold int, watchdogPing time.Duration) (stopped bool) {
	if count >= threshold {
		logger.Printf("respawn_threshold_reached agent=%s trigger=%s count=%d threshold=%d",
			agent, trigger, count, threshold)
		return respawnChamber(stopCtx, s, ops, logger, agent, pane, relaunchCmd, watchdogPing)
	}
	logger.Printf("respawn_shrink_counted agent=%s trigger=%s count=%d/%d",
		agent, trigger, count, threshold)
	return false
}

// respawnWaitReady polls AgentState until the restarted claude reaches a live
// idle prompt (respawnReady), respawnReadyTimeout elapses (respawnTimedOut), or
// stopCtx is cancelled (respawnStopped). A single StateIdle read is sufficient:
// a freshly-booted claude shows working/unknown chrome until it settles to the
// prompt, so the first idle read is the ready signal.
func respawnWaitReady(stopCtx context.Context, ops respawnOps, pane string, watchdogPing time.Duration) respawnReadyResult {
	deadline := time.Now().Add(respawnReadyTimeout)
	lastPing := time.Now()
	for {
		probeCtx, cancel := context.WithTimeout(stopCtx, 2*time.Second)
		state, err := ops.agentState(probeCtx, pane)
		cancel()
		if err == nil && state == tmuxio.StateIdle {
			return respawnReady
		}
		if !time.Now().Before(deadline) {
			return respawnTimedOut
		}
		select {
		case <-stopCtx.Done():
			return respawnStopped
		case <-time.After(respawnPollEvery):
		}
		if watchdogPing > 0 && time.Since(lastPing) >= watchdogPing {
			_ = sdnotify.Watchdog()
			lastPing = time.Now()
		}
	}
}
