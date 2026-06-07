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
