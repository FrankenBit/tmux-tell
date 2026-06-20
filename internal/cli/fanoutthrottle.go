package cli

import (
	"context"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// Internal fan-out throttle (#580). When one sender fans a message to N
// recipients (the to:[array] multi-send path, #158), each recipient's insert
// fires a doorbell that wakes its mailman, which paste-and-enters and triggers
// an LLM API call. If N recipients in the same usage pool wake within the same
// token-quota window, they cascade into provider rate-limiting at the recipient
// layer — the symptom the operator hit on the 2026-06-19 jam-wrap 8-chamber
// broadcast.
//
// This is the INTERNAL (send-time) rate-limit layer, distinct from the EXTERNAL
// (delivery-time) provider-cap + wake-jitter (#448/#543). They share ONE pool
// key — the agent's `provider` (#448), already set by each adapter at serve
// start — so the two layers group recipients identically and can't drift. The
// internal layer spaces the INSERTS (and thus the wakes) before any paste; the
// external layer reacts to a rate-limit banner after a paste.
//
// Pools are asymmetric: the crew's large shared Anthropic subscription sustains
// a wide concurrent-wake burst, the small OpenAI-Codex per-account pool a narrow
// one, and a single-user Ollama GPU has no rate quota at all (its back-pressure
// is GPU time, not a token window). A uniform rate would under-throttle the tight
// pool and over-throttle the loose one, so the throttle is PER-POOL and the
// aggregate cost is max-across-pools, not sum-across-recipients.

// poolThrottle is one pool's fan-out policy. threshold is the sustainable
// concurrent-wake burst: the first `threshold` recipients in a pool insert
// immediately (offset 0); each recipient past that adds `delay` of stagger.
type poolThrottle struct {
	threshold int
	delay     time.Duration
}

// poolThrottles holds the per-pool defaults. These are v1 sketched values
// (Bosun-ratified 2026-06-20); tuning is empirical — if a pool's cadence
// misbehaves, file a follow-up with Loki evidence rather than guessing here.
// The pool key is the agent's `provider` (#448).
var poolThrottles = map[string]poolThrottle{
	"anthropic":    {threshold: 5, delay: 400 * time.Millisecond},
	"openai-codex": {threshold: 2, delay: 1500 * time.Millisecond},
}

// unknownPool is the fail-safe pool for a recipient whose provider is unset
// (mailman not yet started, or an adapter that doesn't declare one). It uses the
// tightest throttle — fails safe (over-throttle an unknown) rather than open.
const unknownPool = "unknown"

var unknownThrottle = poolThrottle{threshold: 1, delay: 1500 * time.Millisecond}

// noThrottlePools never stagger: their back-pressure isn't a token-quota window.
// Ollama is single-user GPU time — staggering its sends would add latency for no
// rate-limit gain.
var noThrottlePools = map[string]bool{"ollama": true}

// maxFanoutStagger bounds the total synchronous stagger so a pathological
// fan-out can't hang the send tool unboundedly. A recipient whose computed
// offset exceeds this is clamped to it (degraded but bounded — beyond the cap,
// recipients release together rather than ever-later).
const maxFanoutStagger = 10 * time.Second

// normPool maps an empty provider to the fail-safe unknown pool.
func normPool(provider string) string {
	if provider == "" {
		return unknownPool
	}
	return provider
}

// throttleForPool returns a pool's throttle policy, or ok=false when the pool
// never throttles (no-throttle pools).
func throttleForPool(pool string) (poolThrottle, bool) {
	if noThrottlePools[pool] {
		return poolThrottle{}, false
	}
	if t, ok := poolThrottles[pool]; ok {
		return t, true
	}
	return unknownThrottle, true
}

// fanoutStaggerOffsets computes each recipient's release offset from send-start,
// given each recipient's pool (provider; "" → unknown), in recipient order.
//
// Within a pool, the first `threshold` recipients get offset 0 (the sustainable
// burst), and each one past that adds the pool's `delay`. Offsets are
// absolute-from-start, so a tight pool's small offsets elapse while a large pool
// is still staggering — different pools overlap and the total wall-clock is
// ≈ max-across-pools, not the sum. Each offset is clamped to maxFanoutStagger.
//
// The guaranteed invariant at t=0: at most `threshold` recipients in any one pool
// share offset 0, so at most `threshold` of that pool wake simultaneously — the
// rest are spaced. That is the anti-cascade property. The one exception is the
// cap: recipients whose computed offset exceeds maxFanoutStagger all clamp to it
// and so bunch (wake together) at t=cap. That is bounded-latency degradation, not
// a cascade regression — it only bites a pool large enough to overflow the cap
// (~30 anthropic recipients at the 10s/400ms defaults, well past crew scale), and
// past-cap bunching at +10s is strictly milder than the at-source simultaneous
// burst this prevents.
//
// Pure + deterministic (no clock), so the spacing is unit-testable without timing.
func fanoutStaggerOffsets(pools []string) []time.Duration {
	offsets := make([]time.Duration, len(pools))
	seen := make(map[string]int, len(pools))
	for i, p := range pools {
		pool := normPool(p)
		k := seen[pool]
		seen[pool]++
		t, throttled := throttleForPool(pool)
		if !throttled {
			continue // offset 0
		}
		excess := k - t.threshold + 1
		if excess <= 0 {
			continue // within the burst → offset 0
		}
		off := time.Duration(excess) * t.delay
		if off > maxFanoutStagger {
			off = maxFanoutStagger
		}
		offsets[i] = off
	}
	return offsets
}

// resolveFanoutOffsets reads each recipient's pool (the agent's #448 provider)
// and computes the per-recipient stagger offsets. A recipient whose provider
// can't be read (unregistered, or a store hiccup) maps to the unknown pool —
// fail-safe to the tightest throttle rather than skipping the stagger.
func resolveFanoutOffsets(ctx context.Context, s *store.Store, recipients []string) []time.Duration {
	pools := make([]string, len(recipients))
	for i, to := range recipients {
		provider, _, err := s.ProviderCapConfig(ctx, to)
		if err != nil {
			provider = "" // → unknown pool (tightest), fail-safe
		}
		pools[i] = provider
	}
	return fanoutStaggerOffsets(pools)
}

// fanoutStaggerWait blocks until start+offset, or returns early on ctx
// cancellation. A no-op when the offset has already elapsed (offset 0, or a
// later recipient whose small-pool offset passed while a larger pool staggered).
// Indirected through a var so tests can record the requested waits without
// real-time sleeping.
var fanoutStaggerWait = func(ctx context.Context, start time.Time, offset time.Duration) {
	d := offset - time.Since(start)
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// fanoutNow returns the stagger reference time. Indirected for tests.
var fanoutNow = time.Now
