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
	// respawnExitGrace bounds the wait for claude to shut down after /exit
	// before the force-killing respawn-pane -k fires. Best-effort: the -k is
	// the guarantee; this window only lets claude flush its (just-cleared)
	// transcript so the resume loads the intended state.
	respawnExitGrace = 8 * time.Second
	// respawnReadyTimeout bounds the wait for the restarted claude to reach a
	// live idle prompt. On timeout the pathway proceeds anyway — the bare-shell
	// guard (#638) + name-resolution fallback backstop delivery until the
	// chamber is up, so no message is mis-delivered in the meantime.
	respawnReadyTimeout = 90 * time.Second
	// respawnPollEvery is the poll cadence for the ready wait.
	respawnPollEvery = 1 * time.Second
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
	respawn    func(ctx context.Context, pane string) error
}

// defaultRespawnOps wires the production tmuxio operations: a read-only
// AgentState probe, a graceful `/exit` via send-keys, and respawn-pane -k with
// the pane's ORIGINAL cmdline (preserving the memory-cap wrapper).
func defaultRespawnOps() respawnOps {
	return respawnOps{
		agentState: func(ctx context.Context, pane string) (tmuxio.State, error) {
			st, _, err := tmuxio.AgentState(ctx, pane)
			return st, err
		},
		sendExit: func(ctx context.Context, pane string) error {
			return tmuxio.SendKeys(ctx, pane, "/exit")
		},
		respawn: func(ctx context.Context, pane string) error {
			return tmuxio.RespawnPaneOriginal(ctx, pane)
		},
	}
}

// respawnChamber performs the #285 bounded post-shrink respawn of agent's
// chamber process in the pane it currently occupies. The caller guarantees the
// preconditions: the shrink counter has reached the configured threshold, and a
// bus-delivered clear just settled to a stable idle (a safe, non-mid-turn
// moment). The pathway follows the #285 sequencing:
//
//  1. Idle gate — confirm the pane is idle right now (not working, not
//     operator-typing). If not, SKIP (return without resetting the counter) so a
//     later clear retries; NEVER respawn under an open operator turn.
//  2. Graceful /exit — ask claude to shut down cleanly so it flushes its
//     transcript before dying.
//  3. Bounded exit grace — give claude respawnExitGrace to exit; respawn-pane -k
//     is the force-kill fallback if it doesn't.
//  4. respawn-pane -k (original cmdline) — restart the pane with the command it
//     was created with, preserving the memory-cap wrapper (alcatraz-infra#50).
//  5. #626 re-establishment — the OLD session-id is now dead; clear it so
//     delivery falls back to name-resolution (bare-shell-guarded during boot).
//     The restarted chamber re-establishes a fresh session-id when it
//     self-registers on launch — the substrate-honest path, and it dodges the
//     launch-era-id-latching fragility filed as #643.
//  6. Bounded ready wait — poll until the restarted claude reaches a live idle
//     prompt so the loop resumes delivery (the clear macro's follow-up /rename,
//     and any #227 deferred rows the chamber flushes on resume) to a READY
//     process, not the dying one.
//  7. Reset the shrink counter — the cycle restarts.
//
// Returns stopped=true when stopCtx was cancelled mid-pathway (the caller should
// return from the serve loop). The pane-id does not change: respawn-pane reuses
// the same pane, so the stored pane_id stays valid throughout.
func respawnChamber(stopCtx context.Context, s *store.Store, ops respawnOps,
	logger *log.Logger, agent, pane string, watchdogPing time.Duration) (stopped bool) {

	// 1. Idle gate. A single AgentState read suffices: the caller only invokes
	// this right after waitForStableIdle confirmed stable idle, so a non-idle
	// read here means the operator (or a fresh turn) grabbed the pane in the
	// interim — defer rather than respawn under them.
	probeCtx, cancel := context.WithTimeout(stopCtx, 2*time.Second)
	state, err := ops.agentState(probeCtx, pane)
	cancel()
	if err != nil || state != tmuxio.StateIdle {
		logger.Printf("respawn_skip agent=%s pane=%s state=%v err=%v - pane not idle; deferring respawn (counter retained for a later clear)",
			agent, pane, state, err)
		return stopCtx.Err() != nil
	}

	logger.Printf("respawn_begin agent=%s pane=%s - shrink threshold reached; graceful /exit then respawn-pane -k (original cmdline)",
		agent, pane)

	// 2. Graceful /exit.
	if err := ops.sendExit(stopCtx, pane); err != nil {
		logger.Printf("respawn_exit_send_err agent=%s pane=%s err=%v - proceeding to force-kill respawn",
			agent, pane, err)
	}

	// 3. Bounded exit grace (best-effort; the -k below force-kills any remnant).
	if stopOrSleepWithUpdates(stopCtx, respawnExitGrace, watchdogPing, func(time.Time) {
		_ = sdnotify.Watchdog()
	}) {
		return true
	}

	// 4. respawn-pane -k with the pane's original cmdline (preserves the wrapper).
	if err := ops.respawn(stopCtx, pane); err != nil {
		logger.Printf("respawn_pane_err agent=%s pane=%s err=%v - respawn FAILED; counter NOT reset so a later clear retries",
			agent, pane, err)
		return stopCtx.Err() != nil
	}

	// 5. #626 re-establishment: the old session is dead. Clear the stale
	// session-id so delivery falls back to name-resolution (which the bare-shell
	// guard makes safe during boot); the restarted chamber re-establishes a
	// fresh session-id on self-register. Best-effort - a failure here only costs
	// exact-match routing until the next register, never correctness.
	if err := s.SetSessionID(stopCtx, agent, ""); err != nil {
		logger.Printf("respawn_session_clear_err agent=%s err=%v - stale session-id retained; name-resolution still delivers",
			agent, err)
	}

	// 6. Bounded ready wait.
	ready := respawnWaitReady(stopCtx, ops, pane, watchdogPing)
	switch ready {
	case respawnStopped:
		return true
	case respawnTimedOut:
		logger.Printf("respawn_ready_timeout agent=%s pane=%s within=%s - proceeding; bare-shell guard + name-resolution backstop delivery until the chamber is up",
			agent, pane, respawnReadyTimeout)
	}

	// 7. Reset the counter - the cycle restarts.
	if err := s.ResetRespawnShrinkCount(stopCtx, agent); err != nil {
		logger.Printf("respawn_counter_reset_err agent=%s err=%v", agent, err)
	}
	logger.Printf("respawn_done agent=%s pane=%s ready=%v - chamber restarted, shrink counter reset",
		agent, pane, ready == respawnReady)
	return false
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
	agent, pane, trigger string, count, threshold int, watchdogPing time.Duration) (stopped bool) {
	if count >= threshold {
		logger.Printf("respawn_threshold_reached agent=%s trigger=%s count=%d threshold=%d",
			agent, trigger, count, threshold)
		return respawnChamber(stopCtx, s, ops, logger, agent, pane, watchdogPing)
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
