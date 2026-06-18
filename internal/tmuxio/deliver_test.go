package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// recordedCall captures one invocation of tmuxRun for assertions.
type recordedCall struct {
	args  []string
	stdin string
}

// fakeRunner returns a tmuxRunner closure plus a slice that records every
// invocation. The provided handler can decide what each call returns; if
// nil, returns ("", nil) for every call.
func fakeRunner(handler func(args []string, stdin string) ([]byte, error)) (
	runner func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error),
	calls *[]recordedCall,
) {
	calls = &[]recordedCall{}
	runner = func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		var in string
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			in = string(b)
		}
		*calls = append(*calls, recordedCall{args: append([]string{}, args...), stdin: in})
		if handler == nil {
			return nil, nil
		}
		return handler(args, in)
	}
	return runner, calls
}

func withFakeRunner(t *testing.T, h func(args []string, stdin string) ([]byte, error)) *[]recordedCall {
	t.Helper()
	runner, calls := fakeRunner(h)
	prev := SetTmuxRunner(runner)
	t.Cleanup(func() { SetTmuxRunner(prev) })
	return calls
}

// shortRetries replaces the default retry backoff with near-zero waits so
// tests don't sleep 250 ms when they're meant to verify failure paths.
// Also collapses the settle delay so paste→Enter tests don't pay the
// production 500ms.
func shortRetries(t *testing.T) {
	t.Helper()
	prev := retryDelays
	retryDelays = []time.Duration{time.Microsecond, time.Microsecond}
	prevSettle := settleDelay
	settleDelay = time.Microsecond
	t.Cleanup(func() {
		retryDelays = prev
		settleDelay = prevSettle
	})
}

func TestDeliver_HappyPath_PastesAndVerifies(t *testing.T) {
	shortRetries(t)
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("...some preceding line...\nrendered body with id 7f3a marker\n"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane:        "%3",
		Body:        "rendered body with id 7f3a marker",
		VerifyToken: "id 7f3a",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Expect: load-buffer, paste-buffer (-d cleans up the buffer on success,
	// so no separate delete-buffer call), send-keys, capture-pane. A single
	// unframed Body pastes as one chunk via pasteChunk (#336).
	wantCmds := []string{"load-buffer", "paste-buffer", "send-keys", "capture-pane"}
	if len(*calls) < len(wantCmds) {
		t.Fatalf("got %d calls, want at least %d: %v", len(*calls), len(wantCmds), *calls)
	}
	for i, want := range wantCmds {
		if (*calls)[i].args[0] != want {
			t.Errorf("call %d = %q, want %q", i, (*calls)[i].args[0], want)
		}
	}
	// load-buffer stdin should carry the body.
	if (*calls)[0].stdin != "rendered body with id 7f3a marker" {
		t.Errorf("load-buffer stdin = %q", (*calls)[0].stdin)
	}
	// paste-buffer should target the right pane and use -d for buffer cleanup.
	pasteArgs := (*calls)[1].args
	if !contains(pasteArgs, "-t") || !contains(pasteArgs, "%3") {
		t.Errorf("paste-buffer not targeting %%3: %v", pasteArgs)
	}
	if !contains(pasteArgs, "-d") {
		t.Errorf("paste-buffer missing -d: %v", pasteArgs)
	}
}

func TestNormalizeCollapsePaste(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"f21d footer shape (trailing newline)", "PR #531 received\n\n— Bosun\n", "PR #531 received\n— Bosun"},
		{"footer no trailing newline", "body\n\n— Bosun", "body\n— Bosun"},
		{"already single newline before tail", "body\n— Bosun", "body\n— Bosun"},
		{"single trailing newline only", "body\n— Bosun\n", "body\n— Bosun"},
		{"multi-line tail not collapsed", "body\n\nline one\nline two", "body\n\nline one\nline two"},
		{"interior paragraph preserved", "a\n\nb\n\n— Sig\n", "a\n\nb\n— Sig"},
		{"no blank-line paragraph", "just one line", "just one line"},
		{"trailing newlines stripped then collapsed", "body\n\n— Bosun\n\n\n", "body\n— Bosun"},
		{"empty tail after blank line", "body\n\n", "body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeCollapsePaste(c.in); got != c.want {
				t.Errorf("normalizeCollapsePaste(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestDeliver_CodexNormalizesTrailingSignoff pins #533: on a collapse-capable
// profile (codex), the trailing `\n\n— Sender` paragraph is collapsed to a
// single `\n` in the PASTE so codex doesn't leave the sign-off literal and
// fragment it. Asserts the bytes handed to load-buffer.
func TestDeliver_CodexNormalizesTrailingSignoff(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	defer SetActivePaneProfile(prev)
	calls := withFakeRunner(t, nil)
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%9",
		Body: "PR #531 received\n\n— Bosun",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if (*calls)[0].args[0] != "load-buffer" {
		t.Fatalf("first call = %v, want load-buffer", (*calls)[0].args)
	}
	if got := (*calls)[0].stdin; got != "PR #531 received\n— Bosun" {
		t.Errorf("codex load-buffer stdin = %q, want trailing paragraph collapsed", got)
	}
}

// TestDeliver_NonCollapseLeavesBodyUnchanged is the gate mutation anchor: a
// profile WITHOUT a collapse marker (Claude) must paste the body byte-identical
// — the #533 normalization is codex-scoped and must never touch the Claude path.
func TestDeliver_NonCollapseLeavesBodyUnchanged(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(PaneProfile{}) // no PasteCollapseMarker
	defer SetActivePaneProfile(prev)
	calls := withFakeRunner(t, nil)
	body := "PR #531 received\n\n— Bosun"
	err := Deliver(context.Background(), DeliverParams{Pane: "%3", Body: body})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := (*calls)[0].stdin; got != body {
		t.Errorf("non-collapse load-buffer stdin = %q, want unchanged %q", got, body)
	}
}

func TestDeliver_UniqueBufferPerCall(t *testing.T) {
	shortRetries(t)
	calls := withFakeRunner(t, nil)
	_ = Deliver(context.Background(), DeliverParams{Pane: "%1", Body: "x"})
	_ = Deliver(context.Background(), DeliverParams{Pane: "%1", Body: "x"})

	var bufNames []string
	for _, c := range *calls {
		if c.args[0] == "load-buffer" {
			for i, a := range c.args {
				if a == "-b" && i+1 < len(c.args) {
					bufNames = append(bufNames, c.args[i+1])
				}
			}
		}
	}
	if len(bufNames) != 2 {
		t.Fatalf("expected 2 load-buffer calls, got %d", len(bufNames))
	}
	if bufNames[0] == bufNames[1] {
		t.Errorf("buffer names collided: %s", bufNames[0])
	}
	if !strings.HasPrefix(bufNames[0], "inject-") {
		t.Errorf("buffer name should start with inject-, got %q", bufNames[0])
	}
}

func TestDeliver_VerifyRetriesOnMiss(t *testing.T) {
	shortRetries(t)
	captureCount := 0
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] != "capture-pane" {
			return nil, nil
		}
		captureCount++
		if captureCount < 3 {
			return []byte("nothing here yet"), nil
		}
		return []byte("eventually the token id 7f3a appears"), nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id 7f3a",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if captureCount != 3 {
		t.Errorf("captureCount = %d, want 3 (1 initial + 2 retries)", captureCount)
	}
}

func TestDeliver_ReturnsUnverifiedSentinelAfterRetriesExhausted(t *testing.T) {
	shortRetries(t)
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("token never lands"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "missing",
	})
	if err == nil {
		t.Fatal("want error after retries exhausted")
	}
	// Soft-fail sentinel: paste/Enter completed mechanically, just
	// couldn't confirm the token surfaced. Caller treats this as a
	// soft success (mark delivered + WARN) rather than hard failure
	// (drop message).
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Errorf("error should wrap ErrUnverifiedDelivery; got %v", err)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should name the missing token; got %v", err)
	}
}

// TestDeliverySubmitted exercises the #336 input-emptied verify predicate
// directly across both adapter profiles and the cursor-less fallback. The
// signal is CURSOR-ANCHORED (#336 cursor-anchor fix): the cursor at the
// sentinel column means an empty input; past it means populated. The
// load-bearing cases: (1) input-emptied verifies even with the token absent
// (collapse-robustness); (2) a paste still in the input row (cursor past
// sentinel) reads as not-submitted even when its token is literally visible
// (the false-positive token-match would hit); (3) codex's empty composer
// verifies EVEN WITH placeholder ghost-text present (the regression this
// fix corrects — a plain-text emptiness scan misreads the dim ghost-text as
// content); (4) when the cursor can't anchor the input row, it degrades to
// token-match.
//
// sentinelCol is 2 for both adapters (`❯`+NBSP and `› ` are each 2 runes),
// so cursorX==2 ⇒ empty, cursorX>2 ⇒ populated.
func TestDeliverySubmitted(t *testing.T) {
	claude := ClaudePaneProfile()
	codex := CodexPaneProfile()
	cases := []struct {
		name     string
		profile  PaneProfile
		capture  string
		cursorX  int
		cursorY  int
		cursorOK bool
		token    string
		want     bool
	}{
		// Claude: cleared input row (cursor at sentinel col 2) verifies
		// without the token — collapse-robust.
		{"claude cleared input verifies without token", claude, "a submitted turn\n[Pasted Content 1800 chars]\n" + PromptSentinel, 2, 2, true, "id 7f3a", true},
		// Claude: paste still in the input row (cursor past sentinel) →
		// not-submitted, even though the token is visible there.
		{"claude paste in input rejects despite token visible", claude, "transcript\n" + PromptSentinel + "rendered body id 7f3a marker", 30, 1, true, "id 7f3a", false},
		// Codex: empty input block (cursor at col 2) on the bottom row;
		// transcript `› ` row above is ignored because the cursor anchors
		// the live row directly.
		{"codex empty input verifies via cursor", codex, CodexPromptSentinel + "[Pasted Content 1800 chars]\n\n" + CodexPromptSentinel, 2, 2, true, "id 7f3a", true},
		// Codex REGRESSION CASE: empty composer carrying dim placeholder
		// ghost-text. cursor at col 2 ⇒ empty ⇒ verifies. The pre-fix
		// plain-text scan saw "Improve documentation in @filename" past the
		// sentinel and false-negatived this exact shape.
		{"codex empty input with ghost-text verifies (regression)", codex, "some codex reply\n" + CodexPromptSentinel + "Improve documentation in @filename", 2, 1, true, "id 7f3a", true},
		// Codex: collapsed paste buffered in the input (cursor past
		// sentinel) → not-submitted.
		{"codex collapsed paste in input rejects", codex, "some codex output\n" + CodexPromptSentinel + "[Pasted Content 1800 chars]", 30, 1, true, "id 7f3a", false},
		// Codex #401 marker override: a stuck collapsed paste where the cursor
		// happens to sit AT the sentinel column (col 2) on the [Pasted Content]
		// row — without the marker override inputRowCleared would false-positive
		// "empty/submitted"; the collapse marker on the live (bottom-most) input
		// is the authoritative not-submitted signal and overrides it.
		{"codex stuck collapse marker overrides cursor false-positive", codex, "transcript\n" + CodexPromptSentinel + "[Pasted Content 2048 chars]", 2, 1, true, "absent-token", false},
		// Cursor on a non-sentinel row → can't anchor → token-match hit.
		{"unanchored cursor falls back to token-match hit", claude, "plain pane with id 7f3a somewhere", 0, 0, true, "id 7f3a", true},
		{"unanchored cursor falls back to token-match miss", claude, "plain pane, nothing here", 0, 0, true, "id 7f3a", false},
		// Cursor query failed (cursorOK=false): input-emptied unavailable
		// even though the input row IS cleared → degrades to token-match,
		// which misses the absent token → unverified.
		{"cursor unavailable degrades to token-match", claude, "transcript\n" + PromptSentinel, 0, 0, false, "id 7f3a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := ActivePaneProfile()
			SetActivePaneProfile(tc.profile)
			defer SetActivePaneProfile(prev)
			if got := deliverySubmitted(tc.capture, tc.cursorX, tc.cursorY, tc.cursorOK, tc.token); got != tc.want {
				t.Errorf("deliverySubmitted(%q, x=%d y=%d ok=%v) = %v, want %v", tc.capture, tc.cursorX, tc.cursorY, tc.cursorOK, got, tc.want)
			}
		})
	}
}

// TestDeliverySubmitted_CodexDualPrompt pins the #360 dual-prompt behavior:
// when codex submits, the submitted prompt LINGERS as a transcript row (codex
// expands the collapsed `[Pasted Content]` in place) while a NEW empty input
// prompt opens below it and the cursor jumps down to it. The capture therefore
// holds MULTIPLE `› ` rows. A row-scanning "is any input row empty?" check
// would be ambiguous — it could latch onto the lingering submitted row (which
// still holds the expanded paste) and misjudge. The cursor-anchored signal is
// unambiguous: it reads emptiness from lines[cursorY] only, and the cursor is
// on the new bottom input, so the lingering submitted row above is irrelevant.
//
// This is the AC that protects an operator from misreading "delivery vanished"
// (per the #360 issue): the bus message DID submit; the dual `›` layout is just
// codex's submit visual, and the verify signal correctly anchors the live row.
func TestDeliverySubmitted_CodexDualPrompt(t *testing.T) {
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	defer SetActivePaneProfile(prev)

	// Realistic post-submit layout: a transcript turn, the LINGERING submitted
	// prompt (a long submitted message that collapsed past the token tail — the
	// >1KB case where the verify token never renders literally), a blank gap,
	// then the NEW empty bottom input. The token is ABSENT from the capture on
	// purpose: token-match cannot verify this, and a naive scan that latched
	// onto the FIRST `›` row would see the non-empty submitted row and wrongly
	// reject. Only the cursor-anchored signal — reading lines[cursorY] where the
	// cursor jumped to the new bottom input — verifies it. This is the case the
	// dual-prompt layout has to survive.
	const sub = CodexPromptSentinel + "[Pasted Content 1800 chars]"
	bottom := CodexPromptSentinel // empty new input
	capture := "codex working on the prior turn\n" + sub + "\n\n" + bottom
	cursorY := 3 // 0:transcript 1:submitted 2:blank 3:bottom input
	sentinelCol := 2

	if !deliverySubmitted(capture, sentinelCol, cursorY, true, "absent-token") {
		t.Errorf("dual-prompt with cursor on the new empty bottom input should verify "+
			"via cursor-anchor even with the token absent (cursor anchors lines[%d]=%q "+
			"at sentinel col %d)", cursorY, bottom, sentinelCol)
	}

	// Same dual-prompt layout, but the operator has started typing into the new
	// bottom input (cursor past the sentinel). Not cleared ⇒ not-submitted: the
	// gate must NOT mistake a half-typed new input for a delivered message just
	// because a submitted `›` row lingers above.
	typed := CodexPromptSentinel + "operator reply in progress"
	capture2 := "codex working on the prior turn\n" + sub + "\n\n" + typed
	if deliverySubmitted(capture2, 12, cursorY, true, "absent-token") {
		t.Errorf("dual-prompt with a half-typed bottom input must NOT verify " +
			"(cursor past sentinel ⇒ populated, lingering submitted row above is irrelevant)")
	}
}

// TestDeliver_InputEmptied_VerifiesOnClearedInput pins the end-to-end loop
// wiring: with the Claude profile active, a post-Enter capture whose
// bottom-most `❯ ` row is empty verifies the delivery even though the
// verify token never appears in the pane (the collapse case).
func TestDeliver_InputEmptied_VerifiesOnClearedInput(t *testing.T) {
	shortRetries(t)
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("> earlier submitted turn\n[Pasted Content 1800 chars]\n" + PromptSentinel), nil
		}
		if args[0] == "display-message" {
			// Cursor on the bottom sentinel row (idx 2) at the sentinel
			// column (2) → empty input → input-emptied verifies.
			return []byte("2/2"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id 7f3a",
	})
	if err != nil {
		t.Fatalf("expected verified via input-emptied (token absent), got %v", err)
	}
}

// TestDeliver_InputEmptied_RejectsPasteStillInInput pins the mid-turn /
// queued-Enter honesty: the paste is still in the bottom input row (token
// literally visible there), so the delivery reads as unverified rather than
// false-positive-confirmed.
func TestDeliver_InputEmptied_RejectsPasteStillInInput(t *testing.T) {
	shortRetries(t)
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("some transcript\n" + PromptSentinel + "rendered body with id 7f3a marker"), nil
		}
		if args[0] == "display-message" {
			// Cursor on the input row (idx 1) PAST the sentinel (col 30) →
			// paste still buffered → not-submitted (honest mid-turn).
			return []byte("30/1"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id 7f3a",
	})
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Fatalf("paste still in input row must read as unverified; got %v", err)
	}
}

// TestPasteStillInInput pins the #401 collapse-marker detection that drives
// both the verify override and the resubmit. The marker is authoritative ONLY
// in the LIVE (bottom-most sentinel) input, so codex's post-submit dual-prompt
// (lingering `[Pasted Content]` in the transcript above a new empty input) is
// NOT mistaken for stuck. No marker configured (Claude) ⇒ always false.
func TestPasteStillInInput(t *testing.T) {
	cases := []struct {
		name    string
		profile PaneProfile
		capture string
		want    bool
	}{
		{
			name:    "codex stuck collapse in live input",
			profile: CodexPaneProfile(),
			capture: "some transcript turn\n" + CodexPromptSentinel + "[Pasted Content 2048 chars] tail",
			want:    true,
		},
		{
			name:    "codex multi-line stuck (marker above cursor sub-line)",
			profile: CodexPaneProfile(),
			capture: CodexPromptSentinel + "[Pasted Content 2048 chars]\n  [· id abcd]\n",
			want:    true,
		},
		{
			// Dual-prompt SUBMITTED: lingering marker in the transcript, new
			// empty input is the bottom-most sentinel → NOT stuck.
			name:    "codex dual-prompt submitted (lingering transcript marker)",
			profile: CodexPaneProfile(),
			capture: CodexPromptSentinel + "[Pasted Content 2048 chars]\n\n" + CodexPromptSentinel,
			want:    false,
		},
		{
			// #443 Obs2 (operator-witnessed probe, 2026-06-15): three collapsed
			// blocks stage on ONE logical input row after a SINGLE sentinel
			// (codex appends " #2"/" #3" to the 2nd+ placeholders). The bottom-
			// most-sentinel scope therefore contains every staged marker, so the
			// detector reports stuck for the N-block composer exactly as for one
			// block — no early false-negative. This is what lets the #401
			// settle-until-empty resubmit loop handle N blocks unchanged.
			name:    "codex multi-block staged (three collapsed on one input row, #443)",
			profile: CodexPaneProfile(),
			capture: "some transcript turn\n" + CodexPromptSentinel +
				"[Pasted Content 2437 chars][Pasted Content 2437 chars] #2[Pasted Content 2437 chars] #3",
			want: true,
		},
		{
			// #443 Obs2: a single Enter on a ready composer submits ALL blocks in
			// one model turn; the expanded LITERAL paste text lands in the
			// transcript (NOT the "[Pasted Content]" placeholder) and the bottom-
			// most sentinel is the new empty composer → NOT stuck. The loop stops
			// on the #336 empty-input signal; no over-send (why fixed-N would be
			// wrong — Lookout's blank-follow-up caution).
			name:    "codex multi-block submitted (expanded literal text, empty input, #443)",
			profile: CodexPaneProfile(),
			capture: "1396 1398 1400 :: BLOCK-C END\n\n" + CodexPromptSentinel,
			want:    false,
		},
		{
			name:    "codex clean empty input",
			profile: CodexPaneProfile(),
			capture: "a reply\n" + CodexPromptSentinel,
			want:    false,
		},
		{
			// Claude has no collapse marker → never reports stuck, even if the
			// literal substring appears in the pane.
			name:    "claude (no marker) ignores literal pasted-content text",
			profile: ClaudePaneProfile(),
			capture: "transcript\n" + PromptSentinel + "[Pasted Content 2048 chars]",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := ActivePaneProfile()
			SetActivePaneProfile(tc.profile)
			defer SetActivePaneProfile(prev)
			if got := pasteStillInInput(tc.capture); got != tc.want {
				t.Errorf("pasteStillInInput(%q) = %v, want %v", tc.capture, got, tc.want)
			}
		})
	}
}

// TestDeliver_Codex_ResubmitsStuckCollapsedPaste pins the #401 fix end-to-end on
// the Deliver loop: codex's first Enter is absorbed while it ingests the
// bracketed paste, so the collapsed `[Pasted Content]` sits stuck in the input.
// While the marker persists Deliver re-sends Enter; once codex goes idle (the
// capture clears) it verifies. Asserts MORE than one Enter was pressed (the
// initial submit + at least one resubmit) and that the delivery verifies.
func TestDeliver_Codex_ResubmitsStuckCollapsedPaste(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var enterPresses, captureN int
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			captureN++
			if captureN <= 2 {
				// Stuck: collapsed paste is the live (bottom) input.
				return []byte("transcript\n" + CodexPromptSentinel + "[Pasted Content 2048 chars] tail"), nil
			}
			// Idle: input cleared (submitted).
			return []byte("transcript\n" + CodexPromptSentinel), nil
		case "display-message":
			return []byte("2/1"), nil // cursor at sentinel (moot while marker present)
		case "send-keys":
			if contains(args, "Enter") {
				enterPresses++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%8", Body: "x", VerifyToken: "tok",
	})
	if err != nil {
		t.Fatalf("expected verify after resubmit, got %v", err)
	}
	if enterPresses < 2 {
		t.Errorf("expected >=2 Enter presses (initial submit + resubmit), got %d", enterPresses)
	}
}

// TestDeliver_Claude_NoResubmit pins that the #401 resubmit is codex-specific:
// Claude has no collapse marker, so a still-unverified capture does NOT trigger
// extra Enter presses — exactly one Enter (the initial submit) is sent.
func TestDeliver_Claude_NoResubmit(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var enterPresses int
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			// Token never surfaces, input never clears → stays unverified, but
			// Claude must NOT resubmit (no collapse marker).
			return []byte("transcript\n" + PromptSentinel + "half-typed operator draft"), nil
		case "display-message":
			return []byte("30/1"), nil // cursor past sentinel → not cleared
		case "send-keys":
			if contains(args, "Enter") {
				enterPresses++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "never-appears",
	})
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Fatalf("want ErrUnverifiedDelivery, got %v", err)
	}
	if enterPresses != 1 {
		t.Errorf("Claude must send exactly 1 Enter (no resubmit); got %d", enterPresses)
	}
}

// TestSetSettleDelay_UpdatesPackageDelay pins the #360 exported setter the
// serve `-settle-delay` flag wires through: it overwrites the process-level
// settle pause (sibling to SetRetrySchedule). Uses SetSettleDelayForTest to
// snapshot/restore so the suite's other settle-sensitive tests are unaffected.
func TestSetSettleDelay_UpdatesPackageDelay(t *testing.T) {
	prev := SetSettleDelayForTest(0)
	t.Cleanup(func() { SetSettleDelayForTest(prev) })
	SetSettleDelay(1234 * time.Millisecond)
	if settleDelay != 1234*time.Millisecond {
		t.Errorf("SetSettleDelay = %v, want 1.234s", settleDelay)
	}
}

// TestDeliver_LargeMessageSinglePaste is the #446 demotion regression pin
// (inverts the old #336 framed-3-part test): a LARGE message delivers as ONE
// load-buffer + ONE paste-buffer + ONE Enter — no separate Header/Footer paste
// events. This structurally closes #389 (no standalone-Header-submit window,
// because there is no separate Header paste) and confirms the moving-parts
// removal. A collapsed large body on codex is still handled by the resubmit
// loop + cursor-anchor verify (orthogonal, separately tested).
func TestDeliver_LargeMessageSinglePaste(t *testing.T) {
	shortRetries(t)
	// A body well over the old byte-marker framing threshold — under #336 this
	// would have framed into 3 paste events; under #446 it is a single paste.
	bigBody := "[Bosun · 11:04:12 · id 7f3a · 2.3k]\n\n" + strings.Repeat("review text. ", 200) + "\n"
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			// Claude sentinel, input cleared past it → input-emptied verifies.
			return []byte("transcript\n" + PromptSentinel), nil
		}
		if args[0] == "display-message" {
			// Cursor on the input row (idx 1) at the sentinel column (2).
			return []byte("2/1"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane:        "%3",
		Body:        bigBody,
		VerifyToken: "id 7f3a",
	})
	if err != nil {
		t.Fatalf("single-paste deliver err: %v", err)
	}
	var loads, pastes, sendKeys int
	var loadStdins []string
	for _, c := range *calls {
		switch c.args[0] {
		case "load-buffer":
			loads++
			loadStdins = append(loadStdins, c.stdin)
		case "paste-buffer":
			pastes++
		case "send-keys":
			sendKeys++
		}
	}
	if loads != 1 || pastes != 1 {
		t.Fatalf("want 1 load-buffer + 1 paste-buffer (single paste, no frame); got loads=%d pastes=%d", loads, pastes)
	}
	if len(loadStdins) != 1 || loadStdins[0] != bigBody {
		t.Errorf("the single load-buffer should carry the whole rendered message; got %q", loadStdins)
	}
	if sendKeys != 1 {
		t.Errorf("want exactly one send-keys Enter for a submitted single paste; got %d", sendKeys)
	}
}

func TestDeliver_LoadBufferFailure(t *testing.T) {
	shortRetries(t)
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "load-buffer" {
			return []byte("can't grab a lock on the buffer"), errors.New("exit 1")
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{Pane: "%3", Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "load-buffer") {
		t.Errorf("err = %v, want load-buffer error", err)
	}
}

func TestDeliver_SkipsVerifyWhenTokenEmpty(t *testing.T) {
	shortRetries(t)
	calls := withFakeRunner(t, nil)
	err := Deliver(context.Background(), DeliverParams{Pane: "%3", Body: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for _, c := range *calls {
		if c.args[0] == "capture-pane" {
			t.Errorf("capture-pane should be skipped when VerifyToken=='', got call %v", c.args)
		}
	}
}

// TestDeliver_SettleDelay_ObservedBetweenPasteAndEnter pins the
// 2026-05-30 fix: a sleep between paste-buffer and send-keys Enter so
// Claude Code's TUI has time to ingest the paste before the submit
// keystroke arrives. The test sets a measurable delay, runs Deliver,
// and asserts the elapsed time covers it.
func TestDeliver_SettleDelay_ObservedBetweenPasteAndEnter(t *testing.T) {
	shortRetries(t)
	// Override just the settle delay (retryDelays stays at micros from
	// shortRetries) so the only meaningful wall clock cost is the settle.
	prev := SetSettleDelayForTest(40 * time.Millisecond)
	t.Cleanup(func() { SetSettleDelayForTest(prev) })

	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("verified id 1\n"), nil
		}
		return nil, nil
	})
	start := time.Now()
	if err := Deliver(context.Background(), DeliverParams{
		Pane: "%1", Body: "x", VerifyToken: "id 1",
	}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("elapsed = %s, want >= 40ms (settle delay should have fired)", elapsed)
	}
}

// Context cancellation during the settle delay should propagate
// without sending Enter.
func TestDeliver_SettleDelay_RespectsContextCancellation(t *testing.T) {
	shortRetries(t)
	prev := SetSettleDelayForTest(50 * time.Millisecond)
	t.Cleanup(func() { SetSettleDelayForTest(prev) })

	calls := withFakeRunner(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	err := Deliver(ctx, DeliverParams{Pane: "%1", Body: "x"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	// Confirm we never reached send-keys Enter (the cancellation
	// fired during the settle).
	for _, c := range *calls {
		if c.args[0] == "send-keys" {
			t.Errorf("send-keys should NOT run when ctx fires during settle; got %v", c.args)
		}
	}
}

func TestDeliver_RequiresPaneAndBody(t *testing.T) {
	withFakeRunner(t, nil)
	if err := Deliver(context.Background(), DeliverParams{Body: "x"}); err == nil {
		t.Error("want error for empty pane")
	}
	if err := Deliver(context.Background(), DeliverParams{Pane: "%1"}); err == nil {
		t.Error("want error for empty body")
	}
}

// TestDeriveRetrySchedule_DefaultPreservesBaseline pins the load-bearing
// invariant: the default budget (5s) reproduces the historical
// 100ms/250ms/500ms/1s/1.5s/1.65s schedule exactly. Operators upgrading
// without setting the verify-retry-budget knob (#153) see zero behavior
// change.
func TestDeriveRetrySchedule_DefaultPreservesBaseline(t *testing.T) {
	got := DeriveRetrySchedule(DefaultRetryBudget)
	want := []time.Duration{
		100 * time.Millisecond,
		250 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		1500 * time.Millisecond,
		1650 * time.Millisecond,
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: got %v, want %v", i, got[i], want[i])
		}
	}
	var sum time.Duration
	for _, d := range got {
		sum += d
	}
	if sum != DefaultRetryBudget {
		t.Errorf("default schedule sum: got %v, want %v", sum, DefaultRetryBudget)
	}
}

// TestDeriveRetrySchedule_ScalesProportionally pins the scaling rule:
// a doubled budget doubles every step, a halved budget halves every
// step, and the schedule sum equals the budget across both directions.
func TestDeriveRetrySchedule_ScalesProportionally(t *testing.T) {
	cases := []struct {
		name   string
		budget time.Duration
	}{
		{"half (2.5s)", 2500 * time.Millisecond},
		{"double (10s)", 10 * time.Second},
		{"triple (15s)", 15 * time.Second},
		{"large hub (30s)", 30 * time.Second},
	}
	baseline := DeriveRetrySchedule(DefaultRetryBudget)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveRetrySchedule(tc.budget)
			if len(got) != len(baseline) {
				t.Fatalf("len: got %d, want %d", len(got), len(baseline))
			}
			scale := float64(tc.budget) / float64(DefaultRetryBudget)
			for i, d := range baseline {
				want := time.Duration(float64(d) * scale)
				if got[i] != want {
					t.Errorf("step %d: got %v, want %v (scale %.2f)",
						i, got[i], want, scale)
				}
			}
			var sum time.Duration
			for _, d := range got {
				sum += d
			}
			// Allow 1ns of rounding drift from the float64 conversion.
			diff := sum - tc.budget
			if diff < -time.Nanosecond || diff > time.Nanosecond {
				t.Errorf("schedule sum: got %v, want %v (diff %v)",
					sum, tc.budget, diff)
			}
		})
	}
}

// TestDeriveRetrySchedule_NonPositiveBudgetFallsBack pins the defensive
// guard: a zero or negative budget produces the default schedule (not
// an empty schedule that would skip verify entirely).
func TestDeriveRetrySchedule_NonPositiveBudgetFallsBack(t *testing.T) {
	for _, budget := range []time.Duration{0, -1 * time.Second} {
		got := DeriveRetrySchedule(budget)
		want := DeriveRetrySchedule(DefaultRetryBudget)
		if len(got) != len(want) {
			t.Errorf("budget %v: len got %d, want %d", budget, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("budget %v step %d: got %v, want %v",
					budget, i, got[i], want[i])
			}
		}
	}
}

// TestSetRetrySchedule_RoundTripsPrevious pins the cleanup contract:
// SetRetrySchedule returns the prior schedule so callers (mailman
// startup, tests) can restore it. The legacy SetRetryDelaysForTest
// alias still works.
func TestSetRetrySchedule_RoundTripsPrevious(t *testing.T) {
	original := append([]time.Duration(nil), retryDelays...)
	t.Cleanup(func() { retryDelays = original })

	fresh := []time.Duration{1 * time.Second, 2 * time.Second}
	prev := SetRetrySchedule(fresh)
	if len(prev) != len(original) {
		t.Fatalf("prev len: got %d, want %d", len(prev), len(original))
	}
	if len(retryDelays) != len(fresh) {
		t.Errorf("retryDelays not replaced; len got %d, want %d",
			len(retryDelays), len(fresh))
	}

	// Legacy alias still works
	prev2 := SetRetryDelaysForTest(original)
	if len(prev2) != len(fresh) {
		t.Errorf("legacy alias: prev len got %d, want %d", len(prev2), len(fresh))
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// Silence unused warnings when nothing else in this file uses fmt.
var _ = fmt.Sprintf
