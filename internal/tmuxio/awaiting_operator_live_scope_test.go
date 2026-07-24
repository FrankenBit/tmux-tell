package tmuxio

import (
	"context"
	"os"
	"strings"
	"testing"
)

// awaitingGoldens are the real live-picker captures the fix MUST keep
// classifying StateAwaitingOperator — the AskUserQuestion popup (two Claude Code
// versions) and the /mcp server picker. They are the positive control for the
// live-scope predicate: if the detector stopped accepting these, every negative
// below would be vacuously satisfied (a detector that accepts nothing rejects
// everything). Same files the AwaitingOperatorMarker canary pins.
var awaitingGoldens = []string{
	"testdata/golden_quartermaster_askuserquestion_2026-06-04.txt",
	"testdata/golden_quartermaster_askuserquestion_2026-06-06.txt",
	"testdata/golden_pilot_mcp_modal_2026-07-22.txt",
}

// realBusAwaitingNegatives are REAL bus-message bodies (frozen 2026-07-24 from
// the live messages.db) that quote AwaitingOperatorMarker ("↑/↓ to navigate ·")
// — the self-referential corpus #852 is about. MEASURED 18 such messages in the
// corpus, growing monotonically as this fix is discussed (two are THIS chamber's
// own /compact resume notes). The frozen subset spans the shapes that matter:
//
//   - 8f07 — the ADVERSARIAL one: a FAITHFUL full-modal capture pasted into a bus
//     message (box-drawing, numbered options, the full footer, the popup's
//     regular-space `❯ ` selection cursor). Its footer row passes the footer gate;
//     ONLY the live-scope belt separates it from a real picker. This is the case
//     that proves live-scope is load-bearing — nothing else can.
//   - 0662 — review-request prose quoting the marker string.
//   - e22c — a PR-approval message quoting the full "· Esc to cancel" footer row
//     (one of the 14/18 that reproduce a full footer — the empirical reason no
//     same-row keybind anchor is durable).
//   - 4761 — an earlier #719/#852 /compact resume note (self-referential).
//
// NONE carries an NBSP-exact `❯ ` composer (verified: they are bus bodies, not
// live panes). Rendered as they appear in a LIVE pane — in the transcript, ABOVE
// the chamber's live composer — the live-scope belt must reject every one, or a
// chamber displaying any such message classifies paste-unsafe and defers ALL
// inbound delivery (the #647 / #852 outage class).
var realBusAwaitingNegatives = []string{
	"testdata/negctl_awaiting_bus_8f07.txt",
	"testdata/negctl_awaiting_bus_0662.txt",
	"testdata/negctl_awaiting_bus_e22c.txt",
	"testdata/negctl_awaiting_bus_4761.txt",
}

// TestCapturedLiveAwaitingOperator exercises the predicate directly: it ACCEPTS
// the live picker goldens and REJECTS every structural near-miss — the real bus
// messages that quote the marker (rendered as they appear in a live pane, above
// the composer), a synthetic with no marker at all, and (the durable guard) a
// FAITHFUL golden capture sitting above a live composer. Mirrors
// TestCapturedResumeModal's shape for the #647/#852 discriminator.
func TestCapturedLiveAwaitingOperator(t *testing.T) {
	profile := ClaudePaneProfile()

	// Positive control FIRST: every live golden must be accepted, or the
	// negatives below prove nothing. See feedback_absence_needs_positive_control.
	for _, path := range awaitingGoldens {
		golden, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read golden %s: %v", path, err)
		}
		if !capturedLiveAwaitingOperator(string(golden), profile) {
			t.Fatalf("capturedLiveAwaitingOperator REJECTED the live golden %s — the positive control failed, so the negative cases below prove nothing", path)
		}

		// Degrade-closed invariant: an empty AwaitingOperatorMarker (codex, which
		// parks this UI) must return false even on a live golden, so codex never
		// gains a spurious paste-unsafe from a Claude picker. Pins the
		// `marker == ""` top-of-function guard (an assumed precondition a
		// diff-derived mutation set would miss).
		if capturedLiveAwaitingOperator(string(golden), CodexPaneProfile()) {
			t.Errorf("capturedLiveAwaitingOperator accepted golden %s under CodexPaneProfile (empty AwaitingOperatorMarker) — the degrade-closed guard must reject when the adapter parks the check", path)
		}
	}

	// Load-bearing negatives: real bus prose quoting the marker, rendered as it
	// appears in a LIVE pane — in the transcript, with the chamber's live composer
	// (`❯`+NBSP, empty) below it. The live-scope belt must reject every one.
	composer := "\n" + PromptSentinel + "\n"
	for _, path := range realBusAwaitingNegatives {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read negative-control %s: %v", path, err)
		}
		pane := string(body) + composer
		if capturedLiveAwaitingOperator(pane, profile) {
			t.Errorf("capturedLiveAwaitingOperator ACCEPTED real bus message %s rendered above a live composer — a scrollback quote of the picker footer must not classify a pane paste-unsafe (#647/#852)", path)
		}
	}

	// Synthetic gates pinning each structural fact independently.
	footerRow := "  " + AwaitingOperatorMarker + " Esc to cancel"

	cases := []struct {
		name    string
		capture string
		want    bool
	}{
		{
			// Pins the FOOTER gate: no marker anywhere → not a picker.
			name:    "no marker anywhere",
			capture: "history line\n  some other footer\n",
			want:    false,
		},
		{
			// Footer present as the bottom-most chrome, no composer below → a live
			// picker takeover. Accepted.
			name:    "footer as bottom chrome, no composer below",
			capture: "──────\n  1. Option A\n  2. Option B\n" + footerRow + "\n\n\n",
			want:    true,
		},
		{
			// Pins the LIVE-SCOPE gate — the DURABLE guard. A faithful footer with a
			// live ❯ composer below it (a chamber quoting the footer in its
			// transcript, now idle at its prompt) must be rejected even though the
			// footer gate passes.
			name:    "faithful footer quoted above a live composer",
			capture: "──────\n  1. Option A\n" + footerRow + "\n" + PromptSentinel + "\n",
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := capturedLiveAwaitingOperator(c.capture, profile); got != c.want {
				t.Errorf("capturedLiveAwaitingOperator(%q) = %v, want %v", c.capture, got, c.want)
			}
		})
	}
}

// TestCapturedLiveAwaitingOperator_LiveScopeIsLoadBearing is the mutation that
// proves live-scope carries the fix: take the ADVERSARIAL negative (8f07, a
// faithful full-modal capture) and show it flips ACROSS the live-scope belt —
// ACCEPTED as its own bottom chrome (indistinguishable from a real picker by any
// footer-shape test), REJECTED the instant a live composer is appended below it.
// If live-scope were inert, both would return the same value. Built from the real
// fixture so it cannot drift from the corpus.
func TestCapturedLiveAwaitingOperator_LiveScopeIsLoadBearing(t *testing.T) {
	profile := ClaudePaneProfile()
	body, err := os.ReadFile("testdata/negctl_awaiting_bus_8f07.txt")
	if err != nil {
		t.Fatalf("read 8f07 fixture: %v", err)
	}
	faithful := string(body)

	// As its own bottom chrome (no live composer below): footer gate passes,
	// live-scope has nothing to reject → ACCEPT. This is WHY no footer-shape test
	// could ever separate a faithful paste from the real picker.
	if !capturedLiveAwaitingOperator(faithful, profile) {
		t.Fatalf("faithful full-modal fixture was rejected as bottom chrome — the mutation's premise (footer gate passes) is void; re-derive")
	}
	// The SAME bytes with a live composer appended → REJECT. Only the live-scope
	// belt changed the verdict.
	withComposer := faithful + "\n" + PromptSentinel + "\n"
	if capturedLiveAwaitingOperator(withComposer, profile) {
		t.Errorf("live composer below a faithful full-modal quote did NOT flip the verdict — the live-scope belt is inert, and #852's sole durable guard is dead")
	}
}

// TestAgentState_AwaitingOperatorLiveScope reproduces the #852 exposure
// measurement (issue comment 88577) as a regression pin, end-to-end through the
// classifier. A pane whose scrollback carries a REAL bus message quoting the
// marker, with a clean idle composer (`❯`+NBSP) at the bottom, stable frame
// (capA == capB), classified three ways by cursor position:
//
//	pane state                          cursor        classifier result
//	clean idle (cursor at composer)     (2, composer) StateIdle   — P6, never reaches P7
//	cursor not on the composer row      (0, 0)        StateIdle   — P7 rejects (live-scope), cursor-less fallback reclaims
//	real live picker (golden)           (0, 0)        StateAwaitingOperator — P7 accepts
//
// Before the fix the middle row was a false StateAwaitingOperator (bare
// whole-pane strings.Contains); the whole point of #852 is that it is now
// reclaimed to StateIdle while the real picker (bottom row) still classifies
// paste-unsafe.
func TestAgentState_AwaitingOperatorLiveScope(t *testing.T) {
	body, err := os.ReadFile("testdata/negctl_awaiting_bus_8f07.txt")
	if err != nil {
		t.Fatalf("read 8f07 fixture: %v", err)
	}
	// The colliding pane: a real marker-quoting bus message in scrollback, a
	// clean idle composer at the bottom. Trailing text past the composer sentinel
	// is empty so the cursor-less fallback reads it as idle.
	collision := string(body) + "\n" + PromptSentinel + "\n"
	composerRow := -1
	rows := strings.Split(collision, "\n")
	for i := len(rows) - 1; i >= 0; i-- {
		if _, found := cutPromptSentinel(rows[i], PromptSentinel); found {
			composerRow = i
			break
		}
	}
	if composerRow < 0 {
		t.Fatal("constructed collision pane has no composer row; test is malformed")
	}

	golden, err := os.ReadFile(awaitingGoldens[0])
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	cases := []struct {
		name      string
		capture   string
		cursorX   int
		cursorY   int
		wantState State
	}{
		{
			// P6: cursor AT the composer sentinel on a stable frame → Idle, never
			// reaches P7. The "already safe" row of the exposure table.
			name:      "clean idle: cursor at composer sentinel",
			capture:   collision,
			cursorX:   2, // right after "❯"+NBSP (2 runes)
			cursorY:   composerRow,
			wantState: StateIdle,
		},
		{
			// The FIX: cursor not on the composer row (a cursor-query hiccup or the
			// cursor parked elsewhere) → P6 skipped → P7. Before #852 this was a
			// false StateAwaitingOperator; now the live-scope belt rejects (composer
			// below the quoted footer) and the cursor-less fallback reclaims it to
			// StateIdle.
			name:      "colliding message, cursor off composer: reclaimed to Idle",
			capture:   collision,
			cursorX:   0,
			cursorY:   0,
			wantState: StateIdle,
		},
		{
			// The real picker still classifies paste-unsafe: the golden is a
			// full-screen takeover (no composer below the footer) → P7 accepts.
			name:      "real live picker still classifies AwaitingOperator",
			capture:   string(golden),
			cursorX:   0,
			cursorY:   0,
			wantState: StateAwaitingOperator,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fastTemporalDelta(t)
			fr := newAgentStateRunner([]string{c.capture, c.capture}, c.cursorX, c.cursorY)
			prev := SetTmuxRunner(fr.run)
			t.Cleanup(func() { SetTmuxRunner(prev) })

			state, ev, err := AgentState(context.Background(), "%5")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if state != c.wantState {
				t.Errorf("state = %v, want %v (evidence: %s)", state, c.wantState, ev.Reason)
			}
		})
	}
}
