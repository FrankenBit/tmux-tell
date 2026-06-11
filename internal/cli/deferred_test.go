package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func TestValidateDeferTrigger(t *testing.T) {
	for _, good := range []string{"resume", "register"} {
		if err := validateDeferTrigger(good); err != nil {
			t.Errorf("%q should be valid: %v", good, err)
		}
	}
	// "register" moved to valid in #258(a); timestamps + OR-composition stay
	// a #295 follow-up. Empty + whitespace/case variants stay rejected.
	for _, bad := range []string{"", "15m", "resume ", "RESUME", "Register"} {
		if err := validateDeferTrigger(bad); err == nil {
			t.Errorf("trigger %q should be rejected", bad)
		}
	}
}

// TestSend_DeferredStages drives the CLI send path: a deferred send returns OK
// with deliver_after echoed, the row is staged (NOT in the queued inbox), and
// it surfaces only under the Deferred opt-in.
func TestSend_DeferredStages(t *testing.T) {
	s := resendStore(t) // registers alice + bob
	withReachability(t, map[string]bool{"%3": true}, true)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(ctx, s, sendParams{
		From: "bob", To: "bob", Body: "post-compact orientation",
		DeliverAfter: "resume",
		MaxBody:      capBodyBytes, MaxRecipient: capRecipientQueue, MaxSender: capSenderBacklog,
		Format: "json",
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if !r.OK || r.DeliverAfter != "resume" {
		t.Fatalf("resp = %+v, want ok + deliver_after=resume", r)
	}

	// Not queued: the inbox (queued) view is empty.
	queued, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateQueued})
	if len(queued) != 0 {
		t.Errorf("deferred send leaked into the queued inbox: %d rows", len(queued))
	}
	// Staged: visible under the Deferred opt-in.
	def, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", Deferred: true})
	if len(def) != 1 || def[0].PublicID != r.ID {
		t.Errorf("deferred view = %v, want the staged message %s", def, r.ID)
	}
}

func TestSend_DeferredRejectsBadTrigger(t *testing.T) {
	s := resendStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(context.Background(), s, sendParams{
		From: "bob", To: "bob", Body: "x", DeliverAfter: "tomorrow",
		MaxBody: capBodyBytes, Format: "json",
	}, &stdout, &stderr)
	if exit != exitUsage {
		t.Fatalf("exit = %d, want exitUsage for bad trigger; out=%s", exit, stdout.String())
	}
	if !strings.Contains(stdout.String(), "unsupported deliver-after trigger") {
		t.Errorf("expected unsupported-trigger error; got %s", stdout.String())
	}
}

func TestSend_DeferredRejectsMultiRecipient(t *testing.T) {
	s := resendStore(t)
	withReachability(t, map[string]bool{"%1": true, "%3": true}, true)
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(context.Background(), s, sendParams{
		From: "bob", ToRecipients: []string{"alice", "bob"}, Body: "x",
		DeliverAfter: "resume", MaxBody: capBodyBytes, Format: "json",
	}, &stdout, &stderr)
	if exit != exitUsage {
		t.Fatalf("exit = %d, want exitUsage for multi-recipient deferral", exit)
	}
	if !strings.Contains(stdout.String(), "single-recipient only") {
		t.Errorf("expected single-recipient-only error; got %s", stdout.String())
	}
}

// TestFlushDeferred_RoundTrip exercises the shared flush logic end-to-end: a
// staged message is promoted by a matching flush and then claimable.
func TestFlushDeferred_RoundTrip(t *testing.T) {
	s := resendStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)
	ctx := context.Background()

	// Stage via the send path.
	var stdout bytes.Buffer
	runSendWithStore(ctx, s, sendParams{
		From: "bob", To: "bob", Body: "handoff", DeliverAfter: "resume",
		MaxBody: capBodyBytes, Format: "json",
	}, &stdout, &bytes.Buffer{})
	staged := decodeSend(t, stdout.Bytes()).ID

	// Flush.
	res, err := doFlushDeferred(ctx, s, "bob", "resume")
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !res.OK || res.Promoted != 1 || res.Trigger != "resume" {
		t.Errorf("flush result = %+v, want ok + promoted 1 + resume", res)
	}

	// Now claimable.
	claimed, _ := s.ClaimNext(ctx, "bob")
	if claimed == nil || claimed.PublicID != staged {
		t.Errorf("after flush, claim = %v, want staged %s", claimed, staged)
	}

	// Idempotent: a second flush (the row is now queued/claimed, none deferred)
	// promotes nothing and is not an error.
	res2, err := doFlushDeferred(ctx, s, "bob", "resume")
	if err != nil {
		t.Fatalf("second flush errored: %v", err)
	}
	if res2.Promoted != 0 {
		t.Errorf("second flush promoted %d, want 0 (idempotent)", res2.Promoted)
	}
}

// TestFlushDeferred_BadTrigger: the shared helper validates the trigger.
func TestFlushDeferred_BadTrigger(t *testing.T) {
	s := resendStore(t)
	if _, err := doFlushDeferred(context.Background(), s, "bob", "midnight"); err == nil {
		t.Error("flush with an unsupported trigger should error")
	}
}
