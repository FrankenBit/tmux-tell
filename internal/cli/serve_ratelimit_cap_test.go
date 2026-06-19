package cli

import (
	"context"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// pinJitterSource swaps the wake-jitter source to a fixed value for the test and
// restores it afterward. Pins the [0,1) draw so the priority-ordering and
// cap-extension assertions are deterministic.
func pinJitterSource(t *testing.T, v float64) {
	t.Helper()
	prev := setRateLimitJitterSourceForTest(func() float64 { return v })
	t.Cleanup(func() { setRateLimitJitterSourceForTest(prev) })
}

// steppingJitterSource returns a source that yields a distinct value on each
// call (1/(n+1), 2/(n+1), …) — models independent draws by N chambers waking
// from the same rate-limit event, so the stagger test does not depend on the
// nondeterministic production source.
func steppingJitterSource(t *testing.T, n int) {
	t.Helper()
	i := 0
	prev := setRateLimitJitterSourceForTest(func() float64 {
		i++
		return float64(i) / float64(n+1)
	})
	t.Cleanup(func() { setRateLimitJitterSourceForTest(prev) })
}

// TestRateLimitWakeDelay_StaggersSimultaneousWakes is the #543 AC3 load-bearing
// pin: N chambers rate-limited on the same provider, same priority, same base
// backoff must NOT all wake on the same tick. The priority-biased jitter spreads
// them. Mutation anchor: drop the `+ rateLimitWakeJitter(...)` term in
// rateLimitWakeDelay and every chamber collapses to `base` — this test fails
// with "want 8 distinct wake times, got 1".
func TestRateLimitWakeDelay_StaggersSimultaneousWakes(t *testing.T) {
	const n = 8
	steppingJitterSource(t, n)
	base := 10 * time.Second

	seen := make(map[time.Duration]struct{}, n)
	for i := 0; i < n; i++ {
		// providerCap=0 → no cap read; isolates the jitter spread.
		d := rateLimitWakeDelay(context.Background(), nil, base, store.PriorityNormal, "anthropic", 0, 0, 0)
		seen[d] = struct{}{}
	}
	if len(seen) != n {
		t.Errorf("want %d distinct wake times (no thundering herd), got %d", n, len(seen))
	}
}

// TestRateLimitWakeDelay_PriorityOrdersWakes pins #543 AC2: higher priority =
// smaller jitter window = wakes sooner. Pinning the draw to its window edge
// (1.0) makes the ordering exact: high gets base+0.25·base, normal base+0.5·base,
// low base+1.0·base.
func TestRateLimitWakeDelay_PriorityOrdersWakes(t *testing.T) {
	pinJitterSource(t, 1.0) // worst case: each lands at its window edge
	base := 8 * time.Second

	high := rateLimitWakeDelay(context.Background(), nil, base, store.PriorityHigh, "anthropic", 0, 0, 0)
	normal := rateLimitWakeDelay(context.Background(), nil, base, store.PriorityNormal, "anthropic", 0, 0, 0)
	low := rateLimitWakeDelay(context.Background(), nil, base, store.PriorityLow, "anthropic", 0, 0, 0)

	if high >= normal || normal >= low {
		t.Errorf("want high < normal < low wake delays; got high=%s normal=%s low=%s", high, normal, low)
	}
	// Exact windows at the edge draw.
	if want := base + time.Duration(rateLimitJitterFracHigh*float64(base)); high != want {
		t.Errorf("high wake delay = %s, want %s", high, want)
	}
	if want := base + time.Duration(rateLimitJitterFracLow*float64(base)); low != want {
		t.Errorf("low wake delay = %s, want %s", low, want)
	}
}

// TestRateLimitWakeJitter_NeverEarlierThanBase pins Fork-1: the jitter is
// additive and non-negative across the full draw range and every priority, so a
// chamber never wakes EARLIER than the provider's backoff floor.
func TestRateLimitWakeJitter_NeverEarlierThanBase(t *testing.T) {
	base := 5 * time.Second
	for _, draw := range []float64{0.0, 0.5, 0.999} {
		pinJitterSource(t, draw)
		for _, p := range []int{store.PriorityLow, store.PriorityNormal, store.PriorityHigh} {
			if j := rateLimitWakeJitter(base, p); j < 0 {
				t.Errorf("jitter negative (draw=%v priority=%d): %s", draw, p, j)
			}
			total := rateLimitWakeDelay(context.Background(), nil, base, p, "anthropic", 0, 0, 0)
			if total < base {
				t.Errorf("wake delay %s < base %s (draw=%v priority=%d) — woke earlier than floor", total, base, draw, p)
			}
		}
	}
	// A zero base yields zero jitter (no negative or runaway value).
	pinJitterSource(t, 0.999)
	if j := rateLimitWakeJitter(0, store.PriorityNormal); j != 0 {
		t.Errorf("jitter for zero base = %s, want 0", j)
	}
}

// TestRateLimitWakeDelay_CapSaturatedExtends pins #543 AC1: when the provider is
// already at its working-cap, the wake delay is extended by one recheck interval
// so the chamber does not wake straight into a #448 cap-defer; under the cap it
// is not extended. Jitter pinned to 0 to isolate the cap term.
func TestRateLimitWakeDelay_CapSaturatedExtends(t *testing.T) {
	pinJitterSource(t, 0.0)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	base := 4 * time.Second
	ttl := 30 * time.Second
	recheck := 1 * time.Second
	const capLimit = 3

	// Saturated: 3 anthropic workers == cap → extend by recheck.
	seedWorkers(t, s, "anthropic", "w1", "w2", "w3")
	got := rateLimitWakeDelay(context.Background(), s, base, store.PriorityNormal, "anthropic", capLimit, ttl, recheck)
	if want := base + recheck; got != want {
		t.Errorf("saturated wake delay = %s, want %s (base + one recheck)", got, want)
	}

	// A different provider is not saturated (independent pool) → no extension.
	got = rateLimitWakeDelay(context.Background(), s, base, store.PriorityNormal, "openai", capLimit, ttl, recheck)
	if got != base {
		t.Errorf("under-cap (other provider) wake delay = %s, want %s (no extension)", got, base)
	}

	// providerCap <= 0 disables the cap read entirely even when saturated.
	got = rateLimitWakeDelay(context.Background(), s, base, store.PriorityNormal, "anthropic", 0, ttl, recheck)
	if got != base {
		t.Errorf("cap-off wake delay = %s, want %s (no cap read)", got, base)
	}
}

// TestJitterFractionForPriority_MonotoneAndFutureTier pins the window mapping:
// strictly narrowing with priority, and a future tier above High (e.g. an
// "urgent" weight) maps to the tightest window via the >= bound, not the default.
func TestJitterFractionForPriority_MonotoneAndFutureTier(t *testing.T) {
	high := jitterFractionForPriority(store.PriorityHigh)
	normal := jitterFractionForPriority(store.PriorityNormal)
	low := jitterFractionForPriority(store.PriorityLow)
	if high >= normal || normal >= low {
		t.Errorf("want frac(high) < frac(normal) < frac(low); got %v, %v, %v", high, normal, low)
	}
	if got := jitterFractionForPriority(store.PriorityHigh + 10); got != high {
		t.Errorf("future above-High tier frac = %v, want tightest window %v (>= bound, not default)", got, high)
	}
	if got := jitterFractionForPriority(store.PriorityLow - 10); got != low {
		t.Errorf("below-Low tier frac = %v, want widest window %v (<= bound)", got, low)
	}
}
