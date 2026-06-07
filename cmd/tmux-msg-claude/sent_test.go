package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func sqlNullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: true}
}

func TestSent_TextFormat(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello bob",
	})

	var stdout, stderr bytes.Buffer
	exit := runSentWithStore(ctx, s, "alice", "", "", 50, "24h", "", "text", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ID\tTO\tSTATE") {
		t.Errorf("missing header in %q", out)
	}
	if !strings.Contains(out, "bob") {
		t.Errorf("missing recipient in %q", out)
	}
	if !strings.Contains(out, "Recent sent") {
		t.Errorf("missing header line in %q", out)
	}
}

func TestSent_JSONFormat(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, _ := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello bob",
	})

	var stdout, stderr bytes.Buffer
	exit := runSentWithStore(ctx, s, "alice", "", "", 50, "24h", "", "json", &stdout, &stderr)
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
	if rows[0]["display_state"] != "queued" {
		t.Errorf("display_state = %v, want queued", rows[0]["display_state"])
	}
}

func TestSent_FilterByTo(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "to bob"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "carol", Body: "to carol"})

	var stdout bytes.Buffer
	exit := runSentWithStore(ctx, s, "alice", "", "bob", 50, "24h", "", "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["to"] != "bob" {
		t.Errorf("to = %v, want bob", rows[0]["to"])
	}
}

func TestSent_FilterByState(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "msg1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "msg2"})
	// Claim one → delivering
	_, _ = s.ClaimNext(ctx, "bob")

	var stdout bytes.Buffer
	exit := runSentWithStore(ctx, s, "alice", "queued", "", 50, "24h", "", "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 1 {
		t.Errorf("queued rows = %d, want 1", len(rows))
	}
}

func TestSent_DeliveredInInputBoxFilter(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res1, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "verified msg"})
	res2, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "unverified msg"})

	// Deliver res1 as verified, res2 as unverified.
	_, _ = s.ClaimNext(ctx, "bob")
	_ = s.MarkDelivered(ctx, res1.PublicID)
	_, _ = s.ClaimNext(ctx, "bob")
	_ = s.MarkDeliveredInInputBox(ctx, res2.PublicID)

	var stdout bytes.Buffer
	exit := runSentWithStore(ctx, s, "alice", "delivered_in_input_box", "", 50, "24h", "", "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (only the unverified one)", len(rows))
	}
	if rows[0]["id"] != res2.PublicID {
		t.Errorf("got id %v, want %s", rows[0]["id"], res2.PublicID)
	}
	if rows[0]["display_state"] != "delivered_in_input_box" {
		t.Errorf("display_state = %v, want delivered_in_input_box", rows[0]["display_state"])
	}
}

func TestSent_DisplayStateDeliveredInInputBox(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "soft fail"})
	_, _ = s.ClaimNext(ctx, "bob")
	_ = s.MarkDeliveredInInputBox(ctx, res.PublicID)

	var stdout, stderr bytes.Buffer
	exit := runSentWithStore(ctx, s, "alice", "", "", 50, "24h", "", "text", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "delivered_in_input_box") {
		t.Errorf("expected 'delivered_in_input_box' in text output, got:\n%s", out)
	}
	if !strings.Contains(out, "1 message(s) in delivered_in_input_box") {
		t.Errorf("expected footer line in output, got:\n%s", out)
	}
}

func TestSent_SinceFilter(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "msg"})

	// With a floor 1 hour in the future — nothing should match.
	future := time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	var stdout bytes.Buffer
	exit := runSentWithStore(ctx, s, "alice", "", "", 50, "1h", future, "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows with future floor, got %d", len(rows))
	}
}

func TestSent_OrderNewestFirst(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res1, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "first"})
	res2, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "second"})

	var stdout bytes.Buffer
	exit := runSentWithStore(ctx, s, "alice", "", "", 50, "24h", "", "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Newest (res2) should be first.
	if rows[0]["id"] != res2.PublicID {
		t.Errorf("first row id = %v, want newest %s", rows[0]["id"], res2.PublicID)
	}
	if rows[1]["id"] != res1.PublicID {
		t.Errorf("second row id = %v, want oldest %s", rows[1]["id"], res1.PublicID)
	}
}

func TestSent_EmptyOutbox(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	var stdout, stderr bytes.Buffer
	exit := runSentWithStore(context.Background(), s, "alice", "", "", 50, "24h", "", "text", &stdout, &stderr)
	if exit != exitOK {
		t.Errorf("exit = %d, want 0", exit)
	}
	out := stdout.String()
	if !strings.Contains(out, "ID\tTO\tSTATE") {
		t.Errorf("should still print header, got %q", out)
	}
}

func TestSent_UnknownFormat(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	var stdout, stderr bytes.Buffer
	exit := runSentWithStore(context.Background(), s, "alice", "", "", 50, "24h", "", "xml", &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d", exit, exitUsage)
	}
}

func TestValidateSentState(t *testing.T) {
	valid := []string{"", "queued", "delivering", "delivered", "failed", "delivered_in_input_box"}
	for _, s := range valid {
		if err := validateSentState(s); err != nil {
			t.Errorf("validateSentState(%q) = %v, want nil", s, err)
		}
	}
	if err := validateSentState("bogus"); err == nil {
		t.Error("validateSentState(bogus) = nil, want error")
	}
}

// TestSent_DeprecatedStateAlias verifies that --state delivered_unverified emits
// WARN deprecated_surface_used and returns the same rows as delivered_in_input_box.
func TestSent_DeprecatedStateAlias(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	for _, name := range []string{"alice", "bob"} {
		if err := s.UpsertAgent(ctx, name, "%99"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	res, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "soft"})
	_, _ = s.ClaimNext(ctx, "bob")
	_ = s.MarkDeliveredInInputBox(ctx, res.PublicID)
	// Set identity env so runSentCLI resolves agent=alice.
	t.Setenv("TMUX_AGENT_NAME", "alice")

	var stdout, stderr bytes.Buffer
	exit := runSentCLI([]string{"--db", dbPath, "--state", "delivered_unverified", "--format", "json"}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "WARN deprecated_surface_used name=--state delivered_unverified") {
		t.Errorf("expected deprecation WARN in stderr; got %q", stderr.String())
	}
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["display_state"] != "delivered_in_input_box" {
		t.Errorf("display_state = %v, want delivered_in_input_box", rows[0]["display_state"])
	}
}

func TestWallTime(t *testing.T) {
	// wallTime parses UTC and renders in local time; test against a known value.
	ts := "2026-06-07T19:42:18.000Z"
	got := wallTime(ts)
	// Just verify it's 8 chars "HH:MM:SS"
	if len(got) != 8 || got[2] != ':' || got[5] != ':' {
		t.Errorf("wallTime(%q) = %q, want HH:MM:SS", ts, got)
	}
}

func TestDisplayState(t *testing.T) {
	cases := []struct {
		m    store.Message
		want string
	}{
		{store.Message{State: store.StateQueued}, "queued"},
		{store.Message{State: store.StateDelivered}, "delivered"},
		{store.Message{State: store.StateDelivered, Verified: sqlNullInt64(1)}, "delivered"},
		{store.Message{State: store.StateDelivered, Verified: sqlNullInt64(0)}, "delivered_in_input_box"},
		{store.Message{State: store.StateFailed}, "failed"},
	}
	for _, c := range cases {
		got := displayState(c.m)
		if got != c.want {
			t.Errorf("displayState(%+v) = %q, want %q", c.m, got, c.want)
		}
	}
}
