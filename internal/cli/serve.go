package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/metrics"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/notify"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/render"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/sdnotify"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
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
	// defaultSessionStaleThreshold is the consecutive session-stale-abort count
	// at which the mailman parks itself (#783). Deliberately far LOWER than
	// defaultStuckThreshold: a pane-not-found probe fails fast, but a
	// session-stale iteration burns the full observe-gate MaxWait (~5min) before
	// the pre-paste-safety abort fires, so 10 would take ~50 minutes to park —
	// well past the ~22min TTL-drainage window observed in the field (#783). The
	// fast-path (see the session-stale exit condition in Run) replaces the ~5min
	// gate with a ~2s probe, so 3 parks in ~SessionStaleThreshold ×
	// sessionStaleRetryInterval ≈ 4-5min — well inside the drainage window — while
	// still tolerating a genuinely transient re-register race (a chamber
	// mid-/compact that re-registers within a couple of cycles never reaches 3).
	// A value <= 0 disables the transition (bare pre-#783 retry).
	defaultSessionStaleThreshold = 3
	// defaultMailmanStaleThreshold is how long a real deliverable may sit
	// queued/delivering to this chamber before the #719(A) freshness alert
	// flags it (edge-triggered notice to the configured conductor). Normal
	// delivery is sub-second (DeliverTimeout 5s); the anchor incident was ~5h
	// of silent stale queue, so a threshold of minutes converts multi-hour
	// silence into a prompt alert. 10m is generous enough never to fire on a
	// normal delivery lull or a short backoff — false positives are prevented
	// by the legitimate-hold STATE exclusions, not by threshold slack — while
	// tight enough that a real freeze surfaces fast. A value <= 0 disables the
	// alert. Emission is additionally gated on a configured --mailman-alert-to
	// target (empty = feature dormant), so the substrate default bakes in no
	// deployment-specific chamber name.
	defaultMailmanStaleThreshold = 10 * time.Minute
	// defaultStuckPollInterval is how often a parked mailman re-reads its
	// agent row to notice a `register --force` clear. No tmux probe — a
	// plain DB read — so a tight-ish cadence costs nothing.
	defaultStuckPollInterval = 5 * time.Second
	// defaultSpinGuardThreshold + defaultSpinGuardWindow bound the serve-loop
	// spin guard (#496). At the 2s default idle-poll (#550), a healthy idle loop
	// does ~5 no-progress iterations in 10s — over two orders of magnitude under
	// 1000 — so the guard only trips on a genuine spin (a path looping without
	// sleeping at thousands/sec). The threshold is interval-independent by
	// construction (spinGuard.record decides on count-within-window, not on the
	// poll cadence; TestSpinGuard_IdleLoopNeverTrips pins this), so #550's
	// idle-poll raise only widens the headroom — the threshold stays 1000.
	defaultSpinGuardThreshold = 1000
	defaultSpinGuardWindow    = 10 * time.Second
	// #448 provider-cap defaults. The cap counts same-provider chambers that
	// are StateWorking; the TTL is ~3× the observe interval so one missed
	// self-write doesn't drop a live chamber out of the count, while a crashed
	// mailman ages out within a few seconds.
	defaultMaxConcurrentPerProvider = 3
	defaultProviderCapTTL           = 6 * time.Second
	defaultProviderCapRecheck       = 1 * time.Second
	defaultObservedStateInterval    = 2 * time.Second
	// defaultPostDeliverCooldown is the #449 per-chamber post-delivery hold —
	// the operator tenet's ≥5s ingest window before the next paste.
	defaultPostDeliverCooldown = 5 * time.Second
	// stuckBackoffCap bounds the exponential pane-not-found retry delay
	// (#291). Even before the stuck threshold, no retry fires faster than
	// once per this interval — the 100/s storm that wedged tmux is capped
	// at 1/60s well before parking.
	stuckBackoffCap = 60 * time.Second
	// rateLimitBackoffCap bounds the exponential fallback when the regex
	// does not surface a parseable retry_seconds hint.
	rateLimitBackoffCap = 60 * time.Second
	// rateLimitMetricsTick refreshes the standing rate-limit gauges while the
	// mailman is sleeping in the defer window so scrapes see live values.
	rateLimitMetricsTick = 50 * time.Millisecond
	// usageLimitRecheckInterval is the fixed park-until-reset cadence for a
	// usage-limited chamber. Hard stops do not exponential-backoff; they park.
	usageLimitRecheckInterval = 30 * time.Second
	// usageLimitMetricsTick refreshes the standing usage-limit gauge while the
	// mailman is parked so scrapes see live values.
	usageLimitMetricsTick = 5 * time.Second
	// rateLimitResumeText is the input the mailman pastes into a rate-limited
	// chamber to resume its interrupted turn (#618) — the prompt the operator
	// types by hand today. StateRateLimited (#504) is a TRANSIENT throttle, so
	// re-issuing the turn once the cooldown elapses is the recovery; this is
	// deliberately NOT done for StateUsageLimited (#540, a hard quota park —
	// pasting "continue" there just re-hits the cap).
	rateLimitResumeText = "continue"
	// defaultRateLimitResumeMaxAttempts bounds the continue-pastes per rate-limit
	// episode. After this many pastes that fail to clear the rate-limit, the
	// mailman gives up and leaves the chamber for the operator rather than
	// spamming the pane against a stuck provider.
	defaultRateLimitResumeMaxAttempts = 5
)

// stuckBackoffBase is the unit for paneNotFoundBackoff's exponential schedule.
// Production: time.Second (1s, 2s, 4s, …, capped at stuckBackoffCap).
// Tests shrink it via setStuckBackoffBaseForTest to avoid real second-scale
// delays without losing the structural test of the backoff path (#299).
var stuckBackoffBase = time.Second

// setStuckBackoffBaseForTest replaces stuckBackoffBase for test isolation and
// returns the previous value so the caller can restore it.
func setStuckBackoffBaseForTest(d time.Duration) time.Duration {
	prev := stuckBackoffBase
	stuckBackoffBase = d
	return prev
}

// sessionStaleRetryInterval is the fixed pause between consecutive session-stale
// fast-path retries (#783). Because the fast-path replaces the ~5min observe-gate
// with a ~2s probe, the loop would otherwise spin; this paces it so the mailman
// parks in ~SessionStaleThreshold × this interval (~4-5min at the default 3) —
// inside the tolerable invisible-stuck window, yet slow enough that a chamber
// re-registering mid-/compact heals before the streak reaches the threshold.
// Fixed (not exponential): the wait-for-re-register condition is human-scale +
// roughly constant, unlike the pane-not-found storm the #291 exponential backoff
// bounds. A var (not const) so tests can shrink it off the second scale.
var sessionStaleRetryInterval = 90 * time.Second

// setSessionStaleRetryIntervalForTest replaces sessionStaleRetryInterval for
// test isolation and returns the previous value so the caller can restore it.
func setSessionStaleRetryIntervalForTest(d time.Duration) time.Duration {
	prev := sessionStaleRetryInterval
	sessionStaleRetryInterval = d
	return prev
}

// freshnessCheckInterval throttles the #719(A) freshness sweep — the cadence at
// which the mailman runs its cheap MIN(created_at) queue-age query. This is the
// detection LATENCY past --mailman-stale-threshold, not the threshold itself:
// on a 10m threshold a 30s sweep flags a freeze within ~30s of it crossing. The
// (more expensive) pane-state probe fires only when a queue is already found
// stale, so the steady-state cost of this sweep is a single DB read. A var (not
// const) so tests can shrink it off the second scale, mirroring
// sessionStaleRetryInterval.
var freshnessCheckInterval = 30 * time.Second

// setFreshnessCheckIntervalForTest replaces freshnessCheckInterval for test
// isolation and returns the previous value so the caller can restore it.
func setFreshnessCheckIntervalForTest(d time.Duration) time.Duration {
	prev := freshnessCheckInterval
	freshnessCheckInterval = d
	return prev
}

// paneNotFoundBackoff returns the delay before the next delivery attempt
// after `consecutive` back-to-back `can't find pane` probe failures (#291):
// base, 2×base, 4×base, …, capped at stuckBackoffCap (60s). The first
// failure already waits base (1s in production), which converts the pre-fix
// ~100/s retry storm into at most 1/s — the cap drops it to 1/60s.
func paneNotFoundBackoff(consecutive int) time.Duration {
	if consecutive < 1 {
		consecutive = 1
	}
	shift := uint(consecutive - 1)
	if shift >= 63 {
		// shift ≥ 63: int64 left-shift by 63 overflows to negative; by ≥ 64
		// wraps to 0 in Go. Both are wrong — clamp to cap immediately.
		return stuckBackoffCap
	}
	d := stuckBackoffBase << shift
	if d < 0 || d > stuckBackoffCap {
		return stuckBackoffCap
	}
	return d
}

// rateLimitBackoff returns the delay before the next retry after a
// rate-limited observation when the banner did not expose a parseable
// retry_seconds hint.
func rateLimitBackoff(consecutive int) time.Duration {
	if consecutive < 1 {
		consecutive = 1
	}
	shift := uint(consecutive - 1)
	if shift >= 63 {
		return rateLimitBackoffCap
	}
	d := time.Second << shift
	if d < 0 || d > rateLimitBackoffCap {
		return rateLimitBackoffCap
	}
	return d
}

// #543 Layer-3 priority-biased wake-jitter windows, expressed as a fraction of
// the base rate-limit backoff. A NARROWER window means the chamber wakes nearer
// the backoff floor (sooner); a wider window spreads it later. Higher-priority
// chambers get the narrow window so they cluster just past the floor and wake
// first; lower-priority chambers spread furthest. The jitter is additive and
// non-negative — a chamber never wakes EARLIER than the provider's backoff floor.
const (
	rateLimitJitterFracHigh   = 0.25
	rateLimitJitterFracNormal = 0.5
	rateLimitJitterFracLow    = 1.0
)

// rateLimitJitterSource yields a uniform sample in [0.0, 1.0) for the #543 wake
// jitter. A package var so tests can pin it deterministically (the wake-stagger
// test injects a stepping source; a mutation pins the collision the jitter
// prevents). Production uses math/rand/v2's per-process auto-seeded source, so
// independent mailman processes draw independent sequences — that cross-process
// independence is what actually desynchronises simultaneously-limited chambers.
var rateLimitJitterSource = rand.Float64

// setRateLimitJitterSourceForTest swaps the jitter source and returns the
// previous value so the caller can restore it.
func setRateLimitJitterSourceForTest(f func() float64) func() float64 {
	prev := rateLimitJitterSource
	rateLimitJitterSource = f
	return prev
}

// jitterFractionForPriority maps a #449 priority weight to its wake-jitter
// window fraction. Monotone in priority: at-or-above High gets the narrowest
// window, at-or-below Low the widest, the normal band in between. The >=/<=
// bounds (not ==) keep a future tier (e.g. an "urgent" weight above High)
// mapping to the tightest window rather than silently falling to the default.
func jitterFractionForPriority(priority int) float64 {
	switch {
	case priority >= store.PriorityHigh:
		return rateLimitJitterFracHigh
	case priority <= store.PriorityLow:
		return rateLimitJitterFracLow
	default:
		return rateLimitJitterFracNormal
	}
}

// rateLimitWakeJitter returns a non-negative jitter to add to the base
// rate-limit backoff, drawn uniformly from [0, frac(priority)*base]. Additive
// and non-negative by construction: jitter only spreads wakes later, never
// earlier than the provider's backoff floor.
func rateLimitWakeJitter(base time.Duration, priority int) time.Duration {
	if base <= 0 {
		return 0
	}
	frac := jitterFractionForPriority(priority)
	return time.Duration(rateLimitJitterSource() * frac * float64(base))
}

// rateLimitWakeDelay computes the #543 Layer-3 wake delay for a rate-limited
// chamber: the base backoff (the provider's Retry-After hint or the exponential
// fallback) plus a priority-biased jitter that desynchronises simultaneous wakes
// and orders chambers by priority, plus a cap-aware extension when the provider
// is already saturated by other working chambers — so the chamber does not wake
// straight into a #448 cap-defer spin. providerCap <= 0 disables the cap-aware
// read (the cap gate is off, or this agent has no provider). The #448 cap gate
// remains the authoritative admission backstop; this layer only reduces the
// thundering-herd pressure on it.
func rateLimitWakeDelay(ctx context.Context, s *store.Store, base time.Duration, priority int, provider string, providerCap int, ttl, recheck time.Duration) time.Duration {
	backoff := base + rateLimitWakeJitter(base, priority)
	if providerCap > 0 {
		if working, err := s.CountWorkingOnProvider(ctx, provider, ttl, time.Now()); err == nil && working >= providerCap {
			backoff += recheck
		}
	}
	return backoff
}

// stopOrSleepWithUpdates waits for d or until stopCtx is cancelled, invoking
// onTick immediately and then at least every tick while the wait is active.
// It returns true when the caller should exit because stopCtx was cancelled.
func stopOrSleepWithUpdates(stopCtx context.Context, d, tick time.Duration, onTick func(time.Time)) bool {
	if d <= 0 {
		onTick(time.Now())
		return stopCtx.Err() != nil
	}
	if tick <= 0 {
		tick = d
	}
	deadline := time.Now().Add(d)
	for {
		now := time.Now()
		onTick(now)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		wait := tick
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-stopCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return true
		case <-timer.C:
		}
	}
}

// rateLimitResumeState tracks one #618 auto-resume episode across the
// throttled self-observe iterations. The zero value means "no active episode."
// It lives as a loop-local in runServeWithStore's mailman loop (single-threaded
// per agent, so no locking), mirroring the rateLimitedMsgID family the delivery
// path keeps for its own rate-limit bookkeeping.
type rateLimitResumeState struct {
	active      bool      // currently inside a rate-limit episode
	since       time.Time // first self-observed rate-limited (for logging)
	attempts    int       // continue-pastes fired this episode
	nextPasteAt time.Time // earliest wall-clock the next continue-paste may fire
	gaveUp      bool      // bounded ceiling hit; stop pasting, surfaced once
}

// rateLimitResumeAction is the decision planRateLimitResume returns for one
// self-observe observation. The wiring layer turns it into the actual paste /
// log / metric side effects.
type rateLimitResumeAction int

const (
	resumeNoop      rateLimitResumeAction = iota // nothing to do this observation
	resumeWait                                   // in an episode, backoff not yet elapsed
	resumePaste                                  // paste rateLimitResumeText now
	resumeGiveUp                                 // bounded ceiling reached — surface once
	resumeRecovered                              // episode ended (state left rate-limited)
)

// planRateLimitResume is the PURE decision core for #618 auto-resume (no I/O):
// given the current observed state + its evidence, the running episode state,
// the wall clock, the bounded-retry ceiling, and a backoff closure, it mutates
// rs and returns the action the caller should take. Extracted as a pure
// function (mirroring #621's maybeAutoClearMetabolism split) so the whole
// state machine is exhaustively table-testable without a live tmux pane.
//
// Only StateRateLimited drives the machine. StateUsageLimited is deliberately
// NOT handled here — it is a hard quota park (#540), not a transient throttle,
// so pasting "continue" cannot help and is left to the delivery path's park.
//
// backoff(attempt, hint) returns how long to wait before the next paste:
// attempt is the upcoming attempt index (1 for the first wait), hint is the
// banner-parsed Evidence.RetryAfter (zero when the regex exposed none). The
// first wait fires BEFORE any paste — the rate-limit must be given its cooldown
// before a retry, so the chamber is never pasted into an un-elapsed throttle.
func planRateLimitResume(observed tmuxio.State, ev tmuxio.Evidence, rs *rateLimitResumeState, now time.Time, maxAttempts int, backoff func(attempt int, hint time.Duration) time.Duration) rateLimitResumeAction {
	if observed != tmuxio.StateRateLimited {
		if rs.active {
			*rs = rateLimitResumeState{}
			return resumeRecovered
		}
		return resumeNoop
	}
	// observed == StateRateLimited.
	if !rs.active {
		rs.active = true
		rs.since = now
		rs.attempts = 0
		rs.gaveUp = false
		// Wait out the first cooldown before any paste — never paste into an
		// un-elapsed throttle.
		rs.nextPasteAt = now.Add(backoff(1, ev.RetryAfter))
		return resumeWait
	}
	if rs.gaveUp {
		return resumeNoop
	}
	if now.Before(rs.nextPasteAt) {
		return resumeWait
	}
	if rs.attempts >= maxAttempts {
		rs.gaveUp = true
		return resumeGiveUp
	}
	rs.attempts++
	// Escalate the next wait (attempts+1) so a paste that fails to clear the
	// throttle backs off further; a fresh banner hint still takes precedence.
	rs.nextPasteAt = now.Add(backoff(rs.attempts+1, ev.RetryAfter))
	return resumePaste
}

// maybeAutoResumeRateLimited runs planRateLimitResume and applies its side
// effects: pasting rateLimitResumeText into the chamber's own pane to resume an
// interrupted turn after a transient rate-limit, plus the structured logs and
// #618 metric. paste is injected (tmuxio.SendKeys in production, a recording
// stub in tests) so the planner+wiring are testable without real tmux. Called
// from the #448 self-observe block on its throttled cadence, so the probe loop
// itself is the verify-and-retry loop — a subsequent observation that finds the
// chamber no longer rate-limited yields resumeRecovered.
func maybeAutoResumeRateLimited(
	ctx context.Context,
	logger *log.Logger,
	m *metrics.Metrics,
	agent, provider, pane string,
	observed tmuxio.State,
	ev tmuxio.Evidence,
	rs *rateLimitResumeState,
	now time.Time,
	maxAttempts int,
	backoff func(attempt int, hint time.Duration) time.Duration,
	paste func(ctx context.Context, pane, text string) error,
) {
	switch planRateLimitResume(observed, ev, rs, now, maxAttempts, backoff) {
	case resumePaste:
		if err := paste(ctx, pane, rateLimitResumeText); err != nil {
			// Don't advance state on a failed paste beyond the attempt count
			// the planner already bumped — the next eligible cadence retries.
			logger.Printf("WARN rate_limit_resume_paste_failed agent=%s pane=%s attempt=%d err=%v (#618)",
				agent, pane, rs.attempts, err)
			return
		}
		logger.Printf("rate_limit_resume agent=%s provider=%s pane=%s attempt=%d — pasted %q to resume after rate-limit (#618)",
			agent, provider, pane, rs.attempts, rateLimitResumeText)
		m.IncRateLimitResume(agent, provider, "attempt")
	case resumeRecovered:
		logger.Printf("rate_limit_resume_recovered agent=%s provider=%s — chamber left rate-limited (#618)",
			agent, provider)
		m.IncRateLimitResume(agent, provider, "recovered")
	case resumeGiveUp:
		logger.Printf("WARN rate_limit_resume_gave_up agent=%s provider=%s attempts=%d — still rate-limited after bounded continue-pastes; leaving for operator (#618)",
			agent, provider, maxAttempts)
		m.IncRateLimitResume(agent, provider, "gave_up")
	}
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
	// SessionStaleThreshold is the number of consecutive session-stale aborts
	// (registered session-id resolves to no live pane → name-resolved pane
	// classifies StateUnknown → #105 pre-paste-safety refuses) after which the
	// mailman parks itself with store.StuckReasonSessionStale (#783). Default
	// defaultSessionStaleThreshold (3) — lower than StuckThreshold because each
	// session-stale iteration is ~5min (full gate MaxWait), not fast. A value
	// <= 0 disables the transition (the loop keeps the bare #105 retry, i.e. the
	// pre-#783 behavior), kept as an escape hatch.
	SessionStaleThreshold int
	// MailmanStaleThreshold is how long a real deliverable (message/control)
	// may sit queued/delivering to this chamber before the #719(A) freshness
	// alert edge-fires a KindStuckChamberNotice to AlertTo. Default
	// defaultMailmanStaleThreshold (10m). A value <= 0 disables the alert.
	// Detection is self-observed (this mailman watches its OWN inbound queue),
	// so it catches the wedge signatures where delivery stops advancing
	// (revert-loop, stuck-delivering, a live-but-Unknown pane) — NOT the
	// false-idle-consumed shape where a modal eats pastes and delivery falsely
	// advances (that is the classifier's job, #719(B)), and NOT a fully dead
	// mailman (which cannot self-report — a distinct observer follow-up).
	MailmanStaleThreshold time.Duration
	// AlertTo is the conductor agent that receives #719(A) freshness alerts.
	// Empty (the substrate default) leaves the alert DORMANT so no
	// deployment-specific chamber name is baked into the substrate — a
	// deployment activates it via `mailman-alert-to = "<conductor>"` in
	// config.toml. A value equal to this mailman's own Agent is ignored (a
	// chamber cannot usefully alert itself about its own wedge: the notice
	// would queue into the same wedged inbox).
	AlertTo string
	// StuckPollInterval is how often a parked (stuck) mailman re-reads its
	// own agent row to notice a `register --force` clear. While stuck the
	// mailman issues NO tmux probes — this is a pure DB read on a slow
	// cadence. Default defaultStuckPollInterval (5s).
	StuckPollInterval time.Duration
	// SpinGuardThreshold is the max consecutive no-progress serve-loop
	// iterations allowed within SpinGuardWindow before the loop is judged to
	// be spinning and panics (#496 — "steady-state serve-loop is bounded").
	// Default defaultSpinGuardThreshold (1000). A value <= 0 disables the
	// guard (escape hatch, mirrors StuckThreshold).
	SpinGuardThreshold int
	// SpinGuardWindow is the sliding window for SpinGuardThreshold. A healthy
	// idle loop sleeps between iterations so it never packs that many
	// no-progress iterations into the window. Default defaultSpinGuardWindow
	// (10s).
	SpinGuardWindow time.Duration
	// MaxConcurrentPerProvider caps how many same-provider chambers may be
	// StateWorking before this mailman defers delivery (#448). Default
	// defaultMaxConcurrentPerProvider (3); 0 = unbounded (gate off).
	MaxConcurrentPerProvider int
	// ProviderCapTTL is the freshness window for a peer's observed "working":
	// a crashed mailman's stale state older than this is not counted, so it
	// can't pin a slot forever. Default defaultProviderCapTTL (6s, ~3× the
	// observe interval).
	ProviderCapTTL time.Duration
	// ProviderCapRecheckInterval is how often a cap-deferred mailman re-checks
	// for an open slot. Default defaultProviderCapRecheck (1s).
	ProviderCapRecheckInterval time.Duration
	// ObservedStateInterval throttles the per-mailman self-probe that writes
	// agents.observed_state for the cross-mailman cap count — bounding the extra
	// capture-pane probes the idle path now does. Default
	// defaultObservedStateInterval (2s).
	ObservedStateInterval time.Duration
	// ProviderCapDisabled is a test seam: when true the mailman skips provider
	// accounting entirely (no SetProvider, no self-probe, no cap gate). Tests
	// that don't fake tmuxio.AgentState set it (mirrors GateDisabled). Default
	// false (cap on in production).
	ProviderCapDisabled bool
	// PriorityStrategy selects the #449 cross-channel scheduler: StrategyMaxPriority
	// (default — uniform priority reduces to FIFO) or StrategyAged. Threaded into
	// ClaimNextWithStrategy.
	PriorityStrategy store.SchedulerStrategy
	// PostDeliverCooldown is the per-chamber hold after a successful delivery
	// before the next paste (#449 operator tenet) — gives the recipient an
	// ingest window so a burst of queued messages doesn't flood + collide during
	// the chamber-read. Default defaultPostDeliverCooldown (5s); 0 disables (the
	// inter-message-delay still applies).
	PostDeliverCooldown time.Duration
	// RateLimitResumeDisabled turns off the #618 auto-resume action: when the
	// self-observe probe sees the chamber rate-limited, the mailman normally
	// waits out the cooldown then pastes `continue` to resume the interrupted
	// turn. Default false (auto-resume ON) — neither Claude Code nor codex
	// auto-resumes natively today, so this doesn't fight upstream (AC4). Set true
	// as the escape hatch if an adapter later adds native rate-limit recovery.
	RateLimitResumeDisabled bool
	// RateLimitResumeMaxAttempts bounds the continue-pastes per rate-limit
	// episode before the mailman gives up and leaves the chamber for the
	// operator. Default defaultRateLimitResumeMaxAttempts (5). <=0 falls back to
	// the default (a zero ceiling would disable resumption silently — use
	// RateLimitResumeDisabled to turn it off explicitly).
	RateLimitResumeMaxAttempts int
}

// runServeCLI parses serve-subcommand flags, sets up signal handling, and
// drives the mailman loop.
//
// Usage: tmux-tell-claude serve --agent NAME [tuning flags]
func runServeCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	agent := fs.String("agent", "", "agent name to serve (required)")
	interMsg := fs.Duration("inter-message-delay", 200*time.Millisecond,
		"pause between successive deliveries")
	idlePoll := fs.Duration("idle-poll", 2*time.Second,
		"queue-empty sleep before re-polling; the #515 doorbell carries delivery latency sub-second, so this is the slow correctness fallback (raised 250ms→2s in #550, matching the 2s self-observe throttle so the #448 provider-cap freshness margin is unchanged)")
	pausePoll := fs.Duration("pause-poll", time.Second,
		"interval to re-check the paused flag")
	deliverTimeout := fs.Duration("deliver-timeout", 30*time.Second,
		"per-message deadline for the tmux delivery sequence")
	settleDelay := fs.Duration("settle-delay", tmuxio.DefaultSettleDelay,
		"pause between paste-buffer and the submit Enter, giving the TUI time to ingest a (possibly collapsed/chunked) paste before it is asked to submit. 500ms suits Claude; codex collapses >~1KB pastes into `[Pasted Content]` chunks that need longer to ingest, so a codex mailman may need a larger value (e.g. 2s) or the submit-Enter is eaten and the paste sits unsubmitted (#360). Per-agent TOML knob: `settle-delay = \"2s\"`.")
	verifyRetryBudget := fs.Duration("verify-retry-budget", tmuxio.DefaultRetryBudget,
		"total verify-token retry window for post-paste verification (#153). The default ~5s schedule (100ms/250ms/500ms/1s/1.5s/1.65s across 7 capture attempts) scales proportionally to this budget — e.g. 10s doubles each delay, 15s triples. Per-agent TOML knob: `verify-retry-budget = \"15s\"` for large-payload hubs. Inspect with #146's tmux_tell_delivery_verify_attempt_seconds histogram before tuning.")
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
		"disable the operator-typing 📫 visibility notification (#95). Default false (notification on). When the observe-gate first detects the operator is typing, the mailman injects a single 📫 character into their input row as a one-shot signal that a message is pending. Operator can Backspace it (gate keeps waiting) or let it ride along with their next message.")
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
	sessionStaleThreshold := fs.Int("session-stale-threshold", defaultSessionStaleThreshold,
		"number of consecutive session-stale aborts (registered session-id resolves to no live pane → name-resolved pane classifies unknown → #105 pre-paste-safety refuses) before the mailman parks itself with reason `session-stale` (#783). Lower than --stuck-threshold because each iteration burns the full ~5min gate MaxWait. Parked mailman is visible in `agents` and cleared via `register --force` (the same re-register that refreshes the stale session-id). 0 disables the transition (bare #105 retry). Per-agent TOML knob: `session-stale-threshold = N`.")
	mailmanStaleThreshold := fs.Duration("mailman-stale-threshold", defaultMailmanStaleThreshold,
		"#719(A) freshness alert: how long a real deliverable (message/control) may sit queued/delivering to this chamber before the mailman edge-fires a `stuck_chamber_notice` to --mailman-alert-to. Normal delivery is sub-second; the anchor freeze was ~5h of silent stale queue. False positives are prevented by legitimate-hold STATE exclusions (rate/usage-limit, awaiting-operator, compaction-rest, copy-mode), not threshold slack. 0 disables the alert. Emission also requires --mailman-alert-to to be set. Per-agent TOML knob: `mailman-stale-threshold = \"10m\"`.")
	mailmanAlertTo := fs.String("mailman-alert-to", "",
		"#719(A) the conductor agent that receives freshness alerts. Empty (the default) leaves the alert DORMANT so no deployment-specific chamber name is baked into the substrate — set it (e.g. `mailman-alert-to = \"bosun\"` in config.toml) to activate. A value equal to this mailman's own agent is ignored (a chamber can't usefully alert itself about its own wedge). Per-agent TOML knob: `mailman-alert-to = \"bosun\"`.")
	stuckPollInterval := fs.Duration("stuck-poll-interval", defaultStuckPollInterval,
		"how often a parked (stuck) mailman re-reads its agent row to notice a `register --force` clear. While stuck the mailman issues NO tmux probes — this is a pure DB read. Per-agent TOML knob: `stuck-poll-interval = \"5s\"`.")
	spinGuardThreshold := fs.Int("spin-guard-threshold", defaultSpinGuardThreshold,
		"max consecutive no-progress serve-loop iterations within --spin-guard-window before the loop is judged spinning and panics (#496 — fails loud for systemd restart rather than burning CPU). A healthy idle loop sleeps between iterations so it never approaches this. 0 disables the guard. Per-agent TOML knob: `spin-guard-threshold = N`.")
	spinGuardWindow := fs.Duration("spin-guard-window", defaultSpinGuardWindow,
		"sliding window for --spin-guard-threshold. Per-agent TOML knob: `spin-guard-window = \"10s\"`.")
	maxConcurrentPerProvider := fs.Int("max-concurrent-per-provider", defaultMaxConcurrentPerProvider,
		"#448 provider-cap: defer delivery while this many same-provider chambers are already StateWorking (prevents burst saturation of the shared provider pool). 0 = unbounded (gate off). Cross-mailman count via the shared DB. Per-agent TOML knob: `max-concurrent-per-provider = N`.")
	providerCapTTL := fs.Duration("provider-cap-ttl", defaultProviderCapTTL,
		"#448 freshness window for a peer's observed working-state — a crashed mailman's stale state older than this stops counting (so it can't pin a slot). Per-agent TOML knob: `provider-cap-ttl = \"6s\"`.")
	providerCapRecheck := fs.Duration("provider-cap-recheck-interval", defaultProviderCapRecheck,
		"#448 how often a cap-deferred mailman re-checks for an open slot. Per-agent TOML knob: `provider-cap-recheck-interval = \"1s\"`.")
	observedStateInterval := fs.Duration("observed-state-interval", defaultObservedStateInterval,
		"#448 throttle for the per-mailman self-probe that publishes agents.observed_state for the cross-mailman cap count. Per-agent TOML knob: `observed-state-interval = \"2s\"`.")
	priorityStrategy := fs.String("priority-strategy", "max",
		"#449 cross-channel scheduler: `max` (default — weight a sender-channel by its top priority; uniform priority → plain FIFO) or `aged` (also depth-ages within same-priority channels, favoring the longest backlog under uniform priority). Per-agent TOML knob: `priority-strategy = \"max\"`.")
	postDeliverCooldown := fs.Duration("post-deliver-cooldown", defaultPostDeliverCooldown,
		"#449 per-chamber hold after a successful delivery before the next paste — an ingest window so a burst doesn't flood + collide during chamber-read. 0 disables (inter-message-delay still applies). Per-agent TOML knob: `post-deliver-cooldown = \"5s\"`.")
	rateLimitResumeDisabled := fs.Bool("rate-limit-resume-disabled", false,
		"#618 disable the auto-resume action: by default the self-observe probe waits out a rate-limit cooldown then pastes `continue` to resume the chamber's interrupted turn. Set true as the escape hatch if an adapter adds native rate-limit recovery (don't-fight-upstream). Per-agent TOML knob: `rate-limit-resume-disabled = true`.")
	rateLimitResumeMaxAttempts := fs.Int("rate-limit-resume-max-attempts", defaultRateLimitResumeMaxAttempts,
		"#618 bound on continue-pastes per rate-limit episode before the mailman gives up and leaves the chamber for the operator. <=0 falls back to the default (use --rate-limit-resume-disabled to turn resumption off). Per-agent TOML knob: `rate-limit-resume-max-attempts = N`.")
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
	if !flagWasSet(fs, "session-stale-threshold") {
		*sessionStaleThreshold = config.ResolveInt(cfg, *agent, "session-stale-threshold", *sessionStaleThreshold)
	}
	if !flagWasSet(fs, "mailman-stale-threshold") {
		*mailmanStaleThreshold = config.ResolveDuration(cfg, *agent, "mailman-stale-threshold", *mailmanStaleThreshold)
	}
	if !flagWasSet(fs, "mailman-alert-to") {
		*mailmanAlertTo = config.ResolveString(cfg, *agent, "mailman-alert-to", *mailmanAlertTo)
	}
	if !flagWasSet(fs, "stuck-poll-interval") {
		*stuckPollInterval = config.ResolveDuration(cfg, *agent, "stuck-poll-interval", *stuckPollInterval)
	}
	if !flagWasSet(fs, "spin-guard-threshold") {
		*spinGuardThreshold = config.ResolveInt(cfg, *agent, "spin-guard-threshold", *spinGuardThreshold)
	}
	if !flagWasSet(fs, "spin-guard-window") {
		*spinGuardWindow = config.ResolveDuration(cfg, *agent, "spin-guard-window", *spinGuardWindow)
	}
	if !flagWasSet(fs, "max-concurrent-per-provider") {
		*maxConcurrentPerProvider = config.ResolveInt(cfg, *agent, "max-concurrent-per-provider", *maxConcurrentPerProvider)
	}
	if !flagWasSet(fs, "provider-cap-ttl") {
		*providerCapTTL = config.ResolveDuration(cfg, *agent, "provider-cap-ttl", *providerCapTTL)
	}
	if !flagWasSet(fs, "provider-cap-recheck-interval") {
		*providerCapRecheck = config.ResolveDuration(cfg, *agent, "provider-cap-recheck-interval", *providerCapRecheck)
	}
	if !flagWasSet(fs, "observed-state-interval") {
		*observedStateInterval = config.ResolveDuration(cfg, *agent, "observed-state-interval", *observedStateInterval)
	}
	if !flagWasSet(fs, "priority-strategy") {
		*priorityStrategy = config.ResolveString(cfg, *agent, "priority-strategy", *priorityStrategy)
	}
	if !flagWasSet(fs, "post-deliver-cooldown") {
		*postDeliverCooldown = config.ResolveDuration(cfg, *agent, "post-deliver-cooldown", *postDeliverCooldown)
	}
	if !flagWasSet(fs, "rate-limit-resume-disabled") {
		*rateLimitResumeDisabled = config.ResolveBool(cfg, *agent, "rate-limit-resume-disabled", *rateLimitResumeDisabled)
	}
	if !flagWasSet(fs, "rate-limit-resume-max-attempts") {
		*rateLimitResumeMaxAttempts = config.ResolveInt(cfg, *agent, "rate-limit-resume-max-attempts", *rateLimitResumeMaxAttempts)
	}
	// Parse the #449 strategy after config resolution so a TOML override is
	// honored; a bad value fails loud (usage error) rather than silently
	// scheduling FIFO.
	strategy, serr := store.ParseStrategy(*priorityStrategy)
	if serr != nil {
		fmt.Fprintf(stderr, "%v\n", serr)
		return exitUsage
	}
	rateLimitPattern := config.ResolveString(cfg, *agent, "rate-limit-pattern", "")
	if rateLimitPattern != "" {
		if _, perr := regexp.Compile(rateLimitPattern); perr != nil {
			fmt.Fprintf(stderr, "ERROR config: rate-limit-pattern %q: %v\n", rateLimitPattern, perr)
			return exitUsage
		}
	}
	usageLimitPattern := config.ResolveString(cfg, *agent, "usage-limit-pattern", "")
	if usageLimitPattern != "" {
		if _, perr := regexp.Compile(usageLimitPattern); perr != nil {
			fmt.Fprintf(stderr, "ERROR config: usage-limit-pattern %q: %v\n", usageLimitPattern, perr)
			return exitUsage
		}
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
	current := tmuxio.ActivePaneProfile()
	current.RateLimitPattern = rateLimitPattern
	current.UsageLimitPattern = usageLimitPattern
	tmuxio.SetActivePaneProfile(current)
	// Resolve the paste→Enter settle delay (#360), same precedence chain as
	// the verify-retry budget: TOML per-agent value unless the CLI flag was
	// set; a malformed value WARNs and keeps the flag value (default 500ms).
	if !flagWasSet(fs, "settle-delay") {
		if raw := config.ResolveString(cfg, *agent, "settle-delay", ""); raw != "" {
			if d, perr := time.ParseDuration(raw); perr != nil {
				fmt.Fprintf(stderr, "WARN config: settle-delay %q: %v — using %v\n",
					raw, perr, *settleDelay)
			} else {
				*settleDelay = d
			}
		}
	}
	// Install the paste→Enter settle pause (#360). Same process-global-set-once
	// shape as the retry schedule above: applies the per-agent settle-delay to
	// the right scope without per-call plumbing through DeliverParams.
	tmuxio.SetSettleDelay(*settleDelay)
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

	resolvedDB := resolveDBPath(*dbPath)
	fmt.Fprintf(stderr, "serve: claude_msg_db=%s source=%s\n", resolvedDB, dbPathSource(*dbPath))
	s, err := store.Open(resolvedDB)
	if err != nil {
		fmt.Fprintf(stderr, "open store: %v\n", err)
		return exitInternal
	}
	defer func() { _ = s.Close() }()

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
		SessionStaleThreshold:       *sessionStaleThreshold,
		MailmanStaleThreshold:       *mailmanStaleThreshold,
		AlertTo:                     *mailmanAlertTo,
		StuckPollInterval:           *stuckPollInterval,
		SpinGuardThreshold:          *spinGuardThreshold,
		SpinGuardWindow:             *spinGuardWindow,
		MaxConcurrentPerProvider:    *maxConcurrentPerProvider,
		ProviderCapTTL:              *providerCapTTL,
		ProviderCapRecheckInterval:  *providerCapRecheck,
		ObservedStateInterval:       *observedStateInterval,
		PriorityStrategy:            strategy,
		PostDeliverCooldown:         *postDeliverCooldown,
		RateLimitResumeDisabled:     *rateLimitResumeDisabled,
		RateLimitResumeMaxAttempts:  *rateLimitResumeMaxAttempts,
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

	// Startup: agent must be registered with a pane_id. Both "no DB row" and
	// "row exists but pane_id is empty" are substrate-PERMANENT for THIS unit
	// instance: a restart won't recover them — only an operator-side `register`
	// or `discover` invocation will. Exit cleanly (#340) so systemd records
	// Result=success and STOPS restarting (Restart=on-failure ignores exit 0),
	// matching the precedent below at the mailbox-only / hook-context /
	// paste-incapable short-circuits. The pre-#340 behavior returned 69
	// (UNAVAILABLE) and tight-restarted, which under enough orphan units
	// hammered the SQLite DB into the recurring-freeze pattern that caused
	// alcatraz-infra#39.
	a, err := s.GetAgent(opCtx, opts.Agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "agent %q not registered in DB — exiting cleanly "+
				"to avoid systemd restart-loop (#340). Re-register with '%s register "+
				"--name %s --pane <id>' (or '%s discover') then restart this unit.\n",
				opts.Agent, active.BinaryName, opts.Agent, active.BinaryName)
			if err := sdnotify.Ready(); err != nil {
				logger.Printf("sdnotify_ready_err err=%v", err)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "get_agent: %v\n", err)
		return exitInternal
	}
	if a.PaneID == "" {
		fmt.Fprintf(stderr, "agent %q has no pane_id — exiting cleanly to avoid "+
			"systemd restart-loop (#340). Re-register with '%s register --name %s "+
			"--pane <id>' (or '%s discover') then restart this unit.\n",
			opts.Agent, active.BinaryName, opts.Agent, active.BinaryName)
		if err := sdnotify.Ready(); err != nil {
			logger.Printf("sdnotify_ready_err err=%v", err)
		}
		return exitOK
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
	// in /etc/tmux-tell/config.toml from silently breaking the mailman.
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

	// RecoverDelivering runs here — before any short-circuit exit — so orphaned
	// `delivering` rows are always reset to `queued` on serve startup, regardless
	// of the agent's delivery mode (#357). The original placement was AFTER the
	// hook-context / mailbox-only / paste-incapable short-circuits, meaning an
	// agent that transitioned FROM paste-and-enter (while a message was mid-flight)
	// to one of those modes would leave rows stuck in `delivering` permanently: the
	// next serve startup would short-circuit before recovering them. Moving recovery
	// here fixes that: short-circuiting paths still log + exit cleanly, but the
	// stale rows are unblocked first. For hook-context agents, doHookContext also
	// calls RecoverDelivering (redundant but idempotent). For mailbox-only, this is
	// the only automatic recovery path.
	if n, err := s.RecoverDelivering(opCtx, opts.Agent); err != nil {
		logger.Printf("recover_failed err=%v", err)
	} else if n > 0 {
		logger.Printf("recovered count=%d", n)
	}

	// Paste-capability safe-default guard (#323, generalized #360). An adapter
	// whose Profile declares PasteCapable=false must never paste-and-enter into
	// its pane: paste-and-enter relies on the internal/tmuxio observe-gate reading
	// the pane's prompt sentinel + cursor to defer during operator-typing, and an
	// adapter that can't be read would clobber in-progress operator input. For such
	// an adapter, force-defer: refuse the paste loop, leave the messages queued,
	// and tell the operator to migrate to a non-paste delivery mode (hook-context
	// or mailbox-only).
	//
	// Originally this guarded Codex specifically (#323), when the observe-gate
	// couldn't yet read Codex's `›` sentinel. #322 dissolved that premise (the gate
	// now reads per-adapter PaneProfile sentinels) and #360 flipped Codex to
	// PasteCapable=true, so Codex now passes this guard. It remains as the general
	// safe-default for ANY future paste-incapable adapter — the truth-claim it
	// enforces shifted from "this adapter is Codex" to "this adapter's Profile
	// signals it can't be observed for paste-safe delivery" (same premise-rewrite
	// shape as the #293/#333 guard).
	//
	// Keyed on the process-global adapter Profile (active.PasteCapable), not a DB
	// column: the mailman runs from the adapter's own binary (the systemd unit is
	// `<BinaryName>-mailman@<agent>`, systemctl.go), so the serving process already
	// knows its adapter — no schema change, and it fires regardless of HOW the agent
	// reached paste-and-enter mode (explicit flag, config override, or register-time
	// default). Exit cleanly (exitOK, like the mailbox-only/hook-context short-
	// circuit below) so systemd records success rather than crash-looping; the loud
	// WARN carries the corrective command.
	if !active.PasteCapable && a.DeliveryMode == store.DeliveryModePasteAndEnter {
		logger.Printf("WARN paste_incapable_adapter adapter=%s agent=%s delivery_mode=%s — "+
			"refusing paste-and-enter; the observe-gate can't safely read this adapter's pane "+
			"and would clobber operator input (#323). Messages stay queued. Migrate to a "+
			"non-paste delivery mode, e.g.: %s register --name %s --delivery-mode hook-context "+
			"(or mailbox-only), then restart this unit (systemctl --user restart %s-mailman@%s).",
			active.BinaryName, opts.Agent, a.DeliveryMode,
			active.BinaryName, opts.Agent, active.BinaryName, opts.Agent)
		if err := sdnotify.Ready(); err != nil {
			logger.Printf("sdnotify_ready_err err=%v", err)
		}
		return exitOK
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

	// #783 session-stale streak. Counts consecutive iterations where the
	// registered session-id resolves to no live pane (session_stale) AND a quick
	// pane probe classifies StateUnknown — the stuck class where #105's
	// pre-paste-safety net refuses forever with no exit. Chamber-level (not keyed
	// on message id like the #291 pane-fail counter): session_stale is a property
	// of the chamber's registration, so every message to it hits the same wall;
	// the streak resets on ANY non-stuck outcome (a benign session_stale that
	// probes deliverable, or a plain non-session_stale iteration). In-memory: a
	// mailman restart re-registers from a clean slate.
	consecutiveSessionStale := 0

	// #300: mirrors the last-known stuck_reason for the mailman_stuck gauge.
	// Tracks what reason the gauge was last Set=1 for, so the Set=0 call on
	// unpark can match the label.  Empty = gauge is at 0 (or not yet seen).
	lastStuckReason := ""

	// #719(A) freshness-alert loop state. lastFreshnessCheck throttles the
	// queue-age sweep to freshnessCheckInterval. lastFreshnessAlerted is the
	// per-episode edge-trigger latch: set true when a stuck-chamber notice
	// fires, reset to false the moment the queue is no longer stale (drained or
	// a delivery advanced) so a later, distinct freeze re-alerts — one notice
	// per freeze episode, never a per-sweep storm. Mirrors the lastStuckReason
	// transition-mirroring above. Zero-value time = "never swept", so the first
	// eligible iteration sweeps immediately.
	var lastFreshnessCheck time.Time
	lastFreshnessAlerted := false

	// Serve-loop spin guard (#496). progressedLastIter carries the previous
	// iteration's progress (a claimed message) into this iteration's top-of-loop
	// check, so the pure spinGuard.record stays the single decision point. Seed
	// true so the first iteration starts a clean window.
	guard := &spinGuard{threshold: opts.SpinGuardThreshold, window: opts.SpinGuardWindow}
	progressedLastIter := true

	// #448 provider-cap accounting. Record our adapter's provider once at
	// startup so peers' cap gate can scope its working-count to it;
	// lastObservedWrite throttles the per-iteration self-probe that publishes
	// our live state. Both no-op when the cap is disabled (test seam) or the
	// adapter declares no provider.
	capOn := !opts.ProviderCapDisabled && active.Provider != ""
	if capOn {
		if perr := s.SetProvider(opCtx, opts.Agent, active.Provider); perr != nil {
			logger.Printf("set_provider_failed err=%v", perr)
		}
		// #507: persist the cap too, so a separate `inbox` process can
		// live-derive the provider-cap deferral state of our queued messages.
		if perr := s.SetProviderCap(opCtx, opts.Agent, opts.MaxConcurrentPerProvider); perr != nil {
			logger.Printf("set_provider_cap_failed err=%v", perr)
		}
		m.SetProviderDeferInflight(active.Provider, 0)
	}
	var lastObservedWrite time.Time

	// #526: materialize the copy-mode deferral counter at 0 from startup so the
	// metric is present in exposition before any scroll-read (present-at-zero
	// idiom, #531). Unconditional — copy-mode defer is independent of the
	// provider cap.
	m.InitCopyModeDefer(opts.Agent)
	if m != nil && tmuxio.ActivePaneProfile().RateLimitPattern != "" {
		m.SetChamberRateLimited(opts.Agent, active.Provider, 0)
		m.SetChamberRateLimitRetryAfter(opts.Agent, active.Provider, 0)
	}
	if m != nil && tmuxio.ActivePaneProfile().UsageLimitPattern != "" {
		m.SetChamberUsageLimited(opts.Agent, active.Provider, 0)
	}

	// #507: per-message first-defer timestamps (publicID → first time the
	// provider cap deferred it this serve run). Stamped on the first defer,
	// observed + cleared at the gate-pass that ends the deferral, feeding the
	// tmux_tell_provider_defer_wait_seconds histogram. Mailman-local runtime
	// state (a restart resets it — an acceptable loss for a wait histogram); it
	// never needs to outlive the process, so it stays out of the DB. Growth is
	// bounded by queued messages that were cap-deferred and have not yet passed
	// the gate; external queue removals are pruned before the next claim, and
	// process exit drops any remaining runtime entries.
	deferStart := make(map[string]time.Time)
	var (
		rateLimitedMsgID    string
		rateLimitedSince    time.Time
		rateLimitedRetryAt  time.Time
		rateLimitedAttempts int
		usageLimitedMsgID   string
		usageLimitedSince   time.Time
		// #618 auto-resume episode tracking for the self-observe path. Distinct
		// from the rateLimited* family above, which is the DELIVERY path's
		// per-message backoff bookkeeping — this rides the throttled self-probe
		// and resumes the chamber regardless of whether a message is queued.
		rlResume rateLimitResumeState
	)
	// resolvedRateLimitResumeMaxAttempts clamps a non-positive configured ceiling
	// back to the default so a stray 0 can't silently disable resumption (the
	// explicit off-switch is RateLimitResumeDisabled).
	resolvedRateLimitResumeMaxAttempts := opts.RateLimitResumeMaxAttempts
	if resolvedRateLimitResumeMaxAttempts <= 0 {
		resolvedRateLimitResumeMaxAttempts = defaultRateLimitResumeMaxAttempts
	}
	// rateLimitResumeBackoff is the wait before the next continue-paste: the
	// banner-parsed Retry-After hint when present, else the exponential fallback
	// (reusing the #613 schedule), plus normal-band #543 jitter so independent
	// chambers resuming after the same provider-wide overload don't all paste on
	// the same tick. No message priority applies here (this isn't a delivery), so
	// the jitter uses the normal band; the #448 cap-aware extension is omitted —
	// a continue-paste resumes the chamber's OWN interrupted turn, it does not
	// consume a delivery slot.
	rateLimitResumeBackoff := func(attempt int, hint time.Duration) time.Duration {
		base := hint
		if base <= 0 {
			base = rateLimitBackoff(attempt)
		}
		return base + rateLimitWakeJitter(base, store.PriorityNormal)
	}
	clearRateLimitState := func() {
		if rateLimitedMsgID == "" {
			return
		}
		if m != nil {
			m.SetChamberRateLimited(opts.Agent, active.Provider, 0)
			m.SetChamberRateLimitRetryAfter(opts.Agent, active.Provider, 0)
		}
		rateLimitedMsgID = ""
		rateLimitedSince = time.Time{}
		rateLimitedRetryAt = time.Time{}
		rateLimitedAttempts = 0
	}
	clearUsageLimitState := func() {
		if usageLimitedMsgID == "" {
			return
		}
		if m != nil {
			m.SetChamberUsageLimited(opts.Agent, active.Provider, 0)
		}
		usageLimitedMsgID = ""
		usageLimitedSince = time.Time{}
	}

	// #515: ring-aware idle wake. Watch this mailman's own recipient doorbell so
	// the no-work idle waits below return sub-second when a message is inserted
	// or promoted for opts.Agent, instead of waiting out the full
	// IdlePollInterval. Best-effort: WatchOrNil yields a nil channel on any
	// fsnotify setup failure, and a nil channel never fires in the select, so the
	// loop falls back to the (still-present) poll. The poll stays the correctness
	// path; this only lowers steady-state latency.
	idleNotifyCh, stopIdleNotify := notify.WatchOrNil(opCtx, opts.Agent)
	defer stopIdleNotify()

	for {
		if stopCtx.Err() != nil {
			return exitOK
		}
		// Fail loud on a spin: if the loop has churned past the no-progress
		// threshold within the window, panic so systemd restarts the mailman
		// (stack in the journal) instead of silently burning CPU. A healthy
		// idle loop sleeps every no-progress iteration and never gets here.
		if guard.record(progressedLastIter, time.Now()) {
			panic(spinPanicMessage(opts.Agent, guard))
		}
		progressedLastIter = false
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

		// #300: update mailman_stuck gauge on stuck_reason transitions.
		// Detected here (not at SetStuck/ClearStuck call sites) so a mailman
		// that starts up against an already-parked agent still sets the gauge.
		// Clear-then-set covers all four transitions (""→A, A→"", A→B, ""→""
		// no-op); without the clear step an A→B flip leaks A's label (#319).
		if a.StuckReason != lastStuckReason {
			if lastStuckReason != "" {
				m.SetMailmanStuck(opts.Agent, lastStuckReason, false)
			}
			if a.StuckReason != "" {
				m.SetMailmanStuck(opts.Agent, a.StuckReason, true)
			}
			lastStuckReason = a.StuckReason
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

		// #448 self-observation: on a throttled cadence, probe our own pane's
		// AgentState and publish it to agents.observed_state so OTHER mailmen's
		// provider-cap gate can count us as working. Not stuck (checked above) so
		// the pane is live; throttled (not every iteration) to bound the extra
		// capture-pane probes. A probe error just skips this round (lastObserved
		// stays put → retried next iteration). The TTL guard on the read side
		// handles a crashed mailman's stale row.
		if capOn && a.PaneID != "" && time.Since(lastObservedWrite) >= opts.ObservedStateInterval {
			obsCtx, obsCancel := context.WithTimeout(opCtx, 2*time.Second)
			if st, ev, perr := tmuxio.AgentState(obsCtx, a.PaneID); perr == nil {
				if werr := s.SetObservedState(opCtx, opts.Agent, st.String(), time.Now()); werr != nil {
					logger.Printf("observed_state_write_failed err=%v", werr)
				}
				// #621: observed-truth supersedes the compact-pending self-report.
				// Once we actually observe ourselves at-rest-in-compaction, the
				// "I'm about to /compact" self-report has done its job (the
				// observed_state now carries the ground truth), so clear it — it
				// would otherwise linger stale after the chamber resumes. Rides
				// this existing throttled self-probe rather than adding a separate
				// tmux capture (substrate-fit).
				if cerr := maybeAutoClearMetabolism(opCtx, s, opts.Agent, st); cerr != nil {
					logger.Printf("metabolism_autoclear_failed err=%v", cerr)
				}
				// #618: auto-resume after a transient rate-limit. Rides the same
				// throttled self-probe (substrate-fit, like #621's auto-clear):
				// once the cooldown elapses, paste `continue` to resume the
				// chamber's interrupted turn — independent of whether a bus
				// message is queued (the delivery-path backoff above only fires
				// when there IS a message to deliver). The probe cadence is the
				// verify-and-retry loop: a later observation that finds the chamber
				// no longer rate-limited ends the episode.
				if !opts.RateLimitResumeDisabled {
					maybeAutoResumeRateLimited(obsCtx, logger, m, opts.Agent,
						active.Provider, a.PaneID, st, ev, &rlResume, time.Now(),
						resolvedRateLimitResumeMaxAttempts, rateLimitResumeBackoff,
						tmuxio.SendKeys)
				}
				lastObservedWrite = time.Now()
			}
			obsCancel()
		}

		pruneProviderDeferStart(opCtx, s, m, active.Provider, deferStart)

		// #719(A) freshness alert. Self-observed: this mailman watches its OWN
		// inbound queue for the "queued but mailman silent" divergence smell — a
		// real deliverable sitting queued/delivering past MailmanStaleThreshold.
		// StuckReason=="" and !Paused are already guaranteed by the continues
		// above, so a parked/#783 chamber is excluded here (that path owns its
		// own #300 surface). Cheap in steady state: only the MIN(created_at) DB
		// query runs per sweep; the pane-state probe fires solely once a queue is
		// found stale, i.e. only when on the verge of alerting. Dormant unless a
		// conductor (AlertTo) is configured and it isn't this mailman itself.
		if opts.MailmanStaleThreshold > 0 && opts.AlertTo != "" && opts.AlertTo != opts.Agent &&
			time.Since(lastFreshnessCheck) >= freshnessCheckInterval {
			now := time.Now()
			lastFreshnessCheck = now
			oldestAt, hasPending, ferr := s.RecipientOldestPendingAt(opCtx, opts.Agent)
			switch {
			case ferr != nil:
				logger.Printf("freshness_check_failed agent=%s err=%v", opts.Agent, ferr)
			case !hasPending:
				lastFreshnessAlerted = false // queue drained → episode over, re-arm
			default:
				age, ok := freshnessAge(oldestAt, now)
				switch {
				case !ok:
					logger.Printf("freshness_parse_failed agent=%s created_at=%q", opts.Agent, oldestAt)
				case age <= opts.MailmanStaleThreshold:
					lastFreshnessAlerted = false // delivery is progressing → re-arm
				case !lastFreshnessAlerted && a.PaneID != "":
					// Stale. Probe the pane once to exclude legitimate holds
					// (rate/usage-limit, awaiting-operator, compaction-rest,
					// copy-mode). Idle/Working/Unknown — and a probe error — are
					// alert-eligible (conservative-toward-detection: a stale queue
					// is the real anomaly, and we don't get to assume a health we
					// couldn't confirm).
					fCtx, fCancel := context.WithTimeout(opCtx, 2*time.Second)
					st, _, perr := tmuxio.AgentState(fCtx, a.PaneID)
					fCancel()
					if perr == nil && tmuxio.IsPasteUnsafe(st) && st != tmuxio.StateUnknown {
						// Legitimate hold — not frozen, don't alert. The latch
						// stays disarmed so it fires when the hold lifts if the
						// queue is still stale.
					} else {
						sendStuckChamberNotice(opCtx, s, logger, opts.Agent, opts.AlertTo, oldestAt, age, st, perr)
						lastFreshnessAlerted = true
					}
				}
			}
		}

		// #285 PR2 self-compact detection. A self-/compact is chamber-driven, not
		// bus-delivered, so it can't be counted inline on delivery like the clear
		// (PR1, below) — the mailman counts it HERE on its self-observation pass.
		// The adapter's post-compaction hook wrote last_self_compact_at (via
		// note-compact); we edge-detect it against the mailman-owned watermark.
		// Gated on opt-in (RespawnAfterShrinks>0) and a live pane; the in-memory
		// compare on the freshly-read agent row (line ~1264) skips the store
		// round-trip on the common no-new-compact iteration, so this is ~free every
		// loop. CountSelfCompactIfNew is the authoritative re-check + atomic count.
		if a.RespawnAfterShrinks > 0 && a.PaneID != "" &&
			a.LastSelfCompactAt != "" && a.LastSelfCompactAt > a.SelfCompactCountedAt {
			counted, count, cerr := s.CountSelfCompactIfNew(opCtx, opts.Agent)
			if cerr != nil {
				logger.Printf("self_compact_count_err agent=%s err=%v", opts.Agent, cerr)
			} else if counted && respawnIfThresholdReached(stopCtx, s, defaultRespawnOps(), logger, opts.Agent,
				a.PaneID, a.RelaunchCmd, "self-compact", count, a.RespawnAfterShrinks, watchdogPing) {
				return exitOK
			}
		}

		msg, err := s.ClaimNextWithStrategy(opCtx, opts.Agent, opts.PriorityStrategy)
		if err != nil {
			logger.Printf("claim_failed err=%v", err)
			if stopSleepOrNotify(stopCtx, opts.IdlePollInterval, idleNotifyCh) {
				return exitOK
			}
			continue
		}
		if msg == nil {
			if stopSleepOrNotify(stopCtx, opts.IdlePollInterval, idleNotifyCh) {
				return exitOK
			}
			continue
		}

		// A claimed message (ping or delivery) is real work — mark progress so
		// the spin guard resets on the next iteration's top-of-loop check (#496).
		progressedLastIter = true

		if rateLimitedMsgID != "" && rateLimitedMsgID != msg.PublicID {
			clearRateLimitState()
		}
		if usageLimitedMsgID != "" && usageLimitedMsgID != msg.PublicID {
			clearUsageLimitState()
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
			// #515: the ping's delivered/failed transition is publicID-keyed;
			// ring the recipient doorbell so a ping --watch (pollPingTerminal,
			// keyed on the pinged agent) wakes promptly.
			notify.Notify(opts.Agent)
			if stopOrSleep(stopCtx, opts.InterMessageDelay) {
				return exitOK
			}
			continue
		}

		// #448 provider-cap gate. Pings already returned above (substrate-only,
		// no token cost). For a real delivery, defer if too many same-provider
		// chambers are currently working: revert the claimed row to queued and
		// re-check after a short interval — the slot reopens when a same-provider
		// chamber leaves StateWorking. The deferral is observable (log + metric)
		// so an operator isn't left wondering why a queued message isn't moving.
		if capOn && opts.MaxConcurrentPerProvider > 0 {
			working, cerr := s.CountWorkingOnProvider(opCtx, active.Provider, opts.ProviderCapTTL, time.Now())
			if cerr != nil {
				logger.Printf("provider_cap_count_failed err=%v", cerr)
			} else if working >= opts.MaxConcurrentPerProvider {
				if _, rerr := s.RecoverDelivering(opCtx, opts.Agent); rerr != nil {
					logger.Printf("provider_cap_revert_failed id=%s err=%v", msg.PublicID, rerr)
				}
				if m != nil {
					m.IncProviderDefer(active.Provider)
				}
				// #507: stamp the FIRST defer of this message so the wait
				// histogram measures the full hold, not just the last recheck.
				if _, seen := deferStart[msg.PublicID]; !seen {
					deferStart[msg.PublicID] = time.Now()
					m.SetProviderDeferInflight(active.Provider, float64(len(deferStart)))
				}
				logger.Printf("provider_cap_deferred id=%s provider=%s working=%d cap=%d",
					msg.PublicID, active.Provider, working, opts.MaxConcurrentPerProvider)
				if stopOrSleep(stopCtx, opts.ProviderCapRecheckInterval) {
					return exitOK
				}
				continue
			}
		}

		// #507: the message passed the cap gate (or the cap is off) — its slot is
		// open. If it had been deferred, observe how long it waited and clear the
		// stamp. This fires for every message that stops being deferred, so the
		// histogram captures the deferred→slot-open wait regardless of the
		// downstream delivery outcome ("waited before its slot opened" per the AC).
		if t0, deferred := deferStart[msg.PublicID]; deferred {
			if m != nil {
				m.ObserveProviderDeferWait(active.Provider, time.Since(t0).Seconds())
			}
			delete(deferStart, msg.PublicID)
			m.SetProviderDeferInflight(active.Provider, float64(len(deferStart)))
		}

		logger.Printf("delivering id=%s kind=%s from=%s body_bytes=%d",
			msg.PublicID, msg.Kind, msg.FromAgent, len(msg.Body))

		paneForDelivery := a.PaneID

		// #626 Phase 1b: session-id is the PRIMARY, exact resolution key. When
		// the agent has a self-discovered session-id, resolve the pane hosting
		// that exact session - it cannot mis-route to a different agent (the
		// fuzzy name match can). On a positive hit we deliver there and SKIP the
		// name-based drift-check below (the session-id match is a stronger
		// liveness guarantee than the name+title resolution). All other outcomes
		// (no stored session-id, lookup error, or session-id resolves nowhere)
		// fall through to the name path - which finds a re-resumed same-name
		// session or blocks a bare shell (Phase-1a) - with a deprecation log so
		// the legacy/stale registration surfaces (#626 AC6).
		sessionResolved := false
		// #783: set when the registered session-id resolves to no live pane (the
		// `default` case below). Read by the session-stale fast-path exit
		// condition after this block to decide whether to accrue the park streak.
		sessionStale := false
		if !opts.DriftCheckDisabled {
			if walker == nil {
				walker = discover.New()
			}
			if a.SessionID != "" {
				sidPane, siderr := walker.LookupBySessionID(opCtx, a.SessionID)
				switch {
				case siderr != nil:
					logger.Printf("session_lookup_err id=%s agent=%s err=%v - falling back to name resolution",
						msg.PublicID, opts.Agent, siderr)
				case sidPane != "":
					if sidPane != paneForDelivery {
						logger.Printf("session_resolved id=%s agent=%s session=%s registered_pane=%s rediscovered=%s (healed)",
							msg.PublicID, opts.Agent, a.SessionID, paneForDelivery, sidPane)
						if uerr := s.UpsertAgent(opCtx, opts.Agent, sidPane); uerr != nil {
							logger.Printf("session_heal_update_failed err=%v", uerr)
						} else {
							paneForDelivery = sidPane
						}
					}
					sessionResolved = true
				default:
					// Stored session-id resolves nowhere (ended, or re-resumed under a
					// new id). Fall back to the name-based drift-check - it finds a
					// re-resumed same-name session, or blocks a bare shell (Phase-1a).
					logger.Printf("session_stale id=%s agent=%s session=%s - not hosted in any pane; falling back to name resolution (re-register to refresh session-id)",
						msg.PublicID, opts.Agent, a.SessionID)
					sessionStale = true
				}
			} else {
				logger.Printf("session_id_absent id=%s agent=%s - legacy registration; name-based resolution (re-register from inside the session to enable exact session-id routing, #626)",
					msg.PublicID, opts.Agent)
			}
		}

		// #783 session-stale fast-path exit condition. When the registered
		// session-id resolves nowhere (sessionStale), name resolution falls back
		// to a pane that is frequently a stale/wrong one classifying
		// StateUnknown — and #105's pre-paste-safety net then refuses the paste
		// EVERY iteration with no exit condition, so the message retry-loops
		// invisibly until TTL drainage (the sender believes it is queued; the
		// recipient never sees it). This is the exit condition, added strictly
		// DOWNSTREAM of #105 (the refusal itself is correct and untouched).
		//
		// A quick AgentState probe here — NOT the full observe-gate, which would
		// burn its ~5min MaxWait first — detects the stuck class cheaply. On a
		// StateUnknown probe we accrue a consecutive streak and, at
		// SessionStaleThreshold, park the mailman with StuckReasonSessionStale:
		// the SAME #291 park that stops probing tmux, surfaces in `agents`, drives
		// the #300 gauge, and clears on `register --force` — which is exactly the
		// re-register that refreshes the stale session-id. Skipping the 5min gate
		// per iteration is what makes the park land in ~threshold×retry-interval
		// (a few minutes) instead of ~threshold×MaxWait (~15min+), so the
		// invisible-stuck window is short.
		//
		// Requiring N CONSECUTIVE session_stale+unknown iterations is the safety
		// margin that keeps this from stealing #105's transient-popup case: a
		// live chamber's session-id RESOLVES (so it is not session_stale in the
		// first place), and a genuine transient unknown clears within a cycle or
		// two (resetting the streak). A benign session_stale — the name-resolved
		// pane is a live, classifiable session — probes idle/working here and
		// falls through to normal delivery, resetting the streak. Disabled
		// (SessionStaleThreshold<=0 or pre-paste-safety-disabled) preserves the
		// exact pre-#783 behavior.
		if sessionStale && opts.SessionStaleThreshold > 0 && !opts.PrePasteSafetyDisabled {
			probeCtx, pcancel := context.WithTimeout(opCtx, 2*time.Second)
			ssState, _, sserr := tmuxio.AgentState(probeCtx, paneForDelivery)
			pcancel()
			if sserr == nil && ssState == tmuxio.StateUnknown {
				consecutiveSessionStale++
				if _, rerr := s.RecoverDelivering(opCtx, opts.Agent); rerr != nil {
					logger.Printf("WARN session_stale_recover_failed id=%s err=%v", msg.PublicID, rerr)
				}
				if consecutiveSessionStale >= opts.SessionStaleThreshold {
					if serr := s.SetStuck(opCtx, opts.Agent, store.StuckReasonSessionStale); serr != nil {
						logger.Printf("WARN stuck_set_failed agent=%s err=%v", opts.Agent, serr)
					} else {
						logger.Printf("WARN stuck agent=%s reason=%s consecutive=%d — mailman parked; stops probing tmux until `register --force` refreshes the stale session-id (#783)",
							opts.Agent, store.StuckReasonSessionStale, consecutiveSessionStale)
					}
					// The DB stuck_reason now gates the loop; reset the in-memory
					// streak so a `register --force` clear resumes from a clean slate.
					consecutiveSessionStale = 0
					continue
				}
				logger.Printf("WARN session_stale_stuck_backoff id=%s agent=%s pane=%s consecutive=%d/%d delay=%s — session-id resolves nowhere + pane unknown; bounding retry, parks at threshold (#783)",
					msg.PublicID, opts.Agent, paneForDelivery, consecutiveSessionStale, opts.SessionStaleThreshold, sessionStaleRetryInterval)
				if stopOrSleep(stopCtx, sessionStaleRetryInterval) {
					return exitOK
				}
				continue
			}
			// Benign session_stale (pane classifies deliverable) or a probe error:
			// break the streak and fall through to the normal gate + delivery.
			consecutiveSessionStale = 0
		} else if !sessionStale {
			// A plain, healthy iteration breaks any prior session-stale streak.
			consecutiveSessionStale = 0
		}

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
		if !opts.DriftCheckDisabled && !sessionResolved {
			if walker == nil {
				walker = discover.New()
			}
			canonicals := buildCanonicals(opCtx, s)
			running, ambiguous, err := walker.PaneAgentNameWithCanonicals(opCtx, paneForDelivery, canonicals)
			driftFailReason := ""
			noLiveSession := false
			switch {
			case err != nil:
				// Soft fail: log and proceed with the registered pane.
				// Errors from /proc reads or tmux list-panes are
				// system-level, not policy-level; don't punish the
				// message for an environmental hiccup.
				logger.Printf("drift_check_err id=%s err=%v", msg.PublicID, err)
			case ambiguous:
				driftFailReason = "drift_check_ambiguous"
				logger.Printf("WARN drift_check_ambiguous id=%s agent=%s registered_pane=%s — multiple canonicals exact-or-substring-match the running --resume value (resolve via: tmux-tell.register name=<canonical> alias=<unique-suffix> force=true; #47)",
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
			case running == "":
				// #626 Phase 1a - bare-shell-paste safety block. The registered
				// pane hosts NO recognizable agent session: PaneAgentName resolved
				// neither opts.Agent nor any other agent there (cmdline + title +
				// window-name all empty). It is most likely a bare shell left after
				// the session ended; pasting the message body there would execute
				// it as a shell command (the operator-surfaced safety gap). Do not
				// trust the registered pane - look the addressed agent up across
				// ALL panes.
				newPane, lambig, lerr := walker.LookupByNameWithCanonicals(opCtx, opts.Agent, canonicals)
				switch {
				case lerr == nil && !lambig && newPane != "" && newPane != paneForDelivery:
					// POSITIVELY found the live session in a DIFFERENT pane: the
					// registry pane went bare-shell while the session relocated.
					// Re-route + heal the registry.
					logger.Printf("session_relocated id=%s agent=%s registered_pane=%s(bare) rediscovered=%s",
						msg.PublicID, opts.Agent, paneForDelivery, newPane)
					if uerr := s.UpsertAgent(opCtx, opts.Agent, newPane); uerr != nil {
						logger.Printf("relocate_update_failed err=%v", uerr)
					} else {
						paneForDelivery = newPane
					}
				case lerr == nil && !lambig && newPane == paneForDelivery:
					// The across-pane lookup re-confirmed the addressed agent IS in
					// the registered pane (the per-pane probe flaked transiently).
					// Proceed with delivery.
				default:
					// The registered pane is CONFIRMED bare (the outer probe read
					// cleanly: err==nil, running==""), and the across-pane lookup
					// did NOT positively locate the live session - it errored
					// (lerr), was ambiguous (lambig), or found nothing
					// (newPane==""). In EVERY one of these outcomes, pasting into
					// the registered pane means pasting into a bare shell, where
					// paste-and-enter executes the body as a shell command. BLOCK
					// unconditionally (#626 AC3 "NEVER paste to a bare shell").
					// Surveyor review 3287: the block must cover all non-positive
					// outcomes, not just newPane=="" - lerr/lambig falling through
					// to the paste was the safety hole. This bypasses
					// --drift-soft-fail, which governs the distinct
					// deliver-to-wrong-agent policy, not this safety invariant.
					noLiveSession = true
					logger.Printf("WARN no_live_session id=%s agent=%s registered_pane=%s rediscovered=%q lookup_err=%v ambiguous=%v - addressed session not positively located in any pane; blocking to prevent bare-shell paste (#626)",
						msg.PublicID, opts.Agent, paneForDelivery, newPane, lerr, lambig)
				}
			}
			if noLiveSession {
				// Unconditional safety block (#626 AC3) - bypasses --drift-soft-fail.
				// Surfaces to the sender like any terminal failure so the
				// stale-registration / ended-session gets noticed instead of
				// silently eating messages.
				reason := "no_live_session: addressed session not present in any pane; delivery blocked to prevent bare-shell paste (#626)"
				if mferr := s.MarkFailed(opCtx, msg.PublicID, reason); mferr != nil {
					logger.Printf("mark_failed_err id=%s err=%v", msg.PublicID, mferr)
				}
				maybeInsertFailureNotice(opCtx, s, logger,
					opts.NotifyOnFailed, opts.Agent, msg, "failed", reason)
				m.RecordDelivery(msg.FromAgent, opts.Agent, metrics.StateFailed)
				if stopOrSleep(stopCtx, opts.InterMessageDelay) {
					return exitOK
				}
				continue
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

		// #761 delivery-path bare-shell gate. The name-based drift block above
		// resolves paneForDelivery via cmdline → tmux TITLE → window-name. The
		// title/window sources are stale METADATA that outlive a dead process, so a
		// bare shell with a lingering agent-named title resolves as the agent and
		// would receive paste-and-enter — executing the bus message body as shell
		// commands (the #761 exploit). The #626 block above catches only the
		// running=="" arm, which a stale title defeats. Before delivering on the
		// non-session path, require that the pane's FOREGROUND process is not an
		// interactive shell — the hazard's actual mechanism, since paste-and-enter
		// executes as commands only into a shell.
		//
		// ADAPTER-NEUTRAL by design: it asks "would a paste execute as shell
		// commands?", NOT "which adapter is this?" (a claude-specific form refused
		// live CODEX chambers) and NOT "the right chamber?" — attribution is the
		// drift block's job, which ran above. So a genuine drift onto a live WRONG-agent pane stays governed by
		// --drift-soft-fail policy (not blocked here), while a bare shell is blocked
		// unconditionally. A session-resolved delivery already proved liveness
		// (sessionResolved) and skips this. Like the #626 block, this is a SAFETY
		// invariant and bypasses --drift-soft-fail.
		if !opts.DriftCheckDisabled && !sessionResolved {
			if walker == nil {
				walker = discover.New()
			}
			if !walker.PaneAcceptsPaste(opCtx, paneForDelivery) {
				reason := "no_live_session: target pane hosts no live agent process (bare shell / stale title); delivery blocked to prevent bare-shell paste (#761)"
				logger.Printf("WARN no_live_session_liveness_gate id=%s agent=%s pane=%s - pane matches only via stale metadata, no live claude/session-id; blocking bare-shell paste (#761)",
					msg.PublicID, opts.Agent, paneForDelivery)
				if mferr := s.MarkFailed(opCtx, msg.PublicID, reason); mferr != nil {
					logger.Printf("mark_failed_err id=%s err=%v", msg.PublicID, mferr)
				}
				maybeInsertFailureNotice(opCtx, s, logger,
					opts.NotifyOnFailed, opts.Agent, msg, "failed", reason)
				m.IncDeliveryRefusedLiveness(opts.Agent, "no_live_session_liveness_gate")
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
			// #526: record the copy-mode deferral wait for any cycle that
			// observed copy-mode — the delivered-on-exit path AND the
			// reverted-at-MaxWait path both populate CopyModeWait.
			if outcome.CopyModeWait > 0 {
				m.IncCopyModeDefer(opts.Agent)
				m.ObserveCopyModeDeferWait(opts.Agent, outcome.CopyModeWait.Seconds())
			}
			if gerr != nil {
				switch {
				case errors.Is(gerr, context.Canceled):
					// SIGTERM during the observe loop — exit cleanly.
					return exitOK
				case errors.Is(gerr, tmuxio.ErrUsageLimited):
					if msg.ForceRateLimited {
						// #558: the operator forced this message through the
						// usage-limit defer. Skip the park; break out of the gate
						// switch to fall through to the pre-paste safety check —
						// which still aborts on copy-mode / popup / unknown /
						// compaction (IsPasteUnsafeForced) — and then deliver.
						logger.Printf("WARN gate_forced_usagelimited id=%s pane=%s — --force-rate-limited bypassing usage-limit defer",
							msg.PublicID, paneForDelivery)
						break
					}
					// #540: the pane is visibly usage-limited. Revert the
					// claim and park until quota resets; this is a hard-stop
					// sibling to rate-limit, not a retryable throttle.
					if _, rerr := s.RecoverDelivering(opCtx, opts.Agent); rerr != nil {
						logger.Printf("WARN gate_usagelimited_recover_failed id=%s err=%v", msg.PublicID, rerr)
					}
					if usageLimitedMsgID != msg.PublicID {
						usageLimitedMsgID = msg.PublicID
						usageLimitedSince = time.Now()
						// #613: count + structurally log this usage-limit
						// episode once at the first-detection transition (the
						// guard fires once per episode; subsequent same-msg
						// re-detections skip it). quota_exceeded is a park-until-
						// reset hard-stop, so there is no retry hint.
						m.IncRateLimit(opts.Agent, active.Provider, "quota_exceeded")
						logger.Printf("rate_limit_event agent=%s provider=%s cause=quota_exceeded retry_after_seconds=0 retry_after_source=none banner_excerpt=%q (#613)",
							opts.Agent, active.Provider, outcome.Banner)
					}
					logger.Printf("WARN gate_usagelimited id=%s pane=%s iter=%d — reverting to queued and parking until reset",
						msg.PublicID, paneForDelivery, outcome.Iterations)
					if m != nil {
						refreshUsageLimitMetrics := func(now time.Time) {
							age := now.Sub(usageLimitedSince).Seconds()
							if age < 0 {
								age = 0
							}
							m.SetChamberUsageLimited(opts.Agent, active.Provider, age)
						}
						refreshUsageLimitMetrics(time.Now())
						if stopOrSleepWithUpdates(stopCtx, usageLimitRecheckInterval, usageLimitMetricsTick, refreshUsageLimitMetrics) {
							return exitOK
						}
					} else if stopOrSleep(stopCtx, usageLimitRecheckInterval) {
						return exitOK
					}
					continue
				case errors.Is(gerr, tmuxio.ErrRateLimited):
					if msg.ForceRateLimited {
						// #558: the operator forced this message through the
						// rate-limit defer. Skip the backoff; break out of the
						// gate switch to fall through to the pre-paste safety
						// check — which still aborts on copy-mode / popup /
						// unknown / compaction (IsPasteUnsafeForced) — and deliver.
						logger.Printf("WARN gate_forced_ratelimited id=%s pane=%s — --force-rate-limited bypassing rate-limit defer",
							msg.PublicID, paneForDelivery)
						break
					}
					// #504: the pane is visibly rate-limited. Revert the claim
					// and defer the retry using the parsed retry hint when
					// available; otherwise fall back to exponential backoff.
					if _, rerr := s.RecoverDelivering(opCtx, opts.Agent); rerr != nil {
						logger.Printf("WARN gate_ratelimited_recover_failed id=%s err=%v", msg.PublicID, rerr)
					}
					rateLimitFirstDetection := rateLimitedMsgID != msg.PublicID
					if rateLimitFirstDetection {
						rateLimitedMsgID = msg.PublicID
						rateLimitedSince = time.Now()
						rateLimitedAttempts = 0
					}
					rateLimitedAttempts++
					base := outcome.RetryAfter
					if base <= 0 {
						base = rateLimitBackoff(rateLimitedAttempts)
					}
					// #543 Layer-3: spread the wake by priority and extend it
					// when the provider is already saturated, so chambers
					// rate-limited on the same tick don't all wake together and
					// thunder the provider (the residue the #448 cap gate, which
					// still runs on the next iteration, cannot itself fix).
					providerCapForWake := 0
					if capOn {
						providerCapForWake = opts.MaxConcurrentPerProvider
					}
					backoff := rateLimitWakeDelay(opCtx, s, base, msg.Priority,
						active.Provider, providerCapForWake,
						opts.ProviderCapTTL, opts.ProviderCapRecheckInterval)
					rateLimitedRetryAt = time.Now().Add(backoff)
					if rateLimitFirstDetection {
						// #613: count + structurally log this rate-limit episode
						// once at the first-detection transition. Emitted here —
						// after the wake delay is computed — so retry_after
						// reflects the actual signal: the banner-parsed hint when
						// the regex exposed one (source=banner), else the computed
						// exponential backoff (source=backoff). Disclosing the
						// source lets an investigator tell an Anthropic-supplied
						// retry window from our local fallback.
						retryAfterSeconds := backoff.Seconds()
						retryAfterSource := "backoff"
						if outcome.RetryAfter > 0 {
							retryAfterSeconds = outcome.RetryAfter.Seconds()
							retryAfterSource = "banner"
						}
						m.IncRateLimit(opts.Agent, active.Provider, "overloaded")
						logger.Printf("rate_limit_event agent=%s provider=%s cause=overloaded retry_after_seconds=%.0f retry_after_source=%s banner_excerpt=%q (#613)",
							opts.Agent, active.Provider, retryAfterSeconds, retryAfterSource, outcome.Banner)
					}
					logger.Printf("WARN gate_ratelimited id=%s pane=%s iter=%d retry_after=%s — reverting to queued for retry",
						msg.PublicID, paneForDelivery, outcome.Iterations, backoff)
					if m != nil {
						refreshRateLimitMetrics := func(now time.Time) {
							age := now.Sub(rateLimitedSince).Seconds()
							if age < 0 {
								age = 0
							}
							remaining := rateLimitedRetryAt.Sub(now).Seconds()
							if remaining < 0 {
								remaining = 0
							}
							m.SetChamberRateLimited(opts.Agent, active.Provider, age)
							m.SetChamberRateLimitRetryAfter(opts.Agent, active.Provider, remaining)
						}
						refreshRateLimitMetrics(time.Now())
						if stopOrSleepWithUpdates(stopCtx, backoff, rateLimitMetricsTick, refreshRateLimitMetrics) {
							return exitOK
						}
					} else if stopOrSleep(stopCtx, backoff) {
						return exitOK
					}
					continue
				case errors.Is(gerr, tmuxio.ErrCopyModeUnsafe):
					// #526: pane still scrolled at MaxWait. Do NOT deliver-
					// anyway — that pastes into a scrolled pane and reproduces
					// the 83b3 bug. Revert to queued and retry; the next cycle
					// delivers promptly once the operator exits copy-mode (the
					// gate poll catches the exit within one interval).
					logger.Printf("WARN gate_copymode_persist id=%s pane=%s iter=%d — pane scrolled past MaxWait; reverting to queued for retry (not pasting into a scrolled pane)",
						msg.PublicID, paneForDelivery, outcome.Iterations)
					if _, rerr := s.RecoverDelivering(opCtx, opts.Agent); rerr != nil {
						logger.Printf("WARN gate_copymode_recover_failed id=%s err=%v", msg.PublicID, rerr)
					}
					if stopOrSleep(stopCtx, opts.InterMessageDelay) {
						return exitOK
					}
					continue
				case errors.Is(gerr, tmuxio.ErrCopyModeQueryFailed):
					// #537: the pane_in_mode query failed on N consecutive polls,
					// so the pane's copy-mode state is genuinely unreadable. Like
					// the copy-mode-persist case, do NOT deliver-anyway (the capture
					// classification could be a stale scrolled-view Idle → the 83b3
					// bug). Revert to queued and retry; a later cycle gets a clean
					// query once the transient tmux condition clears. Logged
					// distinctly from gate_copymode_persist so it reads as
					// "unreadable", not "confirmed scrolled".
					logger.Printf("WARN gate_copymode_query_failed id=%s pane=%s iter=%d — pane_in_mode unreadable across polls; reverting to queued for retry (not delivering on an untrusted copy-mode classification)",
						msg.PublicID, paneForDelivery, outcome.Iterations)
					if _, rerr := s.RecoverDelivering(opCtx, opts.Agent); rerr != nil {
						logger.Printf("WARN gate_copymode_query_recover_failed id=%s err=%v", msg.PublicID, rerr)
					}
					if stopOrSleep(stopCtx, opts.InterMessageDelay) {
						return exitOK
					}
					continue
				case errors.Is(gerr, tmuxio.ErrMaxWaitExceeded):
					logger.Printf("WARN gate_max_wait id=%s pane=%s iter=%d — delivering anyway (%s)",
						msg.PublicID, paneForDelivery, outcome.Iterations, outcome.Reason)
				default:
					logger.Printf("WARN gate_err id=%s err=%v — delivering anyway",
						msg.PublicID, gerr)
				}
			}
			if rateLimitedMsgID == msg.PublicID {
				clearRateLimitState()
			}
			if usageLimitedMsgID == msg.PublicID {
				clearUsageLimitState()
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
					// Clear by line-count (#336 InputControl): codex clears one
					// line per Ctrl+U, so a multi-line stranded draft needs one
					// press per line. outcome.InputContent is extractInputContent's
					// visual-row join, so its line count is the right press count;
					// Claude clears all on the first press and ignores the rest.
					clearLines := strings.Count(outcome.InputContent, "\n") + 1
					clearErr := tmuxio.ClearInput(clearCtx, paneForDelivery, clearLines)
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
			// #558: a --force-rate-limited message uses the narrower predicate
			// that still aborts on content-corrupting states (copy-mode, popup,
			// unknown, compaction) but NOT on the rate-/usage-limit banner the
			// operator chose to push past. Without this, this re-probe would
			// re-block the forced message on the very rate-limit state the gate
			// bypass above just waved through (IsPasteUnsafe lists both).
			pasteUnsafe := tmuxio.IsPasteUnsafe(probeState)
			if msg.ForceRateLimited {
				pasteUnsafe = tmuxio.IsPasteUnsafeForced(probeState)
			}
			if perr != nil || pasteUnsafe {
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
		// #449: a true delivery (verified or input-box) earns the post-deliver
		// cooldown — the recipient now has a message to ingest. The control-skip
		// and hard-failure cases below do not (nothing landed in the chamber's
		// context), so they fall through to the plain inter-message delay.
		delivered := false
		switch {
		case derr == nil:
			logger.Printf("delivered id=%s", msg.PublicID)
			if err := s.MarkDelivered(opCtx, msg.PublicID); err != nil {
				logger.Printf("mark_delivered_err id=%s err=%v", msg.PublicID, err)
			}
			delivered = true
			m.RecordDelivery(msg.FromAgent, opts.Agent, metrics.StateDelivered)
			if sec, ok := deliveryLatencySeconds(msg.CreatedAt); ok {
				m.ObserveDeliveryLatency(opts.Agent, sec)
				m.ObserveDeliveryLatencyByPriority(store.PriorityName(msg.Priority), sec)
			}
			if isSessionResetControl(msg) && opts.PostCompactPause > 0 {
				// #622: wait for the pane to be STABLY idle (compaction settled)
				// rather than a fixed timer — the fixed PostCompactPause under-
				// shoots a large /compact and the next row's paste is swallowed
				// mid-settle. PostCompactPause is now the ceiling, not the target.
				logger.Printf("post_compact_stability_wait id=%s ceiling=%s",
					msg.PublicID, opts.PostCompactPause)
				settled, stopped := waitForStableIdle(stopCtx, paneForDelivery,
					opts.PostCompactPause, postCompactPollEvery, watchdogPing, postCompactStableDebounce)
				if stopped {
					return exitOK
				}
				if !settled {
					logger.Printf("post_compact_stability_ceiling id=%s — pane not stably idle within %s; proceeding (pre-paste gate backstops)",
						msg.PublicID, opts.PostCompactPause)

					// #730 co-trigger: a tmux-tell-triggered /compact that never
					// settled to idle may have EXITED the chamber to a bare shell
					// (the Bosun-bailout shape). When the chamber opted into
					// auto_restart and registered a relaunch_cmd, probe for that bare
					// shell and, if present, relaunch via the shared primitive (the
					// same send-keys path #285 uses). A chamber that merely compacted
					// slowly (still running, no shell) falls through awaitExit's
					// window as a no-op. Gated on isCompactControl so a /clear (whose
					// respawn path is #285 PR1) isn't double-handled.
					if isCompactControl(msg) && a.AutoRestart && a.RelaunchCmd != "" {
						logger.Printf("auto_restart_probe id=%s agent=%s pane=%s - triggered /compact did not settle; checking for chamber exit to relaunch",
							msg.PublicID, opts.Agent, paneForDelivery)
						if _, autoStopped := relaunchAfterExit(stopCtx, s, defaultRespawnOps(), logger,
							opts.Agent, paneForDelivery, a.RelaunchCmd, autoRestartExitWindow, watchdogPing); autoStopped {
							return exitOK
						}
					}
				}
			}
			// #843: resume-deferred auto-fire. A bus-delivered session reset
			// (/compact or /clear) that has just been delivered (and, above,
			// waited out its settle window when the pause is enabled) is the
			// "chamber went away and is coming back" edge — the resume analog of
			// the register auto-fire (#258a: "the register IS the fire"). Promote
			// this agent's `resume`-staged rows so they land in the freshly-reset
			// context instead of rotting in `deferred` forever (the failure mode
			// #843 anchors: staged self-handoffs that never fire because nothing
			// but an explicit flush_deferred{resume} ever promoted them).
			//
			// Deliberately NOT gated on opts.PostCompactPause: disabling the
			// settle pause is a delivery-TIMING choice and must not silently
			// disable a correctness feature. When the pause is off the promoted
			// row is claimed on the next loop; the observe / pre-paste gate still
			// holds its paste until the pane is paste-safe, so promoting before
			// the pane has fully settled is safe. PromoteDeferred rings the
			// delivery doorbell (fireNotify) on a non-zero promote, and created_at
			// ordering lands the staged handoff ahead of any later traffic.
			if isSessionResetControl(msg) {
				if promoted, perr := s.PromoteDeferred(opCtx, opts.Agent, deferTriggerResume); perr != nil {
					logger.Printf("resume_promote_err agent=%s err=%v", opts.Agent, perr)
				} else if promoted > 0 {
					logger.Printf("resume_deferred_promoted agent=%s n=%d after=%s (#843)",
						opts.Agent, promoted, msg.PublicID)
				}
			}
			// #285 PR1: a bus-delivered clear is a counted context-shrink event.
			// When the agent has opted in (RespawnAfterShrinks > 0) and the count
			// reaches the threshold, respawn the chamber's process to release the
			// heap /clear can't — graceful-vs-abrupt hygiene, complementing the
			// memory-cap wrapper's host-protection ceiling (alcatraz-infra#50).
			// Fired INLINE here, right after the clear settled to stable idle: the
			// single-flight loop guarantees nothing else delivers during the
			// respawn window, and the synchronous respawn (it waits for ready)
			// completes before the loop delivers the clear macro's follow-up
			// /rename to the restarted, ready session. /compact is not counted
			// here — self-compact detection is PR2 (see isClearControl).
			if isClearControl(msg) && a.RespawnAfterShrinks > 0 {
				count, ierr := s.IncrementRespawnShrinkCount(opCtx, opts.Agent)
				if ierr != nil {
					logger.Printf("respawn_count_err agent=%s err=%v", opts.Agent, ierr)
				} else if respawnIfThresholdReached(stopCtx, s, defaultRespawnOps(), logger, opts.Agent,
					paneForDelivery, a.RelaunchCmd, "clear", count, a.RespawnAfterShrinks, watchdogPing) {
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
			delivered = true
			// delivered_in_input_box is still a `delivered` row, so it carries
			// a meaningful queued→delivered latency (#146).
			m.RecordDelivery(msg.FromAgent, opts.Agent, metrics.StateDeliveredInInputBox)
			if sec, ok := deliveryLatencySeconds(msg.CreatedAt); ok {
				m.ObserveDeliveryLatency(opts.Agent, sec)
				m.ObserveDeliveryLatencyByPriority(store.PriorityName(msg.Priority), sec)
			}
			maybeInsertFailureNotice(opCtx, s, logger,
				opts.NotifyOnDeliveredInInputBox, opts.Agent, msg,
				"delivered_in_input_box",
				"paste+Enter completed but verify token didn't surface in time")
		case errors.Is(derr, errControlUnsupported):
			// #419: a `/mcp …` control command for an adapter without the `/mcp`
			// slash command (codex). Recognised-and-skipped: mark delivered (the
			// command is consumed, not pasted, so no retry / no failure notice),
			// with a greppable WARN carrying the recipient + id + body for
			// operator-traceability when reading mailman logs after a deploy.
			logger.Printf("WARN control_command_unsupported adapter=%s agent=%s id=%s body=%q — "+
				"skipped paste (the %s CLI has no such slash command; it would land as literal "+
				"text in the prompt); marking delivered",
				active.BinaryName, opts.Agent, msg.PublicID, msg.Body, active.DisplayLabel)
			if err := s.MarkDelivered(opCtx, msg.PublicID); err != nil {
				logger.Printf("mark_delivered_err id=%s err=%v", msg.PublicID, err)
			}
			m.RecordDelivery(msg.FromAgent, opts.Agent, metrics.StateDelivered)
			if sec, ok := deliveryLatencySeconds(msg.CreatedAt); ok {
				m.ObserveDeliveryLatency(opts.Agent, sec)
			}
		case errors.Is(derr, tmuxio.ErrInputRaced):
			// #616: the final pre-paste cursor-anchored check found operator-
			// typed content in the input row — a keystroke that landed in the
			// residual TOCTOU window after the pre-paste safety probe passed.
			// Deliver did NOT paste (no prepend, no corruption). Revert to
			// queued and retry on a later cycle, once the input is clear — the
			// same revert-and-wait posture as pre_paste_safety_abort above for
			// content present AT probe time. The operator's draft is left
			// untouched (lossless); the observe-gate's stranded-draft path
			// archives it if it goes stale. delivered stays false, so no post-
			// deliver cooldown fires (nothing landed in the recipient's context).
			logger.Printf("input_raced id=%s pane=%s — operator typed in the probe→paste window; not pasted, reverting to queued for retry (#616)",
				msg.PublicID, paneForDelivery)
			m.IncPasteUnsafeAbort(opts.Agent, "input_raced")
			if _, rerr := s.RecoverDelivering(opCtx, opts.Agent); rerr != nil {
				logger.Printf("WARN input_raced_recover_failed id=%s err=%v", msg.PublicID, rerr)
			}
		case errors.Is(derr, tmuxio.ErrPriorPasteStuck):
			// #610: the pre-paste check found a prior delivery's collapsed paste
			// still unsubmitted in the codex input — a load case the #616 cursor-
			// anchor can't see (codex parks the cursor on an empty sub-line so the
			// row reads "cleared") and the per-delivery #401 resubmit budget didn't
			// drain. Deliver did NOT paste (no stacking onto the stuck message) and
			// fired one resubmit Enter to drain the prior paste across cycles. Revert
			// THIS message to queued and retry next cycle, by which point the prior
			// paste has had time to submit — the same revert-and-wait posture as the
			// input_raced case above. delivered stays false (nothing of this message
			// landed), so no post-deliver cooldown fires.
			logger.Printf("prior_paste_stuck id=%s pane=%s — prior collapsed paste unsubmitted in input; fired resubmit Enter, reverting to queued for retry (#610)",
				msg.PublicID, paneForDelivery)
			m.IncPasteUnsafeAbort(opts.Agent, "prior_paste_stuck")
			if _, rerr := s.RecoverDelivering(opCtx, opts.Agent); rerr != nil {
				logger.Printf("WARN prior_paste_stuck_recover_failed id=%s err=%v", msg.PublicID, rerr)
			}
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

		// #515: a delivery outcome (delivered / input-box / failed) just landed
		// on a publicID-keyed transition the store can't ring (no recipient
		// without a fetch). The mailman holds its serving agent, so ring here —
		// this wakes a sender blocked in waitForDelivery and a track --watch, both
		// keyed on the recipient. Fires before the cooldown sleep, but the
		// cooldown uses plain stopOrSleep (not notify-aware), so this mailman's own
		// ring can't shorten the recipient's ingest window.
		notify.Notify(opts.Agent)

		// #449 post-deliver cooldown: after a true delivery, hold the longer of
		// the inter-message delay and the cooldown so the recipient gets an
		// ingest window before the next paste. Non-deliveries use the plain delay.
		if stopOrSleep(stopCtx, postDeliverDelay(delivered, opts.InterMessageDelay, opts.PostDeliverCooldown)) {
			return exitOK
		}
	}
}

// postDeliverDelay is the pure choice for the end-of-iteration sleep (#449): a
// true delivery waits the longer of the inter-message delay and the post-deliver
// cooldown (the recipient's ingest window); anything else waits the plain delay.
func postDeliverDelay(delivered bool, interMsg, cooldown time.Duration) time.Duration {
	if delivered && cooldown > interMsg {
		return cooldown
	}
	return interMsg
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
		fmt.Sprintf("  Recover: %s stranded list  →  %s stranded show <id>", active.BinaryName, active.BinaryName),
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

// renderFailureNoticeBody formats the delivery-failure notice sent back to the
// original sender (#362). Compact-by-design: a single greppable line carrying
// the actionable essence — which message, to whom, what went wrong, and the
// recovery verb — rather than the multi-line block that cluttered the pane
// (operator feedback 2026-06-13). The full detail (original body, exact state)
// stays recoverable on demand via `track <id>` / `get <id>`, so compacting
// relocates verbosity to a query rather than dropping it.
//
// The `:warning:` prefix + trailing `resend <id>` keep it both human-legible
// and grep-parseable. failureKind is mapped to a short human headline
// ("unverified" for the delivered_in_input_box soft-fail, "failed" for a hard
// failure); the raw reason carries the specifics. resend is the universal
// recovery verb — the resend primitive replays both `failed` and
// `delivered_in_input_box` rows (#157).
func renderFailureNoticeBody(msg *store.Message, failureKind, reason string) string {
	headline := failureKind
	if failureKind == "delivered_in_input_box" {
		headline = "unverified"
	}
	return fmt.Sprintf(":warning: %s → %s %s: %s — resend %s",
		msg.PublicID, msg.ToAgent, headline, reason, msg.PublicID)
}

// freshnessAge parses a stored created_at (sqliteTimeFormat / RFC3339-like,
// fractional seconds — the same shape agents.go parses for mailman-idle) and
// returns now-created_at. ok=false on a parse failure so the caller logs it
// rather than treating an unparseable stamp as "0 age" (which would silently
// suppress every alert).
func freshnessAge(createdAt string, now time.Time) (time.Duration, bool) {
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return 0, false
	}
	return now.Sub(t), true
}

// freshnessStateLabel renders the pre-alert pane-probe outcome for the #719(A)
// notice body and log line — the observed state, or the probe error when the
// pane couldn't be classified (which is itself alert-eligible).
func freshnessStateLabel(probeState tmuxio.State, probeErr error) string {
	if probeErr != nil {
		return "probe failed (" + probeErr.Error() + ")"
	}
	return probeState.String()
}

// renderStuckChamberNoticeBody builds the operator-facing #719(A) freshness
// alert. self is the chamber whose inbound queue has stalled; oldestAt/age
// locate the oldest undelivered deliverable; probeState/probeErr report what
// the pane looked like at detection.
func renderStuckChamberNoticeBody(self, oldestAt string, age time.Duration, probeState tmuxio.State, probeErr error) string {
	return fmt.Sprintf(
		":warning: Chamber %s looks frozen — delivery has stalled\n"+
			"  Oldest undelivered message queued at %s (%s ago)\n"+
			"  Pane state at detection: %s\n"+
			"  The mailman is live but this chamber's inbound queue is not draining. "+
			"Check the pane for a modal / restart-mode prompt / hang, then clear it or `register --force`.",
		self, oldestAt, age.Round(time.Second), freshnessStateLabel(probeState, probeErr))
}

// sendStuckChamberNotice edge-fires the #719(A) freshness alert: a
// KindStuckChamberNotice from the wedged chamber (self) to the configured
// conductor. A bus insert needs no live recipient pane, so the alert reaches
// the conductor even when THIS chamber's own TUI is wedged. The kind is
// excluded from RecipientOldestPendingAt's query, so the notice cannot itself
// feed the staleness signal that produced it.
func sendStuckChamberNotice(
	ctx context.Context,
	s *store.Store,
	logger *log.Logger,
	self, conductor, oldestAt string,
	age time.Duration,
	probeState tmuxio.State,
	probeErr error,
) {
	body := renderStuckChamberNoticeBody(self, oldestAt, age, probeState, probeErr)
	res, err := s.InsertNotice(ctx, store.InsertParams{
		FromAgent: self,
		ToAgent:   conductor,
		Body:      body,
		Kind:      store.KindStuckChamberNotice,
	})
	if err != nil {
		logger.Printf("stuck_chamber_notice_failed agent=%s to=%s err=%v", self, conductor, err)
		return
	}
	logger.Printf("stuck_chamber_notice_sent agent=%s to=%s id=%s age=%s state=%s",
		self, conductor, res.PublicID, age.Round(time.Second), freshnessStateLabel(probeState, probeErr))
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
// run `tmux-tell-claude discover`); a non-live pane means the agent's session
// is gone. Both are reachability failures. A LivePanes probe error is
// itself treated as a failure with the underlying reason surfaced: we
// can't substantiate reachability, so we don't claim it (sibling to the
// pre-paste-safety "couldn't substantiate → unsafe" stance).
func pingHealthy(ctx context.Context, pane string) (reason string, ok bool) {
	if pane == "" {
		return fmt.Sprintf("agent registered but has no pane_id (run '%s discover')", active.BinaryName), false
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

// errControlUnsupported is returned by deliverOne when a KindControl command
// targets an adapter that lacks the corresponding slash command (#419, #420).
// The serve loop's outcome switch logs a structured WARN and marks the message
// delivered — the command is consumed (recognised-but-skipped), not pasted —
// mirroring the ErrUnverifiedDelivery soft-outcome shape. Which commands trigger
// it is per-adapter: the active profile's SupportedControlCommands allowlist
// (#420 generalized #419's narrow codex-`/mcp`-only case to cover `/cost` and
// any future adapter-incompatible command). adapterSupportsControl is the gate.
var errControlUnsupported = errors.New("cli: control command unsupported by this adapter")

// controlCommandToken extracts the leading whitespace-delimited token of a
// control-command body — the identifier the per-adapter capability map keys on
// ("/mcp" for "/mcp disable tmux-tell", "/compact" for "/compact"). Returns ""
// for an all-whitespace body. #420 generalizes #419's `/mcp`-only detector.
//
// Splits on the FIRST whitespace run via strings.Fields rather than a
// literal-space prefix: the meaning is "the command family", not "a command
// followed by exactly the space separator we happen to emit today". Fields
// splits on any whitespace run (space / tab / newline / multi-space), so a
// peer-constructed or future control row using a tab/newline separator keys the
// same as a space-separated one (Lookout #421 review). `/mcpfoo` keys as
// `/mcpfoo` — a distinct token — so the substring match never over-fires.
func controlCommandToken(body string) string {
	f := strings.Fields(body)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// adapterSupportsControl reports whether the active adapter's CLI implements the
// control command in body (#420). A nil SupportedControlCommands set means the
// adapter supports every control command (the reference-adapter convention —
// Claude); otherwise the body's leading token must be present in the explicit
// allowlist (codex). An unsupported command is skipped by deliverOne (returns
// errControlUnsupported) rather than pasted as literal prompt-polluting text.
func adapterSupportsControl(body string) bool {
	if active.SupportedControlCommands == nil {
		return true
	}
	return active.SupportedControlCommands[controlCommandToken(body)]
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
		// #419/#420: a control command this adapter's CLI doesn't implement (codex
		// has no `/mcp …` and no `/cost`) would land as literal text in the prompt
		// and break the session. Skip the paste; the serve-loop outcome switch logs
		// a WARN and marks it delivered (the command is consumed). The per-adapter
		// allowlist (active.SupportedControlCommands) decides; nil = supports all.
		if !adapterSupportsControl(msg.Body) {
			return errControlUnsupported
		}
		return tmuxio.SendKeys(ctx, pane, msg.Body)
	}
	// Single-paste delivery (#446 demoted #336's header-first 3-part framing):
	// the whole rendered message pastes as ONE buffer + Enter. A large body
	// that collapses in the recipient TUI (codex `[Pasted Content]`) expands on
	// submit and is handled by the #401 resubmit loop; the #336 cursor-anchor
	// verify confirms the submit regardless of collapse. The #160 byte-marker
	// length suffix still rides in the header — only the framing went.
	return tmuxio.Deliver(ctx, tmuxio.DeliverParams{
		Pane:        pane,
		Body:        render.Message(*msg, byteMarkerThreshold, time.Now()),
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

func pruneProviderDeferStart(ctx context.Context, s *store.Store, m *metrics.Metrics, provider string, deferStart map[string]time.Time) {
	if len(deferStart) == 0 {
		return
	}
	changed := false
	for publicID := range deferStart {
		msg, err := s.GetMessage(ctx, publicID)
		if errors.Is(err, store.ErrNotFound) {
			delete(deferStart, publicID)
			changed = true
			continue
		}
		if err != nil {
			continue
		}
		if msg.State != store.StateQueued {
			delete(deferStart, publicID)
			changed = true
		}
	}
	if changed {
		m.SetProviderDeferInflight(provider, float64(len(deferStart)))
	}
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

// isSessionResetControl returns true when msg is a control row that RESETS the
// recipient's session and triggers a re-render the next paste must wait out:
// `/compact` (summarise + continue) or `/clear` (#286 — discard + fresh
// session). Both leave the pane briefly "looks-idle-then-re-renders", the trap
// the post-reset stability-gate (#622) closes: without the wait, the follow-up
// row (compact→resume, or the #286 clear→rename macro) pastes mid-settle and is
// swallowed. /clear has the same settle-race shape as /compact, so it earns the
// same gate. Matched strictly on the exact body (no args) so a future
// arg-bearing sibling doesn't accidentally inherit the long pause — note the
// #286 clear macro's FIRST row is a bare `/clear` (the relabel rides the second
// `/rename …` row, which is correctly NOT a reset and does not pause).
func isSessionResetControl(msg *store.Message) bool {
	if msg.Kind != store.KindControl {
		return false
	}
	body := strings.TrimSpace(msg.Body)
	return body == "/compact" || body == "/clear"
}

// isClearControl returns true when msg is the bus clear primitive's first row —
// a bare `/clear` control (#286). This is the #285 PR1 respawn trigger: a
// bus-delivered clear is a counted context-shrink event the mailman observes on
// delivery, no detection subsystem needed (the bus IS the source of truth).
// /compact is deliberately EXCLUDED: self-compact is chamber-driven, not
// bus-delivered, so it can't be counted here — its detection (PostCompact hook +
// transcript polling) lands in PR2. The clear macro's second `/rename` row is
// not a `/clear` body, so it is correctly NOT counted — one increment per clear,
// not per macro row.
func isClearControl(msg *store.Message) bool {
	return msg.Kind == store.KindControl && strings.TrimSpace(msg.Body) == "/clear"
}

// isCompactControl returns true when msg is a bus-delivered bare `/compact`
// control row — the #730 auto-restart co-trigger. A tmux-tell-TRIGGERED /compact
// is itself the discriminator that separates "the substrate reset this session"
// from an operator-typed /exit (which never flows through a control row): only
// the former arms the per-chamber auto_restart relaunch, so operator-initiated
// exits are left alone with no escape-hatch machinery. /clear is EXCLUDED — its
// respawn path is #285 PR1 (isClearControl), a counted shrink event, not an
// exit-watch; gating on /compact keeps the two triggers from double-handling a
// single clear.
func isCompactControl(msg *store.Message) bool {
	return msg.Kind == store.KindControl && strings.TrimSpace(msg.Body) == "/compact"
}

// Stability-gate tunables for the post-/compact wait (#622). Package vars so
// tests can shrink them; production values are small (the gate polls the
// recipient's pane once per second, not a hot loop).
var (
	postCompactPollEvery      = 1 * time.Second
	postCompactStableDebounce = 2
)

// waitForStableIdle polls pane until AgentState reads StateIdle for `debounce`
// consecutive polls — the pane has settled past a /compact transition — or until
// maxWait elapses (a safety CEILING, not a target). Returns (settled, stopped):
// settled=true once stable-idle is observed; stopped=true if stopCtx cancelled
// mid-wait (the caller then exits).
//
// #622: replaces the fixed PostCompactPause timer at the post-/compact site. A
// fixed 120s under-shoots a large /compact (compaction time scales with context
// size), so the next row's paste lands mid-settle and the context-swap swallows
// it — a silent loss the input-cleared verify then false-positives as delivered
// (QM 7698). Polling for the ACTUAL stable-idle adapts to the real duration:
// faster than the fixed wait in the common (short) case, and correct past 120s
// up to the ceiling. Past the ceiling it returns settled=false and the caller
// proceeds — the pre-paste AgentState gate (defers on StateAtRestInCompaction)
// and the verify+retry layer are the backstops.
//
// Adapter-neutral: for codex StateIdle is just prompt-ready (empty
// CompactionMarker disables the compaction precedence), so the gate degrades to
// prompt-ready + debounce per the codex floor (Lookout 933d). The shared
// stably-idle primitive #616 also keys off (its pre-paste baseline-tightening).
func waitForStableIdle(stopCtx context.Context, pane string, maxWait, pollEvery, pingEvery time.Duration, debounce int) (settled, stopped bool) {
	if maxWait <= 0 || debounce <= 0 {
		return false, stopCtx.Err() != nil
	}
	deadline := time.Now().Add(maxWait)
	lastPing := time.Now()
	consecutive := 0
	for {
		probeCtx, cancel := context.WithTimeout(stopCtx, 2*time.Second)
		state, _, err := tmuxio.AgentState(probeCtx, pane)
		cancel()
		if err == nil && state == tmuxio.StateIdle {
			consecutive++
			if consecutive >= debounce {
				return true, false
			}
		} else {
			consecutive = 0
		}
		if !time.Now().Before(deadline) {
			return false, false
		}
		select {
		case <-stopCtx.Done():
			return false, true
		case <-time.After(pollEvery):
		}
		if pingEvery > 0 && time.Since(lastPing) >= pingEvery {
			_ = sdnotify.Watchdog()
			lastPing = time.Now()
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

// stopSleepOrNotify is stopOrSleep plus an early wake on a #515 doorbell: it
// returns false (keep going) the moment notifyCh fires, so an idle mailman
// re-checks its queue sub-second instead of waiting out the full fallback poll.
// A nil notifyCh (best-effort setup failed) makes that select case block
// forever, degrading cleanly to plain poll-only.
//
// Use ONLY where an early wake is correct — the no-work idle waits. The
// post-deliver cooldown and inter-message delay deliberately stay on plain
// stopOrSleep: there the timer IS the point (the recipient's ingest window,
// #449), and a doorbell ring — including this mailman's own delivery ring —
// must not cut it short.
func stopSleepOrNotify(stopCtx context.Context, d time.Duration, notifyCh <-chan struct{}) bool {
	if d <= 0 {
		return stopCtx.Err() != nil
	}
	select {
	case <-stopCtx.Done():
		return true
	case <-notifyCh:
		return false
	case <-time.After(d):
		return false
	}
}
