package store

import (
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestObservedStateWorkingMatchesTmuxioStateWorking pins the cross-package
// string equivalence the #448 provider cap silently depends on (#510).
//
// The cap counts "working" chambers with
//
//	CountWorkingOnProvider → WHERE observed_state = observedStateWorking
//
// where observedStateWorking is a store-local const ("working"). The mailman
// populates that column by writing tmuxio.State.String() for the live pane
// state. The two are coupled only by a string literal living in two packages:
// the store never imports tmuxio (by design — see provider.go), so a rename of
// the StateWorking rendering would compile cleanly while the cap query stopped
// matching ANY row. The failure mode is invisible — no panic, no error, just a
// cap that never gates because its count is permanently zero.
//
// This test is the seam that turns that silent drift into a build-time failure:
// if either side renames, this fails with a message naming both literals.
func TestObservedStateWorkingMatchesTmuxioStateWorking(t *testing.T) {
	if got := tmuxio.StateWorking.String(); got != observedStateWorking {
		t.Fatalf("provider cap counts on observed_state=%q but tmuxio.StateWorking renders %q — "+
			"the cap query would match zero rows and silently stop gating (#448/#510). "+
			"Keep the store const and tmuxio.State.String() in sync.",
			observedStateWorking, got)
	}
}

// TestStateInCopyModeIsNotWorkingSentinel pins the #526 D3 invariant (the
// #510 discipline applied to the new copy-mode state). The mailman writes
// AgentState().String() into observed_state (serve.go), so "copy-mode" is now
// a persisted wire value. A scroll-read pane's working-ness is UNOBSERVABLE by
// construction, so it must count as NOT-working: StateInCopyMode.String() must
// never collide with the cap's working sentinel. If a rename made them equal, a
// scroll-read chamber would wrongly count toward #448's concurrency cap.
func TestStateInCopyModeIsNotWorkingSentinel(t *testing.T) {
	if got := tmuxio.StateInCopyMode.String(); got == observedStateWorking {
		t.Fatalf("tmuxio.StateInCopyMode renders %q == observedStateWorking %q — a scroll-read "+
			"pane would wrongly count toward the #448 cap (#526 D3). Keep them distinct.",
			got, observedStateWorking)
	}
	if got := tmuxio.StateInCopyMode.String(); got != "copy-mode" {
		t.Fatalf("tmuxio.StateInCopyMode.String() = %q, want \"copy-mode\" (the persisted "+
			"observed_state value the inbox surface + docs reference).", got)
	}
}
