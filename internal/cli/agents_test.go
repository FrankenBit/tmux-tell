package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

func TestAgents_LiveStaleNoPane(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "live-one", "%1")
	_ = s.UpsertAgent(ctx, "stale-one", "%5")
	_ = s.UpsertAgent(ctx, "no-pane-one", "")

	live := map[string]bool{"%1": true} // only %1 is in tmux

	var stdout, stderr bytes.Buffer
	exit := runAgentsWithStore(ctx, s, live, false, "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	var rows []agentView
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		"live-one": "live", "stale-one": "stale", "no-pane-one": "no-pane",
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Name] = r.PaneStatus
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s status = %q, want %q", name, got[name], w)
		}
	}
}

func TestAgents_AvailableOnlyFiltersStaleAndPaused(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "available", "%1")
	_ = s.UpsertAgent(ctx, "paused", "%2")
	_ = s.SetPaused(ctx, "paused", true)
	_ = s.UpsertAgent(ctx, "stale", "%9")

	live := map[string]bool{"%1": true, "%2": true} // both live, but "paused" is paused

	var stdout bytes.Buffer
	exit := runAgentsWithStore(ctx, s, live, true, "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []agentView
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 1 || rows[0].Name != "available" {
		t.Errorf("rows = %v, want only 'available'", rows)
	}
}

func TestAgents_TextFormat(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	live := map[string]bool{"%99": true}

	var stdout bytes.Buffer
	exit := runAgentsWithStore(context.Background(), s, live, false, "text", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(stdout.String(), "NAME\tPANE\tSTATUS") {
		t.Errorf("missing header: %q", stdout.String())
	}
}
