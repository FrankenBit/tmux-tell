package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
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
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")
	id := seedMessage(t, s)

	got := callMCPTool(t, s, "tmux-msg.message_status", map[string]any{
		"id": id,
	})
	if got["ok"] != true || got["id"] != id || got["state"] != "queued" {
		t.Errorf("got %v", got)
	}
}

func TestMCP_MessageStatus_NotFound(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice")

	got := callMCPTool(t, s, "tmux-msg.message_status", map[string]any{
		"id": "nope",
	})
	if got["_isError"] != true {
		t.Errorf("missing row should be error; got %v", got)
	}
}

// The two discipline pins on trackResult's wire shape
// (TestPin_WireShapeSingleSoT_OmitemptyContract +
// TestPin_WireShapeSingleSoT_CLIAndMCPByteIdentity) live in
// pin_test.go per ADR-0001.
