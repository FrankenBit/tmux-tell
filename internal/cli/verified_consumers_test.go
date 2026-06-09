package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// seedDelivered drives a message to `delivered` with a specific #169 verified
// bit: "verified" (=1, confirmed), "unverified" (=0, delivered_in_input_box),
// or "null" (pre-#169 row). The NULL case marks delivered then clears the bit
// via raw SQL, simulating a row that predates the migration.
func seedDeliveredVerified(t *testing.T, s *store.Store, from, to, body, vState string) string {
	t.Helper()
	ctx := context.Background()
	r, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: from, ToAgent: to, Body: body})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.ClaimNext(ctx, to); err != nil {
		t.Fatalf("claim: %v", err)
	}
	switch vState {
	case "verified":
		if err := s.MarkDelivered(ctx, r.PublicID); err != nil {
			t.Fatalf("mark: %v", err)
		}
	case "unverified":
		if err := s.MarkDeliveredInInputBox(ctx, r.PublicID); err != nil {
			t.Fatalf("mark: %v", err)
		}
	case "null":
		if err := s.MarkDelivered(ctx, r.PublicID); err != nil {
			t.Fatalf("mark: %v", err)
		}
		if _, err := s.DB().ExecContext(ctx,
			`UPDATE messages SET verified = NULL WHERE public_id = ?`, r.PublicID); err != nil {
			t.Fatalf("null: %v", err)
		}
	default:
		t.Fatalf("bad vState %q", vState)
	}
	return r.PublicID
}

func mustGetMsg(t *testing.T, s *store.Store, id string) *store.Message {
	t.Helper()
	m, err := s.GetMessage(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return m
}

// TestDisplayState_TriState pins the single-sourced display-state synthesis
// (#230): only the verified=0 soft-fail renders as delivered_in_input_box;
// verified=1 and pre-#169 NULL both render as plain delivered.
func TestDisplayState_TriState(t *testing.T) {
	s := newCmdTestStore(t)
	_ = s.UpsertAgent(context.Background(), "alice", "%1")
	cases := []struct{ vState, want string }{
		{"verified", "delivered"},
		{"unverified", displayStateDeliveredInInputBox},
		{"null", "delivered"},
	}
	for _, c := range cases {
		id := seedDeliveredVerified(t, s, "alice", "alice", "x", c.vState)
		if got := displayState(*mustGetMsg(t, s, id)); got != c.want {
			t.Errorf("displayState(%s) = %q, want %q", c.vState, got, c.want)
		}
	}
}

// TestResendGuard_DecisionC pins decision (C): a delivered_in_input_box message
// replays without --force; a confirmed (verified=1) or pre-marker (NULL)
// delivered message still gates; --force admits everything.
func TestResendGuard_DecisionC(t *testing.T) {
	s := newCmdTestStore(t)
	_ = s.UpsertAgent(context.Background(), "alice", "%1")
	unverified := mustGetMsg(t, s, seedDeliveredVerified(t, s, "alice", "alice", "u", "unverified"))
	verified := mustGetMsg(t, s, seedDeliveredVerified(t, s, "alice", "alice", "v", "verified"))
	nullRow := mustGetMsg(t, s, seedDeliveredVerified(t, s, "alice", "alice", "n", "null"))

	if _, ok := resendGuard(unverified, false); !ok {
		t.Error("delivered_in_input_box should replay WITHOUT force (decision C)")
	}
	if _, ok := resendGuard(verified, false); ok {
		t.Error("confirmed delivered (verified=1) should still gate without force")
	}
	if _, ok := resendGuard(nullRow, false); ok {
		t.Error("pre-marker delivered (verified=NULL) should still gate without force")
	}
	for _, m := range []*store.Message{unverified, verified, nullRow} {
		if _, ok := resendGuard(m, true); !ok {
			t.Errorf("force should admit %s", m.PublicID)
		}
	}
}

// TestResendForceUnverified_WarnOnce pins the ADR-0008 deprecation WARN: it
// fires only for --force against a delivered_in_input_box message, carries the
// canonical fields, and fires at most once per process.
func TestResendForceUnverified_WarnOnce(t *testing.T) {
	s := newCmdTestStore(t)
	_ = s.UpsertAgent(context.Background(), "alice", "%1")
	unverified := mustGetMsg(t, s, seedDeliveredVerified(t, s, "alice", "alice", "u", "unverified"))
	verified := mustGetMsg(t, s, seedDeliveredVerified(t, s, "alice", "alice", "v", "verified"))

	resetResendForceWarnForTest()
	var buf bytes.Buffer
	// No warn for force on a verified message (force IS needed there).
	maybeWarnResendForceUnverified(&buf, verified, true)
	if buf.Len() != 0 {
		t.Errorf("no warn expected for verified+force; got %q", buf.String())
	}
	// No warn when force is absent.
	maybeWarnResendForceUnverified(&buf, unverified, false)
	if buf.Len() != 0 {
		t.Errorf("no warn expected without force; got %q", buf.String())
	}
	// Warn fires for force on unverified.
	maybeWarnResendForceUnverified(&buf, unverified, true)
	out := buf.String()
	for _, want := range []string{
		"deprecated_surface_used", "name=resend_force_unverified", "removal=v0.15.0", "ADR-0008",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warn missing %q; got %q", want, out)
		}
	}
	// Once per process: a second deprecated use is silent.
	buf.Reset()
	maybeWarnResendForceUnverified(&buf, unverified, true)
	if buf.Len() != 0 {
		t.Errorf("warn should fire once per process; second fire = %q", buf.String())
	}
}

// TestResend_UnverifiedReplaysWithoutForce drives the full CLI path: a
// delivered_in_input_box original replays with exit OK, no --force, and the
// replay block surfaces OriginalState=delivered_in_input_box + Forced=false.
func TestResend_UnverifiedReplaysWithoutForce(t *testing.T) {
	s := resendStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)
	resetResendForceWarnForTest()
	orig := seedDeliveredVerified(t, s, "alice", "bob", "soft-failed one", "unverified")

	var stdout, stderr bytes.Buffer
	exit := runResendWithStore(context.Background(), s,
		resendParams{OriginalID: orig, Format: "json"}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d (delivered_in_input_box should replay without force); stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if !r.OK || r.Replay == nil {
		t.Fatalf("resp = %+v, want ok + replay", r)
	}
	if r.Replay.OriginalState != displayStateDeliveredInInputBox {
		t.Errorf("OriginalState = %q, want %q", r.Replay.OriginalState, displayStateDeliveredInInputBox)
	}
	if r.Replay.Forced {
		t.Error("Forced should be false — no force needed for delivered_in_input_box")
	}
}

// TestThreadGlyph_Unverified pins the ⚠ glyph for the soft-fail display-state
// (distinct from ✓), the per-row surfacing #230 wires into the thread view.
func TestThreadGlyph_Unverified(t *testing.T) {
	if got := stateGlyph(displayStateDeliveredInInputBox); got != glyphUnverified {
		t.Errorf("stateGlyph(delivered_in_input_box) = %q, want %q", got, glyphUnverified)
	}
	if got := stateGlyph(string(store.StateDelivered)); got != glyphDelivered {
		t.Errorf("stateGlyph(delivered) = %q, want %q", got, glyphDelivered)
	}
}

// TestInboxMessageToMap_SurfacesUnverified pins that the shared MCP/inbox
// renderer reports the soft-fail display-state (#230).
func TestInboxMessageToMap_SurfacesUnverified(t *testing.T) {
	s := newCmdTestStore(t)
	_ = s.UpsertAgent(context.Background(), "alice", "%1")
	m := mustGetMsg(t, s, seedDeliveredVerified(t, s, "alice", "alice", "x", "unverified"))
	if got := messageToMap(*m)["state"]; got != displayStateDeliveredInInputBox {
		t.Errorf("messageToMap state = %v, want %q", got, displayStateDeliveredInInputBox)
	}
}
