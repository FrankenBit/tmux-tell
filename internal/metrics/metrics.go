// Package metrics is the tmux-msg Prometheus instrumentation surface (#146).
// It wraps a private *prometheus.Registry and the substrate's collector set
// behind a small typed API the mailman daemon calls at the delivery-state-
// write, queue-depth, loop, and paste-unsafe boundaries.
//
// Layering: this is a leaf package — it imports only client_golang, never
// internal/store or internal/tmuxio. The mailman (cmd/tmux-tell-claude) owns
// the wiring (which boundary increments which collector); tmuxio reports
// verify timing back through a callback rather than importing this package,
// so the low-level paste layer stays metrics-agnostic.
//
// Nil-safety is the load-bearing ergonomic: every Record/Observe/Set/Inc
// method is a no-op on a nil *Metrics. The mailman holds a possibly-nil
// handle (nil when --metrics-addr is absent, the no-behavior-change default
// for existing deploys) and calls the methods unconditionally — no per-call
// `if metricsEnabled` branch at the hot path. A disabled mailman pays one
// nil-pointer compare per call and nothing else.
//
// Cardinality note: every metric labels by agent name (from/to/recipient/
// agent). The fleet is a fixed small set of named chambers (Bosun, Pilot,
// Surveyor, Engineer, …), not user-generated identifiers, so the label
// space is bounded — the talk-pair heatmap (from × to) is the whole point
// of #146 and the chamber roster keeps it from exploding.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// State label values for tmux_tell_messages_total. They mirror the durable
// delivery outcomes the mailman writes (#169): a verified delivery, the
// delivered_in_input_box soft-fail (paste+Enter landed but the verify token
// never surfaced), and a hard failure. These are the metric's stable wire
// surface — they intentionally use the codebase's current vocabulary
// (delivered_in_input_box, renamed from the pre-#140 delivered_unverified),
// not the older name in #146's original exposition sketch.
const (
	StateDelivered           = "delivered"
	StateDeliveredInInputBox = "delivered_in_input_box"
	StateFailed              = "failed"
)

// latencyBuckets covers the queued→delivered span. Deliveries are usually
// sub-second, but the observe-gate can hold a message up to its 5-minute
// MaxWait and the post-compact pause adds ~120s, so the buckets reach 600s
// to keep the tail (and the latency heatmap) resolvable rather than piling
// into +Inf.
var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// verifyBuckets covers the post-Enter verify-retry loop, whose production
// budget is ~5s (six backoff steps in tmuxio.Deliver). The finer sub-5s
// resolution + a few buckets past 5s let #153 see where the observed
// verify-attempt mass lands relative to the current budget when it decides
// whether the default needs recalibrating (the metric is defined HERE and
// shared with #153 to avoid double-instrumentation).
var verifyBuckets = []float64{0.1, 0.25, 0.5, 1, 1.5, 2, 3, 4, 5, 6, 8, 10}

// Metrics is the tmux-msg collector set bound to a private registry. Build
// one with New; pass it the lifetime of a mailman run. A nil *Metrics is a
// valid, fully no-op instance — see the package doc.
type Metrics struct {
	reg *prometheus.Registry

	messagesTotal              *prometheus.CounterVec
	deliveryLatency            *prometheus.HistogramVec
	deliveryLatencyByPriority  *prometheus.HistogramVec
	verifyAttempt              *prometheus.HistogramVec
	queueDepth                 *prometheus.GaugeVec
	loopIterations             *prometheus.CounterVec
	pasteUnsafeAborts          *prometheus.CounterVec
	mailmanStuck               *prometheus.GaugeVec
	providerDefer              *prometheus.CounterVec
	providerDeferInflight      *prometheus.GaugeVec
	providerDeferWait          *prometheus.HistogramVec
	chamberRateLimited         *prometheus.GaugeVec
	chamberRateLimitRetryAfter *prometheus.GaugeVec
	chamberUsageLimited        *prometheus.GaugeVec
	rateLimitTotal             *prometheus.CounterVec
	copymodeDefer              *prometheus.CounterVec
	copymodeDeferWait          *prometheus.HistogramVec
}

// New builds the collector set, registers it against a fresh private
// registry (not the global default — so multiple mailmen in one process,
// as in tests, never collide on duplicate registration), and returns the
// handle. The registry is reachable via Handler for the /metrics endpoint.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		messagesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tmux_tell_messages_total",
			Help: "Total messages the mailman drove to a terminal delivery outcome, by sender, recipient, and outcome state.",
		}, []string{"from", "to", "state"}),
		deliveryLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tmux_tell_delivery_latency_seconds",
			Help:    "Wall-clock from a message being queued to it reaching the delivered state, per recipient.",
			Buckets: latencyBuckets,
		}, []string{"recipient"}),
		deliveryLatencyByPriority: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tmux_tell_delivery_latency_by_priority_seconds",
			Help:    "Wall-clock queued→delivered latency by message priority (#449) — low / normal / high.",
			Buckets: latencyBuckets,
		}, []string{"priority"}),
		verifyAttempt: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tmux_tell_delivery_verify_attempt_seconds",
			Help:    "Wall-clock spent in the post-Enter verify-token retry loop, per recipient (shared with #153 budget calibration).",
			Buckets: verifyBuckets,
		}, []string{"recipient"}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tmux_tell_queue_depth",
			Help: "Current count of queued (undelivered) messages addressed to the agent, sampled each mailman loop iteration.",
		}, []string{"agent"}),
		loopIterations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tmux_tell_mailman_loop_iterations_total",
			Help: "Total mailman serve-loop iterations for the agent — a liveness + cadence signal.",
		}, []string{"agent"}),
		pasteUnsafeAborts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tmux_tell_paste_unsafe_aborts_total",
			Help: "Total pre-paste TOCTOU aborts: the gate passed, then the pane became paste-unsafe before paste, by agent and reason. Steady-state rate_limited/usage_limited parks are counted by their gauges, not here.",
		}, []string{"agent", "reason"}),
		mailmanStuck: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tmux_tell_mailman_stuck",
			Help: "1 when the mailman is parked in the #291 stuck state (stopped probing tmux), 0 when clear. Labels: agent name and stuck reason (pane-not-found).",
		}, []string{"agent", "reason"}),
		providerDefer: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tmux_tell_provider_defer_total",
			Help: "Total deliveries deferred by the #448 per-provider concurrency cap (too many same-provider chambers working), by provider.",
		}, []string{"provider"}),
		providerDeferInflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tmux_tell_provider_defer_inflight",
			Help: "Current count of messages held by the #448 per-provider concurrency cap in this mailman process, by provider.",
		}, []string{"provider"}),
		providerDeferWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tmux_tell_provider_defer_wait_seconds",
			Help:    "Wall-clock a cap-deferred message waited from its first #448 provider-cap deferral until the cap slot reopened and it was delivered (#507), by provider.",
			Buckets: latencyBuckets,
		}, []string{"provider"}),
		chamberRateLimited: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tmux_tell_chamber_rate_limited_seconds",
			Help: "Live age of a chamber's rate-limited state, by agent and provider. Present-at-zero when rate-limit detection is configured; zero means not currently rate-limited.",
		}, []string{"agent", "provider"}),
		chamberRateLimitRetryAfter: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tmux_tell_chamber_rate_limit_retry_after_seconds",
			Help: "Live seconds remaining until the next retry after the chamber's last rate-limit observation, by agent and provider. Zero means the banner did not expose a parseable retry_seconds capture or the retry window has elapsed.",
		}, []string{"agent", "provider"}),
		chamberUsageLimited: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tmux_tell_chamber_usage_limited_seconds",
			Help: "Live age of a chamber's usage-limited state, by agent and provider. Present-at-zero when usage-limit detection is configured; zero means not currently usage-limited.",
		}, []string{"agent", "provider"}),
		rateLimitTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tmux_tell_rate_limit_total",
			Help: "Total rate-limit / usage-limit episodes detected from the pane banner, by cause (overloaded ← StateRateLimited #504 transient throttle; quota_exceeded ← StateUsageLimited #540 hard-stop park), agent, and provider. One increment per episode-start (first-detection transition), not per poll. Cumulative complement to the live chamber_rate_limited / chamber_usage_limited gauges — gives rate() a counter to differentiate over.",
		}, []string{"cause", "agent", "provider"}),
		copymodeDefer: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tmux_tell_copymode_defer_total",
			Help: "Total delivery cycles deferred because the recipient pane was scrolled up in tmux copy-mode (#526), by agent. One increment per gate cycle that observed copy-mode (delivered-on-exit or reverted-at-MaxWait).",
		}, []string{"agent"}),
		copymodeDeferWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tmux_tell_copymode_defer_wait_seconds",
			Help:    "Per-gate-cycle wall-clock a delivery waited on copy-mode — from the first copy-mode observation in a cycle until that cycle resolved (delivered on return-to-live, or reverted at MaxWait), by agent (#526). NOT a per-message total: a read outlasting MaxWait records one ~MaxWait sample per revert-retry cycle.",
			Buckets: latencyBuckets,
		}, []string{"agent"}),
	}
	reg.MustRegister(
		m.messagesTotal,
		m.deliveryLatency,
		m.deliveryLatencyByPriority,
		m.verifyAttempt,
		m.queueDepth,
		m.loopIterations,
		m.pasteUnsafeAborts,
		m.mailmanStuck,
		m.providerDefer,
		m.providerDeferInflight,
		m.providerDeferWait,
		m.chamberRateLimited,
		m.chamberRateLimitRetryAfter,
		m.chamberUsageLimited,
		m.rateLimitTotal,
		m.copymodeDefer,
		m.copymodeDeferWait,
	)
	return m
}

// Handler returns an HTTP handler serving the registry's exposition in the
// Prometheus text format. Returns a 503 handler on a nil *Metrics so a
// caller that wires the endpoint without a registry fails visibly rather
// than panicking.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "metrics disabled", http.StatusServiceUnavailable)
		})
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry exposes the private registry for tests that want to Gather and
// assert on raw collector state. Returns nil on a nil *Metrics.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

// RecordDelivery increments tmux_tell_messages_total for one terminal
// delivery outcome. state should be one of the State* constants.
func (m *Metrics) RecordDelivery(from, to, state string) {
	if m == nil {
		return
	}
	m.messagesTotal.WithLabelValues(from, to, state).Inc()
}

// ObserveDeliveryLatency records a queued→delivered duration (seconds) for
// the recipient. Callers should skip non-delivered outcomes (a failed
// message has no meaningful delivery latency).
func (m *Metrics) ObserveDeliveryLatency(recipient string, seconds float64) {
	if m == nil {
		return
	}
	m.deliveryLatency.WithLabelValues(recipient).Observe(seconds)
}

// ObserveDeliveryLatencyByPriority records a queued→delivered duration (seconds)
// labeled by message priority (#449 — "low" / "normal" / "high").
func (m *Metrics) ObserveDeliveryLatencyByPriority(priority string, seconds float64) {
	if m == nil {
		return
	}
	m.deliveryLatencyByPriority.WithLabelValues(priority).Observe(seconds)
}

// ObserveVerifyAttempt records the wall-clock spent in the post-Enter
// verify-token retry loop for the recipient (#146/#153). Fed by the
// tmuxio.Deliver OnVerify callback.
func (m *Metrics) ObserveVerifyAttempt(recipient string, seconds float64) {
	if m == nil {
		return
	}
	m.verifyAttempt.WithLabelValues(recipient).Observe(seconds)
}

// SetQueueDepth sets the current queued-message gauge for the agent.
func (m *Metrics) SetQueueDepth(agent string, depth float64) {
	if m == nil {
		return
	}
	m.queueDepth.WithLabelValues(agent).Set(depth)
}

// IncLoopIteration bumps the serve-loop iteration counter for the agent.
func (m *Metrics) IncLoopIteration(agent string) {
	if m == nil {
		return
	}
	m.loopIterations.WithLabelValues(agent).Inc()
}

// IncProviderDefer bumps the provider-cap deferral counter (#448) for the
// provider whose concurrency cap deferred a delivery.
func (m *Metrics) IncProviderDefer(provider string) {
	if m == nil {
		return
	}
	m.providerDefer.WithLabelValues(provider).Inc()
}

// SetProviderDeferInflight sets the standing count of messages currently held
// by the provider cap in this mailman process (#520). Callers should set it
// from the serve-loop deferStart map length so the gauge mirrors the gate's
// actual runtime state.
func (m *Metrics) SetProviderDeferInflight(provider string, count float64) {
	if m == nil {
		return
	}
	m.providerDeferInflight.WithLabelValues(provider).Set(count)
}

// SetChamberRateLimited sets the current age of a chamber's rate-limited state
// (#504 PR2). Callers should refresh it while the rate-limit wait is active;
// zero means not currently rate-limited.
func (m *Metrics) SetChamberRateLimited(agent, provider string, seconds float64) {
	if m == nil {
		return
	}
	m.chamberRateLimited.WithLabelValues(agent, provider).Set(seconds)
}

// SetChamberRateLimitRetryAfter sets the live seconds remaining until the next
// retry surfaced by the rate-limit regex. Zero means the banner did not expose
// a parseable retry_seconds capture or the retry window has elapsed.
func (m *Metrics) SetChamberRateLimitRetryAfter(agent, provider string, seconds float64) {
	if m == nil {
		return
	}
	m.chamberRateLimitRetryAfter.WithLabelValues(agent, provider).Set(seconds)
}

// SetChamberUsageLimited sets the current age of a chamber's usage-limited
// state (#540). Callers should refresh it while the park-until-reset wait is
// active; zero means not currently usage-limited.
func (m *Metrics) SetChamberUsageLimited(agent, provider string, seconds float64) {
	if m == nil {
		return
	}
	m.chamberUsageLimited.WithLabelValues(agent, provider).Set(seconds)
}

// IncRateLimit bumps the cumulative rate-limit-episode counter for the agent
// (#613). cause is the detected state: "overloaded" (StateRateLimited #504, a
// transient throttle) or "quota_exceeded" (StateUsageLimited #540, a hard-stop
// park). Called once per episode-start (first-detection transition), so each
// increment is one distinct rate-limit episode, not one poll. Pairs with the
// structured rate_limit_event Loki log line emitted at the same transition.
func (m *Metrics) IncRateLimit(agent, provider, cause string) {
	if m == nil {
		return
	}
	m.rateLimitTotal.WithLabelValues(cause, agent, provider).Inc()
}

// ObserveProviderDeferWait records how long a cap-deferred message waited
// (seconds) from its first provider-cap deferral until the slot reopened and it
// was delivered (#507). Fed once per deferred message, at the gate-pass that
// ends its deferral — complements the IncProviderDefer count with a wait
// distribution per provider.
func (m *Metrics) ObserveProviderDeferWait(provider string, seconds float64) {
	if m == nil {
		return
	}
	m.providerDeferWait.WithLabelValues(provider).Observe(seconds)
}

// InitCopyModeDefer materializes the copy-mode deferral counter series for the
// agent at 0 so the metric is present in exposition from mailman startup,
// before any scroll-read event (the present-at-zero idiom, #531/#526). Add(0)
// touches the labeled series without incrementing it; without this the
// dashboard shows "no data" instead of a flat 0 until the first deferral.
func (m *Metrics) InitCopyModeDefer(agent string) {
	if m == nil {
		return
	}
	m.copymodeDefer.WithLabelValues(agent).Add(0)
}

// IncCopyModeDefer bumps the copy-mode deferral counter (#526) for the agent
// whose pane was scrolled up when a delivery cycle observed copy-mode. Fed
// once per gate cycle that saw copy-mode, complementing ObserveCopyModeDeferWait.
func (m *Metrics) IncCopyModeDefer(agent string) {
	if m == nil {
		return
	}
	m.copymodeDefer.WithLabelValues(agent).Inc()
}

// ObserveCopyModeDeferWait records how long a single gate cycle waited on
// copy-mode (seconds) from its first copy-mode observation until the cycle
// resolved — delivered on return-to-live or reverted at MaxWait (#526). Fed
// from GateOutcome.CopyModeWait, by agent. Per-gate-cycle grain, not a
// per-message total (see the GateOutcome.CopyModeWait doc-comment).
func (m *Metrics) ObserveCopyModeDeferWait(agent string, seconds float64) {
	if m == nil {
		return
	}
	m.copymodeDeferWait.WithLabelValues(agent).Observe(seconds)
}

// IncPasteUnsafeAbort bumps the paste-unsafe-abort counter for the agent
// with a stable reason label (awaiting_operator | compaction | unknown |
// probe_failed | drift_*).
func (m *Metrics) IncPasteUnsafeAbort(agent, reason string) {
	if m == nil {
		return
	}
	m.pasteUnsafeAborts.WithLabelValues(agent, reason).Inc()
}

// SetMailmanStuck sets tmux_tell_mailman_stuck for the agent (#300).
// parked=true sets the gauge to 1 (mailman is parked); parked=false sets it
// to 0 (mailman resumed). reason is the stuck reason string from the store
// (store.StuckReasonPaneNotFound = "pane-not-found"); callers must pass the
// same reason on both the set and clear call so the label values match.
func (m *Metrics) SetMailmanStuck(agent, reason string, parked bool) {
	if m == nil {
		return
	}
	v := 0.0
	if parked {
		v = 1.0
	}
	m.mailmanStuck.WithLabelValues(agent, reason).Set(v)
}
