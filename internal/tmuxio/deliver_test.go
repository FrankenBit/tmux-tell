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
		// #616 pre-paste race-check off: this static-capture fake exercises the
		// paste→verify sequence in isolation, not the pre-paste operator-draft check.
		PrePasteRaceCheckDisabled: true,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Expect: load-buffer, paste-buffer (-d cleans up the buffer on success,
	// so no separate delete-buffer call), send-keys, capture-pane. A single
	// atomically framed Body pastes as one chunk via pasteChunk (#336/#831).
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
	// paste-buffer should target the right pane, preserve body bytes inside one
	// bracketed-paste frame, and use -d for buffer cleanup (#831).
	pasteArgs := (*calls)[1].args
	if !contains(pasteArgs, "-t") || !contains(pasteArgs, "%3") {
		t.Errorf("paste-buffer not targeting %%3: %v", pasteArgs)
	}
	if !contains(pasteArgs, "-d") {
		t.Errorf("paste-buffer missing -d: %v", pasteArgs)
	}
	for _, flag := range []string{"-p", "-r"} {
		if !contains(pasteArgs, flag) {
			t.Errorf("paste-buffer missing atomic-paste flag %s: %v", flag, pasteArgs)
		}
	}
}

// TestDeliver_MultilineBodyUsesAtomicPasteFrame is #831's byte invariant at
// the substrate/TUI boundary. The exact body bytes enter tmux once, -r forbids
// LF→CR conversion, and -p asks tmux to enclose them in the application's
// bracketed-paste frame. The explicit Enter therefore follows the end marker;
// no newline inside the body can be interpreted as an early submit.
func TestDeliver_MultilineBodyUsesAtomicPasteFrame(t *testing.T) {
	shortRetries(t)
	body := "head id d5a7\n\nbody line\ntail sentence"
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("submitted id d5a7\n" + PromptSentinel), nil
		}
		if args[0] == "display-message" {
			return []byte("2/1"), nil
		}
		return nil, nil
	})
	if err := Deliver(context.Background(), DeliverParams{
		Pane: "%11", Body: body, VerifyToken: "id d5a7", PrePasteRaceCheckDisabled: true,
	}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(*calls) < 3 {
		t.Fatalf("calls = %v, want load-buffer, paste-buffer, Enter", *calls)
	}
	if got := (*calls)[0].stdin; got != body || len(got) != len(body) {
		t.Fatalf("load-buffer bytes = %d %q, want %d exact body bytes", len(got), got, len(body))
	}
	if got := (*calls)[1].args; !contains(got, "-p") || !contains(got, "-r") {
		t.Fatalf("paste-buffer args = %v, want -p -r atomic frame", got)
	}
	if got := (*calls)[2].args; got[0] != "send-keys" || !contains(got, "Enter") {
		t.Fatalf("call after atomic paste = %v, want explicit Enter", got)
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
		PrePasteRaceCheckDisabled: true, // #616: keep the verify-loop capture count pure
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

// TestDeliver_DoesNotAcceptClearedInputDuringCompaction pins the #622 cheap
// secondary discriminator: deliverySubmitted keys on the input row clearing, but
// the /compact UI keeps the prompt sentinel, so a cursor-at-sentinel "cleared"
// reading while the live compaction marker is visible is the compaction redraw,
// not our submit. Deliver must NOT accept it — it re-queues via
// ErrUnverifiedDelivery so the row re-delivers after the stability-gate.
func TestDeliver_DoesNotAcceptClearedInputDuringCompaction(t *testing.T) {
	shortRetries(t)
	// Cursor (2/1) anchors the cleared input row (PromptSentinel on line 1), but
	// the live compaction marker is present on line 0 → the clear is the redraw.
	capture := "✻ Compacting conversation… (7s · ↑ 2.9k tokens)\n" + PromptSentinel
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			return []byte(capture), nil
		case "display-message":
			return []byte("2/1"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id 7f3a",
	})
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Errorf("err = %v, want ErrUnverifiedDelivery (a cleared input concurrent with a visible /compact must not be accepted)", err)
	}
}

// cmdRan reports whether any recorded tmuxRun call had args[0] == name.
func cmdRan(calls *[]recordedCall, name string) bool {
	for _, c := range *calls {
		if len(c.args) > 0 && c.args[0] == name {
			return true
		}
	}
	return false
}

// TestDeliver_PrePasteRace_OperatorContentAbortsBeforePaste is the #616 load-
// bearing invariant + mutation anchor: when the operator has typed into the
// input row (cursor past the prompt sentinel) at the tightest pre-paste moment,
// Deliver returns ErrInputRaced and NEVER pastes — no load-buffer, no paste-
// buffer, no Enter. This is the residual TOCTOU the mailman's pre-paste probe
// can't catch (a keystroke landing after the probe passes); pasting here would
// prepend the message to the operator's draft, and the input-cleared verify
// couldn't tell the corrupted submit from a clean one.
func TestDeliver_PrePasteRace_OperatorContentAbortsBeforePaste(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			return []byte("an earlier turn\n" + PromptSentinel + "operator half-typed draft"), nil
		case "display-message":
			return []byte("22/1"), nil // cursor past the sentinel col (2) → operator mid-typing
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "the bus message", VerifyToken: "id 7f3a",
	})
	if !errors.Is(err, ErrInputRaced) {
		t.Fatalf("err = %v, want ErrInputRaced (operator content in input at pre-paste)", err)
	}
	// The whole point: we did NOT paste onto the operator's draft.
	for _, cmd := range []string{"load-buffer", "paste-buffer", "send-keys"} {
		if cmdRan(calls, cmd) {
			t.Errorf("%s ran, but a raced input must abort BEFORE any paste/Enter", cmd)
		}
	}
}

// TestDeliver_PrePasteRace_EmptyInputProceeds is the negative control: an empty
// input row (cursor AT the sentinel) is not a race — Deliver pastes and verifies
// normally. Without this, a too-eager pre-paste check would block every delivery.
func TestDeliver_PrePasteRace_EmptyInputProceeds(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			return []byte("an earlier turn\n" + PromptSentinel), nil // empty input row
		case "display-message":
			return []byte("2/1"), nil // cursor at the sentinel col → empty
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "the bus message", VerifyToken: "id 7f3a",
	})
	if err != nil {
		t.Fatalf("empty input must not race; got %v", err)
	}
	if !cmdRan(calls, "load-buffer") || !cmdRan(calls, "send-keys") {
		t.Errorf("clean delivery must paste + Enter; calls=%v", *calls)
	}
}

// TestDeliver_PrePasteRace_CodexGhostTextSafe pins AC#5 (cross-CLI): codex paints
// dim placeholder ghost-text into an EMPTY composer. A plain-text input scan would
// misread it as operator content and false-positive every codex delivery as raced;
// the cursor-anchored check does not — the cursor stays at the sentinel for ghost-
// text and only moves past it for real operator typing. Both arms exercised.
func TestDeliver_PrePasteRace_CodexGhostTextSafe(t *testing.T) {
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	t.Run("ghost-text (cursor at sentinel) proceeds", func(t *testing.T) {
		shortRetries(t)
		calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
			switch args[0] {
			case "capture-pane":
				// Empty composer with dim placeholder ghost-text after the sentinel.
				return []byte("a prior turn\n" + CodexPromptSentinel + "Improve documentation in @file"), nil
			case "display-message":
				return []byte("2/1"), nil // cursor at sentinel col → empty despite ghost-text
			}
			return nil, nil
		})
		err := Deliver(context.Background(), DeliverParams{
			Pane: "%9", Body: "the bus message", VerifyToken: "id 7f3a",
		})
		if errors.Is(err, ErrInputRaced) {
			t.Fatalf("ghost-text must NOT be read as operator content (AC#5); got ErrInputRaced")
		}
		if !cmdRan(calls, "load-buffer") {
			t.Errorf("ghost-text delivery must proceed to paste; calls=%v", *calls)
		}
	})

	t.Run("operator typing (cursor past sentinel) races", func(t *testing.T) {
		shortRetries(t)
		calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
			switch args[0] {
			case "capture-pane":
				return []byte("a prior turn\n" + CodexPromptSentinel + "operator typed this"), nil
			case "display-message":
				return []byte("14/1"), nil // cursor past sentinel → real operator content
			}
			return nil, nil
		})
		err := Deliver(context.Background(), DeliverParams{
			Pane: "%9", Body: "the bus message", VerifyToken: "id 7f3a",
		})
		if !errors.Is(err, ErrInputRaced) {
			t.Fatalf("codex operator content must race; got %v", err)
		}
		if cmdRan(calls, "load-buffer") {
			t.Errorf("a raced codex input must abort before paste; calls=%v", *calls)
		}
	})
}

// TestDeliver_PrePasteRace_DegradesOpenWhenCursorUnanchored pins the best-effort
// posture: when the cursor can't anchor the input row (query failed / no sentinel),
// the pre-paste check degrades OPEN — it pastes anyway rather than blocking, the
// same fallback the verify uses. The check tightens the window; it must not become
// a new way to wedge delivery when the cursor is unreadable.
func TestDeliver_PrePasteRace_DegradesOpenWhenCursorUnanchored(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			return []byte("output with id 7f3a visible"), nil
		case "display-message":
			return []byte("not-a-cursor"), nil // unparseable → cursor query fails
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "the bus message", VerifyToken: "id 7f3a",
	})
	if errors.Is(err, ErrInputRaced) {
		t.Fatalf("cursor-unanchored must degrade open, not race; got ErrInputRaced")
	}
	if !cmdRan(calls, "load-buffer") {
		t.Errorf("degrade-open must still paste; calls=%v", *calls)
	}
}

// TestDeliver_PrePasteRace_PriorStuckPasteDefersAndDrains is the #610 load-
// bearing invariant + mutation anchor: when a PRIOR delivery's collapsed paste
// is still sitting unsubmitted in the codex input (the `[Pasted Content]` marker
// in the bottom-most input block) at pre-paste time, Deliver must NOT paste this
// message onto it — that is the stacking #610 reports. Instead it returns
// ErrPriorPasteStuck and fires ONE resubmit Enter to drain the stuck paste
// across mailman cycles. Critically the cursor reads CLEARED here (codex parks
// it on an empty sub-line of the multi-line input), so the #616 cursor-anchor
// check does NOT fire — this is exactly the hole #616 left to the resubmit
// machinery, which #610 falls through under load.
func TestDeliver_PrePasteRace_PriorStuckPasteDefersAndDrains(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			// A prior collapsed paste stuck unsubmitted in the bottom-most input.
			return []byte("a prior turn\n" + CodexPromptSentinel + CodexPasteCollapseMarker + " 1024 chars]"), nil
		case "display-message":
			// Cursor AT the sentinel col: codex parks it on the empty sub-line, so
			// #616's inputRowCleared reads "cleared" and would NOT race — proving the
			// marker check (not the cursor) is what catches the stuck paste.
			return []byte("2/1"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%9", Body: "the bus message", VerifyToken: "id 7f3a",
	})
	if !errors.Is(err, ErrPriorPasteStuck) {
		t.Fatalf("err = %v, want ErrPriorPasteStuck (prior collapsed paste unsubmitted in input)", err)
	}
	// Must NOT paste onto the stuck message — that is the stacking #610 reports.
	for _, cmd := range []string{"load-buffer", "paste-buffer"} {
		if cmdRan(calls, cmd) {
			t.Errorf("%s ran, but a stuck prior paste must abort BEFORE any paste (no stacking)", cmd)
		}
	}
	// Must fire exactly one drain Enter (the cross-cycle resubmit), no more.
	enters := 0
	for _, c := range *calls {
		if len(c.args) >= 2 && c.args[0] == "send-keys" && c.args[len(c.args)-1] == "Enter" {
			enters++
		}
	}
	if enters != 1 {
		t.Errorf("want exactly 1 drain Enter (send-keys Enter), got %d; calls=%v", enters, *calls)
	}
}

// TestDeliver_PrePasteRace_SubmittedPasteProceeds is the #610 negative control:
// a SUBMITTED paste lingers as a transcript entry ABOVE a fresh empty input
// prompt — the collapse marker is NOT in the bottom-most sentinel block, so
// pasteStillInInput is false and Deliver proceeds normally. Without this, the
// stuck-paste guard would false-positive on every post-submit codex pane (whose
// transcript still shows the collapsed paste) and wedge all delivery.
func TestDeliver_PrePasteRace_SubmittedPasteProceeds(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			// Submitted: the paste lingers in the transcript above; a NEW empty
			// input prompt is the bottom-most sentinel.
			return []byte(CodexPromptSentinel + CodexPasteCollapseMarker + " 1024 chars]\n(some output)\n" + CodexPromptSentinel), nil
		case "display-message":
			return []byte("2/1"), nil // cursor at the fresh empty sentinel
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%9", Body: "the bus message", VerifyToken: "id 7f3a",
	})
	if errors.Is(err, ErrPriorPasteStuck) {
		t.Fatalf("a submitted paste (marker above a fresh empty prompt) must NOT defer; got ErrPriorPasteStuck")
	}
	if !cmdRan(calls, "load-buffer") {
		t.Errorf("submitted-paste pane must proceed to deliver; calls=%v", *calls)
	}
}

// TestDeliver_PrePasteRace_OperatorLargePasteAlsoDrains pins the deliberate LIMIT
// of the #610 defer-on-stuck-marker check (Surveyor's edge-case lens): it keys on
// the collapse marker being present in the bottom-most input block, which looks
// IDENTICAL whether the marker is a prior bus delivery's stuck paste OR an
// operator's own large paste they are still composing. The check cannot tell the
// provenance apart from a capture, so it treats both the same — fires the drain
// Enter (which SUBMITS an operator's mid-flight large paste a beat early) and
// defers the bus message. This is the substrate-honest trade-off: it is
// consistent with the #401 resubmit, which already re-sends Enter on any stuck
// marker without provenance attribution; the alternative (leaving the marker
// untouched) reproduces #610's stuck-chamber pain. Distinguishing provenance
// (drain only markers the mailman itself left stuck) needs cross-cycle
// attribution state — the load-adaptive follow-up, not this fix.
func TestDeliver_PrePasteRace_OperatorLargePasteAlsoDrains(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })
	var enters int
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			// An operator's own large paste, collapsed + not yet submitted —
			// indistinguishable from a prior bus delivery's stuck paste.
			return []byte("transcript\n" + CodexPromptSentinel + CodexPasteCollapseMarker + " 4096 chars] (operator composing)"), nil
		case "send-keys":
			if contains(args, "Enter") {
				enters++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%9", Body: "the bus message", VerifyToken: "tok",
	})
	if !errors.Is(err, ErrPriorPasteStuck) {
		t.Fatalf("err = %v, want ErrPriorPasteStuck (marker present, provenance-blind by design)", err)
	}
	// The drain Enter fires — which submits the operator's paste. Documented trade-off.
	if enters != 1 {
		t.Errorf("want exactly 1 drain Enter (it submits the operator's paste — the documented trade-off), got %d", enters)
	}
	// It still never pastes the BUS message onto the composer (no stacking).
	for _, cmd := range []string{"load-buffer", "paste-buffer"} {
		if cmdRan(calls, cmd) {
			t.Errorf("%s ran, but defer must abort before pasting the bus message", cmd)
		}
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
		// #787 live Admin fixture: Win11 renders Claude's prompt as `>` + NBSP.
		// Cursor column 2 is the authoritative empty-input signal; the verify
		// token is absent because Claude has already consumed the turn.
		{"win11 ascii-nbsp prompt verifies cleared input", claude, "submitted turn\n" + ASCIIPromptSentinel, 2, 1, true, "absent-token", true},
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

// TestDeliver_InputEmptied_VerifiesWin11Prompt pins #787's post-paste half
// end-to-end. #799 made this render variant classify idle before paste; the
// same profile variant must anchor the input-emptied verification after Enter.
func TestDeliver_InputEmptied_VerifiesWin11Prompt(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	defer SetActivePaneProfile(prev)

	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("submitted turn\n" + ASCIIPromptSentinel), nil
		}
		if args[0] == "display-message" {
			return []byte("2/1"), nil
		}
		return nil, nil
	})
	if err := Deliver(context.Background(), DeliverParams{
		Pane: "%11", Body: "x", VerifyToken: "id d5a7",
	}); err != nil {
		t.Fatalf("Win11 cleared input should verify with token absent: %v", err)
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
		PrePasteRaceCheckDisabled: true, // #616: static post-paste capture; not testing the pre-paste check
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
			// captureN==1 is the #610 pre-paste check: the input is CLEAN here (no
			// PRIOR stuck paste — this delivery hasn't pasted yet). THIS delivery's
			// paste then collapses and the first Enter is eaten, so the post-paste
			// verify captures (2,3) show the marker stuck and the #401 resubmit
			// re-sends Enter until the input clears at 4. (A marker already present
			// at the pre-paste capture is the distinct #610 prior-stuck case, which
			// defers — see TestDeliver_PrePasteRace_PriorStuckPasteDefersAndDrains.)
			if captureN >= 2 && captureN <= 3 {
				// Stuck: collapsed paste is the live (bottom) input.
				return []byte("transcript\n" + CodexPromptSentinel + "[Pasted Content 2048 chars] tail"), nil
			}
			// Clean pre-paste (1) / idle-submitted (>=4): input cleared.
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

// TestDeliver_Codex_ResubmitsStuckLiteralPaste pins #758: if codex consumes
// the initial Enter, a short paste remains literal in the composer (no
// [Pasted Content] marker). The delivery's own token in the bottom-most live
// prompt is sufficient ownership proof to re-send Enter after the frame
// settles. Before #758 this exhausted verification with one Enter because the
// resubmit trigger was collapse-marker-only.
func TestDeliver_Codex_ResubmitsStuckLiteralPaste(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var captureN, enterPresses int
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			captureN++
			if captureN <= 2 {
				return []byte("old turn id old\n" + CodexPromptSentinel + "[Carpenter · id fresh] short body"), nil
			}
			return []byte("submitted turn id fresh\n" + CodexPromptSentinel), nil
		case "display-message":
			if captureN >= 3 {
				return []byte("2/1"), nil
			}
			return []byte("36/1"), nil
		case "send-keys":
			if contains(args, "Enter") {
				enterPresses++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%8", Body: "short body", VerifyToken: "id fresh",
		PrePasteRaceCheckDisabled: true,
	})
	if err != nil {
		t.Fatalf("want literal paste to verify after resubmit, got %v", err)
	}
	if enterPresses != 2 {
		t.Errorf("want initial Enter + one token-owned resubmit; got %d", enterPresses)
	}
}

// A token retained in transcript history must not authorize submitting a new
// operator draft in the live composer. This is the safety boundary for #758's
// literal-paste resubmit arm.
func TestDeliver_Codex_DoesNotResubmitOperatorDraftWhenTokenOnlyInTranscript(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var enterPresses int
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			return []byte("submitted turn id fresh\n" + CodexPromptSentinel + "operator draft"), nil
		case "display-message":
			return []byte("16/1"), nil
		case "send-keys":
			if contains(args, "Enter") {
				enterPresses++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%8", Body: "short body", VerifyToken: "id fresh",
		PrePasteRaceCheckDisabled: true,
	})
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Fatalf("want unverified without touching operator draft, got %v", err)
	}
	if enterPresses != 1 {
		t.Errorf("operator draft must not be resubmitted; got %d Enter presses", enterPresses)
	}
}

// TestDeliver_Claude_DoesNotResubmitOperatorDraft pins the operator-draft safety
// invariant on the Claude profile: a composer holding HAND-TYPED text must never
// draw a resubmit Enter, because that Enter would submit the operator's draft.
//
// ⚠️ This test was named TestDeliver_Claude_NoResubmit before #842 and its doc
// claimed the #401 resubmit is "codex-specific". That is no longer true — Claude
// DOES resubmit as of #842 (see TestDeliver_Claude_ResubmitsStuckCollapsedPaste).
// The invariant this fixture actually exercises is narrower and survives #842
// intact: the resubmit predicate keys on PasteEvidenceMarker / the verify token,
// neither of which a hand-typed draft can produce. Renamed so the pin does not
// assert something false — during #842 this test caught a first-draft fix that
// keyed on cursor-anchored input-not-cleared, which would have submitted exactly
// the draft this fixture plants.
func TestDeliver_Claude_DoesNotResubmitOperatorDraft(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var enterPresses int
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			// Token never surfaces, input never clears → stays unverified. The
			// composer holds a hand-typed draft: no paste-evidence marker, no
			// verify token, so pasteUnsubmitted must stay false and no resubmit
			// may fire (#842).
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
		PrePasteRaceCheckDisabled: true, // #616: static post-paste capture; not testing the pre-paste check
	})
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Fatalf("want ErrUnverifiedDelivery, got %v", err)
	}
	if enterPresses != 1 {
		t.Errorf("operator draft must not be resubmitted; want exactly 1 Enter (the initial submit), got %d", enterPresses)
	}
}

// TestDeliver_Claude_ResubmitsStuckCollapsedPaste pins #842: a Claude paste that
// landed in the composer but did NOT submit on the first Enter now draws a
// stability-gated resubmit, instead of sitting there until the operator presses
// Enter by hand.
//
// Pre-#842 this was structurally impossible: the resubmit gate keyed on
// PasteCollapseMarker, which ClaudePaneProfile leaves empty, so pastePresent was
// ALWAYS false for Claude — Deliver sent exactly one Enter and the verify loop
// only re-CAPTURED. The recovery path was the operator's keyboard (tmux-tell#842,
// bus msg 1b75 → Quartermaster).
//
// The capture fixture is substrate-shaped, measured 2026-07-24 on claude 2.1.218
// against a CLEAN composer across three body shapes: a settled large paste
// collapses ENTIRELY to `❯ [Pasted text #N +M lines]` on the sentinel row, cursor
// at col 28.
//
// ⚠️ An earlier revision of this fixture put the paste's first line literally on
// the ❯ row with the placeholder on a following row. That shape was a measurement
// artifact (a probe clearing with C-u on too short a delay, so the "first line"
// was the prior trial's residue) and production does not produce it. It also
// happened to be the arrangement in which the cursor sits OFF the sentinel row —
// so the fixture was pinning the one layout where deliverySubmitted's anchoring
// behaves differently. Corrected to the measured shape (#842 review, Engineer).
func TestDeliver_Claude_ResubmitsStuckCollapsedPaste(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var enterPresses, captureN int
	stuck := func() bool { return captureN >= 2 && captureN <= 3 }
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			captureN++
			if stuck() {
				return []byte("transcript\n" + PromptSentinel + "[Pasted text #1 +38 lines]"), nil
			}
			// captureN==1 is the #610 pre-paste check (clean); >=4 is submitted.
			return []byte("transcript\n" + PromptSentinel), nil
		case "display-message":
			if stuck() {
				return []byte("28/1"), nil // measured col; past sentinel → not cleared
			}
			return []byte("2/1"), nil // cursor at sentinel → cleared
		case "send-keys":
			if contains(args, "Enter") {
				enterPresses++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "tok",
	})
	if err != nil {
		t.Fatalf("expected verify after #842 resubmit, got %v", err)
	}
	// initial submit Enter + at least one stability-gated resubmit.
	if enterPresses < 2 {
		t.Errorf("expected >=2 Enter presses (initial submit + #842 resubmit), got %d", enterPresses)
	}
}

// TestDeliver_Claude_StuckPasteNotReportedSubmittedWhenCursorCannotAnchor is the
// regression test for the gate-ordering defect Engineer found reviewing #848.
//
// deliverySubmitted runs BEFORE the resubmit predicate and can return early. When
// the cursor cannot anchor the input row, it falls back to a WHOLE-PANE token
// match — and the verify token is `id <PublicID>`, which render.Message emits in
// the message HEADER, i.e. inside the live composer of a stuck paste. So a stuck
// Claude paste read as SUBMITTED, Deliver returned nil, and #842's resubmit was
// never reached: the recovery sat behind a gate that already said "delivered".
//
// The reachable production path is a cursor query FAILURE (cursorOK=false), or any
// capture whose cursor row cannot be anchored, combined with a paste small enough
// not to collapse — it renders literally, so the header token sits in the composer
// and satisfies the whole-pane Contains.
//
// ⚠️ Explicitly NOT the `> `-rendering (ASCII-variant) Claude pane: that pane
// anchors fine, because matchCursorRowSentinel DOES honor PromptSentinelVariants.
// Its gap is the inverse — verification works, recovery is inert, because
// liveInputContains is primary-sentinel-only. See pasteUnsubmitted's KNOWN GAP.
// An earlier revision of this comment had that attribution backwards.
//
// Fixture: cursor points at row 0 ("transcript"), which is NOT a sentinel row, so
// anchoring fails; the token sits in the composer alongside the placeholder. With
// the adapter-aware override the paste reads not-submitted and the resubmit fires;
// without it, exactly one Enter is sent and the message is silently marked
// delivered.
func TestDeliver_Claude_StuckPasteNotReportedSubmittedWhenCursorCannotAnchor(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var enterPresses, captureN int
	stuck := func() bool { return captureN >= 2 && captureN <= 3 }
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			captureN++
			if stuck() {
				// Token present in the live composer next to the placeholder —
				// the whole-pane fallback would read this as "submitted".
				return []byte("transcript\n" + PromptSentinel +
					"[Pasted text #1 +38 lines] id fresh"), nil
			}
			return []byte("transcript\n" + PromptSentinel), nil
		case "display-message":
			if stuck() {
				return []byte("5/0"), nil // row 0 is NOT a sentinel row → cannot anchor
			}
			return []byte("2/1"), nil
		case "send-keys":
			if contains(args, "Enter") {
				enterPresses++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id fresh",
	})
	if err != nil {
		t.Fatalf("expected verify after resubmit, got %v", err)
	}
	if enterPresses < 2 {
		t.Errorf("stuck paste must not read as submitted via the unanchored token "+
			"fallback; want >=2 Enter presses (initial + resubmit), got %d", enterPresses)
	}
}

// TestDeliver_Claude_TokenArmCatchesLiteralPasteWhenCursorQueryFails pins the
// TOKEN arm of the deliverySubmitted override (#842 review round 3, Engineer).
//
// Why this test exists when the sibling above already covers an unanchored cursor:
// the sibling exercises the MARKER arm, and the two come apart under a narrower
// mutation than "remove the override" — narrow the override to the marker arm only:
//
//	before:  if pasteUnsubmitted(capture, verifyToken) {
//	after:   if liveInputContains(capture, activeProfile.PasteEvidenceMarker) {
//
// (Tab-indented code block, not a -/+ diff: gofmt parses a leading `-`/`+` as a
// bullet list and rewrites BOTH lines to `-`, which silently destroys the
// before/after and leaves a reader unable to tell the original from the mutation.
// That happened here once already — #842 review round 4, Engineer.)
//
// which leaves the FULL SUITE GREEN while restoring the false verify. Existing
// coverage guarded "the override exists", not "the override consults the token",
// and those separate under exactly the likely refactor (someone simplifying
// deliverySubmitted back toward a marker check).
//
// 🔴 And the arms are not equally reachable. Measured 2026-07-24: a large paste
// collapses onto the sentinel row and the cursor ANCHORS, so marker-arm-under-
// unanchored-cursor has no demonstrated production route. This one does: a cursor
// query FAILURE plus a paste small enough not to collapse renders literally, so
// the header token (`id <PublicID>`, emitted first by render.Message) sits in the
// composer and satisfies the whole-pane Contains. The hypothetical arm was pinned
// first; this is the reachable one.
func TestDeliver_Claude_TokenArmCatchesLiteralPasteWhenCursorQueryFails(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var enterPresses int
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			// Small paste → renders LITERALLY, no `[Pasted text` placeholder, so the
			// marker arm is blind to it. The verify token sits in the live composer.
			return []byte("old turn id old\n" + PromptSentinel + "[Bosun · id fresh] short body"), nil
		case "display-message":
			// Cursor query FAILS → cursorOK=false → inputRowCleared cannot anchor →
			// deliverySubmitted falls through to the whole-pane token match.
			return nil, errors.New("no server running")
		case "send-keys":
			if contains(args, "Enter") {
				enterPresses++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id fresh",
	})
	// The outer assertion: without the token arm this returns nil — a FALSE VERIFY,
	// message marked delivered while the paste sits unsubmitted.
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Fatalf("literal paste stuck in the composer must NOT verify via the "+
			"whole-pane token fallback; want ErrUnverifiedDelivery, got %v", err)
	}
	if enterPresses < 2 {
		t.Errorf("want >=2 Enter presses (initial + token-arm resubmit), got %d", enterPresses)
	}
}

// TestDeliver_Claude_ResubmitsStuckLiteralPasteViaToken covers the small-paste
// half of #842: a Claude paste short enough that the composer does NOT collapse
// it renders literally, so there is no `[Pasted text` marker — the verify token
// visible in the LIVE composer is what proves our paste is sitting unsubmitted.
// Same hole #758 closed on the codex side, now reachable on Claude.
func TestDeliver_Claude_ResubmitsStuckLiteralPasteViaToken(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var enterPresses, captureN int
	stuck := func() bool { return captureN >= 2 && captureN <= 3 }
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			captureN++
			if stuck() {
				// Token BELOW the bottom-most sentinel = still in the composer.
				return []byte("old turn id old\n" + PromptSentinel + "[Bosun · id fresh] short body"), nil
			}
			return []byte("transcript\n" + PromptSentinel), nil
		case "display-message":
			if stuck() {
				return []byte("30/1"), nil
			}
			return []byte("2/1"), nil
		case "send-keys":
			if contains(args, "Enter") {
				enterPresses++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id fresh",
	})
	if err != nil {
		t.Fatalf("expected verify after token-arm resubmit, got %v", err)
	}
	if enterPresses < 2 {
		t.Errorf("expected >=2 Enter presses (initial submit + token-arm resubmit), got %d", enterPresses)
	}
}

// codexCleanCapture / codexStuckCapture build the two codex verify-frame shapes
// the #674 tests script: an empty submitted composer (input cleared, cursor at
// sentinel) vs a collapsed paste still stuck in the input (marker present).
func codexCleanCapture() []byte {
	return []byte("transcript\n" + CodexPromptSentinel)
}
func codexStuckCapture(suffix string) []byte {
	return []byte("transcript\n" + CodexPromptSentinel + "[Pasted Content 2048 chars] " + suffix)
}

// TestDeliver_Codex_LoadScaledBudgetExtension is the #674 dir-2 regression pin:
// a codex paste that is STILL mid-ingest when the base retry schedule exhausts
// (collapse marker present AND the frame changing every poll) gets the verify
// budget EXTENDED, so it submits within its own mailman cycle instead of
// returning ErrUnverifiedDelivery and deferring to the next visit.
//
// Under shortRetries len(retryDelays)==2 (base attempts 0..2); the clean
// (submitted) frame appears at attempt 3, inside the load-adaptive extension
// zone — unreachable without the extension.
// Mutation: set maxLoadAdaptiveExtraAttempts=0 → the loop never reaches attempt
// 3 → ErrUnverifiedDelivery.
func TestDeliver_Codex_LoadScaledBudgetExtension(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var captureN int
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			captureN++
			if captureN <= 3 {
				// attempts 0..2 (base schedule): still mid-ingest, frame changing
				// each poll (unique suffix) → resubmit held, budget must extend.
				return codexStuckCapture(fmt.Sprintf("ingest-%d", captureN)), nil
			}
			// attempt 3 (extension zone): codex finished, input cleared → submitted.
			return codexCleanCapture(), nil
		case "display-message":
			return []byte("2/1"), nil // cursor at sentinel
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%8", Body: "x", VerifyToken: "tok",
		PrePasteRaceCheckDisabled: true, // isolate the verify loop; captureN == attempt+1
	})
	if err != nil {
		t.Fatalf("want in-cycle verify via load-adaptive extension, got %v", err)
	}
	// 4 verify polls (attempts 0..3): the extension carried it one poll past the
	// base schedule of 3 (len(retryDelays)+1). Fewer would mean no extension.
	var captures int
	for _, c := range *calls {
		if c.args[0] == "capture-pane" {
			captures++
		}
	}
	if captures != len(retryDelays)+2 {
		t.Errorf("want %d capture-pane polls (base %d + 1 extension); got %d",
			len(retryDelays)+2, len(retryDelays)+1, captures)
	}
}

// TestDeliver_Codex_StabilityGatedResubmit is the #674 dir-1 regression pin: the
// #401 resubmit Enter fires ONLY on a settled frame (two identical consecutive
// captures) with the marker still present — not while the frame is still
// redrawing (an Enter sent mid-render is eaten). Scripted so the only settled-
// with-marker frame is attempt 1; exactly one resubmit fires.
//
// Mutation: drop the `!frameChanging` gate (fire on markerPresent alone) → the
// resubmit also fires on attempt 0's marker frame → enterPresses==3, not 2.
func TestDeliver_Codex_StabilityGatedResubmit(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var captureN, enterPresses int
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			captureN++
			switch captureN {
			case 1, 2:
				// attempts 0,1: marker present, IDENTICAL frames. attempt 0 is
				// forced "changing" (no prior frame); attempt 1 sees the stable
				// frame vs attempt 0 → the single resubmit fires here.
				return codexStuckCapture("stable"), nil
			default:
				// attempt 2: submitted, input cleared.
				return codexCleanCapture(), nil
			}
		case "display-message":
			return []byte("2/1"), nil
		case "send-keys":
			if contains(args, "Enter") {
				enterPresses++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%8", Body: "x", VerifyToken: "tok",
		PrePasteRaceCheckDisabled: true,
	})
	if err != nil {
		t.Fatalf("want verify after stability-gated resubmit, got %v", err)
	}
	// initial submit Enter + exactly ONE stability-gated resubmit = 2. A blind
	// resubmit (pre-#674) would also fire on attempt 0's changing frame = 3.
	if enterPresses != 2 {
		t.Errorf("want 2 Enter presses (initial + 1 stability-gated resubmit); got %d", enterPresses)
	}
}

// TestDeliver_Codex_BestEffortFinalResubmit pins the #674 safety net + the
// extension ceiling: when the frame changes on EVERY poll (never settles) the
// stability gate never fires a resubmit, so a final best-effort Enter fires once
// at exit to preserve the pre-#674 "a stuck paste always gets one resubmit"
// guarantee — and the extension is bounded at maxLoadAdaptiveExtraAttempts.
//
// Mutation: remove the best-effort block → enterPresses==1 (initial only).
func TestDeliver_Codex_BestEffortFinalResubmit(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var captureN, enterPresses int
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			captureN++
			// Every poll differs (never settles) → dir-1 gate never fires; dir-2
			// extends to the ceiling; marker persists to the end.
			return codexStuckCapture(fmt.Sprintf("churn-%d", captureN)), nil
		case "display-message":
			return []byte("2/1"), nil
		case "send-keys":
			if contains(args, "Enter") {
				enterPresses++
			}
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%8", Body: "x", VerifyToken: "tok",
		PrePasteRaceCheckDisabled: true,
	})
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Fatalf("want ErrUnverifiedDelivery (never settled), got %v", err)
	}
	// initial Enter + exactly one best-effort final Enter (no in-loop resubmit
	// ever fired, since the frame never settled).
	if enterPresses != 2 {
		t.Errorf("want 2 Enter presses (initial + best-effort final); got %d", enterPresses)
	}
	// Extension ran to the ceiling: base+extra verify polls.
	var captures int
	for _, c := range *calls {
		if c.args[0] == "capture-pane" {
			captures++
		}
	}
	if want := len(retryDelays) + maxLoadAdaptiveExtraAttempts + 1; captures != want {
		t.Errorf("want %d capture-pane polls (extension to ceiling); got %d", want, captures)
	}
}

// TestDeliver_Claude_NoLoadAdaptiveExtension is the #674 codex-scoping negative-
// space pin: with no collapse marker (Claude) the load-adaptive extension never
// triggers — the verify loop runs exactly the base schedule (len(retryDelays)+1
// polls) and returns unverified, never extending into the ceiling zone.
func TestDeliver_Claude_NoLoadAdaptiveExtension(t *testing.T) {
	shortRetries(t)
	prev := ActivePaneProfile()
	SetActivePaneProfile(ClaudePaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			// Never clears, no collapse marker → unverified, no extension.
			return []byte("transcript\n" + PromptSentinel + "half-typed draft"), nil
		case "display-message":
			return []byte("30/1"), nil // cursor past sentinel → not cleared
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "never-appears",
		PrePasteRaceCheckDisabled: true,
	})
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Fatalf("want ErrUnverifiedDelivery, got %v", err)
	}
	var captures int
	for _, c := range *calls {
		if c.args[0] == "capture-pane" {
			captures++
		}
	}
	if captures != len(retryDelays)+1 {
		t.Errorf("Claude must run exactly the base schedule (%d polls, no extension); got %d",
			len(retryDelays)+1, captures)
	}
}

// TestDeliver_EmptyRetrySchedule_NoPanic pins the #695 defensive guard: a
// degenerate empty retryDelays (SetRetrySchedule([])) must NOT panic on the
// load-adaptive patient-tail index (retryDelays[-1]). The extension ceiling
// (len+extra) makes attempts past attempt 0 executable even with no schedule;
// the idx<0 guard stops after the immediate poll, matching the pre-#674 empty-
// schedule behavior (one verify capture). Mutation: remove the guard → panic.
func TestDeliver_EmptyRetrySchedule_NoPanic(t *testing.T) {
	shortRetries(t)   // near-zero settle; restores retryDelays on cleanup
	retryDelays = nil // #695: degenerate empty schedule
	prev := ActivePaneProfile()
	SetActivePaneProfile(CodexPaneProfile())
	t.Cleanup(func() { SetActivePaneProfile(prev) })

	var captures int
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			captures++
			// Marker present + never clears: would extend if the schedule were
			// non-empty; with an empty schedule the loop must stop after one poll
			// rather than index retryDelays[-1].
			return codexStuckCapture("stuck"), nil
		case "display-message":
			return []byte("2/1"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%8", Body: "x", VerifyToken: "tok",
		PrePasteRaceCheckDisabled: true,
	})
	if !errors.Is(err, ErrUnverifiedDelivery) {
		t.Fatalf("want ErrUnverifiedDelivery (no panic), got %v", err)
	}
	if captures != 1 {
		t.Errorf("empty schedule must run exactly one verify poll; got %d", captures)
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
