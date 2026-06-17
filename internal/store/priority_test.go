package store

import "testing"

func TestParsePriority(t *testing.T) {
	cases := map[string]int{"": PriorityNormal, "low": PriorityLow, "normal": PriorityNormal, "high": PriorityHigh, " High ": PriorityHigh}
	for in, want := range cases {
		got, err := ParsePriority(in)
		if err != nil || got != want {
			t.Errorf("ParsePriority(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParsePriority("urgent"); err == nil {
		t.Errorf("ParsePriority(urgent) should reject (3-level only)")
	}
}

// TestSelectScheduled_MaxPriority_UniformIsFIFO is the load-bearing property
// that drove the A-default decision (#449): under uniform priority, Strategy A
// reduces to plain global FIFO (lowest id wins), so no-priority traffic is
// unchanged — across channels and within them.
func TestSelectScheduled_MaxPriority_UniformIsFIFO(t *testing.T) {
	// Two channels (alice, bob) all normal-priority; ids interleaved.
	cs := []claimCandidate{
		{ID: 3, FromAgent: "bob", Priority: PriorityNormal},
		{ID: 1, FromAgent: "alice", Priority: PriorityNormal},
		{ID: 2, FromAgent: "bob", Priority: PriorityNormal},
		{ID: 4, FromAgent: "alice", Priority: PriorityNormal},
	}
	got, ok := selectScheduled(cs, StrategyMaxPriority)
	if !ok || got != 1 {
		t.Errorf("uniform-priority A: chose id %d (ok=%v), want 1 (global FIFO head)", got, ok)
	}
}

// TestSelectScheduled_MaxPriority_BumpsBuriedHighPrio: a high-priority message
// buried behind normal ones in alice's channel lifts alice's channel above
// bob's normal-only channel — even though bob's head is older. The chosen
// message is alice's HEAD (FIFO within the channel preserved), not the buried
// high-prio itself.
func TestSelectScheduled_MaxPriority_BumpsBuriedHighPrio(t *testing.T) {
	cs := []claimCandidate{
		{ID: 1, FromAgent: "bob", Priority: PriorityNormal},   // older, but normal-only channel
		{ID: 2, FromAgent: "alice", Priority: PriorityNormal}, // alice head
		{ID: 3, FromAgent: "alice", Priority: PriorityHigh},   // buried high-prio lifts alice's channel
	}
	got, ok := selectScheduled(cs, StrategyMaxPriority)
	if !ok || got != 2 {
		t.Errorf("chose id %d (ok=%v), want 2 (alice's HEAD — channel bumped by its buried high-prio, FIFO preserved)", got, ok)
	}
}

// TestSelectScheduled_Aged_FavorsLongestUnderUniform pins Strategy B's
// distinguishing (and surprising) behavior: under uniform priority it favors the
// LONGEST channel, NOT global FIFO — the exact property that made A the better
// default. bob has 3 messages, alice 1; B picks bob's head despite alice's being
// older.
func TestSelectScheduled_Aged_FavorsLongestUnderUniform(t *testing.T) {
	cs := []claimCandidate{
		{ID: 1, FromAgent: "alice", Priority: PriorityNormal}, // oldest overall, but short channel
		{ID: 2, FromAgent: "bob", Priority: PriorityNormal},
		{ID: 3, FromAgent: "bob", Priority: PriorityNormal},
		{ID: 4, FromAgent: "bob", Priority: PriorityNormal}, // bob channel weight = 20*3 = 60 > alice 20*1
	}
	got, _ := selectScheduled(cs, StrategyAged)
	if got != 2 {
		t.Errorf("aged: chose id %d, want 2 (bob's head — longest-channel bias). Contrast A would pick 1.", got)
	}
	// And A on the same input picks the global-FIFO head (1) — the documented contrast.
	if a, _ := selectScheduled(cs, StrategyMaxPriority); a != 1 {
		t.Errorf("max-priority on same input chose %d, want 1 (global FIFO)", a)
	}
}

// TestSelectScheduled_WithinChannelFIFO confirms the invariant directly: the
// chosen id within the winning channel is always the lowest (head), regardless
// of where the high-priority message sits.
func TestSelectScheduled_WithinChannelFIFO(t *testing.T) {
	// Single channel; high-prio is NOT at the head. Head must still be chosen.
	cs := []claimCandidate{
		{ID: 10, FromAgent: "alice", Priority: PriorityLow},
		{ID: 11, FromAgent: "alice", Priority: PriorityHigh},
	}
	for _, strat := range []SchedulerStrategy{StrategyMaxPriority, StrategyAged} {
		got, _ := selectScheduled(cs, strat)
		if got != 10 {
			t.Errorf("strategy %d violated within-channel FIFO: chose %d, want 10 (head)", strat, got)
		}
	}
}

func TestSelectScheduled_Empty(t *testing.T) {
	if _, ok := selectScheduled(nil, StrategyMaxPriority); ok {
		t.Errorf("empty candidates should return ok=false")
	}
}
