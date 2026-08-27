package budget_test

import (
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/budget"
)

// simulate runs a cadence through the SHIPPED defaults and reports refusals.
//
// It drives budget.Cost and budget.Refilled directly, so it cannot drift from the
// functions it calibrates — a re-implementation here would test the test.
func simulate(t *testing.T, recipients, bytes int, gap time.Duration, sends int) (refused int, end float64) {
	t.Helper()
	bal := budget.DefaultCap
	floor := budget.Overdraft(budget.DefaultCap)
	var hist []time.Time
	seenAt := map[int][]time.Time{}
	now := time.Time{}
	for i := 0; i < sends; i++ {
		if i > 0 {
			now = now.Add(gap)
			bal = budget.Refilled(bal, budget.DefaultCap, budget.DefaultRefillPerHour, gap)
		}
		prior := 0
		for r, ts := range seenAt {
			for _, at := range ts {
				if now.Sub(at) <= budget.DefaultWindow {
					prior++
					_ = r
					break
				}
			}
		}
		newDistinct := prior
		for r := 0; r < recipients; r++ {
			fresh := true
			for _, at := range seenAt[r] {
				if now.Sub(at) <= budget.DefaultWindow {
					fresh = false
					break
				}
			}
			if fresh {
				newDistinct++
			}
		}
		c := budget.Cost(prior, newDistinct, recipients, bytes, budget.DefaultAlpha)
		if bal-c < floor {
			refused++
			continue
		}
		bal -= c
		for r := 0; r < recipients; r++ {
			seenAt[r] = append(seenAt[r], now)
		}
		hist = append(hist, now)
	}
	_ = hist
	return refused, bal
}

// THE CALIBRATION IS A PROPERTY, NOT A NUMBER (#918).
//
// The shipped defaults were once cap=500/refill=80, calibrated against a cost
// function that is not the one running, and they were INERT: zero refusals across
// a full day of six-chamber churn. Nothing went red, because no test asserted that
// the budget can bind at all.
//
// MEASURED SENSITIVITY, so neither arm reads as decorative:
//
//	burst arm    RED at cap=500/refill=80  — the shipped-then-retired inert defaults
//	normal arm   RED at cap<=8             — green at cap=20, and that is CORRECT:
//	                                         a repeat directed send inside the window
//	                                         costs 0.417 (alpha only, no marginal
//	                                         breadth), so 24 of them spend ~10 total.
//
// The normal arm is insensitive to a MILD over-tightening by design — directed work
// is supposed to be nearly free. It fires when the cap approaches the cost of the
// cadence itself. I checked that it fires at all rather than assuming it; an arm
// that cannot go red is not a control.
//
// These two arms are differential. A future change to Cost, to alpha, or to the
// defaults that makes the budget inert fails the burst arm; one that makes it tax
// ordinary work fails the normal arm. Neither arm pins a magic number.
func TestCalibration_NormalWorkIsNeverRefused(t *testing.T) {
	// One recipient, a full-length message, every five minutes for two hours.
	// This is the directed working cadence #918 exists to protect.
	refused, end := simulate(t, 1, 2000, 5*time.Minute, 24)
	if refused != 0 {
		t.Errorf("normal cadence refused %d of 24 sends; the budget is taxing ordinary work", refused)
	}
	if end < 0 {
		t.Errorf("normal cadence ended overdrawn at %.1f; it should not even approach the floor", end)
	}
}

func TestCalibration_SustainedBroadcastIsRefused(t *testing.T) {
	// Six recipients, long body, every thirty seconds. This is the shape that
	// consumed the allowance; the budget MUST bind on it.
	refused, _ := simulate(t, 6, 4000, 30*time.Second, 60)
	if refused == 0 {
		t.Fatal("sustained 6-recipient broadcast was never refused — the budget is INERT, " +
			"which is the exact defect the shipped cap=500/refill=80 had (#918, @bosun)")
	}
}

// The gap between the two arms is what makes them a control rather than a pair of
// assertions: if a single setting satisfied both trivially they would not
// discriminate. Assert the burst is refused MUCH harder than normal work.
func TestCalibration_DiscriminatesBetweenTheTwo(t *testing.T) {
	normal, _ := simulate(t, 1, 2000, 5*time.Minute, 24)
	burst, _ := simulate(t, 6, 4000, 30*time.Second, 60)
	if !(normal == 0 && burst > 10) {
		t.Errorf("defaults do not discriminate: normal refused=%d, burst refused=%d", normal, burst)
	}
}
