package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// seedMessage inserts one message addressed bob→alice and returns its
// public_id. Useful so each track test has a known target.
func seedMessage(t *testing.T, s *store.Store) string {
	t.Helper()
	res, err := s.InsertMessage(context.Background(), store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "trackable",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return res.PublicID
}

func TestTrack_CLI_JSON_QueuedRow(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	id := seedMessage(t, s)
	t.Setenv("CLAUDE_MSG_DB", ":memory:")

	var stdout, stderr bytes.Buffer
	exit := runTrackCLI([]string{"--format", "json", id}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true || got["id"] != id || got["state"] != "queued" {
		t.Errorf("got %v", got)
	}
	if got["kind"] != "message" {
		t.Errorf("kind = %v, want message", got["kind"])
	}
	if _, ok := got["delivered_at"]; ok {
		t.Errorf("queued row should omit delivered_at; got %v", got)
	}
}

func TestTrack_CLI_Text_DeliveredRow(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	id := seedMessage(t, s)
	t.Setenv("CLAUDE_MSG_DB", ":memory:")

	// Walk the row through delivering → delivered.
	ctx := context.Background()
	if _, err := s.ClaimNext(ctx, "bob"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.MarkDelivered(ctx, id); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exit := runTrackCLI([]string{id}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"ID\t" + id,
		"FROM\talice",
		"TO\tbob",
		"STATE\tdelivered",
		"KIND\tmessage",
		"CREATED\t",
		"DELIVERED\t",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestTrack_CLI_FailedRow_SurfacesError(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	id := seedMessage(t, s)
	t.Setenv("CLAUDE_MSG_DB", ":memory:")

	ctx := context.Background()
	if _, err := s.ClaimNext(ctx, "bob"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.MarkFailed(ctx, id, "tmux: can't find pane"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exit := runTrackCLI([]string{"--format", "json", id}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["state"] != "failed" {
		t.Errorf("state = %v, want failed", got["state"])
	}
	if got["error"] != "tmux: can't find pane" {
		t.Errorf("error = %v, want the failure reason", got["error"])
	}
}

func TestTrack_CLI_NotFound(t *testing.T) {
	_ = newCmdTestStore(t)
	t.Setenv("CLAUDE_MSG_DB", ":memory:")

	var stdout, stderr bytes.Buffer
	exit := runTrackCLI([]string{"abcd"}, &stdout, &stderr)
	if exit != exitUnavailable {
		t.Errorf("exit = %d, want exitUnavailable", exit)
	}
	if !strings.Contains(stderr.String(), "no such message: abcd") {
		t.Errorf("stderr should name the missing id; got %q", stderr.String())
	}
}

func TestTrack_CLI_RequiresID(t *testing.T) {
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	var stdout, stderr bytes.Buffer
	exit := runTrackCLI(nil, &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want exitUsage", exit)
	}
}

func TestTrack_CLI_ReplyToThreaded(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	first, _ := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "first",
	})
	reply, _ := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "bob", ToAgent: "alice", Body: "reply",
		ReplyTo: first.PublicID,
	})
	t.Setenv("CLAUDE_MSG_DB", ":memory:")

	var stdout, stderr bytes.Buffer
	exit := runTrackCLI([]string{"--format", "json", reply.PublicID}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["reply_to"] != first.PublicID {
		t.Errorf("reply_to = %v, want %s", got["reply_to"], first.PublicID)
	}
}

// MCP-side parity: the tool returns the same struct, so the wire
// shape is byte-identical to the CLI's --format json output.
func TestMCP_MessageStatus_HappyPath(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")
	id := seedMessage(t, s)

	got := callMCPTool(t, s, "semaphore.message_status", map[string]any{
		"id": id,
	})
	if got["ok"] != true || got["id"] != id || got["state"] != "queued" {
		t.Errorf("got %v", got)
	}
}

func TestMCP_MessageStatus_NotFound(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice")

	got := callMCPTool(t, s, "semaphore.message_status", map[string]any{
		"id": "nope",
	})
	if got["_isError"] != true {
		t.Errorf("missing row should be error; got %v", got)
	}
}

// Pins the omitempty contract on trackResult's optional fields. With
// just OK/ID/From/To/State/Kind/CreatedAt populated and the three
// state-dependent fields empty, the rendered JSON must NOT include
// delivered_at, error, or reply_to as keys. Surveyor's #31 Q(d)
// follow-up: the cross-CLI/MCP shape test verifies byte-identity
// between the two callers, but not that the omitempty contract
// itself holds — this test pins it.
func TestTrackResult_OmitemptyContract(t *testing.T) {
	res := &trackResult{
		OK:        true,
		ID:        "abcd",
		From:      "alice",
		To:        "bob",
		State:     "queued",
		Kind:      "message",
		CreatedAt: "2026-05-30T11:00:00Z",
		// DeliveredAt, Error, ReplyTo intentionally empty
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, banned := range []string{"delivered_at", "error", "reply_to"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("omitempty contract: empty %q field should not appear in:\n%s",
				banned, raw)
		}
	}

	// Inverse pin: when all three are non-empty, they must appear.
	res.DeliveredAt = "2026-05-30T11:01:00Z"
	res.Error = "boom"
	res.ReplyTo = "1234"
	raw, _ = json.Marshal(res)
	for _, required := range []string{"delivered_at", "error", "reply_to"} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("populated %q field should appear in:\n%s", required, raw)
		}
	}
}

// Wire-shape contract: the CLI's --format json and the MCP tool's
// response must serialise to the same JSON. Pins the single-source-
// of-truth invariant (Surveyor's Q3 carry-over).
func TestTrack_WireShape_CLIAndMCPMatch(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	id := seedMessage(t, s)
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Setenv("CLAUDE_AGENT_NAME", "alice")

	// CLI shape.
	var stdout bytes.Buffer
	if exit := runTrackCLI([]string{"--format", "json", id}, &stdout, &bytes.Buffer{}); exit != exitOK {
		t.Fatalf("cli exit = %d", exit)
	}
	cliMap := parseJSONResult(t, stdout.Bytes())

	// MCP shape.
	mcpMap := callMCPTool(t, s, "semaphore.message_status", map[string]any{"id": id})

	// Strip MCP-private fields injected by the test harness.
	delete(mcpMap, "_text")
	delete(mcpMap, "_isError")

	cliJSON, _ := json.Marshal(cliMap)
	mcpJSON, _ := json.Marshal(mcpMap)
	if string(cliJSON) != string(mcpJSON) {
		t.Errorf("wire-shape drift:\n CLI: %s\n MCP: %s", cliJSON, mcpJSON)
	}
}
