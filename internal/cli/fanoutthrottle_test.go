package cli

import (
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/provider"
)

// anthropic/codex defaults, named here so the tests read against the policy
// rather than hard-coded magic numbers (and fail loudly if the defaults move).
var (
	aThr = poolThrottles[provider.Anthropic].threshold
	aDel = poolThrottles[provider.Anthropic].delay
	cThr = poolThrottles[provider.OpenAI].threshold
)

// TestPoolKeysMatchAdapterProviders is the #597 regression guard: the throttle
// pool keys MUST be the `provider` values the adapters write, or a pool's
// recipients silently fall through to unknownThrottle (the bug — codex keyed
// "openai-codex" matched nothing → unknown throttle instead of its own). Since
// #600 the adapter literals live in internal/provider, so this test enumerates
// provider.All() as the source of truth: every known provider MUST resolve to
// its own non-unknown throttle, and no pool key may drift off the canonical set.
func TestPoolKeysMatchAdapterProviders(t *testing.T) {
	cases := []struct {
		provider string // a provider constant (internal/provider)
		wantThr  int
		wantDel  time.Duration
	}{
		{provider.Anthropic, 5, 400 * time.Millisecond}, // cmd/tmux-tell-claude/main.go
		{provider.OpenAI, 2, 1500 * time.Millisecond},   // cmd/tmux-tell-codex/main.go
	}
	for _, c := range cases {
		got, throttled := throttleForPool(normPool(c.provider))
		if !throttled {
			t.Errorf("provider %q resolves to no-throttle, want its own throttle", c.provider)
			continue
		}
		if got.threshold == unknownThrottle.threshold && got.delay == unknownThrottle.delay {
			t.Errorf("provider %q fell through to unknownThrottle — the pool key does not match the adapter literal (the #597 bug)", c.provider)
		}
		if got.threshold != c.wantThr || got.delay != c.wantDel {
			t.Errorf("provider %q throttle = {%d, %v}, want {%d, %v}", c.provider, got.threshold, got.delay, c.wantThr, c.wantDel)
		}
	}

	// Enumerator drift-guard: every canonical provider MUST have its own
	// non-unknown throttle. Adding a provider constant without a poolThrottles
	// entry fails here — the co-addition #600 makes structural.
	for _, p := range provider.All() {
		if got, throttled := throttleForPool(normPool(p)); !throttled ||
			(got.threshold == unknownThrottle.threshold && got.delay == unknownThrottle.delay) {
			t.Errorf("provider.All() member %q has no dedicated throttle — add a poolThrottles entry", p)
		}
	}

	// And no pool key may drift off the canonical set (a stray key is dead code).
	known := map[string]bool{}
	for _, p := range provider.All() {
		known[p] = true
	}
	for key := range poolThrottles {
		if !known[key] {
			t.Errorf("poolThrottles has key %q not in provider.All() — dead key (the #597 drift class)", key)
		}
	}
}

// TestFanoutStaggerOffsets_BurstThenStagger pins the core per-pool rule: the
// first `threshold` recipients in a pool get offset 0 (the sustainable burst),
// each one past that adds the pool's delay.
func TestFanoutStaggerOffsets_BurstThenStagger(t *testing.T) {
	pools := make([]string, aThr+3) // threshold + 3 excess
	for i := range pools {
		pools[i] = "anthropic"
	}
	got := fanoutStaggerOffsets(pools)
	for i := 0; i < aThr; i++ {
		if got[i] != 0 {
			t.Errorf("burst recipient %d: offset = %v, want 0", i, got[i])
		}
	}
	for j := 0; j < 3; j++ {
		i := aThr + j
		want := time.Duration(j+1) * aDel
		if got[i] != want {
			t.Errorf("excess recipient %d: offset = %v, want %v", i, got[i], want)
		}
	}
}

// TestFanoutStaggerOffsets_BelowThresholdNoStagger: a pool at or under its
// threshold never staggers (the common small fan-out stays instant).
func TestFanoutStaggerOffsets_BelowThresholdNoStagger(t *testing.T) {
	pools := make([]string, cThr) // exactly the codex threshold
	for i := range pools {
		pools[i] = "openai"
	}
	for i, off := range fanoutStaggerOffsets(pools) {
		if off != 0 {
			t.Errorf("recipient %d: offset = %v, want 0 (at-threshold pool must not stagger)", i, off)
		}
	}
}

// TestFanoutStaggerOffsets_PoolsIndependent: mixed pools stagger independently;
// a recipient in a below-threshold pool is NOT delayed by a sibling pool that is
// staggering. Offsets are per-pool, so the single codex recipient stays at 0
// while the over-threshold anthropic pool staggers.
func TestFanoutStaggerOffsets_PoolsIndependent(t *testing.T) {
	pools := []string{}
	for i := 0; i < aThr+2; i++ {
		pools = append(pools, "anthropic")
	}
	pools = append(pools, "openai") // 1 codex, below its threshold
	got := fanoutStaggerOffsets(pools)

	// codex recipient (last) is below threshold → 0.
	if got[len(got)-1] != 0 {
		t.Errorf("lone codex recipient: offset = %v, want 0 (its pool is below threshold)", got[len(got)-1])
	}
	// anthropic excess still staggers as if codex weren't there.
	if got[aThr] != aDel || got[aThr+1] != 2*aDel {
		t.Errorf("anthropic excess offsets = %v, %v; want %v, %v", got[aThr], got[aThr+1], aDel, 2*aDel)
	}
}

// TestFanoutStaggerOffsets_UnknownPoolTightest: an empty provider maps to the
// unknown pool with the tightest throttle (threshold 1) — fails safe.
func TestFanoutStaggerOffsets_UnknownPoolTightest(t *testing.T) {
	got := fanoutStaggerOffsets([]string{"", "", ""})
	if got[0] != 0 {
		t.Errorf("unknown[0] = %v, want 0 (one bursts)", got[0])
	}
	if got[1] != unknownThrottle.delay || got[2] != 2*unknownThrottle.delay {
		t.Errorf("unknown excess = %v, %v; want %v, %v", got[1], got[2], unknownThrottle.delay, 2*unknownThrottle.delay)
	}
}

// TestFanoutStaggerOffsets_NoThrottlePool: ollama never staggers (GPU-time
// back-pressure, not a token window).
func TestFanoutStaggerOffsets_NoThrottlePool(t *testing.T) {
	pools := []string{"ollama", "ollama", "ollama", "ollama", "ollama"}
	for i, off := range fanoutStaggerOffsets(pools) {
		if off != 0 {
			t.Errorf("ollama recipient %d: offset = %v, want 0 (no-throttle pool)", i, off)
		}
	}
}

// TestFanoutStaggerOffsets_Cap clamps a pathological same-pool fan-out so the
// last recipients can't be pushed arbitrarily far out.
func TestFanoutStaggerOffsets_Cap(t *testing.T) {
	n := aThr + int(maxFanoutStagger/aDel) + 10 // enough excess to exceed the cap
	pools := make([]string, n)
	for i := range pools {
		pools[i] = "anthropic"
	}
	got := fanoutStaggerOffsets(pools)
	for i, off := range got {
		if off > maxFanoutStagger {
			t.Errorf("recipient %d: offset = %v exceeds cap %v", i, off, maxFanoutStagger)
		}
	}
	if got[n-1] != maxFanoutStagger {
		t.Errorf("last recipient: offset = %v, want clamp to %v", got[n-1], maxFanoutStagger)
	}
	// Make the past-cap degradation explicit (Surveyor #586 review): every
	// recipient whose uncapped offset would be >= the cap bunches at offset=cap,
	// so they wake together at t=cap. Count them — this is the one place the t=0
	// "≤ threshold simultaneous" invariant relaxes, and it's bounded-latency
	// (a single delayed batch at +10s), not an unbounded cascade. offset =
	// excess*delay with excess = k - threshold + 1 (k = per-pool index, == position
	// here since all one pool), so offset hits the cap at k = threshold-1 + cap/delay.
	firstAtCap := aThr - 1 + int(maxFanoutStagger/aDel)
	wantBunched := n - firstAtCap
	bunched := 0
	for _, off := range got {
		if off == maxFanoutStagger {
			bunched++
		}
	}
	if bunched != wantBunched {
		t.Errorf("recipients bunched at the cap = %d, want %d (past-cap overflow wakes together at t=cap)", bunched, wantBunched)
	}
}

// TestFanoutStaggerOffsets_AntiCascadeInvariant is the #580 worked-instance: an
// 8-chamber broadcast (the jam-wrap incident shape — 6 anthropic + 2 codex)
// must NOT wake more than each pool's threshold simultaneously. Asserts the
// anti-cascade property directly: at most `threshold` recipients per pool share
// offset 0, and any excess is spaced by at least the pool's delay.
func TestFanoutStaggerOffsets_AntiCascadeInvariant(t *testing.T) {
	// The actual jam-wrap recipients: lookout + carpenter are codex, the rest
	// anthropic.
	pools := []string{
		"anthropic", // engineer
		"anthropic", // shipwright
		"openai",    // carpenter
		"anthropic", // pilot
		"anthropic", // herald
		"anthropic", // quartermaster
		"anthropic", // surveyor
		"openai",    // lookout
	}
	got := fanoutStaggerOffsets(pools)

	// Count simultaneous (offset == 0) per pool; must be <= that pool's threshold.
	zeroByPool := map[string]int{}
	for i, p := range pools {
		if got[i] == 0 {
			zeroByPool[p]++
		}
	}
	if zeroByPool["anthropic"] > aThr {
		t.Errorf("%d anthropic recipients wake simultaneously, want <= threshold %d (cascade not prevented)",
			zeroByPool["anthropic"], aThr)
	}
	if zeroByPool["openai"] > cThr {
		t.Errorf("%d codex recipients wake simultaneously, want <= threshold %d", zeroByPool["openai"], cThr)
	}
	// 6 anthropic > threshold 5 → exactly the 6th is staggered by one delay.
	staggered := 0
	for i, p := range pools {
		if p == "anthropic" && got[i] > 0 {
			staggered++
			if got[i] < aDel {
				t.Errorf("staggered anthropic recipient %d: offset %v < delay %v", i, got[i], aDel)
			}
		}
	}
	if staggered != 1 {
		t.Errorf("anthropic staggered count = %d, want 1 (6 recipients, threshold 5)", staggered)
	}
}
