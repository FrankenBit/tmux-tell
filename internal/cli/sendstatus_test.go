package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// withReachability installs test doubles for the pane-liveness + systemctl
// probes resolveRecipientStatus depends on, restoring them on cleanup.
func withReachability(t *testing.T, livePanes map[string]bool, mailmanActive bool) {
	t.Helper()
	prevPanes := livePanesFn
	livePanesFn = func(context.Context) (map[string]bool, error) { return livePanes, nil }
	state := "inactive"
	if mailmanActive {
		state = "active"
	}
	prevSysctl := setSystemctlRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(state), nil
	})
	t.Cleanup(func() {
		livePanesFn = prevPanes
		setSystemctlRunner(prevSysctl)
	})
}

func baseSendParams(from, to string) sendParams {
	return sendParams{From: from, To: to, Body: "hello", MaxRecipient: 5, MaxSender: 2, MaxBody: 16 * 1024, Format: "json"}
}

func decodeSend(t *testing.T, raw []byte) SendResponse {
	t.Helper()
	var r SendResponse
	if err := json.Unmarshal(bytes.TrimSpace(raw), &r); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return r
}

func TestSend_UnknownRecipient_FailLoudRegardlessOfStrict(t *testing.T) {
	// Day-one safety (#3/#4/#15) preserved: unknown recipient fails even
	// WITHOUT --strict — it is not queued (Option B, QM f1e4).
	s := newCmdTestStore(t, "alice") // bob NOT registered
	withReachability(t, map[string]bool{}, false)

	for _, strict := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		p := baseSendParams("alice", "ghost")
		p.Strict = strict
		exit := runSendWithStore(context.Background(), s, p, &stdout, &stderr)
		if exit != exitUnavailable {
			t.Errorf("strict=%v: exit = %d, want exitUnavailable (%d)", strict, exit, exitUnavailable)
		}
		if !strings.Contains(stdout.String()+stderr.String(), "unknown recipient") {
			t.Errorf("strict=%v: missing 'unknown recipient'; out=%s err=%s", strict, stdout.String(), stderr.String())
		}
	}
}

func TestSend_KnownAlive_RecipientBlock(t *testing.T) {
	s := newCmdTestStore(t) // register bob with a specific pane
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	withReachability(t, map[string]bool{"%3": true}, true)

	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(ctx, s, baseSendParams("alice", "bob"), &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if !r.OK || r.Recipient == nil {
		t.Fatalf("resp = %+v, want ok + recipient block", r)
	}
	rc := r.Recipient
	if !rc.Registered || !rc.Alive || !rc.MailmanRunning || rc.PaneStatus != "live" {
		t.Errorf("recipient = %+v, want registered+alive+mailman+live", rc)
	}
	if rc.DeliveryMode != store.DeliveryModePasteAndEnter {
		t.Errorf("delivery_mode = %q, want paste-and-enter", rc.DeliveryMode)
	}
	if r.Receipt == nil {
		t.Fatalf("receipt missing")
	}
	if r.Receipt.Enqueue.State != "accepted" || r.Receipt.Enqueue.At == "" {
		t.Errorf("enqueue receipt = %+v, want accepted with timestamp", r.Receipt.Enqueue)
	}
	if r.Receipt.Dispatch.State != "not_requested" {
		t.Errorf("dispatch receipt = %+v, want not_requested without wait", r.Receipt.Dispatch)
	}
	if r.Receipt.PasteConfirmed.State != "not_requested" {
		t.Errorf("paste receipt = %+v, want not_requested without wait", r.Receipt.PasteConfirmed)
	}
}

func TestSend_KnownDead_DefaultQueuesStrictRejects(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	withReachability(t, map[string]bool{}, false) // %3 NOT live → pane gone

	// Default (no --strict): still queues, reports alive:false.
	var out1, err1 bytes.Buffer
	if exit := runSendWithStore(ctx, s, baseSendParams("alice", "bob"), &out1, &err1); exit != exitOK {
		t.Fatalf("default dead-pane exit = %d; stderr=%s", exit, err1.String())
	}
	r1 := decodeSend(t, out1.Bytes())
	if !r1.OK || r1.Recipient.Alive || r1.Recipient.PaneStatus != "unknown" {
		t.Errorf("default dead = %+v, want ok + alive:false + pane unknown", r1.Recipient)
	}

	// --strict: rejected (ok:false, exitUnavailable) with the recipient block.
	var out2, err2 bytes.Buffer
	p := baseSendParams("alice", "bob")
	p.Strict = true
	if exit := runSendWithStore(ctx, s, p, &out2, &err2); exit != exitUnavailable {
		t.Errorf("strict dead-pane exit = %d, want exitUnavailable", exit)
	}
	r2 := decodeSend(t, out2.Bytes())
	if r2.OK || r2.Recipient == nil || r2.Error == "" {
		t.Errorf("strict dead = %+v, want ok:false + recipient + error", r2)
	}
}

func TestSend_MailboxOnly_MailmanFalse(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "human", "%7")
	_ = s.SetDeliveryMode(ctx, "human", store.DeliveryModeMailboxOnly)
	// Even if systemctl would say active, mailbox-only is reported mailman:false.
	withReachability(t, map[string]bool{"%7": true}, true)

	var stdout, stderr bytes.Buffer
	if exit := runSendWithStore(ctx, s, baseSendParams("alice", "human"), &stdout, &stderr); exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	rc := decodeSend(t, stdout.Bytes()).Recipient
	if rc.DeliveryMode != store.DeliveryModeMailboxOnly || rc.MailmanRunning {
		t.Errorf("mailbox-only recipient = %+v, want mode=mailbox-only + mailman:false", rc)
	}
}

func TestSend_DeferAfterReceiptReportsStaged(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	withReachability(t, map[string]bool{"%3": true}, true)

	p := baseSendParams("alice", "bob")
	p.DeliverAfter = deferTriggerResume
	var stdout, stderr bytes.Buffer
	if exit := runSendWithStore(ctx, s, p, &stdout, &stderr); exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if r.DeliverAfter != deferTriggerResume {
		t.Fatalf("deliver_after = %q, want %q", r.DeliverAfter, deferTriggerResume)
	}
	if r.Receipt == nil {
		t.Fatalf("receipt missing")
	}
	if r.Receipt.Enqueue.State != "staged" || r.Receipt.Enqueue.At == "" {
		t.Errorf("enqueue receipt = %+v, want staged with timestamp", r.Receipt.Enqueue)
	}
	if r.Receipt.Dispatch.State != "not_requested" {
		t.Errorf("dispatch receipt = %+v, want not_requested for deferred send", r.Receipt.Dispatch)
	}
}

func TestSend_WaitForDelivered_HappyPath(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	withReachability(t, map[string]bool{"%3": true}, true)

	// Pre-deliver: insert happens inside send, so we drive delivery via a
	// goroutine that claims+marks once the row lands. Simpler: send first
	// without wait to learn the lifecycle isn't auto — instead, use a short
	// poll where we mark delivered concurrently.
	p := baseSendParams("alice", "bob")
	p.WaitForDelivered = true
	p.Timeout = 2 * time.Second

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Spin until the row is queued, then claim+deliver it.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			m, _ := s.ClaimNext(context.Background(), "bob")
			if m != nil {
				_ = s.MarkDelivered(context.Background(), m.PublicID)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(ctx, s, p, &stdout, &stderr)
	<-done
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if r.Delivery == nil || r.Delivery.State != string(store.StateDelivered) {
		t.Errorf("delivery = %+v, want state delivered", r.Delivery)
	}
	if r.Receipt == nil {
		t.Fatalf("receipt missing")
	}
	if r.Receipt.Enqueue.State != "accepted" {
		t.Errorf("enqueue receipt = %+v, want accepted", r.Receipt.Enqueue)
	}
	if r.Receipt.Dispatch.State != string(store.StateDelivered) {
		t.Errorf("dispatch receipt = %+v, want delivered", r.Receipt.Dispatch)
	}
	if r.Receipt.PasteConfirmed.State != "confirmed" {
		t.Errorf("paste receipt = %+v, want confirmed", r.Receipt.PasteConfirmed)
	}
}

func TestSend_WaitForDelivered_Timeout(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	withReachability(t, map[string]bool{"%3": true}, true)

	p := baseSendParams("alice", "bob")
	p.WaitForDelivered = true
	p.Timeout = 150 * time.Millisecond // nobody delivers → timeout

	var stdout, stderr bytes.Buffer
	if exit := runSendWithStore(ctx, s, p, &stdout, &stderr); exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if r.Delivery == nil || r.Delivery.State != pingStateTimeout {
		t.Errorf("delivery = %+v, want state timeout", r.Delivery)
	}
	// The message is still queued — timeout is informational, not a failure.
	if !r.OK {
		t.Errorf("ok = false on wait-timeout; the row should still be queued")
	}
	if r.Receipt == nil {
		t.Fatalf("receipt missing")
	}
	if r.Receipt.Enqueue.State != "accepted" {
		t.Errorf("enqueue receipt = %+v, want accepted", r.Receipt.Enqueue)
	}
	if r.Receipt.Dispatch.State != pingStateTimeout {
		t.Errorf("dispatch receipt = %+v, want timeout", r.Receipt.Dispatch)
	}
	if r.Receipt.PasteConfirmed.State != pingStateTimeout {
		t.Errorf("paste receipt = %+v, want timeout", r.Receipt.PasteConfirmed)
	}
}

func TestReceiptLayersFromDelivery_PasteConfirmationEvidence(t *testing.T) {
	tests := []struct {
		name         string
		delivery     DeliveryStatus
		wantDispatch string
		wantPaste    string
		wantEvidence string
	}{
		{
			name: "delivered_in_input_box stays unconfirmed",
			delivery: DeliveryStatus{
				State:       displayStateDeliveredInInputBox,
				DeliveredAt: "2026-07-01T20:00:00Z",
			},
			wantDispatch: string(store.StateDelivered),
			wantPaste:    "unconfirmed",
			wantEvidence: "verification token was not observed",
		},
		{
			name: "failed delivery cannot confirm paste",
			delivery: DeliveryStatus{
				State:       string(store.StateFailed),
				DeliveredAt: "2026-07-01T20:01:00Z",
			},
			wantDispatch: string(store.StateFailed),
			wantPaste:    "failed",
			wantEvidence: "delivery failed before paste confirmation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dispatch, paste := receiptLayersFromDelivery(&tt.delivery)
			if dispatch.State != tt.wantDispatch {
				t.Errorf("dispatch state = %q, want %q", dispatch.State, tt.wantDispatch)
			}
			if paste.State != tt.wantPaste {
				t.Errorf("paste state = %q, want %q", paste.State, tt.wantPaste)
			}
			if paste.At != tt.delivery.DeliveredAt {
				t.Errorf("paste at = %q, want %q", paste.At, tt.delivery.DeliveredAt)
			}
			if !strings.Contains(paste.Evidence, tt.wantEvidence) {
				t.Errorf("paste evidence = %q, want to contain %q", paste.Evidence, tt.wantEvidence)
			}
		})
	}
}

func TestSend_TextFormat_OneLiner(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	withReachability(t, map[string]bool{"%3": true}, true)

	p := baseSendParams("alice", "bob")
	p.Format = "text"
	var stdout, stderr bytes.Buffer
	if exit := runSendWithStore(ctx, s, p, &stdout, &stderr); exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	out := stdout.String()
	for _, want := range []string{"sent id=", "recipient bob", "pane live", "mailman up"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q; got:\n%s", want, out)
		}
	}
}
