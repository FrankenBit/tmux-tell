package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/config"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/discover"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/render"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/sdnotify"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

// flagWasSet reports whether a flag was set via the CLI (vs. left at
// its default). Used by the #54 precedence chain to decide when to
// consult the host-level config file: CLI flags override config-file
// values; absent CLI flags consult the config chain.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	wasSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

// serveOpts is the resolved configuration for runServeWithStore.
type serveOpts struct {
	Agent              string
	InterMessageDelay  time.Duration
	IdlePollInterval   time.Duration
	PauseCheckInterval time.Duration
	DeliverTimeout     time.Duration
	// PostCompactPause is the quiescent window the mailman holds after
	// delivering a `/compact` control message. /compact takes ~90s in
	// practice and leaves the recipient waiting on input afterwards; a
	// well-timed follow-up message wants to land after the compact has
	// settled, not into the slash-command parser mid-compaction. Zero
	// disables the pause entirely.
	PostCompactPause time.Duration
	// QuietOpts configures the pre-delivery probe-and-watch gate so the
	// mailman doesn't fragment the operator's in-progress typing. See
	// internal/tmuxio.QuietOpts for the per-field semantics.
	QuietOpts tmuxio.QuietOpts
	// QuietDisabled bypasses the probe-and-watch gate entirely. Useful
	// in tests (the existing fast-opts helper sets this so the fake
	// tmux runner doesn't need to handle the probe sequence) and as an
	// escape hatch if the probe pattern misbehaves with a future TUI.
	QuietDisabled bool
	// QuickPresenceProbe (opt-in, #63) activates the asymmetric
	// pre-check that handles mid-typing collisions when the full gate
	// is disabled. When BOTH QuietDisabled=true AND this flag=true:
	// the mailman runs a one-shot ~50ms probe before delivery; if it
	// detects operator activity, it falls back to the full gate; if
	// quiet, it delivers immediately. The speed win of QuietDisabled
	// is preserved for the common empty-buffer case, while the safety
	// of the gate is restored when the operator is mid-draft.
	// Defaults to false to preserve the v0.2.x ship-fast default; an
	// operator opts in per-mailman via --quick-presence-probe or the
	// quick-presence-probe config knob.
	QuickPresenceProbe bool
	// QuickPresenceOpts configures the asymmetric pre-check. Only
	// consulted when QuickPresenceProbe is true. Zero values pick the
	// aggressive defaults sized for fast empty-buffer delivery.
	QuickPresenceOpts tmuxio.QuickPresenceOpts
	// DriftCheckDisabled bypasses the pre-delivery silent-drift guard
	// (#37). Production keeps it enabled. Tests that don't fake
	// ListPanesWithPID + /proc readers should set this to true so the
	// check doesn't hit real system state non-deterministically.
	DriftCheckDisabled bool
	// DriftSoftFail keeps the pre-v0.2.1 behaviour where ambiguous
	// and unrecoverable drift cases log WARN and deliver to the
	// drifted (or ambiguous) pane. Surveyor Q(b) review of v0.2.0:
	// silent-bad-delivery cascades on autonomous-Pilot receivers
	// ("merge PR #X" landing on the wrong agent is real damage), so
	// the new default is MarkFailed. Operators with operator-watched
	// panes who prefer the old behaviour set this true.
	DriftSoftFail bool
	// NotifyOnFailed enables the auto-generated delivery-failure notice
	// when one of the recipient's outbound messages transitions to
	// `failed` state (#53). The notice is inserted as
	// KindDeliveryFailureNotice from this agent back to the original
	// sender, bypassing recipient/sender caps. Loop prevention: notices
	// that themselves fail to deliver do NOT generate further notices.
	// Default on (the operator's half-day waiting incident on
	// 2026-05-31 motivated this; silent-doesn't-know was the failure
	// mode).
	NotifyOnFailed bool
	// NotifyOnDeliveredUnverified enables the same notice path for the
	// `delivered_unverified` soft-failure case (paste+Enter completed
	// but the verify token didn't surface). Independent toggle from
	// NotifyOnFailed; both default on.
	NotifyOnDeliveredUnverified bool
	// Walker resolves pane-id drift via the shared discover package. When
	// nil, runServeWithStore constructs a discover.New() — tests can inject
	// a fake walker that doesn't touch real tmux/proc.
	Walker *discover.Walker
}

// runServeCLI parses serve-subcommand flags, sets up signal handling, and
// drives the mailman loop.
//
// Usage: claude-msg serve --agent NAME [tuning flags]
func runServeCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	agent := fs.String("agent", "", "agent name to serve (required)")
	interMsg := fs.Duration("inter-message-delay", 200*time.Millisecond,
		"pause between successive deliveries")
	idlePoll := fs.Duration("idle-poll", 250*time.Millisecond,
		"queue-empty sleep before re-checking")
	pausePoll := fs.Duration("pause-poll", time.Second,
		"interval to re-check the paused flag")
	deliverTimeout := fs.Duration("deliver-timeout", 30*time.Second,
		"per-message deadline for the tmux delivery sequence")
	postCompactPause := fs.Duration("post-compact-pause", 120*time.Second,
		"quiescent window after delivering /compact before claiming the next message (0 to disable)")
	quietObserve := fs.Duration("quiet-observe-window", 3*time.Second,
		"per-probe wait between pasting each of the two dashes and capturing/observing — gives the operator time to react to each dash individually")
	quietInputBackoff := fs.Duration("quiet-input-backoff", 60*time.Second,
		"how long to wait before re-probing after detecting operator activity in the input row")
	quietMaxWait := fs.Duration("quiet-max-wait", 5*time.Minute,
		"total cap on the pre-delivery quiet wait; on cap we deliver anyway with a WARN log")
	quietDisabled := fs.Bool("quiet-disabled", true,
		"bypass the probe-and-watch gate (delivery happens immediately on every queue head). Default true since 2026-06-01: empirical use showed the gate added 5-min worst-case latency without preventing mid-turn collisions in practice — the verify-token retry + delivered_unverified notice path (independent toggle, on by default) is the load-bearing safety net. Re-enable per-agent via TOML `quiet-disabled = false` if a polite-wait shape is wanted")
	quickPresenceProbe := fs.Bool("quick-presence-probe", false,
		"opt-in asymmetric pre-check (#63): when --quiet-disabled is also true, run a ~50ms one-shot probe before each delivery; if it detects operator typing during the probe window, fall back to the full probe-and-watch gate; if quiet, deliver immediately. Catches the active-typing collision class without paying the full gate's latency on idle panes. Defaults to false (preserves v0.2.x ship-fast). Per-agent TOML knob: `quick-presence-probe = true`. Caveat: does NOT yet detect operator-drafts-sitting-in-the-buffer (a passive non-typing operator with an unsent draft) — that requires the prompt-sentinel detection deferred to #63 Part 2.")
	driftSoftFail := fs.Bool("drift-soft-fail", false,
		"when pre-delivery drift detection hits ambiguous or unrecoverable, log WARN and deliver to the (potentially wrong) pane instead of marking the message failed. Default off — fail-loud is safer for autonomous receivers")
	notifyOnFailed := fs.Bool("notify-on-failed", true,
		"on a recipient's outbound message transitioning to `failed`, auto-insert a delivery-failure notice back to the original sender (#53)")
	notifyOnDeliveredUnverified := fs.Bool("notify-on-delivered-unverified", true,
		"on a recipient's outbound message transitioning to `delivered_unverified` (paste+Enter ran but verify token didn't surface), auto-insert a notice back to the original sender (#53)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	// Load host-level config (#54). Missing-file → silent defaults;
	// malformed-file → WARN + fall back to defaults so a bad config
	// doesn't kill the mailman.
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		fmt.Fprintf(stderr, "WARN config: %v — using defaults\n", cfgErr)
	}
	// Precedence: CLI flags > per-agent block > defaults block >
	// hardcoded compile-time defaults. fs.Lookup("X").DefValue is the
	// hardcoded default; fs.Lookup("X").Value.String() is the resolved
	// flag value. If they differ, the flag was explicitly set (or the
	// caller passed the same as default — indistinguishable in stdlib
	// flag, accepted limitation per #54 AC).
	if !flagWasSet(fs, "notify-on-failed") {
		*notifyOnFailed = config.ResolveBool(cfg, *agent, "notify-on-failed", *notifyOnFailed)
	}
	if !flagWasSet(fs, "notify-on-delivered-unverified") {
		*notifyOnDeliveredUnverified = config.ResolveBool(cfg, *agent, "notify-on-delivered-unverified", *notifyOnDeliveredUnverified)
	}
	if !flagWasSet(fs, "drift-soft-fail") {
		*driftSoftFail = config.ResolveBool(cfg, *agent, "drift-soft-fail", *driftSoftFail)
	}
	if !flagWasSet(fs, "quiet-disabled") {
		*quietDisabled = config.ResolveBool(cfg, *agent, "quiet-disabled", *quietDisabled)
	}
	if !flagWasSet(fs, "quick-presence-probe") {
		*quickPresenceProbe = config.ResolveBool(cfg, *agent, "quick-presence-probe", *quickPresenceProbe)
	}
	if !flagWasSet(fs, "quiet-observe-window") {
		*quietObserve = config.ResolveDuration(cfg, *agent, "quiet-observe-window", *quietObserve)
	}
	if !flagWasSet(fs, "quiet-input-backoff") {
		*quietInputBackoff = config.ResolveDuration(cfg, *agent, "quiet-input-backoff", *quietInputBackoff)
	}
	if !flagWasSet(fs, "quiet-max-wait") {
		*quietMaxWait = config.ResolveDuration(cfg, *agent, "quiet-max-wait", *quietMaxWait)
	}
	if *agent == "" {
		fmt.Fprintln(stderr, "--agent required")
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		fmt.Fprintf(stderr, "open store: %v\n", err)
		return exitInternal
	}
	defer s.Close()

	stopCtx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger := log.New(stderr,
		fmt.Sprintf("[mailman/%s] ", *agent),
		log.LstdFlags|log.Lmicroseconds)

	return runServeWithStore(stopCtx, s, serveOpts{
		Agent:              *agent,
		InterMessageDelay:  *interMsg,
		IdlePollInterval:   *idlePoll,
		PauseCheckInterval: *pausePoll,
		DeliverTimeout:     *deliverTimeout,
		PostCompactPause:   *postCompactPause,
		QuietOpts: tmuxio.QuietOpts{
			ObserveWindow:        *quietObserve,
			InputActivityBackoff: *quietInputBackoff,
			MaxWait:              *quietMaxWait,
		},
		QuietDisabled:               *quietDisabled,
		QuickPresenceProbe:          *quickPresenceProbe,
		DriftSoftFail:               *driftSoftFail,
		NotifyOnFailed:              *notifyOnFailed,
		NotifyOnDeliveredUnverified: *notifyOnDeliveredUnverified,
	}, logger, stdout, stderr)
}

// runServeWithStore is the testable mailman loop. stopCtx is the signal
// context — cancellation requests a graceful exit at the next loop edge.
// SQL and tmux operations use independent contexts so an in-flight message
// completes cleanly even when SIGTERM has already fired.
func runServeWithStore(stopCtx context.Context, s *store.Store,
	opts serveOpts, logger *log.Logger,
	_ io.Writer, stderr io.Writer,
) int {
	// Background context for store + tmux operations. We don't want a
	// SIGTERM mid-Deliver to leave a half-pasted message; instead we let
	// the current iteration finish, then exit at the top of the next.
	opCtx := context.Background()

	// Startup: agent must be registered with a pane_id.
	a, err := s.GetAgent(opCtx, opts.Agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "agent %q not registered — run 'claude-msg discover'\n", opts.Agent)
			return exitUnavailable
		}
		fmt.Fprintf(stderr, "get_agent: %v\n", err)
		return exitInternal
	}
	if a.PaneID == "" {
		fmt.Fprintf(stderr, "agent %q has no pane_id — run 'claude-msg discover'\n", opts.Agent)
		return exitUnavailable
	}

	if n, err := s.RecoverDelivering(opCtx, opts.Agent); err != nil {
		logger.Printf("recover_failed err=%v", err)
	} else if n > 0 {
		logger.Printf("recovered count=%d", n)
	}

	walker := opts.Walker
	logger.Printf("starting pane=%s", a.PaneID)
	defer logger.Printf("stopped")

	// systemd watchdog: tell the manager we're up, log the interval that
	// will keep WatchdogSec= happy. The ping at the bottom of each loop
	// iteration covers the busy path; the idle-poll select includes the
	// watchdog window for empty queues.
	if err := sdnotify.Ready(); err != nil {
		logger.Printf("sdnotify_ready_err err=%v", err)
	}
	watchdogPing, _ := sdnotify.WatchdogInterval()
	if watchdogPing > 0 {
		logger.Printf("watchdog interval=%s", watchdogPing)
	}

	// Wire a ping closure into the quiet-pane gate so its internal
	// sleeps (ObserveWindow + BackoffInterval) keep the systemd
	// watchdog ticking. Without this, a 60s activity-detected backoff
	// trips WatchdogSec=30s and SIGABRTs the mailman mid-backoff
	// (2026-05-30 incident).
	if opts.QuietOpts.Ping == nil {
		opts.QuietOpts.Ping = func() { _ = sdnotify.Watchdog() }
	}
	if opts.QuietOpts.PingInterval == 0 && watchdogPing > 0 {
		opts.QuietOpts.PingInterval = watchdogPing
	}

	for {
		if stopCtx.Err() != nil {
			return exitOK
		}
		if watchdogPing > 0 {
			_ = sdnotify.Watchdog()
		}

		// Re-read every iteration so pause/resume and discover updates
		// are picked up without restarting the daemon.
		a, err := s.GetAgent(opCtx, opts.Agent)
		if err != nil {
			logger.Printf("get_agent_failed err=%v", err)
			if stopOrSleep(stopCtx, opts.PauseCheckInterval) {
				return exitOK
			}
			continue
		}
		if a.Paused {
			if stopOrSleep(stopCtx, opts.PauseCheckInterval) {
				return exitOK
			}
			continue
		}

		msg, err := s.ClaimNext(opCtx, opts.Agent)
		if err != nil {
			logger.Printf("claim_failed err=%v", err)
			if stopOrSleep(stopCtx, opts.IdlePollInterval) {
				return exitOK
			}
			continue
		}
		if msg == nil {
			if stopOrSleep(stopCtx, opts.IdlePollInterval) {
				return exitOK
			}
			continue
		}

		logger.Printf("delivering id=%s kind=%s from=%s body_bytes=%d",
			msg.PublicID, msg.Kind, msg.FromAgent, len(msg.Body))

		paneForDelivery := a.PaneID

		// Silent-drift guard (#37). Before the gate or the actual
		// delivery, verify the registered pane is still running the
		// expected agent. The 2026-05-31 incident was: tmux restored
		// panes with new ids after a host reboot; admin's message to
		// surveyor landed in Pilot's pane because surveyor's row
		// still pointed at the pane that now belonged to Pilot. The
		// verify-token check passed (the text was in that pane) and
		// the message was marked delivered, but to the wrong agent.
		// auto-heal couldn't catch it because the pane existed.
		//
		// This check uses discover.PaneAgentName to read the pane's
		// own --resume argument and confirms it matches opts.Agent.
		// On mismatch we run discover.LookupByName to find where the
		// expected agent is now, UPDATE the registry, and retry on
		// the new pane. If LookupByName can't find the agent either,
		// we proceed with the registered (drifted) pane — the
		// existing delivery + auto-heal paths take it from there.
		if !opts.DriftCheckDisabled {
			if walker == nil {
				walker = discover.New()
			}
			canonicals := buildCanonicals(opCtx, s)
			running, ambiguous, err := walker.PaneAgentNameWithCanonicals(opCtx, paneForDelivery, canonicals)
			driftFailReason := ""
			switch {
			case err != nil:
				// Soft fail: log and proceed with the registered pane.
				// Errors from /proc reads or tmux list-panes are
				// system-level, not policy-level; don't punish the
				// message for an environmental hiccup.
				logger.Printf("drift_check_err id=%s err=%v", msg.PublicID, err)
			case ambiguous:
				driftFailReason = "drift_check_ambiguous"
				logger.Printf("WARN drift_check_ambiguous id=%s agent=%s registered_pane=%s — multiple canonicals exact-or-substring-match the running --resume value (resolve via: semaphore.register name=<canonical> alias=<unique-suffix> force=true; #47)",
					msg.PublicID, opts.Agent, paneForDelivery)
			case running != "" && running != opts.Agent:
				newPane, lambig, lerr := walker.LookupByNameWithCanonicals(opCtx, opts.Agent, canonicals)
				switch {
				case lerr != nil:
					logger.Printf("drift_lookup_err id=%s err=%v", msg.PublicID, lerr)
				case lambig:
					driftFailReason = "drift_lookup_ambiguous"
					logger.Printf("WARN drift_lookup_ambiguous id=%s agent=%s — multiple canonicals match a candidate pane",
						msg.PublicID, opts.Agent)
				case newPane != "" && newPane != paneForDelivery:
					logger.Printf("drift_detected id=%s agent=%s registered_pane=%s runs=%s — rediscovered=%s",
						msg.PublicID, opts.Agent, paneForDelivery, running, newPane)
					if uerr := s.UpsertAgent(opCtx, opts.Agent, newPane); uerr != nil {
						logger.Printf("drift_update_failed err=%v", uerr)
					} else {
						paneForDelivery = newPane
					}
				default:
					driftFailReason = "drift_detected_unrecoverable"
					logger.Printf("WARN drift_detected_unrecoverable id=%s agent=%s registered_pane=%s runs=%s — discover couldn't find %s anywhere",
						msg.PublicID, opts.Agent, paneForDelivery, running, opts.Agent)
				}
			}
			if driftFailReason != "" && !opts.DriftSoftFail {
				// Fail-loud default (Surveyor Q(b) review): silent
				// delivery to the wrong agent cascades on autonomous
				// receivers. MarkFailed surfaces the issue to the
				// sender (and to operator monitoring) immediately.
				// Operators who prefer the soft-fail behaviour set
				// --drift-soft-fail.
				reason := driftFailReason + ": delivery aborted under fail-loud default; set --drift-soft-fail to deliver-anyway"
				if mferr := s.MarkFailed(opCtx, msg.PublicID, reason); mferr != nil {
					logger.Printf("mark_failed_err id=%s err=%v", msg.PublicID, mferr)
				}
				maybeInsertFailureNotice(opCtx, s, logger,
					opts.NotifyOnFailed, opts.Agent, msg, "failed", reason)
				if stopOrSleep(stopCtx, opts.InterMessageDelay) {
					return exitOK
				}
				continue
			}
		}

		// Asymmetric quick-presence pre-check (#63). When QuietDisabled
		// is the default (skip full gate), an opt-in cheap probe still
		// catches the mid-typing case: if the operator is currently
		// drafting, the quick probe detects activity and we promote to
		// the full gate; if the input row is quiet, we proceed straight
		// to delivery without paying the multi-second observe windows.
		// Net cost ~50ms when the gate isn't needed, identical to the
		// pre-#63 fast path when QuickPresenceProbe is false.
		runFullGate := !opts.QuietDisabled
		if opts.QuietDisabled && opts.QuickPresenceProbe {
			qCtx, qCancel := context.WithTimeout(stopCtx,
				200*time.Millisecond+opts.QuickPresenceOpts.PaintWait)
			verdict, qerr := tmuxio.QuickPresenceProbe(qCtx, paneForDelivery,
				opts.QuickPresenceOpts)
			qCancel()
			if qerr != nil || verdict == tmuxio.DeltaInputActivity {
				// Probe error → conservative: gate. Operator activity
				// → safety: gate. Both paths converge on the existing
				// full-gate behaviour.
				if qerr != nil {
					logger.Printf("quick-presence probe error on pane %s: %v; falling back to full gate",
						paneForDelivery, qerr)
				}
				runFullGate = true
			}
		}

		// Pre-delivery quiet-pane gate (probe-and-watch). On any error
		// other than a clean quiet exit, log and proceed — we'd rather
		// risk a fragmented delivery than starve the queue. The
		// per-iteration cap inside WaitForQuietPane handles the truly
		// pathological "operator never stops typing" case.
		//
		// We derive the quiet ctx from stopCtx (not opCtx) so SIGTERM
		// wakes us out of a long quiet wait — the operator shouldn't
		// have to wait up to 30 minutes for the mailman to notice it
		// should stop. The ClaimNext above already transitioned the
		// row to 'delivering'; on SIGTERM exit, RecoverDelivering at
		// the next startup resets it to 'queued' for a clean retry.
		if runFullGate {
			quietCtx, qcancel := context.WithTimeout(stopCtx,
				opts.QuietOpts.MaxWait+5*time.Second)
			qerr := tmuxio.WaitForQuietPane(quietCtx, paneForDelivery, opts.QuietOpts)
			qcancel()
			if qerr != nil {
				switch {
				case errors.Is(qerr, context.Canceled):
					// SIGTERM during the quiet wait — exit cleanly, do
					// not deliver. Row stays 'delivering'; recovered
					// on next startup.
					return exitOK
				case errors.Is(qerr, tmuxio.ErrCapExceeded):
					logger.Printf("WARN quiet_cap_exceeded id=%s pane=%s — delivering anyway",
						msg.PublicID, paneForDelivery)
				default:
					logger.Printf("WARN quiet_check_err id=%s err=%v — delivering anyway",
						msg.PublicID, qerr)
				}
			}
		}

		deliverCtx, cancel := context.WithTimeout(opCtx, opts.DeliverTimeout)
		derr := deliverOne(deliverCtx, paneForDelivery, msg)
		cancel()

		// Auto-heal on pane-id drift: if tmux says the pane is gone, ask
		// the discover walker for the agent's current pane, update the
		// row, retry once. Avoids marking messages 'failed' when the
		// operator just respawned a pane in a new window.
		if derr != nil && isCantFindPaneError(derr) {
			if walker == nil {
				walker = discover.New()
			}
			newPane, lerr := walker.LookupByName(opCtx, opts.Agent)
			if lerr == nil && newPane != "" && newPane != paneForDelivery {
				logger.Printf("auto_heal id=%s agent=%s old_pane=%s new_pane=%s",
					msg.PublicID, opts.Agent, paneForDelivery, newPane)
				if uerr := s.UpsertAgent(opCtx, opts.Agent, newPane); uerr != nil {
					logger.Printf("auto_heal_update_failed err=%v", uerr)
				} else {
					retryCtx, rcancel := context.WithTimeout(opCtx, opts.DeliverTimeout)
					derr = deliverOne(retryCtx, newPane, msg)
					rcancel()
				}
			} else if lerr != nil {
				logger.Printf("auto_heal_lookup_err err=%v", lerr)
			}
		}

		// Three outcomes:
		//   - derr == nil: verified delivery (verify token observed in
		//     the post-Enter capture). Normal success path.
		//   - errors.Is(derr, tmuxio.ErrUnverifiedDelivery): paste +
		//     Enter completed mechanically, but Claude Code didn't
		//     surface the token in the retry budget. Typically means
		//     Claude was mid-turn and Enter was queued. We mark
		//     delivered + log WARN; the operator sees the text in
		//     their pane and submits it manually if Claude was busy.
		//     Marking failed here would drop the message permanently
		//     even though it's sitting in the input box.
		//   - other err: hard failure (tmux command errored, ctx
		//     cancelled, etc.). Mark failed.
		switch {
		case derr == nil:
			logger.Printf("delivered id=%s", msg.PublicID)
			if err := s.MarkDelivered(opCtx, msg.PublicID); err != nil {
				logger.Printf("mark_delivered_err id=%s err=%v", msg.PublicID, err)
			}
			if isCompactControl(msg) && opts.PostCompactPause > 0 {
				logger.Printf("post_compact_pause id=%s duration=%s",
					msg.PublicID, opts.PostCompactPause)
				if sleepRespectingWatchdog(stopCtx, opts.PostCompactPause, watchdogPing) {
					return exitOK
				}
			}
		case errors.Is(derr, tmuxio.ErrUnverifiedDelivery):
			logger.Printf("WARN delivered_unverified id=%s — paste+Enter completed but token not surfaced in time (Claude likely mid-turn); marking delivered, operator may need to submit manually",
				msg.PublicID)
			if err := s.MarkDelivered(opCtx, msg.PublicID); err != nil {
				logger.Printf("mark_delivered_err id=%s err=%v", msg.PublicID, err)
			}
			maybeInsertFailureNotice(opCtx, s, logger,
				opts.NotifyOnDeliveredUnverified, opts.Agent, msg,
				"delivered_unverified",
				"paste+Enter completed but verify token didn't surface in time")
		default:
			logger.Printf("deliver_failed id=%s err=%v", msg.PublicID, derr)
			if err := s.MarkFailed(opCtx, msg.PublicID, derr.Error()); err != nil {
				logger.Printf("mark_failed_err id=%s err=%v", msg.PublicID, err)
			}
			maybeInsertFailureNotice(opCtx, s, logger,
				opts.NotifyOnFailed, opts.Agent, msg, "failed", derr.Error())
		}

		if stopOrSleep(stopCtx, opts.InterMessageDelay) {
			return exitOK
		}
	}
}

// maybeInsertFailureNotice generates a delivery-failure notice back to
// the original sender when the relevant config toggle is on (#53).
//
// Loop prevention: if the failed message is itself a notice
// (msg.Kind == KindDeliveryFailureNotice), this is a no-op. Otherwise
// a wedged sender pane would compound notice-on-notice-on-notice and
// burn the queue.
//
// The notice is inserted via store.InsertNotice which bypasses the
// recipient-queue and sender-backlog caps — failure notifications are
// operationally critical and shouldn't be silently dropped on cap.
//
// `failureKind` is the human-readable failure-class label ("failed"
// or "delivered_unverified") used in the notice body.
// `reason` is the underlying error message or WARN reason.
func maybeInsertFailureNotice(
	ctx context.Context,
	s *store.Store,
	logger *log.Logger,
	enabled bool,
	this string,
	msg *store.Message,
	failureKind string,
	reason string,
) {
	if !enabled {
		return
	}
	// Loop prevention: don't notify on a notice's own failure.
	if msg.Kind == store.KindDeliveryFailureNotice {
		return
	}
	body := renderFailureNoticeBody(msg, failureKind, reason)
	res, err := s.InsertNotice(ctx, store.InsertParams{
		FromAgent: this,           // the mailman's own agent (original recipient)
		ToAgent:   msg.FromAgent,  // the original sender
		Body:      body,
		Kind:      store.KindDeliveryFailureNotice,
	})
	if err != nil {
		logger.Printf("notify_insert_err orig_id=%s to=%s err=%v",
			msg.PublicID, msg.FromAgent, err)
		return
	}
	logger.Printf("notify_inserted notice_id=%s orig_id=%s class=%s to=%s",
		res.PublicID, msg.PublicID, failureKind, msg.FromAgent)
}

// renderFailureNoticeBody formats the human-readable body of a
// delivery-failure notice. The shape is stable enough for both
// human reading in the recipient's pane AND simple grep-based parsing
// by future tools.
func renderFailureNoticeBody(msg *store.Message, failureKind, reason string) string {
	preview := msg.Body
	const maxPreview = 200
	if len(preview) > maxPreview {
		preview = preview[:maxPreview] + "...(truncated)"
	}
	return fmt.Sprintf(`:warning: Delivery failure
  Original message id: %s
  Recipient: %s
  Failure class: %s
  Reason: %s
  Original body preview:
    %s`,
		msg.PublicID, msg.ToAgent, failureKind, reason, preview)
}

// deliverOne dispatches a single message to a pane based on its Kind:
// regular messages go through the paste-buffer renderer with verification;
// control commands type their body directly via send-keys -l so they hit
// Claude Code's slash-command parser without the chat header.
func deliverOne(ctx context.Context, pane string, msg *store.Message) error {
	if msg.Kind == store.KindControl {
		return tmuxio.SendKeys(ctx, pane, msg.Body)
	}
	return tmuxio.Deliver(ctx, tmuxio.DeliverParams{
		Pane:        pane,
		Body:        render.Message(*msg),
		VerifyToken: "id " + msg.PublicID,
	})
}

// buildCanonicals snapshots the agents registry into the
// discover.CanonicalAgent shape so the walker can do canonical-name
// + alias matching (#38). Returns nil on any error — the drift-check
// path treats nil canonicals as "fall back to raw --resume value,"
// which preserves the pre-#38 behaviour.
func buildCanonicals(ctx context.Context, s *store.Store) []discover.CanonicalAgent {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return nil
	}
	out := make([]discover.CanonicalAgent, 0, len(agents))
	for _, a := range agents {
		out = append(out, discover.CanonicalAgent{Name: a.Name, Aliases: a.Aliases})
	}
	return out
}

// isCantFindPaneError detects the tmux delivery failure mode that means
// the recipient's stored pane_id no longer exists. tmux 3.x phrases this
// as "can't find pane: %N"; we match on the substring so the format can
// drift across versions without breaking the auto-heal path.
func isCantFindPaneError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "can't find pane")
}

// isCompactControl returns true when msg is a control row whose body is
// exactly `/compact` (no args today — kept strict so a future arg-bearing
// /compact-style command doesn't accidentally pull in the long pause).
func isCompactControl(msg *store.Message) bool {
	return msg.Kind == store.KindControl && strings.TrimSpace(msg.Body) == "/compact"
}

// sleepRespectingWatchdog blocks for d, returning early when stopCtx
// cancels. It pings sd_notify every pingEvery so the systemd watchdog
// doesn't trip during long quiescent windows (the post-compact pause is
// ~120s, well above WatchdogSec=30s). pingEvery <= 0 falls back to a
// single uninterrupted sleep — fine for tests, fine on hosts without a
// configured watchdog.
func sleepRespectingWatchdog(stopCtx context.Context, d, pingEvery time.Duration) bool {
	if d <= 0 {
		return stopCtx.Err() != nil
	}
	if pingEvery <= 0 || pingEvery >= d {
		select {
		case <-stopCtx.Done():
			return true
		case <-time.After(d):
			return false
		}
	}
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		wait := pingEvery
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-stopCtx.Done():
			return true
		case <-time.After(wait):
			_ = sdnotify.Watchdog()
		}
	}
}

// stopOrSleep waits for d or until stopCtx is cancelled. Returns true on
// cancellation so the caller can exit.
func stopOrSleep(stopCtx context.Context, d time.Duration) bool {
	if d <= 0 {
		return stopCtx.Err() != nil
	}
	select {
	case <-stopCtx.Done():
		return true
	case <-time.After(d):
		return false
	}
}
