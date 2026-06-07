package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// The send-response schema (#152). These are NAMED struct types, deliberately
// not inlined map[string]any: the send response is a contract that #155
// (crossed-message freshness) and #157 (delivered_in_input_box recovery) inherit
// and typed-bind against. Adding a field is additive/non-breaking; the existing
// ok/id/queued keys keep their meaning.

// RecipientStatus is the recipient's send-time disposition: does it exist, is
// its pane live, how is it served, is its mailman up. Populated on every send
// (the registry query is cheap + local). PaneStatus is one of live|paused|
// unknown.
type RecipientStatus struct {
	Registered     bool   `json:"registered"`
	Alive          bool   `json:"alive"`
	DeliveryMode   string `json:"delivery_mode,omitempty"`
	MailmanRunning bool   `json:"mailman_running"`
	PaneStatus     string `json:"pane_status"`
}

// DeliveryStatus is the terminal delivery outcome, populated only when
// --wait-for-delivered (CLI) / wait_for_delivered (MCP) is set. State is a
// store terminal state ("delivered"/"failed") or the synthetic "timeout" when
// the wait bound elapsed first. VerifyMs is the wait duration.
//
// Note: there is no "delivered_in_input_box" DB state to wait on — the mailman
// records that soft-failure as "delivered" (see #169). So a returned
// state="delivered" means delivered (verified or not); the verified/unverified
// split stays out of band per #169.
type DeliveryStatus struct {
	State       string `json:"state"`
	DeliveredAt string `json:"delivered_at,omitempty"`
	VerifyMs    int64  `json:"verify_ms"`
}

// ThreadFreshness is the crossed-message signal, populated only when the send
// carries a --reply_to (#155). It answers a substrate-knowable question: has
// the thread moved since the sender last spoke in it? Specifically, NewerInThread
// lists messages in the reply_to chain that are addressed to the sender AND
// arrived after the sender's own last message in that chain — "the thread moved
// since you last spoke," which the substrate can compute from the reply_to walk
// + arrival order + to/from.
//
// It deliberately does NOT claim "messages you haven't processed": the substrate
// tracks `delivered` (paste landed in the pane), not `processed` (the recipient
// instance read/acted on it). A delivered message is in the context stream but
// may not be attended-to — so "processed" isn't substrate-knowable and is out of
// scope (see #155 §Semantic correction). Stale is a soft signal: the send still
// succeeds unless --block-on-stale is set.
type ThreadFreshness struct {
	Stale          bool     `json:"stale"`
	YouRepliedTo   string   `json:"you_replied_to"`
	NewerInThread  []string `json:"newer_in_thread,omitempty"`
	LatestInThread string   `json:"latest_in_thread,omitempty"`
}

// ReplayStatus is the replay-notice block, populated only on a `resend` (#157
// PR1). It tells the caller this send is a replay of an earlier message: which
// original, when it was first sent, what state that original was in at resend
// time, and whether --force was needed to override the duplicate guard.
//
// Note on OriginalState: the substrate has no `delivered_in_input_box` state —
// verified and unverified deliveries both read as "delivered" (#169). So a
// resend of an unverified message reports OriginalState="delivered" and required
// --force; the journal-aware distinction that would let unverified recover
// without --force is gated on #169.
type ReplayStatus struct {
	OriginalID     string `json:"original_id"`
	OriginalSentAt string `json:"original_sent_at,omitempty"`
	OriginalState  string `json:"original_state"`
	Forced         bool   `json:"forced"`
}

// SendResponse is the full structured result of a send. Recipient is always
// present; Delivery only when the caller opted into the wait; Freshness only
// when the send carries a reply_to; Replay only on a `resend`. Error is set on a
// --strict / --block-on-stale / resend-guard rejection (OK=false).
type SendResponse struct {
	OK        bool             `json:"ok"`
	ID        string           `json:"id,omitempty"`
	Queued    int              `json:"queued"`
	Recipient *RecipientStatus `json:"recipient,omitempty"`
	Delivery  *DeliveryStatus  `json:"delivery,omitempty"`
	Freshness *ThreadFreshness `json:"thread_freshness,omitempty"`
	Replay    *ReplayStatus    `json:"replay,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// MultiSendResult is one recipient's outcome within a multi-recipient send
// (#158). Identical to SendResponse but carries the recipient name at the top
// level so the caller can correlate each row without parsing the recipient block.
type MultiSendResult struct {
	To        string           `json:"to"`
	OK        bool             `json:"ok"`
	ID        string           `json:"id,omitempty"`
	Queued    int              `json:"queued"`
	Recipient *RecipientStatus `json:"recipient,omitempty"`
	Delivery  *DeliveryStatus  `json:"delivery,omitempty"`
	Freshness *ThreadFreshness `json:"thread_freshness,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// MultiSendResponse is the top-level response shape for a multi-recipient send.
// OK is true only when every recipient row succeeded. When any row fails (unknown
// recipient, cap exceeded, strict rejection) OK is false, but the Messages array
// still carries the per-recipient breakdown so the caller can retry selectively.
type MultiSendResponse struct {
	OK       bool              `json:"ok"`
	Messages []MultiSendResult `json:"messages"`
}

// renderSendResult writes a SendResponse in the requested format. JSON (the
// default, back-compatible) emits the full structure; text emits a brief
// human one-liner per the #152 "brief one-liner in text" AC. Used by the CLI;
// the MCP path returns the SendResponse value directly.
func renderSendResult(stdout io.Writer, res SendResponse, to, format string) {
	if format == "text" {
		if !res.OK {
			fmt.Fprintf(stdout, "send FAILED: %s\n", res.Error)
		} else if rp := res.Replay; rp != nil {
			fmt.Fprintf(stdout, "resent id=%s queued=%d (replay of %s, originally sent %s)\n",
				res.ID, res.Queued, rp.OriginalID, rp.OriginalSentAt)
		} else {
			fmt.Fprintf(stdout, "sent id=%s queued=%d\n", res.ID, res.Queued)
		}
		if r := res.Recipient; r != nil {
			fmt.Fprintf(stdout, "  recipient %s: %s\n", to, recipientOneLine(r))
		}
		if d := res.Delivery; d != nil {
			at := d.DeliveredAt
			if at == "" {
				at = "—"
			}
			fmt.Fprintf(stdout, "  delivery: %s (%dms, at %s)\n", d.State, d.VerifyMs, at)
		}
		if f := res.Freshness; f != nil && f.Stale {
			fmt.Fprintf(stdout, "  ⚠ %d newer message(s) in this thread since you last spoke: %s\n",
				len(f.NewerInThread), strings.Join(f.NewerInThread, ", "))
			fmt.Fprintf(stdout, "    latest in thread: %s\n", f.LatestInThread)
		}
		return
	}
	_ = writeJSONResult(stdout, res)
}

// recipientOneLine renders the recipient status compactly for text output,
// shouting the actionable states (UNREGISTERED / pane not-live).
func recipientOneLine(r *RecipientStatus) string {
	if !r.Registered {
		return "UNREGISTERED (message will sit unclaimed)"
	}
	reach := "pane " + r.PaneStatus
	if !r.Alive {
		reach = "pane NOT-LIVE"
	}
	mailman := "mailman up"
	if !r.MailmanRunning {
		mailman = "mailman down"
		if r.DeliveryMode == store.DeliveryModeMailboxOnly {
			mailman = "mailbox-only (no daemon)"
		}
	}
	return reach + ", " + mailman + ", mode=" + r.DeliveryMode
}

// livePanesFn is the pane-liveness lookup, swappable in tests. Wraps the
// tmuxio entry point (whose own shell-out runner is internal to that package).
var livePanesFn = tmuxio.LivePanes

// resolveRecipientStatus queries the recipient's send-time disposition: the
// registry row, pane liveness (tmux), and mailman unit state (systemd). It is
// best-effort and never fails the send — an unregistered recipient returns a
// zero-ish status with Registered=false (the caller decides, via --strict,
// whether that's fatal). A genuine store error (not "not found") is surfaced.
func resolveRecipientStatus(ctx context.Context, s *store.Store, agent string) (*RecipientStatus, error) {
	a, err := s.GetAgent(ctx, agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &RecipientStatus{Registered: false, PaneStatus: "unknown"}, nil
		}
		return nil, err
	}

	rs := &RecipientStatus{
		Registered:   true,
		DeliveryMode: a.DeliveryMode,
	}
	if rs.DeliveryMode == "" {
		rs.DeliveryMode = store.DeliveryModePasteAndEnter
	}

	live, err := livePanesFn(ctx)
	if err != nil {
		// tmux unreachable → treat every pane as stale rather than failing
		// the send (mirrors LivePanes's own no-server tolerance).
		live = map[string]bool{}
	}
	rs.Alive = a.PaneID != "" && live[a.PaneID]

	// A mailbox-only recipient has no daemon by design (the operator polls),
	// so mailman_running is definitionally false; only probe for paste-and-
	// enter recipients.
	if rs.DeliveryMode == store.DeliveryModePasteAndEnter {
		rs.MailmanRunning = mailmanActive(ctx, agent)
	}

	switch {
	case a.Paused:
		rs.PaneStatus = "paused"
	case rs.Alive:
		rs.PaneStatus = "live"
	default:
		rs.PaneStatus = "unknown"
	}
	return rs, nil
}

// defaultDeliveredWaitTimeout bounds --wait-for-delivered when the caller
// doesn't set one. An idle recipient typically delivers in ~3–5s (observe-gate
// + paste); 10s leaves headroom without risking a long sender stall.
const defaultDeliveredWaitTimeout = 10 * time.Second

// waitForDelivery polls the inserted row until it reaches a store-terminal
// state (delivered/failed) or timeout elapses, returning the DeliveryStatus.
// Mirrors ping's pollPingTerminal shape (reuses pingPollInterval) but builds
// the send-schema type. ctx cancellation reports as timeout.
func waitForDelivery(ctx context.Context, s *store.Store, id string, timeout, pollInterval time.Duration) *DeliveryStatus {
	if pollInterval <= 0 {
		pollInterval = pingPollInterval
	}
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		m, err := s.GetMessage(ctx, id)
		if err == nil && (m.State == store.StateDelivered || m.State == store.StateFailed) {
			ds := &DeliveryStatus{State: string(m.State), VerifyMs: time.Since(start).Milliseconds()}
			if m.DeliveredAt.Valid {
				ds.DeliveredAt = m.DeliveredAt.String
			}
			return ds
		}
		if !time.Now().Before(deadline) {
			return &DeliveryStatus{State: pingStateTimeout, VerifyMs: time.Since(start).Milliseconds()}
		}
		select {
		case <-ctx.Done():
			return &DeliveryStatus{State: pingStateTimeout, VerifyMs: time.Since(start).Milliseconds()}
		case <-time.After(pollInterval):
		}
	}
}

// resolveThreadFreshness computes the crossed-message signal for a send that
// carries replyTo (#155). It walks the reply_to chain via the shared
// store.GetThread primitive (#141) — NOT a bespoke walk — then applies the
// substrate-knowable definition: a message is "newer in thread you haven't seen"
// when it is addressed to the sender AND arrived after the sender's own last
// message in the chain.
//
// Ordering uses the rowid (Message.ID), the substrate's true insert/arrival
// order: id and created_at are co-monotonic (both assigned in the same INSERT),
// but id is tie-free where created_at can collide at the same millisecond. The
// sender's new message is not inserted yet, so it never counts as its own
// baseline or as a newer entry.
//
// Baseline = the high-water-mark of what the sender has demonstrably seen: the
// LATER of (their own last message in the chain, the reply_to target they're
// replying to). The reply_to target is always folded into the max — the sender
// is holding the message they reply to, so it (and anything before it) can never
// count as "newer than you've seen." That is what keeps the signal honest on the
// common case: replying to the latest message must report not-stale, even though
// that message is addressed to you and postdates your own last send. Without it,
// every normal reply-to-the-latest would false-positive (Surveyor review of
// #189). When the sender hasn't spoken at all, the max collapses to just the
// reply_to target — the cold-entry case.
//
// Returns store.ErrNotFound if replyTo doesn't resolve (the caller maps that to
// the same "unknown reply-to id" error the insert path would raise).
func resolveThreadFreshness(ctx context.Context, s *store.Store, replyTo, sender string) (*ThreadFreshness, error) {
	thread, err := s.GetThread(ctx, replyTo)
	if err != nil {
		return nil, err
	}
	tf := &ThreadFreshness{YouRepliedTo: replyTo}
	if n := len(thread); n > 0 {
		// GetThread returns ascending by id, so the last row is the most
		// recent message in the chain.
		tf.LatestInThread = thread[n-1].PublicID
	}

	var baselineID int64
	for _, m := range thread {
		if (m.PublicID == replyTo || m.FromAgent == sender) && m.ID > baselineID {
			baselineID = m.ID
		}
	}

	for _, m := range thread {
		if m.ToAgent == sender && m.ID > baselineID {
			tf.NewerInThread = append(tf.NewerInThread, m.PublicID)
		}
	}
	tf.Stale = len(tf.NewerInThread) > 0
	return tf, nil
}
