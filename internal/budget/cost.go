// Package budget prices bus traffic so that directed, concise, small-circle
// messages at a reasonable cadence almost never hit a limit, while frequent long
// messages to half the crew exhaust the voice (tmux-tell#918).
//
// The pure cost and refill arithmetic lives here with no I/O, so every property
// below is unit-testable without a store, a clock or a message.
package budget

import "math"

// BreadthExponent makes breadth super-linear and length sub-linear: breadth
// compounds through replies (each reply to N people is itself an N-recipient
// message), length does not. Taxing length as hard as breadth would push senders
// to drop the caveats and the "here is what I did NOT check" lines, which is
// where most of this crew's value sits.
const BreadthExponent = 1.5

// LengthDivisor sets how sub-linearly length is charged: a message of exactly
// this many bytes costs twice a zero-length one.
const LengthDivisor = 3000.0

// breadth is the windowed breadth term W(n) = n^1.5.
func breadth(distinctRecipients int) float64 {
	if distinctRecipients <= 0 {
		return 0
	}
	return math.Pow(float64(distinctRecipients), BreadthExponent)
}

// Cost prices one send.
//
// # Breadth is MARGINAL over a window, not per-send
//
//	cost = [W(after) - W(before)] * (1 + bytes/LengthDivisor)   ... breadth
//	     + alpha * deliveries     * (1 + bytes/LengthDivisor)   ... volume
//
// where W(n) = n^BreadthExponent over the DISTINCT recipients this sender has
// addressed inside the window.
//
// 🔑 WHY MARGINAL AND NOT PER-SEND. #918 as filed charged r^1.5 against a single
// send's recipient count and asserted that "splitting one 3-recipient send into
// three 1-recipient sends costs the same — closing the obvious evasion." It does
// not: measured, splitting was 42% cheaper at r=3 and 65% cheaper at r=8, so the
// evasion was MOST profitable exactly where the mechanism tried hardest to charge.
// The leak is structural rather than a tuning error — any function super-linear in
// PER-SEND recipients has it, because splitting drives that count to 1, the
// cheapest point on the curve. Charging the marginal window increase instead makes
// splitting cost the identical amount: A then B then C charges 1 + 1.83 + 2.37 =
// 5.196 = 3^1.5, and one 3-recipient send charges 5.196.
//
// 🔑 WHY alpha EXISTS — DO NOT REMOVE IT AS A FUDGE FACTOR. The marginal form
// closes the splitting leak and opens its mirror: once a recipient is inside the
// window their breadth is already paid, so W(after) == W(before) and further
// messages to them cost ZERO. That makes volume free precisely in the clique case
// #918 was written about, and silently retires the tracker's own requirement that
// length be charged "explicitly to stop message inflation over time".
//
// The alpha*deliveries term restores a per-delivery floor. It is breadth-INVARIANT
// — three singles and one triple both carry three deliveries — so it costs nothing
// in the splitting neutrality just bought. Removing it reopens the volume hole
// while every splitting test still passes, which is why this comment is long.
//
// newDistinct is the window's distinct-recipient count AFTER this send; prior is
// the count BEFORE it. deliveries is the number of rows this send creates.
func Cost(prior, newDistinct, deliveries, bytes int, alpha float64) float64 {
	if deliveries <= 0 {
		return 0
	}
	lengthFactor := 1 + float64(bytes)/LengthDivisor
	marginalBreadth := breadth(newDistinct) - breadth(prior)
	if marginalBreadth < 0 {
		marginalBreadth = 0
	}
	return (marginalBreadth + alpha*float64(deliveries)) * lengthFactor
}
