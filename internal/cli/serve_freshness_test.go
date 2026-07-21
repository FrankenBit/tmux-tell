package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// unknownPaneRunner fakes a live-but-unclassifiable pane: capture-pane returns
// content with no ❯ prompt sentinel (→ StateUnknown), and the cursor sits off
// any sentinel row. StateUnknown is BOTH the wedge signature the #719(A)
// freshness alert targets AND paste-unsafe, so the message under delivery
// reverts to queued every cycle (staying stale) instead of draining.
func unknownPaneRunner() func(context.Context, io.Reader, ...string) ([]byte, error) {
	return func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			return []byte("recent tool output\n$ \n"), nil // no ❯ sentinel → unknown
		case "display-message":
			last := args[len(args)-1]
			switch {
			case strings.Contains(last, "pane_in_mode"):
				return []byte("0\n"), nil
			case strings.Contains(last, "cursor"):
				return []byte("0/0\n"), nil // col 0, row 0 — not a sentinel row
			default:
				return []byte("node\n"), nil // live adapter
			}
		}
		return nil, nil
	}
}

// copyModePaneRunner fakes a pane the operator has scrolled into copy-mode
// (pane_in_mode=1 → StateInCopyMode). That is a LEGITIMATE hold: paste-unsafe
// (so the message stays queued and goes stale) but NOT frozen — the freshness
// alert must exclude it. Same shape as a rate-limit / awaiting-operator hold.
func copyModePaneRunner() func(context.Context, io.Reader, ...string) ([]byte, error) {
	return func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			return []byte("scrolled-back history\n"), nil
		case "display-message":
			last := args[len(args)-1]
			switch {
			case strings.Contains(last, "pane_in_mode"):
				return []byte("1\n"), nil // in copy-mode → StateInCopyMode
			case strings.Contains(last, "cursor"):
				return []byte("0/0\n"), nil
			default:
				return []byte("node\n"), nil
			}
		}
		return nil, nil
	}
}

func countStuckNotices(t *testing.T, s *store.Store, to string) int {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), store.ListFilter{
		ToAgent: to,
		Kind:    store.KindStuckChamberNotice,
	})
	if err != nil {
		t.Fatalf("list stuck notices: %v", err)
	}
	return len(msgs)
}

// freshnessTestSetup wires the common fakes: a tiny temporal-delta (static
// captures classify without waiting), a fast freshness sweep, a live pane in
// list-panes, and the given per-pane runner. Returns a serveOpts for `bob` with
// pre-paste-safety ON (so an unsafe pane keeps the message queued) and drift
// OFF (so #783 session-stale never interferes).
func freshnessTestSetup(t *testing.T, runner func(context.Context, io.Reader, ...string) ([]byte, error)) serveOpts {
	t.Helper()
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })
	prevIvl := setFreshnessCheckIntervalForTest(2 * time.Millisecond)
	t.Cleanup(func() { setFreshnessCheckIntervalForTest(prevIvl) })
	prevList := tmuxio.SetListPanesRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%3\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesRunner(prevList) })
	prev := tmuxio.SetTmuxRunner(runner)
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	opts := fastOpts("bob")
	opts.PrePasteSafetyDisabled = false // an Unknown/unsafe pane aborts → message stays queued
	opts.SessionStaleThreshold = 0      // disable #783 parking (drift is off anyway)
	return opts
}

// TestServe_Freshness_AlertsOnStaleQueue is the #719(A) core AC: a real
// deliverable that sits queued past MailmanStaleThreshold to a live-but-Unknown
// pane edge-fires a stuck_chamber_notice to the configured conductor.
func TestServe_Freshness_AlertsOnStaleQueue(t *testing.T) {
	opts := freshnessTestSetup(t, unknownPaneRunner())
	opts.MailmanStaleThreshold = time.Millisecond // any queued message is immediately stale
	opts.AlertTo = "conductor"

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "are you frozen?"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	waitFor(t, 3*time.Second, func() bool {
		return countStuckNotices(t, s, "conductor") >= 1
	}, "no stuck_chamber_notice fired for a stale queue")

	msgs, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "conductor", Kind: store.KindStuckChamberNotice})
	n := msgs[0]
	if n.FromAgent != "bob" {
		t.Errorf("notice from_agent = %q, want bob (self-observed)", n.FromAgent)
	}
	if !strings.Contains(n.Body, "frozen") {
		t.Errorf("notice body missing 'frozen': %q", n.Body)
	}
	if !strings.Contains(logbuf.String(), "stuck_chamber_notice_sent") {
		t.Errorf("log missing stuck_chamber_notice_sent; got:\n%s", logbuf.String())
	}
}

// TestServe_Freshness_NoAlertWhenFresh: the same Unknown-pane setup (message
// stays queued) but a long threshold means the queue is never "stale", so no
// alert fires. Pins that the alert keys on age, not merely on "queued+unknown".
func TestServe_Freshness_NoAlertWhenFresh(t *testing.T) {
	opts := freshnessTestSetup(t, unknownPaneRunner())
	opts.MailmanStaleThreshold = time.Hour // a just-queued message is never stale
	opts.AlertTo = "conductor"

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "recent"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stop, wait, _ := runServeInBackgroundOpts(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	// Let many freshness sweeps run, then assert nothing fired.
	time.Sleep(120 * time.Millisecond)
	if got := countStuckNotices(t, s, "conductor"); got != 0 {
		t.Fatalf("stuck notices = %d, want 0 (queue is fresh, under threshold)", got)
	}
}

// TestServe_Freshness_LegitimateHoldExcluded: a stale queue whose pane is in a
// LEGITIMATE hold (copy-mode here; same class as rate-limit / awaiting-operator
// / compaction-rest) must NOT alert — the chamber is deliberately not receiving,
// not frozen. This is the exclusion that separates the wedge (Unknown, alert)
// from a hold (paste-unsafe-but-not-Unknown, no alert).
func TestServe_Freshness_LegitimateHoldExcluded(t *testing.T) {
	opts := freshnessTestSetup(t, copyModePaneRunner())
	opts.MailmanStaleThreshold = time.Millisecond
	opts.AlertTo = "conductor"

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "held"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stop, wait, _ := runServeInBackgroundOpts(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	time.Sleep(120 * time.Millisecond)
	if got := countStuckNotices(t, s, "conductor"); got != 0 {
		t.Fatalf("stuck notices = %d, want 0 (copy-mode is a legitimate hold, not a freeze)", got)
	}
}

// TestServe_Freshness_EdgeTriggeredOncePerEpisode: a queue that stays stale
// across many sweeps produces EXACTLY ONE notice — the latch fires once per
// freeze episode, never a per-sweep storm.
func TestServe_Freshness_EdgeTriggeredOncePerEpisode(t *testing.T) {
	opts := freshnessTestSetup(t, unknownPaneRunner())
	opts.MailmanStaleThreshold = time.Millisecond
	opts.AlertTo = "conductor"

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "stuck a while"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stop, wait, _ := runServeInBackgroundOpts(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	waitFor(t, 3*time.Second, func() bool {
		return countStuckNotices(t, s, "conductor") >= 1
	}, "no stuck_chamber_notice fired")
	// Let many more sweeps run while the queue is still stale.
	time.Sleep(120 * time.Millisecond)
	if got := countStuckNotices(t, s, "conductor"); got != 1 {
		t.Fatalf("stuck notices = %d, want exactly 1 (edge-triggered, one per episode)", got)
	}
}

// TestServe_Freshness_DormantWithoutAlertTo: with no conductor configured the
// alert is DORMANT — no notice even on a stale, Unknown-pane queue. This is the
// substrate-neutral default (no deployment chamber-name baked in); a deployment
// activates it by setting mailman-alert-to.
func TestServe_Freshness_DormantWithoutAlertTo(t *testing.T) {
	opts := freshnessTestSetup(t, unknownPaneRunner())
	opts.MailmanStaleThreshold = time.Millisecond
	opts.AlertTo = "" // dormant

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "no sink"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stop, wait, _ := runServeInBackgroundOpts(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	time.Sleep(120 * time.Millisecond)
	// The notice would address whatever AlertTo is; with AlertTo empty, none is
	// inserted for anyone. Check the two plausible sinks are both empty.
	if got := countStuckNotices(t, s, "conductor") + countStuckNotices(t, s, ""); got != 0 {
		t.Fatalf("stuck notices = %d, want 0 (alert dormant without a configured conductor)", got)
	}
}
