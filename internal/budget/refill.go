package budget

import "time"

// Defaults from #918. All four are configurable; these are the calibrated
// starting points.
//
// ✅ CALIBRATED AGAINST THE SHIPPED FUNCTION (#918, re-derived 2026-08-27).
//
// An earlier revision shipped cap=500/refill=80, derived by replaying against the
// AS-FILED cost function (per-send r^1.5, alpha implicitly 0). @bosun ran the
// replay this comment said was owed and found those values INERT on the shipped
// marginal-window form: zero refusals across 689 sends of six-chamber churn, every
// chamber ending at a full balance. A default carries the authority of a
// measurement, and that one had been measured against a function no longer running.
//
// Re-derived by replaying messages.db through THIS package's Cost and Refilled —
// not a re-implementation, so the calibration cannot drift from the code it
// calibrates. Refusal rate by day:
//
//	cap/refill   08-24   08-25   08-27   08-26   08-19
//	              quiet   quiet   today    busy  incident
//	500/80        0.0%    0.0%    0.0%    0.0%     0.0%   ← inert, the old default
//	150/20        0.0%    0.0%    0.0%   14.0%    12.8%
//	120/16        0.0%    0.0%    0.0%   17.4%    24.0%   ← shipped
//	100/14        0.0%    0.0%    1.5%   20.9%    31.9%
//	 80/10        0.0%    0.0%    6.1%   30.9%    47.0%
//
// 🔑 TWO QUIET DAYS ARE THE CONTROL AND THEY HOLD AT EVERY SETTING TRIED. A budget
// that taxes normal work is worse than none; 08-24 and 08-25 stay at 0.0% down to
// 80/10, so the discrimination is real rather than an artifact of a gentle cap.
//
// 120/16 is chosen over the harder settings because at 100/14 the incident day puts
// TWO chambers past 50% refusals, which is past "shapes behaviour" into "breaks the
// day". At 120/16 today's two heaviest senders still end at 8 and -8 — the mechanism
// is live and the next burst binds — while nobody is refused mid-work.
//
// ⚠️ WHAT THIS REPLAY ASSUMES, and it changes the numbers by more than the cap does:
// a fan-out is ONE send with N deliveries, grouped by sender+body within 2s. That
// matches the shipped path (store/budget.go passes len(p.Recipients) to Cost), but
// an UNGROUPED replay prices each leg as a 1-recipient send, the breadth term never
// engages, and the same settings read far gentler. 55% of rows on 08-19 are fan-out
// legs. @bosun's independent replay was ungrouped and reports 5.3%/10.7% where this
// one reports 17.4%/24.0% — the shape agrees, the magnitudes do not, and the
// grouping is the whole difference. Cited so the next person re-deriving these knows
// which question they must answer first.
//
// ⚠️ NOT varied: alpha, the window, the overdraft ratio. Four days, one host.
const (
	DefaultCap            = 120.0
	DefaultRefillPerHour  = 16.0
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
