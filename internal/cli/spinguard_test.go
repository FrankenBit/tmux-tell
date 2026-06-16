package cli

import (
	"testing"
	"time"
)

// TestSpinGuard_TripsOnBurst pins the core invariant: more than `threshold`
// no-progress iterations packed into a single `window` trips the guard. This is
// the pure decision the serve-loop wraps in a panic — tested directly here so
// the panic stays unit-verifiable without crashing the test (#496).
func TestSpinGuard_TripsOnBurst(t *testing.T) {
	g := &spinGuard{threshold: 5, window: 10 * time.Second}
	base := time.Unix(0, 0)
	// 5 no-progress iterations within the window: count climbs 1..5, no trip.
	for i := 0; i < 5; i++ {
		if g.record(false, base.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("tripped early at iteration %d (count=%d)", i, g.count)
		}
	}
	// The 6th no-progress iteration (count → 6 > threshold 5) inside the window trips.
	if !g.record(false, base.Add(6*time.Millisecond)) {
		t.Fatalf("did not trip on the (threshold+1)-th no-progress iteration; count=%d", g.count)
	}
}

// TestSpinGuard_IdleLoopNeverTrips models a healthy idle loop: each no-progress
// iteration is spaced past the window, so the window resets every iteration and
// the count never accumulates toward the threshold — exactly the property that
// keeps a sleeping idle serve-loop from panicking.
func TestSpinGuard_IdleLoopNeverTrips(t *testing.T) {
	g := &spinGuard{threshold: 5, window: 10 * time.Second}
	base := time.Unix(0, 0)
	// 100 no-progress iterations, each 11s apart (> window): never trips.
	for i := 0; i < 100; i++ {
		if g.record(false, base.Add(time.Duration(i)*11*time.Second)) {
			t.Fatalf("idle loop tripped at iteration %d (count=%d) — window should have reset", i, g.count)
		}
		if g.count != 1 {
			t.Errorf("iteration %d: count=%d, want 1 (each spaced iteration starts a fresh window)", i, g.count)
		}
	}
}

// TestSpinGuard_ProgressResets shows a single progress iteration zeroes the
// counter, so an almost-spinning burst that makes progress before the threshold
// never trips.
func TestSpinGuard_ProgressResets(t *testing.T) {
	g := &spinGuard{threshold: 3, window: time.Second}
	base := time.Unix(0, 0)
	// 3 fast no-progress iterations (count → 3, not yet > 3).
	for i := 0; i < 3; i++ {
		if g.record(false, base.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("tripped early at %d", i)
		}
	}
	// Progress resets the counter.
	if g.record(true, base.Add(3*time.Millisecond)) {
		t.Fatal("progress iteration must not trip")
	}
	if g.count != 0 {
		t.Errorf("count=%d after progress, want 0", g.count)
	}
	// Another 3 fast no-progress iterations: count climbs 1..3, still no trip.
	for i := 4; i < 7; i++ {
		if g.record(false, base.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("tripped at %d after reset (count=%d); progress should have bought a fresh budget", i, g.count)
		}
	}
}

// TestSpinGuard_WindowBoundaryResets pins the sliding-window edge: a burst that
// straddles the window boundary resets rather than accumulating across windows.
func TestSpinGuard_WindowBoundaryResets(t *testing.T) {
	g := &spinGuard{threshold: 5, window: 10 * time.Second}
	base := time.Unix(0, 0)
	// 5 no-progress within the first window (count → 5, no trip).
	for i := 0; i < 5; i++ {
		if g.record(false, base.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("tripped early at %ds", i)
		}
	}
	// Next iteration is > window past windowStart (0s) → fresh window, count → 1.
	if g.record(false, base.Add(11*time.Second)) {
		t.Fatal("should not trip — the window elapsed, counter resets")
	}
	if g.count != 1 {
		t.Errorf("count=%d after window reset, want 1", g.count)
	}
}

// TestSpinGuard_DisabledThreshold confirms threshold<=0 is the escape hatch:
// the guard never trips regardless of iteration rate.
func TestSpinGuard_DisabledThreshold(t *testing.T) {
	for _, th := range []int{0, -1} {
		g := &spinGuard{threshold: th, window: time.Second}
		base := time.Unix(0, 0)
		for i := 0; i < 10_000; i++ {
			if g.record(false, base) { // all at the same instant — maximal spin
				t.Fatalf("threshold=%d should disable the guard; tripped at %d", th, i)
			}
		}
	}
}
