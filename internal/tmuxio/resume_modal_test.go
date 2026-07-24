package tmuxio

import (
	"context"
	"os"
	"strings"
	"testing"
)

const resumeModalGolden = "testdata/golden_pilot_resume_modal_2026-07-24.txt"

// realBusResumeNegatives are REAL bus-message bodies extracted from the live
// messages.db (2026-07-24) that quote the resume-modal markers — the
// self-referential-marker corpus #719/#852 is about. Two of them (f5ae, 4761)
// carry the footer legend AND the ⌕ glyph AND the "Resume session" header (f5ae
// is the #719 dispatch itself; 4761 is a /compact resume note); the other two
// quote only the header. NONE reproduces the box-drawing search widget (│+⌕ on
// one row) — MEASURED 0 of 19355 messages carry it. capturedResumeModal must
// reject every one, or a chamber displaying any such message classifies
// paste-unsafe and defers ALL inbound delivery (the #647 outage class).
//
// This is the load-bearing test: a detector that only accepts the fixture is
// vacuous. The negatives are what prove the structural anchor discriminates.
var realBusResumeNegatives = []string{
	"testdata/negctl_resume_bus_f5ae.txt", // #719 dispatch: header+footer+⌕, no widget
	"testdata/negctl_resume_bus_4761.txt", // /compact resume note: header+footer+⌕, no widget
	"testdata/negctl_resume_bus_95cc.txt", // header-only quote
	"testdata/negctl_resume_bus_4f7c.txt", // header-only quote
}

// TestCapturedResumeModal exercises the predicate directly: it ACCEPTS the live
// fixture and REJECTS every structural near-miss — the real bus messages that
// quote the markers, a synthetic missing the footer legend, and (the durable
// guard) a full modal capture sitting above a live composer (a scrollback
// quote). Mirrors TestCapturedLiveCompaction's shape for the #647 discriminator.
func TestCapturedResumeModal(t *testing.T) {
	profile := ClaudePaneProfile()

	golden, err := os.ReadFile(resumeModalGolden)
	if err != nil {
		t.Fatalf("read resume-modal golden: %v", err)
	}
	fixture := string(golden)

	// Positive control: the live fixture must be accepted, or every negative
	// below is vacuously satisfied (a detector that accepts nothing rejects
	// everything). See feedback_absence_needs_positive_control.
	if !capturedResumeModal(fixture, profile) {
		t.Fatalf("capturedResumeModal REJECTED the live golden fixture — the positive control failed, so the negative cases below prove nothing")
	}

	// Degrade-closed invariant (the guard a diff-derived mutation set misses —
	// it is an assumed precondition, not an added line): an empty
	// ResumeModalMarker (codex, which parks this UI) must return false even on
	// the live fixture, so codex never gains a spurious paste-unsafe from a
	// Claude modal. Pins the `marker == ""` top-of-function guard.
	if capturedResumeModal(fixture, CodexPaneProfile()) {
		t.Errorf("capturedResumeModal accepted the fixture under CodexPaneProfile (empty ResumeModalMarker) — the degrade-closed guard must reject when the adapter parks the check")
	}

	// Load-bearing negatives: real bus prose quoting the markers.
	for _, path := range realBusResumeNegatives {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read negative-control %s: %v", path, err)
		}
		if capturedResumeModal(string(body), profile) {
			t.Errorf("capturedResumeModal ACCEPTED real bus message %s — a prose quote of the modal markers must not classify a pane paste-unsafe (#647/#852)", path)
		}
	}

	// Synthetic near-misses pinning each structural gate independently.
	footerRow := "    " + ResumeModalMarker
	widgetRow := "  │ ⌕ Pilot                    │"
	composerRow := "❯ " // NBSP-exact live composer sentinel

	cases := []struct {
		name    string
		capture string
		want    bool
	}{
		{
			// Pins the SEARCH-WIDGET gate: footer legend present, but no │+⌕
			// row. This is the shape every real bus negative has; the synthetic
			// makes the gate's role explicit.
			name:    "footer legend but no search widget",
			capture: "history\n" + footerRow + "\n",
			want:    false,
		},
		{
			// Pins the FOOTER gate: the search widget present, but no keybind
			// legend anywhere. A bare box-drawn search field is not, on its own,
			// the resume modal.
			name:    "search widget but no footer legend",
			capture: "history\n" + widgetRow + "\nsome other footer\n",
			want:    false,
		},
		{
			// Both structural halves present and correctly ordered — accepted.
			name:    "widget above footer, no composer below",
			capture: "──────\n  Resume session\n" + widgetRow + "\n    Pilot\n" + footerRow + "\n\n\n",
			want:    true,
		},
		{
			// Pins the LIVE-SCOPE gate — the DURABLE guard. This is a FAITHFUL
			// full modal capture (widget + footer, box-drawing and all) but with
			// the chamber's live ❯ composer below it: i.e. a chamber that pasted
			// a whole modal capture into its transcript and is now idle at its
			// prompt. Even though the corpus does not carry the box-drawing today
			// (and may as this very fix is discussed — the temporal-extension),
			// the live-scope belt rejects it because the composer sits below the
			// footer. Built from the real fixture so it cannot drift from it.
			name:    "faithful full modal quoted above a live composer",
			capture: fixture + "\n" + composerRow + "\n",
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := capturedResumeModal(c.capture, profile); got != c.want {
				t.Errorf("capturedResumeModal(%q) = %v, want %v", c.capture, got, c.want)
			}
		})
	}
}

// TestResumeModalMarker_MatchesGoldenCapture is the sibling-shape canary for the
// ResumeModalMarker constant (#719) — same capture-derived-vs-spec-derived
// discipline as the PromptSentinel / AwaitingOperatorMarker / CompactionMarker /
// APIErrorMarker canaries in state_canary_test.go. It pins that
// capturedResumeModal fires on the real Pilot capture (frozen 2026-07-24), so a
// drift in Claude Code's modal footer/chrome — or a regression in the structural
// helper — fails loudly and names the re-capture recipe.
func TestResumeModalMarker_MatchesGoldenCapture(t *testing.T) {
	// Guard against the empty-marker regression: an accidentally-emptied
	// ResumeModalMarker disables the StateAwaitingOperator branch for the resume
	// modal entirely, and a future revert / merge-conflict needs to surface here
	// loudly, not just in the classification pin below. (Sibling guard to
	// TestCompactionMarker_MatchesGoldenCapture.)
	if ResumeModalMarker == "" {
		t.Fatal("ResumeModalMarker is empty — the resume-modal StateAwaitingOperator branch is disabled; re-populate from a re-captured golden fixture (see ResumeModalMarker doc-comment)")
	}
	golden, err := os.ReadFile(resumeModalGolden)
	if err != nil {
		t.Fatalf("read golden capture: %v", err)
	}
	if !capturedResumeModal(string(golden), ClaudePaneProfile()) {
		t.Errorf("golden %q is NOT recognized as a live resume modal — Claude Code's session-resume UI may have drifted; re-verify via `tmux capture-pane -p -t <pane>` on a live resume modal (raw `claude` with >1 resumable session) + update ResumeModalMarker + re-capture the golden fixture", resumeModalGolden)
	}
}

// TestAgentState_ResumeModalClassifiesAwaitingOperator pins the end-to-end
// classification: a pane showing the live resume modal classifies
// StateAwaitingOperator (paste-unsafe), so the mailman's pre-paste gate refuses
// delivery instead of pasting search input into the modal (#719).
//
// The two sub-cases are the load-bearing pair:
//
//   - "stable frame": capA == capB. Straightforward — the modal is caught.
//   - "animating frame": capA != capB, differing ONLY in a relative-time entry
//     ("17 seconds ago" → "18 seconds ago"), as the modal's timestamps tick
//     across the 200ms temporal-delta window. This pins the PRECEDENCE: the
//     check sits BEFORE P5's frame-change branch. Were it placed at P7 (with the
//     existing AwaitingOperatorMarker), P5 would classify the ticking pane
//     StateWorking — which is paste-SAFE — and the mailman would deliver INTO
//     the modal. This case fails loudly if a future refactor moves the check
//     below P5.
func TestAgentState_ResumeModalClassifiesAwaitingOperator(t *testing.T) {
	golden, err := os.ReadFile(resumeModalGolden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	modal := string(golden)
	// The animating capA: same modal, one timestamp aged by a second. Must
	// differ from capB or the sub-case's precedence claim is void.
	animated := strings.Replace(modal, "17 seconds ago", "18 seconds ago", 1)
	if animated == modal {
		t.Fatalf("golden lacks the '17 seconds ago' entry the animating sub-case mutates; re-derive the tick from the current fixture")
	}

	cases := []struct {
		name       string
		capA, capB string
	}{
		{"stable frame", modal, modal},
		{"animating frame (timestamp ticked)", animated, modal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fastTemporalDelta(t)
			fr := newAgentStateRunner([]string{c.capA, c.capB}, 0, 0)
			prev := SetTmuxRunner(fr.run)
			t.Cleanup(func() { SetTmuxRunner(prev) })

			state, ev, err := AgentState(context.Background(), "%5")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if state != StateAwaitingOperator {
				t.Errorf("state = %v, want StateAwaitingOperator (live resume modal must classify paste-unsafe, beating P5-Working even when the timestamp ticks)", state)
			}
			if ev.Marker != ResumeModalMarker {
				t.Errorf("Evidence.Marker = %q, want %q", ev.Marker, ResumeModalMarker)
			}
		})
	}
}
