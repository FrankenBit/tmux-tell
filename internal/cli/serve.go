package cli

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
	"git.frankenbit.de/frankenbit/tmux-msg/internal/metrics"
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

const (
	// defaultStuckThreshold is the consecutive `can't find pane` count at
	// which the mailman parks itself (#291). 10 with the backoff schedule
	// below means a truly-broken registration parks after ~4 minutes of
	// exponentially-spaced retries — long enough that a transient pane
	// outage (operator restarting tmux, a pane respawn) self-heals first,
	// short enough that a stale registration stops hammering tmux promptly.
	defaultStuckThreshold = 10
	// defaultStuckPollInterval is how often a parked mailman re-reads its
	// agent row to notice a `register --force` clear. No tmux probe — a
	// plain DB read — so a tight-ish cadence costs nothing.
	defaultStuckPollInterval = 5 * time.Second
	// stuckBackoffCap bounds the exponential pane-not-found retry delay
	// (#291). Even before the stuck threshold, no retry fires faster than
	// once per this interval — the 100/s storm that wedged tmux is capped
	// at 1/60s well before parking.
	stuckBackoffCap = 60 * time.Second
)

// paneNotFoundBackoff returns the delay before the next delivery attempt
// after `consecutive` back-to-back `can't find pane` probe failures (#291):
// 1s, 2s, 4s, 8s, 16s, 32s, then capped at stuckBackoffCap (60s). The first
// failure already waits 1s, which alone converts the pre-fix ~100/s retry
// storm into at most 1/s — the cap then drops it to 1/60s. The shift is
// guarded so a large counter can't overflow the duration.
func paneNotFoundBackoff(consecutive int) time.Duration {
	if consecutive < 1 {
		consecutive = 1
	}
	// time.Second << 6 = 64s already exceeds the cap, so anything from the
	// 7th consecutive failure onward is the cap — return early to avoid
	// shifting by a large amount.
	if consecutive >= 7 {
		return stuckBackoffCap
	}
	d := time.Second << uint(consecutive-1)
	if d > stuckBackoffCap {
		return stuckBackoffCap
	}
	return d
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
	// ObserveGateOpts configures the observe-only-with-one-named-
	// visibility-side-effect gate (#92; the side-effect is the 📫
	// typing-notification per #95, opt-out via notify-emoji-disabled)
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
	// PrePasteSafetyDisabled bypasses the #105 Half 2 pre-paste safety
	// check (one final AgentState probe before the actual paste; aborts
	// when paste-unsafe states are observed). Production keeps it
	// enabled — the check is the load-bearing safety net against the
	// popup-as-Unknown failure mode where MaxWait fires with
	// lastState=Unknown after the operator drafted an AskUserQuestion
	// popup that didn't match AwaitingOperatorMarker. Tests that don't
	// fake AgentState set this true so the check doesn't classify the
	// fake runner's body-echoed pane content as Unknown and abort
	// every delivery.
	PrePasteSafetyDisabled bool
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
	// NotifyOnDeliveredInInputBox enables the same notice path for the
	// `delivered_in_input_box` soft-failure case (paste+Enter completed
	// but the verify token didn't surface). Independent toggle from
	// NotifyOnFailed; both default on.
	NotifyOnDeliveredInInputBox bool
	// Walker resolves pane-id drift via the shared discover package. When
	// nil, runServeWithStore constructs a discover.New() — tests can inject
	// a fake walker that doesn't touch real tmux/proc.
	Walker *discover.Walker
	// ConfigDeliveryMode is the resolved per-agent delivery-mode from
	// the TOML config (#132 follow-up to #116). Empty when no config
	// override is in effect; in that case the DB column wins. Valid
	// non-empty values are store.DeliveryModePasteAndEnter and
	// store.DeliveryModeMailboxOnly; invalid values are logged and the
	// DB column wins (fail-loud, not fail-stop).
	ConfigDeliveryMode string
	// ByteMarkerThreshold is the resolved body-byte cutoff above which
	// the rendered bracket header gains a length marker (#160). Parsed
	// from the render-byte-marker-threshold TOML string at startup;
	// defaults to render.DefaultByteMarkerThreshold. A value < 0 disables
	// the marker.
	ByteMarkerThreshold int
	// MetricsAddr is the bind address for the Prometheus /metrics endpoint
	// (#146). Empty disables the endpoint entirely — the no-behavior-change
	// default for existing deploys. When non-empty and Metrics is nil,
	// runServeWithStore builds a registry and starts an HTTP server on this
	// address for the run's lifetime.
	MetricsAddr string
	// Metrics is a test seam: when non-nil it is used directly (no HTTP
	// server started, MetricsAddr ignored) so a test can inject a registry,
	// drive a delivery, and assert the increments. Production leaves it nil
	// and lets MetricsAddr drive creation.
	Metrics *metrics.Metrics
	// Retention is the resolved per-agent retention window (#245). "infinite"
	// (the default) disables the background sweep. Any positive duration
	// string accepted by parseWindow (e.g. "30d", "7d", "24h") enables it.
	// Resolved from TOML at startup; no CLI flag (config-only surface).
	Retention string
	// RetentionSweepInterval is how often the background retention goroutine
	// wakes to prune old delivered+failed rows. Default 1h.
	RetentionSweepInterval time.Duration
	// DedupeWindow is the look-back window for the recipient-side delivery
	// dedupe (#157 PR2). When > 0, the mailman checks each incoming message
	// against prior delivered_in_input_box rows from the same sender within
	// this window: if the original is now visible in scrollback it is confirmed
	// and the duplicate is absorbed; otherwise the replay is delivered normally.
	// 0 disables the check entirely — zero behavior change for existing deploys.
	// Default config.DefaultDedupeWindow (60s).
	DedupeWindow time.Duration
	// StuckThreshold is the number of consecutive `can't find pane` probe
	// failures after which the mailman parks itself in the #291 stuck state
	// (writes agents.stuck_reason and stops probing tmux entirely). Default
	// defaultStuckThreshold (10). A value <= 0 disables the stuck transition
	// (backoff still applies), kept as an escape hatch for tests / operators
	// who want unbounded backoff-only behavior.
	StuckThreshold int
	// StuckPollInterval is how often a parked (stuck) mailman re-reads its
	// own agent row to notice a `register --force` clear. While stuck the
	// mailman issues NO tmux probes — this is a pure DB read on a slow
	// cadence. Default defaultStuckPollInterval (5s).
	StuckPollInterval time.Duration
}

// runServeCLI parses serve-subcommand flags, sets up signal handling, and
// drives the mailman loop.
//
// Usage: tmux-msg-claude serve --agent NAME [tuning flags]
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
	verifyRetryBudget := fs.Duration("verify-retry-budget", tmuxio.DefaultRetryBudget,
		"total verify-token retry window for post-paste verification (#153). The default ~5s schedule (100ms/250ms/500ms/1s/1.5s/1.65s across 7 capture attempts) scales proportionally to this budget — e.g. 10s doubles each delay, 15s triples. Per-agent TOML knob: `verify-retry-budget = \"15s\"` for large-payload hubs. Inspect with #146's tmux_msg_delivery_verify_attempt_seconds histogram before tuning.")
	postCompactPause := fs.Duration("post-compact-pause", 120*time.Second,
		"quiescent window after delivering /compact before claiming the next message (0 to disable)")
	// Observe-gate knobs (#92). The observe-only-with-one-named-
	// visibility-side-effect gate (📫 per #95, opt-out via
	// notify-emoji-disabled) replaces the probe-and-watch flow; see
	// internal/tmuxio/observe_gate.go.
	gateDisabled := fs.Bool("gate-disabled", false,
		"bypass the observe-gate entirely (delivery happens immediately on every queue head). Default false (gate on). Operators rarely need to disable; the gate is near-read-only (one optional 📫 nudge when you're typing, opt-out via notify-emoji-disabled) and adds ~3-5s in the typical idle case. Per-agent TOML knob: `gate-disabled = true`.")
	pollIntervalMin := fs.Duration("poll-interval-min", 3*time.Second,
		"observe-gate initial poll interval. The gate samples AgentState at this cadence on the fast path (#92).")
	pollIntervalMax := fs.Duration("poll-interval-max", 15*time.Second,
		"observe-gate maximum poll interval. The cadence backs off multiplicatively (1.5×) up to this cap when the agent is not yet ready (#92).")
	inputStaleThreshold := fs.Duration("input-stale-threshold", 2*time.Minute,
		"observe-gate stale-draft threshold. When the operator's input-row content remains unchanged this long, the gate decides the draft is abandoned and proceeds with archive-then-clear-then-paste (kind=stranded_draft snapshot + Ctrl+U). Per #92's 2026-06-04 design call.")
	notifyEmojiDisabled := fs.Bool("notify-emoji-disabled", false,
		"disable the operator-typing 📫 visibility notification (#95). Default false (notification on). When the observe-gate first detects the operator is typing, the mailman injects a single 📫 character into their input row as a one-shot signal that a bus message is pending. Operator can Backspace it (gate keeps waiting) or let it ride along with their next message.")
	prePasteSafetyDisabled := fs.Bool("pre-paste-safety-disabled", false,
		"bypass the #105 Half 2 pre-paste safety check (one final AgentState probe before each paste; aborts when paste-unsafe states are observed). Default false (safety check on). Operators rarely need to disable; the check is structurally inexpensive (one capture-pane probe per delivery) and is the load-bearing safety net against the popup-as-Unknown failure mode where MaxWait fires with lastState=Unknown after the operator drafted an AskUserQuestion popup. Per-agent TOML knob: `pre-paste-safety-disabled = true`.")
	workingDeliverImmediately := fs.Bool("working-deliver-immediately", false,
		"opt the observe-gate's StateWorking branch into a fast-path return — deliver immediately to a busy chamber instead of deferring (#106). Default false (defer on Working, the v0.3.0-through-v0.6.0 conservative behavior). When on, mid-turn deliveries land in the recipient's input row while Claude is still streaming and are read as the next operator turn after the current one completes. Eligibility: StateWorking only — AwaitingOperator / Compaction / Unknown stay hard-deferred regardless. Per-agent TOML knob: `working-deliver-immediately = true`.")
	driftSoftFail := fs.Bool("drift-soft-fail", false,
		"when pre-delivery drift detection hits ambiguous or unrecoverable, log WARN and deliver to the (potentially wrong) pane instead of marking the message failed. Default off — fail-loud is safer for autonomous receivers")
	notifyOnFailed := fs.Bool("notify-on-failed", true,
		"on a recipient's outbound message transitioning to `failed`, auto-insert a delivery-failure notice back to the original sender (#53)")
	notifyOnDeliveredInInputBox := fs.Bool("notify-on-delivered-in-input-box", true,
		"on a recipient's outbound message transitioning to `delivered_in_input_box` (paste+Enter ran but verify token didn't surface), auto-insert a notice back to the original sender (#53)")
	notifyOnDeliveredUnverifiedLegacy := fs.Bool("notify-on-delivered-unverified", true,
		"deprecated: use --notify-on-delivered-in-input-box (removal v1.0 — extended from v0.12.0 per ADR-0008 §Discretion clause, #140)")
	metricsAddr := fs.String("metrics-addr", "",
		"expose a Prometheus /metrics endpoint on this address (e.g. ':9099' or '127.0.0.1:9099'). Empty (the default) disables the endpoint entirely — no behavior change for deploys that don't scrape. Per-agent TOML knob: `metrics-addr = \":PORT\"` (each per-agent mailman is its own process, so assign a distinct port per agent). (#146)")
	stuckThreshold := fs.Int("stuck-threshold", defaultStuckThreshold,
		"number of consecutive `can't find pane` probe failures before the mailman parks itself in the #291 stuck state (stops probing tmux entirely; visible in `agents`, cleared via `register --force`). Exponential backoff applies on every failure regardless; this is the cutover to zero-probe parking. 0 disables parking (backoff-only). Per-agent TOML knob: `stuck-threshold = N`.")
	stuckPollInterval := fs.Duration("stuck-poll-interval", defaultStuckPollInterval,
		"how often a parked (stuck) mailman re-reads its agent row to notice a `register --force` clear. While stuck the mailman issues NO tmux probes — this is a pure DB read. Per-agent TOML knob: `stuck-poll-interval = \"5s\"`.")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if flagWasSet(fs, "notify-on-delivered-unverified") {
		fmt.Fprintf(stderr,
			"WARN deprecated_surface_used name=--notify-on-delivered-unverified removal=v1.0 — use --notify-on-delivered-in-input-box instead (ADR-0008)\n")
		*notifyOnDeliveredInInputBox = *notifyOnDeliveredUnverifiedLegacy
	}

	// Load host-level config (#54). Missing-file → silent defaults;
	// malformed-file → WARN + fall back to defaults so a bad config
	// doesn't kill the mailman.
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		fmt.Fprintf(stderr, "WARN config: %v — using defaults\n", cfgErr)
	}
	if config.HasDeprecatedNotifyOnDeliveredUnverified(cfg, *agent) {
		fmt.Fprintf(stderr,
			"WARN deprecated_surface_used name=notify-on-delivered-unverified removal=v1.0 — use notify-on-delivered-in-input-box in config instead (ADR-0008)\n")
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
	if !flagWasSet(fs, "notify-on-delivered-in-input-box") {
		*notifyOnDeliveredInInputBox = config.ResolveBool(cfg, *agent, "notify-on-delivered-in-input-box", *notifyOnDeliveredInInputBox)
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
	if !flagWasSet(fs, "pre-paste-safety-disabled") {
		*prePasteSafetyDisabled = config.ResolveBool(cfg, *agent, "pre-paste-safety-disabled", *prePasteSafetyDisabled)
	}
	if !flagWasSet(fs, "metrics-addr") {
		*metricsAddr = config.ResolveString(cfg, *agent, "metrics-addr", "")
	}
	if !flagWasSet(fs, "stuck-threshold") {
		*stuckThreshold = config.ResolveInt(cfg, *agent, "stuck-threshold", *stuckThreshold)
	}
	if !flagWasSet(fs, "stuck-poll-interval") {
		*stuckPollInterval = config.ResolveDuration(cfg, *agent, "stuck-poll-interval", *stuckPollInterval)
	}
	configDeliveryMode := config.ResolveString(cfg, *agent, "delivery-mode", "")
	// Resolve the verify-retry budget (#153). Stored as a duration string
	// in TOML; CLI flag overrides per the standard precedence chain. A
	// malformed value WARNs and falls back to the flag value (default 5s
	// preserves today's schedule) — fail-loud-not-fail-stop.
	if !flagWasSet(fs, "verify-retry-budget") {
		if raw := config.ResolveString(cfg, *agent, "verify-retry-budget", ""); raw != "" {
			if d, perr := time.ParseDuration(raw); perr != nil {
				fmt.Fprintf(stderr, "WARN config: verify-retry-budget %q: %v — using %v\n",
					raw, perr, *verifyRetryBudget)
			} else {
				*verifyRetryBudget = d
			}
		}
	}
	// Apply the resolved budget by overwriting tmuxio's package-level
	// retry schedule. Each mailman is its own process per agent, so this
	// effectively applies the per-agent verify-retry-budget to the right
	// scope without per-call plumbing through DeliverParams.
	tmuxio.SetRetrySchedule(tmuxio.DeriveRetrySchedule(*verifyRetryBudget))
	// Resolve the render length-marker threshold (#160) once at startup.
	// Stored as a human byte-size string; parse it here so the hot
	// delivery path holds a plain int. A malformed value WARNs and falls
	// back to the hardcoded default rather than taking the mailman down —
	// same fail-loud-not-fail-stop stance as the rest of config resolution.
	byteMarkerThreshold := render.DefaultByteMarkerThreshold
	if raw := config.ResolveString(cfg, *agent, "render-byte-marker-threshold", ""); raw != "" {
		if n, perr := config.ParseByteSize(raw); perr != nil {
			fmt.Fprintf(stderr, "WARN config: render-byte-marker-threshold %q: %v — using %d\n",
				raw, perr, byteMarkerThreshold)
		} else {
			byteMarkerThreshold = n
		}
	}
	// Retention policy (#245). TOML-only knob (no CLI flag); resolved once
	// at startup. "infinite" is the hardcoded default so absent config →
	// zero behavior change.
	retention := config.ResolveString(cfg, *agent, "retention", config.DefaultRetention)
	retentionSweepInterval := config.ResolveDuration(cfg, *agent, "retention-sweep-interval", config.DefaultRetentionSweepInterval)
	// Dedupe window (#157 PR2). TOML-only knob; resolved once at startup.
	// Default 60s; "0s" disables the dedupe check entirely.
	dedupeWindow := config.ResolveDuration(cfg, *agent, "dedupe-window", config.DefaultDedupeWindow)

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
		NotifyEmojiDisabled:         *notifyEmojiDisabled,
		PrePasteSafetyDisabled:      *prePasteSafetyDisabled,
		ConfigDeliveryMode:          configDeliveryMode,
		ByteMarkerThreshold:         byteMarkerThreshold,
		MetricsAddr:                 *metricsAddr,
		DriftSoftFail:               *driftSoftFail,
		NotifyOnFailed:              *notifyOnFailed,
		NotifyOnDeliveredInInputBox: *notifyOnDeliveredInInputBox,
		Retention:                   retention,
		RetentionSweepInterval:      retentionSweepInterval,
		DedupeWindow:                dedupeWindow,
		StuckThreshold:              *stuckThreshold,
		StuckPollInterval:           *stuckPollInterval,
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
			fmt.Fprintf(stderr, "agent %q not registered — run 'tmux-msg-claude discover'\n", opts.Agent)
			return exitUnavailable
		}
		fmt.Fprintf(stderr, "get_agent: %v\n", err)
		return exitInternal
	}
	if a.PaneID == "" {
		fmt.Fprintf(stderr, "agent %q has no pane_id — run 'tmux-msg-claude discover'\n", opts.Agent)
		return exitUnavailable
	}

	// TOML config override for delivery_mode (#132 follow-up to #116).
	// When the host-level config sets `[agent.<name>] delivery-mode = X`
	// (or via [defaults]), the mailman uses that value rather than the
	// DB column for this run. Lets operators who manage state via config
	// (rather than via register-time CLI/MCP calls) declare the mode
	// without writing to the agents table.
	//
	// Validation: invalid mode values from config are logged + the DB
	// column wins (fail-loud rather than fail-stop). This keeps a typo
	// in /etc/tmux-msg/config.toml from silently breaking the mailman.
	if opts.ConfigDeliveryMode != "" && opts.ConfigDeliveryMode != a.DeliveryMode {
		if store.ValidDeliveryMode(opts.ConfigDeliveryMode) {
			logger.Printf("delivery_mode overridden by config: db=%s → config=%s",
				a.DeliveryMode, opts.ConfigDeliveryMode)
			a.DeliveryMode = opts.ConfigDeliveryMode
		} else {
			logger.Printf("WARN config_delivery_mode_invalid %q — DB value (%s) wins",
				opts.ConfigDeliveryMode, a.DeliveryMode)
		}
	}

	// No-paste short-circuit (#116 mailbox-only, #249 hook-context). When the
	// agent's delivery_mode is one where the mailman does NOT paste into the
	// pane, the daemon has no work to do — exit cleanly so systemd records
	// Result=success rather than burning CPU on a poll loop that would never
	// deliver anything.
	//   - mailbox-only (#116): messages stay queued; the operator polls `inbox`.
	//   - hook-context (#249, ADR-0009): messages stay queued; the recipient's
	//     own Claude session presents them via the `hook-context` hook-helper on
	//     its next turn (the adapter delivers, not the mailman).
	// Flip-back is asymmetric in both cases: setting delivery_mode back to
	// paste-and-enter requires a manual unit restart.
	if a.DeliveryMode == store.DeliveryModeMailboxOnly || a.DeliveryMode == store.DeliveryModeHookContext {
		logger.Printf("delivery_mode=%s — mailman does not paste; exiting cleanly. "+
			"NOTE: flip-back is asymmetric — if you later set delivery_mode=paste-and-enter, "+
			"restart this unit manually (systemctl --user restart %s-mailman@%s)",
			a.DeliveryMode, active.BinaryName, opts.Agent)
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

	// Prometheus metrics (#146). Built only when enabled — the handle stays
	// nil otherwise and every m.* call below is a no-op (the nil-safe metrics
	// API), so the disabled path is the existing behavior plus one nil-compare
	// per call. A test injects opts.Metrics to assert increments without
	// standing up an HTTP server; production leaves it nil and lets
	// MetricsAddr drive creation. Started here (post mailbox-only
	// short-circuit) so a mailbox-only mailman that exits never binds a port.
	m := opts.Metrics
	if m == nil && opts.MetricsAddr != "" {
		m = metrics.New()
		startMetricsServer(stopCtx, m, opts.MetricsAddr, logger)
	}
	// onVerify feeds the verify-attempt histogram (#146/#153). The verified
	// bool is intentionally unused: the metric records the verify-loop
	// wall-clock regardless of outcome, which is what #153 needs to judge the
	// retry budget. Nil-safe through m.
	onVerify := func(elapsed time.Duration, _ bool) {
		m.ObserveVerifyAttempt(opts.Agent, elapsed.Seconds())
	}

	// Background retention sweep (#245). "infinite" (the default) disables
	// the goroutine entirely — zero behavior change for unconfigured deploys.
	// Single-writer invariant: the goroutine only deletes rows for opts.Agent,
	// matching the per-agent mailman ownership model.
	if opts.Retention != "" && opts.Retention != config.DefaultRetention {
		go runRetentionSweep(stopCtx, s, logger, opts.Agent, opts.Retention, opts.RetentionSweepInterval)
	}

	// #291 pane-not-found backoff state. consecutivePaneFails counts
	// back-to-back `can't find pane` aborts; paneFailMsgID is the message the
	// streak is accumulating against. The counter is per-agent in effect (a
	// missing pane fails every message identically), but we key it on the
	// claimed message id so an unrelated message reaching delivery resets the
	// streak — which also satisfies the AC's "per-message + per-agent" wording
	// with one mechanism. In-memory (not persisted): a restart re-registers
	// and re-probes from a clean slate, which is the right reset semantics.
	consecutivePaneFails := 0
	paneFailMsgID := ""

	for {
		if stopCtx.Err() != nil {
			return exitOK
		}
		if watchdogPing > 0 {
			_ = sdnotify.Watchdog()
		}

		// Metrics sampling (#146): count this loop iteration (a liveness +
		// cadence signal) and refresh the queue-depth gauge. Gated on m != nil
		// so the disabled path adds no per-iteration COUNT query.
		if m != nil {
			m.IncLoopIteration(opts.Agent)
			if depth, derr := s.RecipientQueueDepth(opCtx, opts.Agent); derr == nil {
				m.SetQueueDepth(opts.Agent, float64(depth))
			}
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

		// #291 stuck-state: a parked mailman stops probing tmux entirely.
		// This is the load-bearing property — once a persistent pane-not-found
		// failure has tripped the threshold, NO ClaimNext / observe-gate /
		// pre-paste probe runs (each of those shells out to tmux), so the
		// retry storm that wedged the tmux server cannot occur. Messages stay
		// queued (no loss). We re-read the agent row every iteration (above),
		// so a `register --force` that clears stuck_reason resumes delivery on
		// the next loop with a clean counter.
		if a.StuckReason != "" {
			if stopOrSleep(stopCtx, opts.StuckPollInterval) {
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

		// kind=ping: substrate-only reachability probe (#144). The mere
		// fact that this mailman claimed the row already proves the
		// daemon is alive and the recipient is registered (an
		// unregistered agent's GetAgent above would have `continue`d).
		// The remaining health signal is pane liveness. We transition
		// the row straight to delivered/failed and SKIP all delivery
		// machinery below (drift guard, observe-gate, pre-paste safety,
		// paste-and-Enter) — a ping MUST NOT mutate the recipient's pane
		// or load their context. That is the whole point of the kind.
		if msg.Kind == store.KindPing {
			handlePing(opCtx, s, logger, opts.Agent, a.PaneID, msg)
			if stopOrSleep(stopCtx, opts.InterMessageDelay) {
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
				// Drift abort is a terminal `failed` outcome (distinct from a
				// paste-unsafe pane-state abort, which reverts to queued); it
				// counts in messages_total, not paste_unsafe_aborts (#146).
				m.RecordDelivery(msg.FromAgent, opts.Agent, metrics.StateFailed)
				if stopOrSleep(stopCtx, opts.InterMessageDelay) {
					return exitOK
				}
				continue
			}
		}

		// Recipient-side dedupe (#157 PR2). Before entering the observe-gate
		// (which may hold for seconds), check whether a prior delivery attempt
		// for the same body from the same sender landed as delivered_in_input_box
		// within the configured window. If the original is now visible in pane
		// scrollback, confirm it (mark verified=1) and absorb this duplicate
		// (mark failed + notify sender). If not visible, deliver the replay.
		// DedupeWindow=0 disables the check entirely — zero behavior change.
		if opts.DedupeWindow > 0 {
			cutoff := time.Now().Add(-opts.DedupeWindow).UTC().Format(strandedTimeFormat)
			prior, merr := s.FindDedupeMatch(opCtx, msg.FromAgent, msg.ToAgent, msg.Body, cutoff)
			if merr != nil {
				logger.Printf("WARN dedupe_match_err id=%s err=%v — delivering anyway", msg.PublicID, merr)
			} else if prior != nil {
				verifyToken := "id " + prior.PublicID
				visible, verr := tmuxio.CheckTokenVisible(opCtx, paneForDelivery, verifyToken)
				if verr != nil {
					logger.Printf("WARN dedupe_reverify_err id=%s original=%s err=%v — delivering anyway",
						msg.PublicID, prior.PublicID, verr)
				} else if visible {
					if err := s.MarkVerifiedByDedupe(opCtx, prior.PublicID); err != nil {
						logger.Printf("WARN dedupe_mark_verified_err original=%s err=%v", prior.PublicID, err)
					}
					absorbReason := "dedupe_absorbed: original " + prior.PublicID +
						" confirmed via scrollback re-verify"
					if err := s.MarkFailed(opCtx, msg.PublicID, absorbReason); err != nil {
						logger.Printf("WARN dedupe_absorb_err id=%s err=%v", msg.PublicID, err)
					}
					logger.Printf("dedupe_absorbed id=%s original=%s", msg.PublicID, prior.PublicID)
					m.RecordDelivery(msg.FromAgent, opts.Agent, metrics.StateFailed)
					insertDedupeNotice(opCtx, s, logger, opts.Agent, msg, prior.PublicID)
					if stopOrSleep(stopCtx, opts.InterMessageDelay) {
						return exitOK
					}
					continue
				}
				// Original not visible in scrollback — deliver the replay normally.
				logger.Printf("dedupe_original_gone id=%s original=%s — delivering replay",
					msg.PublicID, prior.PublicID)
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

		// Pre-paste safety check (#105 Half 2). Even if the observe-gate
		// decided to flush (idle classification OR MaxWait-fired Stale=true),
		// take one more AgentState reading immediately before the paste.
		// If the pane is now paste-unsafe (StateAwaitingOperator or
		// StateUnknown), abort the delivery and revert the message back
		// to 'queued' for the next mailman cycle. Belt-and-suspenders
		// against the popup-as-Unknown failure mode the gate's
		// classification might have missed (#105 — the load-bearing case
		// is MaxWait firing with lastState=Unknown after the operator
		// drafted an AskUserQuestion popup that didn't match the
		// AwaitingOperatorMarker).
		//
		// Independent of GateDisabled — the safety check is a separate
		// concern from the observe-gate's pre-delivery wait. Tests that
		// don't fake AgentState set PrePasteSafetyDisabled=true to skip.
		if !opts.PrePasteSafetyDisabled {
			probeCtx, pcancel := context.WithTimeout(opCtx, 2*time.Second)
			probeState, _, perr := tmuxio.AgentState(probeCtx, paneForDelivery)
			pcancel()
			// Probe-failure is treated as paste-unsafe per IsPasteUnsafe's
			// "couldn't substantiate → can't paste safely" semantic
			// (Surveyor PR #134 S1). AgentState's error path already
			// returns StateUnknown so IsPasteUnsafe(probeState) would
			// catch it, but the explicit `perr != nil ||` keeps the
			// codification symmetric with the doc-comment and avoids
			// depending on AgentState's internal contract.
			if perr != nil || tmuxio.IsPasteUnsafe(probeState) {
				reason := probeState.String()
				if perr != nil {
					reason = fmt.Sprintf("probe-failed (%v)", perr)
				}
				logger.Printf("WARN pre_paste_safety_abort id=%s pane=%s state=%s — popup-suspected-or-probe-failed; reverting to queued for retry (#105)",
					msg.PublicID, paneForDelivery, reason)
				// #146: the message reverts to queued (not a terminal
				// outcome), so this counts in paste_unsafe_aborts only — never
				// messages_total.
				m.IncPasteUnsafeAbort(opts.Agent, pasteUnsafeReason(probeState, perr))
				// Single-flight assumption: the mailman loop processes
				// at most one message in 'delivering' state at a time
				// (ClaimNext is atomic + state-change-on-claim). So
				// RecoverDelivering reverts ONLY the current message
				// even though it operates batch-style on the agent's
				// rows. If future multi-flight delivery lands (#NNN),
				// this needs to become a per-publicID revert.
				if _, rerr := s.RecoverDelivering(opCtx, opts.Agent); rerr != nil {
					logger.Printf("WARN pre_paste_safety_recover_failed id=%s err=%v", msg.PublicID, rerr)
				}

				// #291: a `can't find pane` probe failure is the storm driver.
				// Distinguish it from a legitimate transient paste-unsafe abort
				// (operator drafting an AskUserQuestion popup, which the gate
				// already waits out on a 3–15s cadence): only the pane-not-found
				// shape accrues toward exponential backoff + parking. Everything
				// else keeps the bare #105 revert-and-retry and resets the streak.
				if perr != nil && isCantFindPaneError(perr) {
					if msg.PublicID != paneFailMsgID {
						paneFailMsgID = msg.PublicID
						consecutivePaneFails = 0
					}
					consecutivePaneFails++
					if opts.StuckThreshold > 0 && consecutivePaneFails >= opts.StuckThreshold {
						if serr := s.SetStuck(opCtx, opts.Agent, store.StuckReasonPaneNotFound); serr != nil {
							logger.Printf("WARN stuck_set_failed agent=%s err=%v", opts.Agent, serr)
						} else {
							logger.Printf("WARN stuck agent=%s reason=%s consecutive=%d — mailman parked; stops probing tmux until `register --force` clears it (#291)",
								opts.Agent, store.StuckReasonPaneNotFound, consecutivePaneFails)
						}
						// The DB stuck_reason now gates the loop; reset the
						// in-memory streak so a clear resumes from a clean slate.
						consecutivePaneFails = 0
						paneFailMsgID = ""
						continue
					}
					backoff := paneNotFoundBackoff(consecutivePaneFails)
					logger.Printf("WARN pane_not_found_backoff id=%s pane=%s consecutive=%d delay=%s — bounding retry rate (#291)",
						msg.PublicID, paneForDelivery, consecutivePaneFails, backoff)
					if stopOrSleep(stopCtx, backoff) {
						return exitOK
					}
					continue
				}
				// Non-pane-not-found abort: not a persistent pane failure, so
				// the consecutive-streak resets.
				consecutivePaneFails = 0
				paneFailMsgID = ""
				continue
			}
			// Probe passed: the pane is reachable, so any prior pane-not-found
			// streak is broken (#291 consecutive-count semantics).
			consecutivePaneFails = 0
			paneFailMsgID = ""
		}

		deliverCtx, cancel := context.WithTimeout(opCtx, opts.DeliverTimeout)
		derr := deliverOne(deliverCtx, paneForDelivery, msg, opts.ByteMarkerThreshold, onVerify)
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
					derr = deliverOne(retryCtx, newPane, msg, opts.ByteMarkerThreshold, onVerify)
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
			m.RecordDelivery(msg.FromAgent, opts.Agent, metrics.StateDelivered)
			if sec, ok := deliveryLatencySeconds(msg.CreatedAt); ok {
				m.ObserveDeliveryLatency(opts.Agent, sec)
			}
			if isCompactControl(msg) && opts.PostCompactPause > 0 {
				logger.Printf("post_compact_pause id=%s duration=%s",
					msg.PublicID, opts.PostCompactPause)
				if sleepRespectingWatchdog(stopCtx, opts.PostCompactPause, watchdogPing) {
					return exitOK
				}
			}
		case errors.Is(derr, tmuxio.ErrUnverifiedDelivery):
			logger.Printf("WARN delivered_in_input_box id=%s — paste+Enter completed but token not surfaced in time (Claude likely mid-turn); message is in recipient's input box pending submit",
				msg.PublicID)
			// Same DB state (delivered) as the verified branch, but verified=0
			// so the soft-fail is durable in the table — not just this WARN
			// journal line (#169). The line stays: healthscan still derives the
			// journal-sourced count, and #146/#147 now also read it from the DB.
			if err := s.MarkDeliveredInInputBox(opCtx, msg.PublicID); err != nil {
				logger.Printf("mark_delivered_err id=%s err=%v", msg.PublicID, err)
			}
			// delivered_in_input_box is still a `delivered` row, so it carries
			// a meaningful queued→delivered latency (#146).
			m.RecordDelivery(msg.FromAgent, opts.Agent, metrics.StateDeliveredInInputBox)
			if sec, ok := deliveryLatencySeconds(msg.CreatedAt); ok {
				m.ObserveDeliveryLatency(opts.Agent, sec)
			}
			maybeInsertFailureNotice(opCtx, s, logger,
				opts.NotifyOnDeliveredInInputBox, opts.Agent, msg,
				"delivered_in_input_box",
				"paste+Enter completed but verify token didn't surface in time")
		default:
			logger.Printf("deliver_failed id=%s err=%v", msg.PublicID, derr)
			if err := s.MarkFailed(opCtx, msg.PublicID, derr.Error()); err != nil {
				logger.Printf("mark_failed_err id=%s err=%v", msg.PublicID, err)
			}
			// Hard failure: a terminal `failed` row, no delivery latency.
			m.RecordDelivery(msg.FromAgent, opts.Agent, metrics.StateFailed)
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
// or "delivered_in_input_box") used in the notice body.
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
// The marker lines are built from the shared stranded* constants
// (stranded.go) so this renderer and parseStrandedBody can't drift. The
// recovery-hint line (#142) is emitted BEFORE the content marker so the
// trailing block stays exactly the cleared content for the parser.
func renderStrandedDraftBody(pane, triggerMsgID, content string) string {
	return strings.Join([]string{
		strandedHeaderLine,
		strandedPanePrefix + pane,
		strandedTriggerPrefix + triggerMsgID,
		"  Recover: tmux-msg-claude stranded list  →  tmux-msg-claude stranded show <id>",
		strandedContentMarker,
		indentForBody(content),
	}, "\n")
}

// indentForBody indents each line of s by two spaces so multi-line
// content nests visually inside a bullet-style body block. No
// trimming — we preserve the operator's original whitespace verbatim
// so they can recover the draft exactly as they typed it.
func indentForBody(s string) string {
	if s == "" {
		return strandedBodyIndent + strandedEmptyMarker
	}
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strandedBodyIndent)
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

// handlePing processes a kind=ping reachability probe (#144). It runs
// the recipient's substrate-health checks and transitions the message
// straight to delivered (healthy) or failed (unreachable) — it never
// pastes into the recipient's pane. Mailman-alive and agent-registered
// are proven by the fact that this mailman (serving a registered agent)
// claimed the row at all; the remaining signal is whether the
// registered pane is live.
func handlePing(ctx context.Context, s *store.Store, logger *log.Logger, agent, pane string, msg *store.Message) {
	reason, ok := pingHealthy(ctx, pane)
	if ok {
		logger.Printf("ping_ok id=%s agent=%s pane=%s from=%s", msg.PublicID, agent, pane, msg.FromAgent)
		if err := s.MarkDelivered(ctx, msg.PublicID); err != nil {
			logger.Printf("ping_mark_delivered_err id=%s err=%v", msg.PublicID, err)
		}
		return
	}
	logger.Printf("ping_failed id=%s agent=%s pane=%s reason=%q", msg.PublicID, agent, pane, reason)
	if err := s.MarkFailed(ctx, msg.PublicID, reason); err != nil {
		logger.Printf("ping_mark_failed_err id=%s err=%v", msg.PublicID, err)
	}
}

// pingHealthy reports whether the recipient's registered pane is live —
// the load-bearing substrate-health signal for a ping (#144). An empty
// pane id means the agent is registered but has no pane (operator should
// run `tmux-msg-claude discover`); a non-live pane means the agent's session
// is gone. Both are reachability failures. A LivePanes probe error is
// itself treated as a failure with the underlying reason surfaced: we
// can't substantiate reachability, so we don't claim it (sibling to the
// pre-paste-safety "couldn't substantiate → unsafe" stance).
func pingHealthy(ctx context.Context, pane string) (reason string, ok bool) {
	if pane == "" {
		return "agent registered but has no pane_id (run 'tmux-msg-claude discover')", false
	}
	live, err := tmuxio.LivePanes(ctx)
	if err != nil {
		return fmt.Sprintf("pane-liveness probe failed: %v", err), false
	}
	if !live[pane] {
		return fmt.Sprintf("registered pane %s is not live (agent unreachable)", pane), false
	}
	return "", true
}

// deliverOne dispatches a single message to a pane based on its Kind:
// regular messages go through the paste-buffer renderer with verification;
// control commands type their body directly via send-keys -l so they hit
// Claude Code's slash-command parser without the chat header.
//
// onVerify (may be nil) is forwarded to tmuxio.Deliver's verify-attempt
// callback (#146); it never fires for control messages (no verification).
func deliverOne(ctx context.Context, pane string, msg *store.Message, byteMarkerThreshold int, onVerify func(time.Duration, bool)) error {
	if msg.Kind == store.KindControl {
		return tmuxio.SendKeys(ctx, pane, msg.Body)
	}
	return tmuxio.Deliver(ctx, tmuxio.DeliverParams{
		Pane:        pane,
		Body:        render.Message(*msg, byteMarkerThreshold),
		VerifyToken: "id " + msg.PublicID,
		OnVerify:    onVerify,
	})
}

// deliveryLatencySeconds returns the queued→now duration in seconds for a
// message by parsing its created_at (ISO 8601 UTC — the schema's strftime
// format, with or without millis). ok is false when created_at can't be
// parsed (an unexpected format), so the caller skips the observation rather
// than recording a garbage latency. It uses time.Now() rather than
// re-reading the just-stamped delivered_at: the in-process clock is within a
// millisecond of the DB stamp and avoids a round-trip on the hot path.
func deliveryLatencySeconds(createdAt string) (float64, bool) {
	for _, layout := range []string{"2006-01-02T15:04:05.000Z", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, createdAt); err == nil {
			d := time.Since(t).Seconds()
			if d < 0 {
				d = 0
			}
			return d, true
		}
	}
	return 0, false
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

// insertDedupeNotice sends a positive resolution notice to the original sender
// when the dedupe path confirms the original and absorbs a replay (#157 PR2).
// Distinct from KindDeliveryFailureNotice — this is a success signal, not a
// failure. Loop prevention: if the absorbed message is itself a notice, skip
// (mirrors the same guard in maybeInsertFailureNotice).
func insertDedupeNotice(
	ctx context.Context,
	s *store.Store,
	logger *log.Logger,
	this string,
	replay *store.Message,
	originalID string,
) {
	if replay.Kind != store.KindMessage {
		return
	}
	body := fmt.Sprintf(":white_check_mark: Dedupe resolved\n  Replay id: %s\n  Original id: %s (now confirmed delivered via scrollback re-verify)\n  Replay dropped; no further action needed.",
		replay.PublicID, originalID)
	res, err := s.InsertNotice(ctx, store.InsertParams{
		FromAgent: this,
		ToAgent:   replay.FromAgent,
		Body:      body,
		Kind:      store.KindDedupeNotice,
	})
	if err != nil {
		logger.Printf("dedupe_notice_err replay_id=%s to=%s err=%v",
			replay.PublicID, replay.FromAgent, err)
		return
	}
	logger.Printf("dedupe_notice_inserted notice_id=%s replay_id=%s original_id=%s to=%s",
		res.PublicID, replay.PublicID, originalID, replay.FromAgent)
}

// runRetentionSweep is the background goroutine that periodically deletes
// delivered + failed rows older than the configured retention window (#245).
// It runs for opts.Agent's rows only (single-writer invariant). The goroutine
// exits when stopCtx is cancelled (SIGTERM / mailman shutdown).
//
// retention is a window spec accepted by parseWindow (e.g. "30d", "7d").
// interval is the sweep cadence; 0 falls back to DefaultRetentionSweepInterval.
func runRetentionSweep(stopCtx context.Context, s *store.Store, logger *log.Logger,
	agent, retention string, interval time.Duration,
) {
	if interval <= 0 {
		interval = config.DefaultRetentionSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	logger.Printf("retention_sweep_started agent=%s retention=%s interval=%s", agent, retention, interval)
	for {
		select {
		case <-stopCtx.Done():
			return
		case <-ticker.C:
			w, err := parseWindow(retention, time.Now())
			if err != nil {
				logger.Printf("WARN retention_parse_err retention=%q err=%v — skipping sweep", retention, err)
				continue
			}
			if w.All {
				// "all" would retain nothing — treat as a misconfiguration and skip.
				logger.Printf("WARN retention_sweep_skipped retention=%q resolves to All — use a duration like '30d'", retention)
				continue
			}
			cutoff := w.Since.UTC().Format(strandedTimeFormat)
			n, err := s.DeleteMessagesBefore(context.Background(), agent, cutoff,
				[]store.State{store.StateDelivered, store.StateFailed})
			if err != nil {
				logger.Printf("WARN retention_sweep_err retention=%q err=%v", retention, err)
				continue
			}
			if n > 0 {
				logger.Printf("retention_sweep_deleted agent=%s retention=%s cutoff=%s n=%d",
					agent, retention, cutoff, n)
			}
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
