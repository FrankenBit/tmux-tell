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

	"git.frankenbit.de/frankenbit/tmux-msg/internal/config"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/render"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/sdnotify"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
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
	// ObserveGateOpts configures the read-only-observe-only gate (#92)
	// that replaced the probe-and-watch flow. See
	// internal/tmuxio/observe_gate.go for the per-field semantics.
	ObserveGateOpts tmuxio.ObserveGateOpts
	// GateDisabled bypasses the observe-gate entirely; delivery
	// happens immediately on every queue head. Useful in tests (avoids
	// faking AgentState in the runtime tmux runner) and as an
	// operator escape hatch. Default false (gate on).
	GateDisabled bool
	// NotifyEmojiDisabled suppresses the operator-typing 📫
	// visibility notification (#95). Default false (notification on).
	// When false, the mailman wires ObserveGate's OnOperatorTyping
	// callback to NotifyPendingMessage so a single 📫 lands in the
	// operator's input row the first iteration the gate observes
	// StateAwaitingOperator.
	NotifyEmojiDisabled bool
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
	// Observe-gate knobs (#92). The read-only-observe-only gate replaces
	// the probe-and-watch flow; see internal/tmuxio/observe_gate.go.
	gateDisabled := fs.Bool("gate-disabled", false,
		"bypass the observe-gate entirely (delivery happens immediately on every queue head). Default false (gate on). Operators rarely need to disable; the gate is read-only-observe-only and adds ~3-5s in the typical idle case. Per-agent TOML knob: `gate-disabled = true`.")
	pollIntervalMin := fs.Duration("poll-interval-min", 3*time.Second,
		"observe-gate initial poll interval. The gate samples AgentState at this cadence on the fast path (#92).")
	pollIntervalMax := fs.Duration("poll-interval-max", 15*time.Second,
		"observe-gate maximum poll interval. The cadence backs off multiplicatively (1.5×) up to this cap when the agent is not yet ready (#92).")
	inputStaleThreshold := fs.Duration("input-stale-threshold", 2*time.Minute,
		"observe-gate stale-draft threshold. When the operator's input-row content remains unchanged this long, the gate decides the draft is abandoned and proceeds with archive-then-clear-then-paste (kind=stranded_draft snapshot + Ctrl+U). Per #92's 2026-06-04 design call.")
	notifyEmojiDisabled := fs.Bool("notify-emoji-disabled", false,
		"disable the operator-typing 📫 visibility notification (#95). Default false (notification on). When the observe-gate first detects the operator is typing, the mailman injects a single 📫 character into their input row as a one-shot signal that a bus message is pending. Operator can Backspace it (gate keeps waiting) or let it ride along with their next message.")
	workingDeliverImmediately := fs.Bool("working-deliver-immediately", false,
		"opt the observe-gate's StateWorking branch into a fast-path return — deliver immediately to a busy chamber instead of deferring (#106). Default false (defer on Working, the v0.3.0-through-v0.6.0 conservative behavior). When on, mid-turn deliveries land in the recipient's input row while Claude is still streaming and are read as the next operator turn after the current one completes. Eligibility: StateWorking only — AwaitingOperator / Compaction / Unknown stay hard-deferred regardless. Per-agent TOML knob: `working-deliver-immediately = true`.")
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
	if !flagWasSet(fs, "gate-disabled") {
		*gateDisabled = config.ResolveBool(cfg, *agent, "gate-disabled", *gateDisabled)
	}
	if !flagWasSet(fs, "poll-interval-min") {
		*pollIntervalMin = config.ResolveDuration(cfg, *agent, "poll-interval-min", *pollIntervalMin)
	}
	if !flagWasSet(fs, "poll-interval-max") {
		*pollIntervalMax = config.ResolveDuration(cfg, *agent, "poll-interval-max", *pollIntervalMax)
	}
	if !flagWasSet(fs, "input-stale-threshold") {
		*inputStaleThreshold = config.ResolveDuration(cfg, *agent, "input-stale-threshold", *inputStaleThreshold)
	}
	if !flagWasSet(fs, "notify-emoji-disabled") {
		*notifyEmojiDisabled = config.ResolveBool(cfg, *agent, "notify-emoji-disabled", *notifyEmojiDisabled)
	}
	if !flagWasSet(fs, "working-deliver-immediately") {
		*workingDeliverImmediately = config.ResolveBool(cfg, *agent, "working-deliver-immediately", *workingDeliverImmediately)
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
		GateDisabled:       *gateDisabled,
		ObserveGateOpts: tmuxio.ObserveGateOpts{
			PollIntervalMin:           *pollIntervalMin,
			PollIntervalMax:           *pollIntervalMax,
			InputStaleThreshold:       *inputStaleThreshold,
			WorkingDeliverImmediately: *workingDeliverImmediately,
			// MaxWait stays at the gate's default (5m); not exposed as
			// a CLI flag in v1 — operators can tune via TOML if needed
			// once the migration settles.
		},
		NotifyEmojiDisabled: *notifyEmojiDisabled,
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

	// Mailbox-only short-circuit (#116). When the agent's delivery_mode
	// is mailbox-only, the mailman daemon has no work to do — messages
	// stay in state=queued and the operator polls via `claude-msg inbox`.
	// Exit cleanly so systemd records a Result=success rather than
	// burning CPU on a poll loop that would never deliver anything. If
	// the operator later flips delivery_mode back to paste-and-enter,
	// they need to manually restart the unit.
	if a.DeliveryMode == store.DeliveryModeMailboxOnly {
		logger.Printf("delivery_mode=mailbox-only — no daemon work; exiting cleanly. " +
			"NOTE: flip-back is asymmetric — if you later set delivery_mode=paste-and-enter, " +
			"restart this unit manually (systemctl --user restart claude-mailman@%s)", opts.Agent)
		if err := sdnotify.Ready(); err != nil {
			logger.Printf("sdnotify_ready_err err=%v", err)
		}
		return exitOK
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

	// Wire a ping closure into the observe-gate so its internal sleeps
	// (PollInterval up to 15s by default) keep the systemd watchdog
	// ticking. Without this, a long poll interval could trip
	// WatchdogSec=30s and SIGABRT the mailman mid-observe (sibling to
	// the 2026-05-30 incident on the legacy probe-and-watch path).
	if opts.ObserveGateOpts.Ping == nil {
		opts.ObserveGateOpts.Ping = func() { _ = sdnotify.Watchdog() }
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
				logger.Printf("WARN drift_check_ambiguous id=%s agent=%s registered_pane=%s — multiple canonicals exact-or-substring-match the running --resume value (resolve via: tmux-msg.register name=<canonical> alias=<unique-suffix> force=true; #47)",
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

		// Pre-delivery observe-gate (#92). Read-only AgentState
		// polling + content-hash stale-detection. Replaces the legacy
		// probe-and-watch flow (the dashes + 60s backoff path
		// per the tmux-msg #91 investigation). On any error other
		// than a clean exit, log and proceed — we'd rather risk a
		// fragmented delivery than starve the queue.
		//
		// We derive the gate ctx from stopCtx (not opCtx) so SIGTERM
		// wakes us out of a long observe loop. The ClaimNext above
		// already transitioned the row to 'delivering'; on SIGTERM
		// exit, RecoverDelivering at the next startup resets it to
		// 'queued' for a clean retry.
		if !opts.GateDisabled {
			gateOpts := opts.ObserveGateOpts
			if gateOpts.Ping == nil {
				gateOpts.Ping = func() { _ = sdnotify.Watchdog() }
			}
			// Wire the operator-typing 📫 notification (#95) unless the
			// operator disabled it. The gate fires this callback ONCE
			// per delivery cycle on its first StateAwaitingOperator
			// observation; subsequent iterations skip.
			if !opts.NotifyEmojiDisabled {
				pane := paneForDelivery
				gateOpts.OnOperatorTyping = func() {
					nCtx, nCancel := context.WithTimeout(stopCtx, 1*time.Second)
					defer nCancel()
					if err := tmuxio.NotifyPendingMessage(nCtx, pane); err != nil {
						logger.Printf("notify_pending_failed id=%s pane=%s err=%v",
							msg.PublicID, pane, err)
					}
				}
			}
			gateBudget := gateOpts.MaxWait
			if gateBudget <= 0 {
				gateBudget = 5 * time.Minute
			}
			gateCtx, gcancel := context.WithTimeout(stopCtx, gateBudget+5*time.Second)
			outcome, gerr := tmuxio.ObserveGate(gateCtx, paneForDelivery, gateOpts)
			gcancel()
			if gerr != nil {
				switch {
				case errors.Is(gerr, context.Canceled):
					// SIGTERM during the observe loop — exit cleanly.
					return exitOK
				case errors.Is(gerr, tmuxio.ErrMaxWaitExceeded):
					logger.Printf("WARN gate_max_wait id=%s pane=%s iter=%d — delivering anyway (%s)",
						msg.PublicID, paneForDelivery, outcome.Iterations, outcome.Reason)
				default:
					logger.Printf("WARN gate_err id=%s err=%v — delivering anyway",
						msg.PublicID, gerr)
				}
			}
			// (c) primary path: when the operator had stable content in
			// the input row past InputStaleThreshold, archive the
			// content as kind=stranded_draft before clearing + pasting.
			// On archive failure, fall back to (a) compound delivery
			// per #92's 2026-06-04 design call. (b) Clear-and-discard
			// is OFF the table — the input content might be a half-
			// delivered bus message from a previous failed delivery.
			//
			// Step ordering: archive FIRST, then Ctrl+U. Archive is
			// load-bearing data capture (the operator's typed work);
			// Ctrl+U is opportunistic UX cleanup. The (archive OK,
			// Ctrl+U fails) edge case is acceptable: the operator sees
			// a compound message AND has the snapshot in the bus —
			// recoverable, not lossy. The inverse order (clear first
			// then archive) would risk silent draft loss on archive
			// failure after a successful clear, which the (b)-rejected
			// rationale exists to prevent.
			if outcome.Stale && outcome.InputContent != "" {
				if archiveErr := archiveStrandedDraft(opCtx, s, opts.Agent,
					paneForDelivery, msg.PublicID, outcome.InputContent); archiveErr != nil {
					logger.Printf("WARN stranded_draft_archive_failed id=%s err=%v — falling back to (a) compound delivery",
						msg.PublicID, archiveErr)
					// Skip the Ctrl+U; deliverOne will paste onto the
					// operator's content producing a compound message.
				} else {
					clearCtx, ccancel := context.WithTimeout(stopCtx, 2*time.Second)
					clearErr := tmuxio.SendCtrlU(clearCtx, paneForDelivery)
					ccancel()
					if clearErr != nil {
						logger.Printf("WARN stranded_draft_clear_failed id=%s err=%v — falling back to (a) compound delivery; draft snapshot already archived",
							msg.PublicID, clearErr)
					} else {
						logger.Printf("stranded_draft archived+cleared id=%s pane=%s bytes=%d (gate iter=%d)",
							msg.PublicID, paneForDelivery, len(outcome.InputContent), outcome.Iterations)
					}
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
		FromAgent: this,          // the mailman's own agent (original recipient)
		ToAgent:   msg.FromAgent, // the original sender
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

// archiveStrandedDraft snapshots operator-typed content that the
// observe-gate (#92) is about to flush from the receiver's input row
// to make room for delivery. Inserted as kind=stranded_draft from the
// receiving agent to itself (self-addressed) via the cap-bypass
// InsertNotice path — operator-typed work shouldn't be silently
// dropped because the inbox is congested.
//
// Per #92's 2026-06-04 design call: this is the (c) Clear-paste-
// archive primary path. If this insert fails the caller falls back
// to (a) compound delivery (paste appends to the operator's content
// without clearing) rather than risk a silent draft loss via (b).
//
// `receiver` is the agent the mailman is serving (the agent whose
// pane had the stranded content). `triggerMsgID` is the public_id of
// the message whose delivery triggered the flush — referenced in the
// body so the operator can correlate.
func archiveStrandedDraft(
	ctx context.Context,
	s *store.Store,
	receiver string,
	pane string,
	triggerMsgID string,
	content string,
) error {
	body := renderStrandedDraftBody(pane, triggerMsgID, content)
	_, err := s.InsertNotice(ctx, store.InsertParams{
		FromAgent: receiver, // self-addressed
		ToAgent:   receiver,
		Body:      body,
		Kind:      store.KindStrandedDraft,
	})
	return err
}

// renderStrandedDraftBody formats the human-readable body of a
// stranded-draft snapshot. The shape parallels renderFailureNoticeBody
// for visual consistency in the inbox view.
func renderStrandedDraftBody(pane, triggerMsgID, content string) string {
	return fmt.Sprintf(`:bookmark: Stranded draft snapshot
  Pane: %s
  Triggered by delivery of: %s
  Cleared content:
%s`, pane, triggerMsgID, indentForBody(content))
}

// indentForBody indents each line of s by two spaces so multi-line
// content nests visually inside a bullet-style body block. No
// trimming — we preserve the operator's original whitespace verbatim
// so they can recover the draft exactly as they typed it.
func indentForBody(s string) string {
	if s == "" {
		return "    (empty)"
	}
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("    ")
		b.WriteString(line)
	}
	return b.String()
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
