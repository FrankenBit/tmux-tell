package main

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// TestMaybeInsertFailureNotice_GeneratesNoticeOnFailed verifies that
// when NotifyOnFailed is enabled and the failed message is NOT itself
// a notice, a KindDeliveryFailureNotice is inserted back to the
// original sender.
func TestMaybeInsertFailureNotice_GeneratesNoticeOnFailed(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	// Original message: alice → bob.
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "the original message body",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	failed := &store.Message{
		PublicID:  res.PublicID,
		FromAgent: "alice", ToAgent: "bob",
		Body: "the original message body",
		Kind: store.KindMessage,
	}

	var logbuf bytes.Buffer
	logger := log.New(&logbuf, "", 0)

	maybeInsertFailureNotice(ctx, s, logger,
		true, "bob", failed, "failed",
		"tmux says pane is gone")

	// One queued notice from bob → alice should now exist.
	inbox, err := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "alice", State: store.StateQueued, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("alice's inbox = %d notices, want 1; log=%s", len(inbox), logbuf.String())
	}
	notice := inbox[0]
	if notice.Kind != store.KindDeliveryFailureNotice {
		t.Errorf("notice.Kind = %q, want %q", notice.Kind, store.KindDeliveryFailureNotice)
	}
	if notice.FromAgent != "bob" {
		t.Errorf("notice.FromAgent = %q, want bob", notice.FromAgent)
	}
	if !strings.Contains(notice.Body, res.PublicID) {
		t.Errorf("notice body should cite original public_id; got %q", notice.Body)
	}
	if !strings.Contains(notice.Body, "the original message body") {
		t.Errorf("notice body should include original body preview; got %q", notice.Body)
	}
	if !strings.Contains(notice.Body, "failed") {
		t.Errorf("notice body should mention failure class; got %q", notice.Body)
	}
	if !strings.Contains(logbuf.String(), "notify_inserted") {
		t.Errorf("expected notify_inserted log line; got %s", logbuf.String())
	}
}

// TestMaybeInsertFailureNotice_LoopPrevention verifies that a failed
// notice does NOT generate another notice (the wedged-pane loop).
func TestMaybeInsertFailureNotice_LoopPrevention(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	// The "failed" message is itself a notice: bob → alice.
	failedNotice := &store.Message{
		PublicID:  "TEST_NOTICE",
		FromAgent: "bob", ToAgent: "alice",
		Body: "[a prior failure notice]",
		Kind: store.KindDeliveryFailureNotice,
	}

	var logbuf bytes.Buffer
	logger := log.New(&logbuf, "", 0)

	maybeInsertFailureNotice(ctx, s, logger,
		true, "alice", failedNotice, "failed", "wedged pane")

	// No new messages should exist anywhere — loop prevention fired.
	all, err := s.ListMessages(ctx, store.ListFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("loop prevention failed: %d messages exist (expected 0)", len(all))
	}
}

// TestMaybeInsertFailureNotice_DisabledByConfig verifies the toggle
// (NotifyOnFailed=false) suppresses the notice.
func TestMaybeInsertFailureNotice_DisabledByConfig(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	failed := &store.Message{
		PublicID:  "TEST",
		FromAgent: "alice", ToAgent: "bob",
		Body: "original",
		Kind: store.KindMessage,
	}

	var logbuf bytes.Buffer
	logger := log.New(&logbuf, "", 0)

	maybeInsertFailureNotice(ctx, s, logger,
		false, "bob", failed, "failed", "tmux gone")

	all, _ := s.ListMessages(ctx, store.ListFilter{Limit: 10})
	if len(all) != 0 {
		t.Errorf("disabled toggle should suppress notice; got %d messages", len(all))
	}
}

// TestMaybeInsertFailureNotice_BypassesRecipientQueueCap verifies the
// cap-exemption commitment: a notice gets through even when the
// sender's queue is "full" by normal-cap standards.
//
// This is the operationally-critical-signal protection — losing a
// failure notice because alice's queue is congested would defeat the
// notification's whole point.
func TestMaybeInsertFailureNotice_BypassesRecipientQueueCap(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	// Saturate alice's queue with 5 regular messages.
	for i := 0; i < 5; i++ {
		_, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: "bob", ToAgent: "alice",
			Body: "filler", MaxRecipientQueue: 10,
		})
		if err != nil {
			t.Fatalf("filler %d: %v", i, err)
		}
	}

	failed := &store.Message{
		PublicID:  "ORIG",
		FromAgent: "alice", ToAgent: "bob",
		Body: "original",
		Kind: store.KindMessage,
	}

	var logbuf bytes.Buffer
	logger := log.New(&logbuf, "", 0)

	maybeInsertFailureNotice(ctx, s, logger,
		true, "bob", failed, "failed", "tmux gone")

	// alice's inbox should now have 6 entries: the 5 fillers + the
	// notice. The notice bypassed normal caps.
	inbox, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "alice", State: store.StateQueued, Limit: 10,
	})
	if len(inbox) != 6 {
		t.Errorf("alice's inbox = %d, want 6 (5 fillers + 1 notice); cap-exemption may have failed",
			len(inbox))
	}
	// And the last one should be the notice.
	var foundNotice bool
	for _, m := range inbox {
		if m.Kind == store.KindDeliveryFailureNotice {
			foundNotice = true
			break
		}
	}
	if !foundNotice {
		t.Errorf("no KindDeliveryFailureNotice found among inbox messages")
	}
}

// TestRenderFailureNoticeBody_Shape pins the body format the
// notification renders, since future tooling may grep for these
// stable fields.
func TestRenderFailureNoticeBody_Shape(t *testing.T) {
	msg := &store.Message{
		PublicID:  "abcd",
		FromAgent: "alice", ToAgent: "bob",
		Body: "the original body content",
		Kind: store.KindMessage,
		// State/CreatedAt/etc. left zero — rendering shouldn't touch.
		DeliveredAt: sql.NullString{},
		Error:       sql.NullString{},
	}
	body := renderFailureNoticeBody(msg, "failed", "tmux pane gone")
	required := []string{
		":warning:", "Delivery failure",
		"Original message id: abcd",
		"Recipient: bob",
		"Failure class: failed",
		"Reason: tmux pane gone",
		"Original body preview:",
		"the original body content",
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q in:\n%s", want, body)
		}
	}
}

// TestRenderFailureNoticeBody_TruncatesLongBodies verifies the
// preview cap doesn't blow up the notice body on large messages.
func TestRenderFailureNoticeBody_TruncatesLongBodies(t *testing.T) {
	long := strings.Repeat("x", 500)
	msg := &store.Message{
		PublicID: "id", FromAgent: "a", ToAgent: "b",
		Body: long, Kind: store.KindMessage,
	}
	body := renderFailureNoticeBody(msg, "failed", "reason")
	if !strings.Contains(body, "...(truncated)") {
		t.Errorf("body should carry the truncation marker; got len=%d", len(body))
	}
	if strings.Count(body, "x") > 250 {
		t.Errorf("preview not bounded; saw %d x's", strings.Count(body, "x"))
	}
}
