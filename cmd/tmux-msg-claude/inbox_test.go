package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func TestInbox_TextFormat(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello bob",
	})

	var stdout, stderr bytes.Buffer
	exit := runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, "text", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ID\tFROM\tTO") {
		t.Errorf("missing header in %q", out)
	}
	if !strings.Contains(out, "alice\tbob") {
		t.Errorf("missing data row in %q", out)
	}
}

func TestInbox_JSONFormat(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, _ := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello bob",
	})

	var stdout, stderr bytes.Buffer
	exit := runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["id"] != res.PublicID {
		t.Errorf("id = %v, want %s", rows[0]["id"], res.PublicID)
	}
	if rows[0]["from"] != "alice" || rows[0]["to"] != "bob" {
		t.Errorf("from/to = %v/%v, want alice/bob", rows[0]["from"], rows[0]["to"])
	}
}

func TestInbox_FilterByState(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "2"})
	_, _ = s.ClaimNext(ctx, "bob")

	var stdout bytes.Buffer
	exit := runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 1 {
		t.Errorf("queued rows = %d, want 1", len(rows))
	}

	stdout.Reset()
	exit = runInboxWithStore(ctx, s, "bob", store.StateDelivering, 100, "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 1 {
		t.Errorf("delivering rows = %d, want 1", len(rows))
	}
}

func TestInbox_EmptyTable(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	var stdout, stderr bytes.Buffer
	exit := runInboxWithStore(context.Background(), s, "bob", store.StateQueued, 100, "text", &stdout, &stderr)
	if exit != exitOK {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.Contains(stdout.String(), "ID\tFROM\tTO") {
		t.Errorf("should still print header, got %q", stdout.String())
	}
}

func TestInbox_UnknownFormat(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	var stdout, stderr bytes.Buffer
	exit := runInboxWithStore(context.Background(), s, "bob", store.StateQueued, 100, "xml", &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d", exit, exitUsage)
	}
}
