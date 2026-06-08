package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func newCmdTestStore(t *testing.T, agents ...string) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	for _, name := range agents {
		if err := s.UpsertAgent(ctx, name, "%99"); err != nil {
			t.Fatalf("seed agent %s: %v", name, err)
		}
	}
	return s
}

func parseJSONResult(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &v); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return v
}

func TestSend_HappyPath(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(context.Background(), s, sendParams{
		From: "alice", To: "bob", Body: "hello",
		MaxRecipient: 5, MaxSender: 2, MaxBody: 16 * 1024,
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", exit, exitOK, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if id, _ := got["id"].(string); len(id) != 4 {
		t.Errorf("id = %v, want 4 hex chars", got["id"])
	}
	if q, _ := got["queued"].(float64); q != 1 {
		t.Errorf("queued = %v, want 1", got["queued"])
	}
}

// TestSend_QuickNoReplyExpectedMultiRecipient exercises the 3-way combined
// path: quick + no_reply_expected + multi-recipient fan-out via the CLI surface
// (#220 S1 test-gap closure).
// TestSend_QuickNoReplyExpectedMultiRecipient exercises the 3-way combined
// path: quick + no_reply_expected + multi-recipient fan-out via the CLI surface
// (#220 S1 test-gap closure).
func TestSend_QuickNoReplyExpectedMultiRecipient(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol", "dave")
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(context.Background(), s, sendParams{
		From:            "alice",
		ToRecipients:    []string{"bob", "carol", "dave"},
		Body:            "quick fyi",
		Quick:           true,
		NoReplyExpected: true,
		MaxRecipient:    5,
		MaxSender:       5, // above cap to allow 3 queued messages from alice
		MaxBody:         16 * 1024,
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true {
		t.Errorf("ok = %v, want true; got=%v", got["ok"], got)
	}
	msgs, ok := got["messages"].([]any)
	if !ok {
		t.Fatalf("messages field missing or wrong type; got=%v", got)
	}
	if len(msgs) != 3 {
		t.Errorf("messages = %d, want 3", len(msgs))
	}
	// Verify both flags survive the round-trip through the store.
	ctx := context.Background()
	for _, to := range []string{"bob", "carol", "dave"} {
		m, err := s.ClaimNext(ctx, to)
		if err != nil || m == nil {
			t.Fatalf("ClaimNext(%s): m=%v err=%v", to, m, err)
		}
		if !m.Quick {
			t.Errorf("recipient %s: Quick = false, want true", to)
		}
		if !m.NoReplyExpected {
			t.Errorf("recipient %s: NoReplyExpected = false, want true", to)
		}
	}
}

func TestSend_ValidationErrors(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")

	cases := []struct {
		name string
		p    sendParams
		exit int
	}{
		{"missing from", sendParams{To: "bob", Body: "x", MaxRecipient: 5, MaxSender: 2, MaxBody: 1024}, exitUsage},
		{"missing to", sendParams{From: "alice", Body: "x", MaxRecipient: 5, MaxSender: 2, MaxBody: 1024}, exitUsage},
		{"empty body", sendParams{From: "alice", To: "bob", MaxRecipient: 5, MaxSender: 2, MaxBody: 1024}, exitDataErr},
		{"body too big", sendParams{From: "alice", To: "bob", Body: strings.Repeat("x", 33), MaxRecipient: 5, MaxSender: 2, MaxBody: 32}, exitDataErr},
		{"unknown to", sendParams{From: "alice", To: "ghost", Body: "x", MaxRecipient: 5, MaxSender: 2, MaxBody: 1024}, exitUnavailable},
		{"unknown from", sendParams{From: "ghost", To: "bob", Body: "x", MaxRecipient: 5, MaxSender: 2, MaxBody: 1024}, exitUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := runSendWithStore(context.Background(), s, tc.p, &stdout, &stderr)
			if exit != tc.exit {
				t.Errorf("exit = %d, want %d; stderr=%s", exit, tc.exit, stderr.String())
			}
			got := parseJSONResult(t, stdout.Bytes())
			if got["ok"] != false {
				t.Errorf("ok = %v, want false", got["ok"])
			}
		})
	}
}

func TestSend_CapsEnforced(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()

	// recipient cap: enqueue 5, the 6th should fail.
	for i := 0; i < 5; i++ {
		var stdout, stderr bytes.Buffer
		exit := runSendWithStore(ctx, s, sendParams{
			From: "alice", To: "bob", Body: "m",
			MaxRecipient: 5, MaxSender: 100, MaxBody: 1024,
		}, &stdout, &stderr)
		if exit != exitOK {
			t.Fatalf("setup send %d failed: %s", i, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(ctx, s, sendParams{
		From: "alice", To: "bob", Body: "m",
		MaxRecipient: 5, MaxSender: 100, MaxBody: 1024,
	}, &stdout, &stderr)
	if exit != exitTempFail {
		t.Errorf("recipient cap exit = %d, want %d", exit, exitTempFail)
	}
	got := parseJSONResult(t, stdout.Bytes())
	errStr, _ := got["error"].(string)
	if !strings.Contains(errStr, "recipient queue full") || !strings.Contains(errStr, "bob") {
		t.Errorf("error = %q, want mention of recipient queue full + bob", errStr)
	}
}

func TestSend_SenderBacklogCapEnforced(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	// sender cap: alice queues 2, the 3rd from alice (anywhere) should fail.
	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		exit := runSendWithStore(ctx, s, sendParams{
			From: "alice", To: "bob", Body: "m",
			MaxRecipient: 100, MaxSender: 2, MaxBody: 1024,
		}, &stdout, &stderr)
		if exit != exitOK {
			t.Fatalf("setup %d: %s", i, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(ctx, s, sendParams{
		From: "alice", To: "bob", Body: "m",
		MaxRecipient: 100, MaxSender: 2, MaxBody: 1024,
	}, &stdout, &stderr)
	if exit != exitTempFail {
		t.Errorf("sender cap exit = %d, want %d", exit, exitTempFail)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if errStr, _ := got["error"].(string); !strings.Contains(errStr, "sender backlog full") || !strings.Contains(errStr, "alice") {
		t.Errorf("error = %q, want mention of sender backlog full + alice", errStr)
	}
}

func TestSend_ReplyToValidated(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	// First a valid message so we have an id to reply to.
	var stdout, stderr bytes.Buffer
	_ = runSendWithStore(ctx, s, sendParams{
		From: "alice", To: "bob", Body: "ping",
		MaxRecipient: 5, MaxSender: 100, MaxBody: 1024,
	}, &stdout, &stderr)
	first := parseJSONResult(t, stdout.Bytes())
	origID := first["id"].(string)

	// Reply to non-existent id → exitDataErr.
	stdout.Reset()
	stderr.Reset()
	exit := runSendWithStore(ctx, s, sendParams{
		From: "bob", To: "alice", ReplyTo: "ffff", Body: "pong",
		MaxRecipient: 5, MaxSender: 100, MaxBody: 1024,
	}, &stdout, &stderr)
	if exit != exitDataErr {
		t.Errorf("bad reply-to exit = %d, want %d", exit, exitDataErr)
	}

	// Reply to valid id succeeds.
	stdout.Reset()
	stderr.Reset()
	exit = runSendWithStore(ctx, s, sendParams{
		From: "bob", To: "alice", ReplyTo: origID, Body: "pong",
		MaxRecipient: 5, MaxSender: 100, MaxBody: 1024,
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Errorf("valid reply exit = %d, want 0; stderr=%s", exit, stderr.String())
	}
}

// --- multi-recipient tests (#158) ---

func TestSend_MultiRecipient_HappyPath(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(ctx, s, sendParams{
		From:                 "alice",
		ToRecipients:         []string{"bob", "carol"},
		Body:                 "broadcast",
		MaxRecipient:         5,
		MaxSender:            10,
		MaxBody:              1024,
		MaxRecipientsPerSend: 10,
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr.String())
	}
	var resp MultiSendResponse
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v (raw: %s)", err, stdout.String())
	}
	if !resp.OK {
		t.Errorf("ok = false, want true")
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(resp.Messages))
	}
	for _, m := range resp.Messages {
		if !m.OK {
			t.Errorf("recipient %s ok = false, want true; error = %s", m.To, m.Error)
		}
		if len(m.ID) != 4 {
			t.Errorf("recipient %s id = %q, want 4 hex chars", m.To, m.ID)
		}
	}
	tos := map[string]bool{resp.Messages[0].To: true, resp.Messages[1].To: true}
	if !tos["bob"] || !tos["carol"] {
		t.Errorf("expected bob and carol in messages, got %v", tos)
	}
}

func TestSend_MultiRecipient_MaxCapExceeded(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol", "dave")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(ctx, s, sendParams{
		From:                 "alice",
		ToRecipients:         []string{"bob", "carol", "dave"},
		Body:                 "too wide",
		MaxRecipient:         5,
		MaxSender:            10,
		MaxBody:              1024,
		MaxRecipientsPerSend: 2, // cap is 2, 3 recipients → fail
	}, &stdout, &stderr)
	if exit == exitOK {
		t.Errorf("expected non-zero exit for cap exceeded")
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != false {
		t.Errorf("ok = %v, want false", got["ok"])
	}
	if errStr, _ := got["error"].(string); !strings.Contains(errStr, "too many recipients") {
		t.Errorf("error = %q, want mention of too many recipients", errStr)
	}
}

func TestSend_MultiRecipient_MixOutcome(t *testing.T) {
	// "ghost" is not registered — that row fails; "bob" succeeds.
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(ctx, s, sendParams{
		From:                 "alice",
		ToRecipients:         []string{"bob", "ghost"},
		Body:                 "fan-out",
		MaxRecipient:         5,
		MaxSender:            10,
		MaxBody:              1024,
		MaxRecipientsPerSend: 10,
	}, &stdout, &stderr)
	// Partial failure → exitTempFail, not exitOK
	if exit != exitTempFail {
		t.Errorf("exit = %d, want exitTempFail (%d)", exit, exitTempFail)
	}
	var resp MultiSendResponse
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v (raw: %s)", err, stdout.String())
	}
	if resp.OK {
		t.Errorf("outer ok = true, want false (partial failure)")
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(resp.Messages))
	}
	byTo := map[string]MultiSendResult{}
	for _, m := range resp.Messages {
		byTo[m.To] = m
	}
	if b, ok := byTo["bob"]; !ok || !b.OK {
		t.Errorf("bob row: ok = %v, want true", byTo["bob"].OK)
	}
	if g, ok := byTo["ghost"]; !ok || g.OK {
		t.Errorf("ghost row: ok = %v, want false", byTo["ghost"].OK)
	}
	if errStr := byTo["ghost"].Error; !strings.Contains(errStr, "unknown recipient") {
		t.Errorf("ghost error = %q, want mention of unknown recipient", errStr)
	}
}

func TestSend_SingleRecipient_BackCompat(t *testing.T) {
	// ToRecipients with len==1 should dispatch through the single path and
	// produce the scalar SendResponse shape, not MultiSendResponse.
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(ctx, s, sendParams{
		From:         "alice",
		ToRecipients: []string{"bob"},
		Body:         "single via list",
		MaxRecipient: 5,
		MaxSender:    10,
		MaxBody:      1024,
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr.String())
	}
	// Should decode as scalar SendResponse (has "id" at top level, not "messages")
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if _, hasMessages := got["messages"]; hasMessages {
		t.Errorf("got messages key, want scalar SendResponse shape")
	}
	if id, _ := got["id"].(string); len(id) != 4 {
		t.Errorf("id = %q, want 4 hex chars", id)
	}
}
