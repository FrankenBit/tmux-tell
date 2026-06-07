package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// The send-response schema (#152). These are NAMED struct types, deliberately
// not inlined map[string]any: the send response is a contract that #155
// (crossed-message freshness) and #157 (delivered_unverified recovery) inherit
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
// Note: there is no "delivered_unverified" DB state to wait on — the mailman
// records that soft-failure as "delivered" (see #169). So a returned
// state="delivered" means delivered (verified or not); the verified/unverified
// split stays out of band per #169.
type DeliveryStatus struct {
	State       string `json:"state"`
	DeliveredAt string `json:"delivered_at,omitempty"`
	VerifyMs    int64  `json:"verify_ms"`
}

// SendResponse is the full structured result of a send. Recipient is always
// present; Delivery only when the caller opted into the wait. Error is set on
// a --strict rejection (OK=false).
type SendResponse struct {
	OK        bool             `json:"ok"`
	ID        string           `json:"id,omitempty"`
	Queued    int              `json:"queued"`
	Recipient *RecipientStatus `json:"recipient,omitempty"`
	Delivery  *DeliveryStatus  `json:"delivery,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// renderSendResult writes a SendResponse in the requested format. JSON (the
// default, back-compatible) emits the full structure; text emits a brief
// human one-liner per the #152 "brief one-liner in text" AC. Used by the CLI;
// the MCP path returns the SendResponse value directly.
func renderSendResult(stdout io.Writer, res SendResponse, to, format string) {
	if format == "text" {
		if !res.OK {
			fmt.Fprintf(stdout, "send FAILED: %s\n", res.Error)
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
