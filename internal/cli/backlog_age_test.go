package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// The nudge's four shapes. The DISCRIMINATING assertion is that a repeat is
// distinguishable from a first — asserting only "it mentions the count" would
// pass against the pre-#934 body, which is the defect this exists for.
func TestBacklogNudgeBody_RepeatIsDistinguishableFromFirst(t *testing.T) {
	first := backlogNudgeBody(2, 32*time.Hour, true, 0)
	repeat := backlogNudgeBody(2, 32*time.Hour, true, 2)

	if first == repeat {
		t.Fatal("a repeated nudge renders identically to a first — the #934 defect")
	}
	if strings.Contains(first, "announced") {
		t.Errorf("a FIRST nudge claims a prior announce: %q", first)
	}
	if !strings.Contains(repeat, "announced 2× before") {
		t.Errorf("a repeat does not name the prior count: %q", repeat)
	}
	for _, b := range []string{first, repeat} {
		if !strings.Contains(b, "oldest 1d") {
			t.Errorf("age missing from %q", b)
		}
		if !strings.Contains(b, "run tmux-tell.inbox") {
			t.Errorf("the actionable instruction was dropped from %q", b)
		}
	}
}

// Unknown age must be OMITTED, never rendered as zero: "oldest 0s" asserts the
// backlog just arrived, which is the opposite of could-not-tell.
func TestBacklogNudgeBody_UnknownAgeIsOmittedNotZero(t *testing.T) {
	b := backlogNudgeBody(2, 0, false, 0)
	if strings.Contains(b, "oldest") {
		t.Errorf("could-not-tell rendered an age: %q", b)
	}
	if b != "📬 2 queued — run tmux-tell.inbox" {
		t.Errorf("unknown-age body drifted from the pre-#934 wording: %q", b)
	}
}

func TestHumanBacklogAge_Bands(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{7 * time.Minute, "7m"},
		{32 * time.Hour, "1d"},
		{3 * time.Hour, "3h"},
		{70 * time.Hour, "2d"},
	} {
		if got := humanBacklogAge(c.d); got != c.want {
			t.Errorf("humanBacklogAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// THE NO-CONSUMER ARM (#934 AC). A mailbox-only agent gets no nudge at all, so
// the announced-and-unread surface never fires for it. This is the "1 row vs
// 44" distinction: the 44 have nobody to notify, and the remedy must stay
// silent for them rather than inventing a surface no one reads.
func TestApplyBacklogPolicy_NoConsumerGetsNoAgeSurface(t *testing.T) {
	s := newCmdTestStore(t, "gina")
	ctx := context.Background()
	seedQueued(t, s, "mailboxer", 3)

	res := applyBacklogPolicy(ctx, s, nil, "mailboxer", store.DeliveryModeMailboxOnly, 3)
	if res.Policy != "" {
		t.Fatalf("policy applied to a mailbox-only agent: %+v", res)
	}
	if res.OldestAgeOK || res.PriorAnnounces != 0 {
		t.Errorf("age surfaced for an agent with no consumer: %+v", res)
	}
	if n := len(backlogAnnounces(t, s, "mailboxer")); n != 0 {
		t.Errorf("%d nudge(s) inserted for a mailbox-only agent", n)
	}
}

// The announced-and-unread arm: a paste-and-enter agent WITH backlog gets the
// age surfaced on the nudge and in the register response.
func TestApplyBacklogPolicy_AnnouncedBacklogCarriesAge(t *testing.T) {
	s := newCmdTestStore(t, "gina")
	ctx := context.Background()
	seedQueued(t, s, "gina", 2)

	// BACKDATE the seeded rows. A freshly-seeded backlog is milliseconds old
	// and correctly falls under backlogAgeFloor, so without this the arm
	// asserts on the very case the feature deliberately stays quiet for — it
	// would test the floor, not the age surface.
	backdateAllQueued(t, s, "gina", "2026-08-01T00:00:00.000Z")

	res := applyBacklogPolicy(ctx, s, nil, "gina", store.DeliveryModePasteAndEnter, 2)
	if res.Err != nil {
		t.Fatalf("policy error: %v", res.Err)
	}
	if !res.OldestAgeOK {
		t.Fatal("age not determined for a real backlog")
	}
	got := backlogAnnounces(t, s, "gina")
	if len(got) != 1 {
		t.Fatalf("want 1 nudge, got %d", len(got))
	}
	if !strings.Contains(got[0].Body, "oldest") {
		t.Errorf("nudge carries no age: %q", got[0].Body)
	}

	out := map[string]any{}
	addBacklogPolicyFields(out, res)
	if _, ok := out["backlog_oldest_age_seconds"]; !ok {
		t.Error("register response omits backlog_oldest_age_seconds")
	}
}

// backdateAllQueued ages every queued row for an agent, so a test can exercise
// the age surface rather than the below-floor quiet path.
func backdateAllQueued(t *testing.T, s *store.Store, to, ts string) {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), store.ListFilter{ToAgent: to})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Kind != store.KindMessage {
			continue
		}
		if err := s.SetCreatedAtForTest(context.Background(), m.PublicID, ts); err != nil {
			t.Fatal(err)
		}
	}
}
