package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestRunNoteCompactCLI_StampsSignal is the #285 PR2 note-compact round-trip: the
// subcommand resolves the target, stamps the self-compact signal, and emits a JSON
// {ok, agent} result. The persisted last_self_compact_at is what the mailman later
// edge-detects to count a self-compact.
func TestRunNoteCompactCLI_StampsSignal(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "messages.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_ = s.Close()

	var stdout, stderr bytes.Buffer
	exit := runNoteCompactCLI(
		[]string{"--db", db, "--from", "bob"},
		strings.NewReader(`{"hook_event_name":"PostCompact"}`),
		&stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want exitOK; stderr=%s", exit, stderr.String())
	}
	var out struct {
		OK    bool   `json:"ok"`
		Agent string `json:"agent"`
		Event string `json:"event"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout.String())
	}
	if !out.OK || out.Agent != "bob" {
		t.Errorf("result = %+v, want {OK:true Agent:bob ...}", out)
	}
	// The event name from stdin is echoed (observability only).
	if out.Event != "PostCompact" {
		t.Errorf("event = %q, want PostCompact (echoed from stdin)", out.Event)
	}

	// The signal is persisted for the mailman to observe.
	s2, err := store.Open(db)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s2.Close() //nolint:errcheck // best-effort close in test
	a, _ := s2.GetAgent(ctx, "bob")
	if a.LastSelfCompactAt == "" {
		t.Error("last_self_compact_at empty after note-compact, want stamped")
	}
	// Fail-loud contract corollary: the hook path never advanced the counter or
	// the watermark (mailman-owned) — only the signal column moved.
	if a.RespawnShrinkCount != 0 || a.SelfCompactCountedAt != "" {
		t.Errorf("note-compact touched counter=%d watermark=%q, want the hook to touch NEITHER",
			a.RespawnShrinkCount, a.SelfCompactCountedAt)
	}
}

// TestRunNoteCompactCLI_UnregisteredFromAgentErrors pins the fail-loud guard: a
// hook wired for an unregistered agent must exit non-zero with a JSON error, not
// silently drop the signal — a silently-dropped self-compact defeats the respawn
// the operator opted into. Mutation anchor: removing the GetAgent registration
// check flips this red (SetSelfCompactSignal itself returns ErrNotFound, but the
// explicit guard gives the operator-actionable "not registered" hint).
func TestRunNoteCompactCLI_UnregisteredFromAgentErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "messages.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_ = s.Close() // "ghost" is never registered.

	var stdout, stderr bytes.Buffer
	exit := runNoteCompactCLI(
		[]string{"--db", db, "--from", "ghost"},
		nil, &stdout, &stderr)
	if exit == exitOK {
		t.Fatalf("expected non-zero exit for unregistered --from agent; stdout=%s", stdout.String())
	}
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(out.Error, "ghost") || !strings.Contains(out.Error, "not registered") {
		t.Errorf("error should mention the agent name and 'not registered'; got %q", out.Error)
	}
}
