package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/render"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func seedThread(t *testing.T, s *store.Store) (root, mid, leaf string) {
	t.Helper()
	ctx := context.Background()
	r, _ := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "ping",
	})
	m, _ := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "bob", ToAgent: "alice",
		ReplyTo: r.PublicID, Body: "pong",
	})
	l, _ := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		ReplyTo: m.PublicID, Body: "thanks",
	})
	return r.PublicID, m.PublicID, l.PublicID
}

func TestLog_TextRendersAllMessages(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	root, _, leaf := seedThread(t, s)

	var stdout, stderr bytes.Buffer
	exit := runLogWithStore(context.Background(), s, leaf, "text", render.DefaultByteMarkerThreshold, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	out := stdout.String()
	for _, want := range []string{"ping", "pong", "thanks", "[Alice · ", "[Bob → Alice · re "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "ping") > strings.Index(out, "pong") {
		t.Errorf("messages out of order:\n%s", out)
	}
	_ = root
}

func TestLog_JSONReturnsArray(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	root, _, _ := seedThread(t, s)

	var stdout bytes.Buffer
	exit := runLogWithStore(context.Background(), s, root, "json", render.DefaultByteMarkerThreshold, &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("rows = %d, want 3", len(rows))
	}
}

func TestLog_UnknownIDReturnsDataErr(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	var stdout, stderr bytes.Buffer
	exit := runLogWithStore(context.Background(), s, "deadbeef", "json", render.DefaultByteMarkerThreshold, &stdout, &stderr)
	if exit != exitDataErr {
		t.Errorf("exit = %d, want %d", exit, exitDataErr)
	}
}

func TestLogCLI_RequiresThread(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := runLogCLI([]string{"--db", ":memory:"}, &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d", exit, exitUsage)
	}
}
