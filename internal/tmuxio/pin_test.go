// Discipline pins for the internal/tmuxio package. Per ADR-0001,
// these tests guard architectural commitments rather than behavioral
// contracts. On failure, triage per ADR-0001 §Triage before changing
// the assertion. The pin_test.go file location, the TestPin_ prefix,
// and the testpin.Triage call are the orthogonal grep handles for the
// discipline.
package tmuxio

import (
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/testpin"
)

// PIN: the pre-delivery probe-and-watch gate gates on operator-input-
// row quiet, NOT pane-quiet. Recipient mid-conversation, TUI
// animations, status-line ticks, and streaming output above the input
// row are explicitly OUT of scope. The bus's only justified gate is
// operator-typing-protection on the receiving pane.
//
// The #52 redesign retracted the v0.2.1 four-way verdict
// (DeltaTUINoise / DeltaProbeMissing) on the explicit observation that
// recipient-busy was never actually a reason to delay delivery. If
// recipient-busy ever becomes a legitimate gate (e.g., a future TUI
// client that can't ingest paste while rendering), the commitment
// retracts via superseding ADR.
//
// Empirical witness: the 2026-05-31 msg 28ca incident — 30 consecutive
// DeltaTUINoise verdicts over 5 minutes during heavy Claude Code work
// before quiet_cap_exceeded fired and delivery proceeded anyway. The
// gate correctly identified TUI activity above the input row, but
// gating on that activity was a 5-minute false-positive that
// fragmented the delivery WARN.
func TestPin_OperatorInputRowGate_StreamingAboveInputIsIgnored(t *testing.T) {
	testpin.Triage(t, "OperatorInputRowGate",
		"the gate gates on operator-input-row quiet, NOT pane-quiet — streaming above the input row is out of scope")

	// Conversation area above the input row gained a new streamed line
	// AND the input row gained exactly the two probes we pasted.
	// Under the post-#52 design this is DeltaQuiet (the gate ignores
	// non-input rows). Under v0.2.1's four-way verdict, this would
	// have been DeltaTUINoise → 5min cap-hit.
	before := "old line 1\nold line 2\n> \n"
	after := "old line 1\nNEW STREAMED LINE\nold line 2\n> ──\n"
	if v := analyzeDelta(before, after, "─", 2); v != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet — streaming above input row must be ignored", v)
	}
}

// PIN: the gate's verdict surface is binary (DeltaQuiet +
// DeltaInputActivity only). DeltaTUINoise and DeltaProbeMissing are
// explicitly NOT part of the contract — adding them back would
// re-introduce the recipient-busy gating that the OperatorInputRowGate
// commitment retracted.
//
// This pin would also catch an accidental enum-value collision if a
// future verdict slug were added without an ADR amendment ratifying
// the new commitment shape.
func TestPin_OperatorInputRowGate_VerdictSurfaceIsBinary(t *testing.T) {
	testpin.Triage(t, "OperatorInputRowGate",
		"the gate's verdict surface is binary — DeltaQuiet + DeltaInputActivity only")

	// Constants exist + carry the documented values + nothing else.
	if DeltaQuiet != 0 {
		t.Errorf("DeltaQuiet = %d, want 0 (iota anchor)", DeltaQuiet)
	}
	if DeltaInputActivity != 1 {
		t.Errorf("DeltaInputActivity = %d, want 1", DeltaInputActivity)
	}
	// Stringer coverage proves the value space.
	cases := map[DeltaKind]string{
		DeltaQuiet:         "quiet",
		DeltaInputActivity: "input_activity",
		DeltaKind(99):      "unknown",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", k, got, want)
		}
	}
}
