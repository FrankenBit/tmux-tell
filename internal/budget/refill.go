package budget

import "time"

// Defaults from #918. All four are configurable; these are the calibrated
// starting points.
//
// ⚠️ CALIBRATION CAVEAT. Cap and RefillPerHour were derived by replaying
// messages.db against the AS-FILED cost function (per-send r^1.5, alpha
// implicitly 0). This package ships the marginal-window form with alpha=1.0,
// which prices every send higher — so "binds on exactly the three who churned and
// never touches the six who worked normally" is a result about a function that is
// no longer the one running. The replay at alpha=1.0 is owed (Bosun); these values
// are deliberately LEFT AS MEASURED rather than silently rescaled to fit a new
// function, because a rescaled default would carry the authority of a measurement
// nobody made.
const (
	DefaultCap            = 500.0
	DefaultRefillPerHour  = 80.0
	DefaultOverdraftRatio = -0.25 // of cap: a send passes if it leaves balance above this
	// DefaultAlpha is the per-delivery volume floor. It exists to stop a repeat
	// message to someone already inside the window costing ZERO; see Cost.
	//
	// 🔑 WHY 0.25 AND NOT 1.0. alpha is LINEAR in deliveries while the breadth term
	// is SUPER-linear, so alpha's share of a send is largest where breadth is
	// smallest. Measured against the as-filed function at 1500 bytes:
	//
	//	alpha=1.0   r=1 costs 2.00x as-filed   r=8 costs 1.35x
	//	alpha=0.25  r=1 costs 1.25x            r=8 costs 1.09x
	//
	// alpha=1.0 therefore taxes the CHEAP path hardest — the directed 1-recipient
	// send #918 exists to protect, and the shape pilot and carpenter shipped real
	// work in. That inverts the design intent. 0.25 keeps repeats non-free (0.38 at
	// 1500 bytes) while leaving the calibrated costs close enough that the replay
	// is a check rather than a re-derivation.
	DefaultAlpha  = 0.25
	DefaultWindow = 15 * time.Minute

	// RecoveryFactor halves the refill rate while the balance is negative, so
	// overdrawing costs more than the amount overdrawn.
	RecoveryFactor = 0.5
)

// Refilled returns the balance after `elapsed` of continuous refill, capped at
// cap. While the balance is negative the rate is multiplied by RecoveryFactor —
// the recovery is deliberately slower than the spend.
//
// Refill is LAZY: nothing runs in the background. A balance is stored with the
// time it was last touched and brought forward on read, so a chamber that has not
// sent for a day is not carrying a day of scheduler wakeups.
func Refilled(balance, capacity, ratePerHour float64, elapsed time.Duration) float64 {
	if elapsed <= 0 || ratePerHour <= 0 {
		return min(balance, capacity)
	}
	hours := elapsed.Hours()
	if balance >= 0 {
		return min(balance+ratePerHour*hours, capacity)
	}
	// Negative: refill at the recovery rate until it reaches zero, then at the
	// full rate for whatever time is left. Computing it in two legs rather than
	// applying one rate to the whole span is what makes the crossing exact.
	toZero := -balance / (ratePerHour * RecoveryFactor)
	if hours <= toZero {
		return balance + ratePerHour*RecoveryFactor*hours
	}
	return min(ratePerHour*(hours-toZero), capacity)
}

// Overdraft returns the floor a send may not push the balance below.
func Overdraft(capacity float64) float64 { return capacity * DefaultOverdraftRatio }

// Affordable reports whether spending cost leaves the balance at or above the
// overdraft floor.
//
// A send is allowed to push the balance NEGATIVE — down to the floor — so that a
// sender who is nearly empty can still deliver one more message rather than being
// cut off mid-thought. What it cannot do is start from below the floor, and the
// recovery rate above makes that position expensive to hold.
func Affordable(balance, cost, capacity float64) bool {
	return balance-cost >= Overdraft(capacity)
}

// TimeToAfford returns how long until `cost` becomes affordable from `balance`,
// or 0 if it already is. Used by the refusal message: a refusal that does not say
// when the sender may try again invites a retry loop.
func TimeToAfford(balance, cost, capacity, ratePerHour float64) time.Duration {
	if Affordable(balance, cost, capacity) {
		return 0
	}
	if ratePerHour <= 0 {
		return 0
	}
	need := cost + Overdraft(capacity) - balance
	rate := ratePerHour
	if balance < 0 {
		rate *= RecoveryFactor
	}
	return time.Duration(need / rate * float64(time.Hour))
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
