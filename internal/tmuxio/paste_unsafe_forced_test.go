package tmuxio

import "testing"

// TestIsPasteUnsafeForced_ExcludesExactlyRateLimitFamily pins the #558 predicate
// split: IsPasteUnsafeForced (the --force-rate-limited pre-paste predicate) must
// differ from IsPasteUnsafe by EXACTLY the rate-limit family {StateRateLimited,
// StateUsageLimited}. Every content-corrupting state stays unsafe under force;
// the rate-limit family is the only thing force waves through; safe states stay
// safe. This is what guarantees force is narrow — it can't punch through
// copy-mode / popup / unknown / compaction.
func TestIsPasteUnsafeForced_ExcludesExactlyRateLimitFamily(t *testing.T) {
	all := []State{
		StateIdle, StateWorking, StateAwaitingOperator, StateUnknown,
		StateAtRestInCompaction, StateInCopyMode, StateRateLimited, StateUsageLimited,
	}
	for _, s := range all {
		forced := IsPasteUnsafeForced(s)
		full := IsPasteUnsafe(s)
		rateFamily := s == StateRateLimited || s == StateUsageLimited
		// IsPasteUnsafe == IsPasteUnsafeForced OR rate-family, for every state.
		if full != (forced || rateFamily) {
			t.Errorf("state %s: IsPasteUnsafe=%v, want forced(%v)||rateFamily(%v)", s, full, forced, rateFamily)
		}
		// The rate-limit family is the exact set difference: full but not forced.
		if rateFamily && (forced || !full) {
			t.Errorf("rate-family %s: forced=%v full=%v, want forced=false full=true", s, forced, full)
		}
	}

	// Content-corrupting states stay paste-unsafe even when forced.
	for _, s := range []State{StateAwaitingOperator, StateUnknown, StateAtRestInCompaction, StateInCopyMode} {
		if !IsPasteUnsafeForced(s) {
			t.Errorf("content-corrupting state %s must stay paste-unsafe under force", s)
		}
	}
	// Safe states stay safe under both predicates.
	for _, s := range []State{StateIdle, StateWorking} {
		if IsPasteUnsafeForced(s) || IsPasteUnsafe(s) {
			t.Errorf("state %s must be paste-safe", s)
		}
	}
}
