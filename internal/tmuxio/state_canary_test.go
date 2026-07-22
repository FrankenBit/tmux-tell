package tmuxio

import (
	"os"
	"strings"
	"testing"
)

// --- PromptSentinel + marker encoding canary tests ---
//
// These tests anchor the PromptSentinel, AwaitingOperatorMarker, and
// CompactionMarker constants to the actual byte encodings + paint
// formats Claude Code emits in production. They sit in a dedicated
// file so the canary discipline (capture-derived rather than spec-
// derived, per Surveyor's O69 framing) is obvious at the file-level
// glance and survives test-file reshuffling.
//
// History — the canary discipline emerged from the #69
// substrate-discovery: PR #66 + PR #77 shipped with PromptSentinel
// using a regular space ("❯ ") but Claude Code actually paints NBSP
// (U+00A0). The bug was invisible to unit tests because the test
// fixtures themselves used the regular-space variant. The canary
// pattern fixes that by anchoring against frozen `tmux capture-pane`
// output instead of synthesized strings.

// TestPromptSentinel_BytesMatchNBSP pins the byte-level encoding of
// PromptSentinel against the empirically-captured production bytes.
// If a future contributor changes the constant to use a regular space
// (U+0020) instead of NBSP (U+00A0), this test catches it before
// merge.
func TestPromptSentinel_BytesMatchNBSP(t *testing.T) {
	// The Claude Code TUI emits ❯ (U+276F, hex e2 9d af) followed by
	// NBSP (U+00A0, hex c2 a0). Empirically captured across all 6
	// agents on 2026-06-04 via `tmux capture-pane | od -An -tx1`.
	want := []byte{0xe2, 0x9d, 0xaf, 0xc2, 0xa0}
	got := []byte(PromptSentinel)
	if !bytesEqual(got, want) {
		t.Errorf("PromptSentinel bytes = % x, want % x (❯ + U+00A0 NBSP)", got, want)
	}
}

// bytesEqual is a tiny helper so the canary test doesn't import
// "bytes" just for one Equal call.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPromptSentinel_MatchesGoldenCapture pins PromptSentinel against
// a real `tmux capture-pane` output frozen as testdata. This is the
// capture-derived (vs spec-derived) anchor per Surveyor's O69
// discipline-class — if Claude Code's emission encoding changes
// (theme update, terminal switch, version bump), the golden fixture
// stops matching and surfaces the divergence loudly.
//
// Forward-watch: re-capture the golden file when Claude Code TUI
// changes, or when this test fails after a Claude Code version bump.
// The capture command is documented in PromptSentinel's doc-comment.
func TestPromptSentinel_MatchesGoldenCapture(t *testing.T) {
	golden, err := os.ReadFile("testdata/golden_bosun_idle_2026-06-04.txt")
	if err != nil {
		t.Fatalf("read golden capture: %v", err)
	}
	found := false
	for _, line := range strings.Split(string(golden), "\n") {
		if strings.HasPrefix(line, PromptSentinel) {
			found = true
			t.Logf("matched sentinel on golden line: %q", line[:min(50, len(line))])
			break
		}
	}
	if !found {
		t.Errorf("golden capture has NO line starting with PromptSentinel %q (% x) — Claude Code emission encoding may have drifted; re-verify via tmux capture-pane | od -An -tx1 on a live agent + update PromptSentinel + re-capture the golden fixture", PromptSentinel, []byte(PromptSentinel))
	}
}

// TestAwaitingOperatorMarker_MatchesGoldenCapture is the sibling-shape
// canary for the AwaitingOperatorMarker constant — same capture-
// derived-vs-spec-derived discipline as the PromptSentinel canary
// above. Two golden files are checked, both real `tmux capture-pane`
// outputs frozen from a Quartermaster pane displaying a live
// AskUserQuestion popup. If Claude Code's popup UI drifts (footer
// keybinding text changes, separator character flips), at least one
// fixture stops matching and the test fails loudly + names the
// re-capture recipe.
//
// Two-fixture shape pins coverage across multiple operator-coordinated
// capture sessions:
//   - 2026-06-04: original #79 capture, pinned the marker as captured
//   - 2026-06-06: #133 follow-up capture coordinated via Bosun during
//     a real AskUserQuestion popup with question + 3 options + hint
//     line. Confirms the marker still matches canonical popups
//     post-v0.6.0 cutover. Per `feedback_filed_rootcause_is_hypothesis`:
//     the 2026-06-05 incident's "marker mismatch" theory was
//     hypothesis until empirical verification; this capture
//     disconfirms the theory (existing marker DOES match the popup
//     it failed on, so the failure was capture-window-scroll or a
//     non-AskUserQuestion popup variant). The Half 2 safety net
//     (#105 / PR #134) is the load-bearing protection regardless.
//
// The substring is structurally unique to Claude Code's popup UI —
// regular chat / response text never combines U+00B7 middle-dot
// separators with keybinding hints. Catches single-select and multi-
// select popups (both end with the same footer).
func TestAwaitingOperatorMarker_MatchesGoldenCapture(t *testing.T) {
	// Guard against the empty-marker regression: strings.Contains(g, "")
	// returns true for any g, so an accidentally-emptied constant
	// would silently pass the substring check below. The empty value
	// is the pre-#79 placeholder; a future revert (intentional or via
	// merge conflict) needs to surface loudly here, not just in the
	// e2e classification pin in state_test.go. Pattern retrofitted
	// from TestCompactionMarker_MatchesGoldenCapture per #89.
	if AwaitingOperatorMarker == "" {
		t.Fatal("AwaitingOperatorMarker is empty — the StateAwaitingOperator branch is disabled; re-populate from a re-captured golden fixture (see AwaitingOperatorMarker doc-comment)")
	}
	cases := []struct {
		name string
		path string
	}{
		{"original_79_capture", "testdata/golden_quartermaster_askuserquestion_2026-06-04.txt"},
		{"post_v060_133_capture", "testdata/golden_quartermaster_askuserquestion_2026-06-06.txt"},
		// #719-B: the /mcp server-picker footer inserts "· Enter to confirm ·"
		// between "navigate" and "Esc", so the pre-broadening full-footer marker
		// ("↑/↓ to navigate · Esc to cancel") did NOT match this capture and the
		// live modal fell through to StateUnknown. Pins the broadened
		// nav-hint-core marker against the picker variant the AskUserQuestion
		// captures don't cover.
		{"mcp_picker_719b_capture", "testdata/golden_pilot_mcp_modal_2026-07-22.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			golden, err := os.ReadFile(c.path)
			if err != nil {
				t.Fatalf("read golden capture %s: %v", c.path, err)
			}
			if !strings.Contains(string(golden), AwaitingOperatorMarker) {
				t.Errorf("golden capture %s does NOT contain AwaitingOperatorMarker %q — Claude Code popup UI may have drifted; re-verify via `tmux capture-pane -p -t <pane>` on a live AskUserQuestion popup + update AwaitingOperatorMarker + re-capture the golden fixture",
					c.path, AwaitingOperatorMarker)
			}
		})
	}
}

// TestCompactionMarker_MatchesGoldenCapture is the sibling-shape canary
// for the CompactionMarker constant — same capture-derived-vs-spec-
// derived discipline as the PromptSentinel + AwaitingOperatorMarker
// canaries above. Two golden files are checked, both real `tmux
// capture-pane` outputs frozen from a Quartermaster pane mid-`/compact`
// (#70, captured 2026-06-04). The two captures differ in
// progress (8% vs 68%) and — critically — in the spinner glyph (✻
// U+273B vs ✢ U+2722); the marker excludes the glyph and matches the
// trailing phrase that survives the spinner animation. If Claude Code's
// compaction UI drifts (phrase changes, ellipsis encoding flips),
// either or both fixtures stop matching and the test fails loudly +
// names the re-capture recipe.
//
// The marker phrase is NOT structurally unique on its own: a chamber
// discussing compaction writes "Compacting conversation…" in ordinary
// message text, which the bare whole-pane substring match read as
// mid-/compact and wedged all inbound delivery (#647). The MATCH therefore
// requires the live-elapsed-timer parenthetical (capturedLiveCompaction);
// the canary below pins that both goldens carry that live form, so a drift
// that drops the parenthetical fails here loudly. The two-fixture shape also
// pins spinner-frame robustness: a contributor who folds the animated glyph
// into the constant (e.g. `"✻ Compacting…"`) gets at least one failing case.
func TestCompactionMarker_MatchesGoldenCapture(t *testing.T) {
	// Guard against the empty-marker regression: strings.Contains(g, "")
	// returns true for any g, so an accidentally-emptied constant
	// would silently pass the substring check below. The empty value
	// is the pre-#70 placeholder; a future revert (intentional or via
	// merge conflict) needs to surface loudly here, not just in the
	// e2e classification pin in state_test.go.
	if CompactionMarker == "" {
		t.Fatal("CompactionMarker is empty — the StateAtRestInCompaction branch is disabled; re-populate from a re-captured golden fixture (see CompactionMarker doc-comment)")
	}
	cases := []struct {
		name string
		path string
	}{
		{"early_8pct_six_pointed_star_spinner", "testdata/golden_quartermaster_compaction_2026-06-04.txt"},
		{"advanced_68pct_teardrops_spoked_asterisk_spinner", "testdata/golden_quartermaster_compaction_advanced_2026-06-04.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			golden, err := os.ReadFile(c.path)
			if err != nil {
				t.Fatalf("read golden capture: %v", err)
			}
			if !capturedLiveCompaction(string(golden), CompactionMarker) {
				t.Errorf("golden %q does NOT contain CompactionMarker %q — Claude Code compaction UI may have drifted; re-verify via `tmux capture-pane -p -t <pane>` during a live /compact + update CompactionMarker + re-capture both golden fixtures",
					c.path, CompactionMarker)
			}
		})
	}
}

// TestAPIErrorMarker_MatchesGoldenCapture is the sibling-shape canary for the
// APIErrorMarker constant (#719) — same capture-derived-vs-spec-derived
// discipline as the PromptSentinel / AwaitingOperatorMarker / CompactionMarker
// canaries above. It pins that capturedLiveErrorChrome fires on the real
// terminal 529 capture (frozen from Bosun's live pane 2026-07-22), so a drift in
// Claude Code's API-error line phrasing — or a regression in the live-scope
// helper — fails loudly + names the re-capture recipe.
//
// The bare "API Error:" substring is NOT structurally unique (a chamber
// discussing an error writes it in prose), so the match is via
// capturedLiveErrorChrome, not strings.Contains: the golden must carry the
// LIVE terminal chrome (coded error line in the current-turn region, no
// `esc to interrupt` footer), which prose-quotes lack.
func TestAPIErrorMarker_MatchesGoldenCapture(t *testing.T) {
	// Guard against the empty-marker regression: an accidentally-emptied
	// APIErrorMarker disables the StateErrored branch entirely (the classifier's
	// `m != ""` guard parks it), and this test must surface that loudly rather
	// than silently passing. Same pattern as the Compaction/AwaitingOperator
	// canaries.
	if APIErrorMarker == "" {
		t.Fatal("APIErrorMarker is empty — the StateErrored branch is disabled; re-populate from a re-captured golden fixture (see APIErrorMarker doc-comment)")
	}
	golden, err := os.ReadFile("testdata/golden_bosun_api_error_529_2026-07-22.txt")
	if err != nil {
		t.Fatalf("read API-error golden capture: %v", err)
	}
	line, ok := capturedLiveErrorChrome(string(golden), ClaudePaneProfile())
	if !ok {
		t.Errorf("golden terminal 529 capture did NOT classify as live API-error chrome — Claude Code's API-error line may have drifted, or the live-scope helper regressed; re-verify via `tmux capture-pane -p -t <pane>` on a live terminal API error + update APIErrorMarker + re-capture the golden fixture")
	}
	if !strings.Contains(line, APIErrorMarker) {
		t.Errorf("matched error line %q does not contain APIErrorMarker %q", line, APIErrorMarker)
	}
}
