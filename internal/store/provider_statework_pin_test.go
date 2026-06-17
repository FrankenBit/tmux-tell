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
