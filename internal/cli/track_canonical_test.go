package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestTrack_CanonicalOpensXDGDefault proves `track --canonical` reads the
// XDG-default DB by name and IGNORES $CLAUDE_MSG_DB (#348) — the operator's
// ground-truth "is id X actually in the canonical DB?" query, immune to a stale
// env binding that an MCP view might be stuck on.
func TestTrack_CanonicalOpensXDGDefault(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	// A decoy env binding that --canonical must NOT follow.
	t.Setenv("CLAUDE_MSG_DB", filepath.Join(t.TempDir(), "decoy.db"))
	ctx := context.Background()

	canon := defaultDBLocation() // = <xdg>/tmux-msg/messages.db
	cs, err := store.Open(canon)
	if err != nil {
		t.Fatalf("open canonical store: %v", err)
	}
	for _, a := range []string{"alice", "bob"} {
		if err := cs.UpsertAgent(ctx, a, "%99"); err != nil {
			t.Fatalf("seed %s: %v", a, err)
		}
	}
	res, err := cs.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hi", Kind: store.KindMessage,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = cs.Close()

	var out, errb bytes.Buffer
	code := runTrackCLI([]string{"--canonical", "--format", "json", res.PublicID}, &out, &errb)
	if code != exitOK {
		t.Fatalf("track --canonical exit=%d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), res.PublicID) {
		t.Errorf("track --canonical didn't find the message in the canonical DB; out=%s", out.String())
	}
}

// TestTrack_CanonicalConflictsWithDB confirms --canonical and --db are
// mutually exclusive (the flag forces the XDG default, so an explicit path is
// contradictory).
func TestTrack_CanonicalConflictsWithDB(t *testing.T) {
	var out, errb bytes.Buffer
	code := runTrackCLI([]string{"--canonical", "--db", "/tmp/x.db", "someid"}, &out, &errb)
	if code != exitUsage {
		t.Errorf("exit=%d, want exitUsage(%d) for --canonical+--db", code, exitUsage)
	}
}
