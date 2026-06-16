package cli

import (
	"fmt"
	"time"
)

// spinGuard enforces the "steady-state serve-loop is bounded" invariant (#496).
//
// A healthy mailman serve-loop sleeps on every no-progress iteration — the
// paused / stuck / idle-empty / claim-error paths all `stopOrSleep` — so it
// iterates at most a few times per second. A spin bug (a path that loops
// without sleeping, whether a real defect or an upstream-library mutation)
// iterates thousands of times per second. The guard counts consecutive
// no-progress iterations within a sliding window and reports a violation when
// the count exceeds the threshold inside that window. The caller panics on a
// violation, so a spin **fails loud** — systemd's `Restart=on-failure` recovers
// the process and the panic + stack lands in the journal — rather than silently
// burning CPU while messages queue behind the stuck worker.
//
// `record` is pure (the caller passes `now`), so the spin decision is unit-
// tested directly without crashing the test on the panic the caller wraps it
// in. This mirrors the pure-core / thin-IO-shell split used elsewhere in the
// substrate (e.g. tools/changelog-assemble).
type spinGuard struct {
	threshold   int           // max no-progress iterations allowed within window (<=0 disables)
	window      time.Duration // sliding window
	count       int           // consecutive no-progress iterations in the current window
	windowStart time.Time     // when the current no-progress window began
}

// record is called once per serve-loop iteration. progress is true when the
// iteration did real work (claimed a message — a delivery or a ping). It returns
// true iff the no-progress count has exceeded threshold within window (the loop
// is spinning), at which point the caller panics.
//
// A progress iteration resets the counter. A no-progress iteration either starts
// a fresh window (first one, or the prior window has fully elapsed — so a slow
// idle loop never accumulates toward the threshold) or increments within the
// current window. Only a burst of >threshold no-progress iterations packed into
// a single window trips the guard.
func (g *spinGuard) record(progress bool, now time.Time) bool {
	if g.threshold <= 0 {
		return false // disabled (escape hatch, mirrors stuck-threshold<=0)
	}
	if progress {
		g.count = 0
		return false
	}
	if g.count == 0 || now.Sub(g.windowStart) > g.window {
		g.count = 1
		g.windowStart = now
		return false
	}
	g.count++
	return g.count > g.threshold
}

// spinPanicMessage renders the greppable panic string for a tripped guard.
func spinPanicMessage(agent string, g *spinGuard) string {
	return fmt.Sprintf(
		"serve-loop spin guard tripped: agent=%s iterated %d no-progress times within %s "+
			"(threshold=%d) — likely a spin bug; failing loud for systemd restart (#496)",
		agent, g.count, g.window, g.threshold)
}
