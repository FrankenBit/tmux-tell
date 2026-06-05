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
	// chambers on 2026-06-04 via `tmux capture-pane | od -An -tx1`.
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
		t.Errorf("golden capture has NO line starting with PromptSentinel %q (% x) — Claude Code emission encoding may have drifted; re-verify via tmux capture-pane | od -An -tx1 on a live chamber + update PromptSentinel + re-capture the golden fixture", PromptSentinel, []byte(PromptSentinel))
	}
}

// TestAwaitingOperatorMarker_MatchesGoldenCapture is the sibling-shape
// canary for the AwaitingOperatorMarker constant — same capture-
// derived-vs-spec-derived discipline as the PromptSentinel canary
// above. The golden file is a real `tmux capture-pane` output frozen
// from a Quartermaster pane displaying a live AskUserQuestion popup
// (#79, captured 2026-06-04). If Claude Code's popup UI
// drifts (footer keybinding text changes, separator character flips),
// this test fails loudly + names the re-capture recipe.
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
	golden, err := os.ReadFile("testdata/golden_quartermaster_askuserquestion_2026-06-04.txt")
	if err != nil {
		t.Fatalf("read golden capture: %v", err)
	}
	if !strings.Contains(string(golden), AwaitingOperatorMarker) {
		t.Errorf("golden capture does NOT contain AwaitingOperatorMarker %q — Claude Code popup UI may have drifted; re-verify via `tmux capture-pane -p -t <pane>` on a live AskUserQuestion popup + update AwaitingOperatorMarker + re-capture the golden fixture",
			AwaitingOperatorMarker)
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
// The substring is structurally unique to Claude Code's compaction UI
// — regular chat / response text doesn't combine "Compacting" with
// "conversation" plus the U+2026 ellipsis. The two-fixture shape pins
// the spinner-frame robustness: a future contributor who accidentally
// includes the glyph (e.g., `CompactionMarker = "✻ Compacting…"`) gets
// at least one failing case here.
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
			if !strings.Contains(string(golden), CompactionMarker) {
				t.Errorf("golden %q does NOT contain CompactionMarker %q — Claude Code compaction UI may have drifted; re-verify via `tmux capture-pane -p -t <pane>` during a live /compact + update CompactionMarker + re-capture both golden fixtures",
					c.path, CompactionMarker)
			}
		})
	}
}
