package budget

import (
	"math"
	"testing"
	"time"
)

const eps = 1e-9

// #918 as filed asserted that splitting a fan-out into singles "costs the same".
// Under the as-filed per-send function it was 42% cheaper at r=3 and 65% at r=8.
// This is the arm that pins the property the marginal form was adopted to buy: it
// must hold at EVERY breadth, because the as-filed leak GREW with breadth — an arm
// that only checked r=2 would have passed on the function this replaces.
func TestSplittingIsExactlyNeutral(t *testing.T) {
	for _, alpha := range []float64{0, 0.25, 1.0, 3.0} {
		for _, r := range []int{2, 3, 5, 8, 12} {
			one := Cost(0, r, r, 1500, alpha)
			var split float64
			for i := 0; i < r; i++ {
				split += Cost(i, i+1, 1, 1500, alpha)
			}
			if math.Abs(one-split) > eps {
				t.Errorf("alpha=%.2f r=%d: one-send %.6f vs split %.6f (delta %.6f)",
					alpha, r, one, split, split-one)
			}
		}
	}
}

// The four representative costs #918 published, against a FRESH window. If these
// move, the tracker's calibration section is describing a different function.
func TestFreshWindowMatchesTheFiledCosts(t *testing.T) {
	for _, c := range []struct {
		recipients, bytes int
		want              float64
	}{
		{1, 400, 1.133}, {1, 1500, 1.5}, {3, 1500, 7.794}, {8, 2000, 37.712},
	} {
		got := Cost(0, c.recipients, c.recipients, c.bytes, 0)
		if math.Abs(got-c.want) > 0.01 {
			t.Errorf("%d recipients %d bytes: got %.3f want %.3f", c.recipients, c.bytes, got, c.want)
		}
	}
}

// 🔑 THE ARM alpha EXISTS FOR. With alpha=0 the marginal form makes a repeat
// message to someone already inside the window cost ZERO — volume becomes free in
// exactly the clique case #918 was written about. Removing the alpha term
// reopens that hole while TestSplittingIsExactlyNeutral still passes, so this is
// the only arm standing between the two.
func TestRepeatingInsideTheWindowIsNotFree(t *testing.T) {
	if got := Cost(3, 3, 1, 1500, 0); got != 0 {
		t.Fatalf("precondition: with alpha=0 a repeat must be free, got %.3f", got)
	}
	got := Cost(3, 3, 1, 1500, DefaultAlpha)
	if got <= 0 {
		t.Fatalf("with alpha=%.2f a repeat must cost something, got %.3f", DefaultAlpha, got)
	}
	// A repeat costs the alpha term alone: alpha*(1+bytes/3000). The VALUE of
	// alpha is defended in refill.go against the measured distortion it causes;
	// this arm pins only that a repeat is charged the alpha term and nothing else.
	want := DefaultAlpha * (1 + 1500.0/LengthDivisor)
	if math.Abs(got-want) > eps {
		t.Errorf("repeat cost %.3f, want the alpha term %.3f", got, want)
	}
}

func TestRefillCapsAndHalvesWhileNegative(t *testing.T) {
	if got := Refilled(400, 500, 80, time.Hour*10); got != 500 {
		t.Errorf("refill must cap at 500, got %.2f", got)
	}
	// Negative balances recover at half rate.
	if got := Refilled(-40, 500, 80, time.Minute*30); math.Abs(got-(-20)) > eps {
		t.Errorf("-40 for 30min at 80/hr half-rate should reach -20, got %.2f", got)
	}
	// Crossing zero switches to the full rate for the remainder, so the two legs
	// are computed separately rather than one rate applied to the whole span.
	// -40 at the 40/hr recovery rate reaches zero in exactly 1h, so 1h lands on 0
	// and 1h30 lands on 40 — the second leg runs at the FULL rate.
	if got := Refilled(-40, 500, 80, time.Hour); math.Abs(got) > eps {
		t.Errorf("-40 for 1h at half-rate should reach exactly 0, got %.2f", got)
	}
	if got := Refilled(-40, 500, 80, time.Hour+30*time.Minute); math.Abs(got-40) > eps {
		t.Errorf("-40 for 1h30: 1h to zero then 30min at 80/hr = 40, got %.2f", got)
	}
}

func TestOverdraftFloorAndTimeToAfford(t *testing.T) {
	capacity := 500.0
	if Overdraft(capacity) != -125 {
		t.Fatalf("overdraft floor must be -25%% of cap, got %.2f", Overdraft(capacity))
	}
	if !Affordable(-100, 20, capacity) {
		t.Error("a send landing at -120 is above the floor and must pass")
	}
	if Affordable(-100, 30, capacity) {
		t.Error("a send landing at -130 is below the floor and must be refused")
	}
	if d := TimeToAfford(-100, 20, capacity, 80); d != 0 {
		t.Errorf("an affordable send has no wait, got %v", d)
	}
	if d := TimeToAfford(-100, 30, capacity, 80); d <= 0 {
		t.Errorf("an unaffordable send must name a wait, got %v", d)
	}
}
