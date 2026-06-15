package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestStrandedRenderParseRoundTrip is the drift-resistant invariant:
// renderStrandedDraftBody (serve.go) and parseStrandedBody (stranded.go)
// share the marker constants, so anything rendered must parse back
// identically — including the AC5 recovery-hint line being skipped.
func TestStrandedRenderParseRoundTrip(t *testing.T) {
	cases := []struct {
		name, pane, trigger, content string
	}{
		{"simple", "%5", "7501", "line one\nline two"},
		{"leading-space preserved", "%3", "abcd", "    indented draft line"},
		{"empty content", "%9", "ffff", ""},
		{"content with colon lines", "%1", "1234", "foo: bar\nbaz: qux"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := renderStrandedDraftBody(c.pane, c.trigger, c.content)
			pane, trig, content, ok := parseStrandedBody(body)
			if !ok {
				t.Fatalf("parse failed for rendered body:\n%s", body)
			}
			if pane != c.pane {
				t.Errorf("pane = %q, want %q", pane, c.pane)
			}
			if trig != c.trigger {
				t.Errorf("trigger = %q, want %q", trig, c.trigger)
			}
			if content != c.content {
				t.Errorf("content = %q, want %q", content, c.content)
			}
			// The recovery hint must be present (AC5) but not leak into content.
			if !strings.Contains(body, "tmux-tell-claude stranded show <id>") {
				t.Errorf("recovery hint missing from body:\n%s", body)
			}
			if strings.Contains(content, "stranded show <id>") {
				t.Errorf("recovery hint leaked into parsed content: %q", content)
			}
		})
	}
}

func TestParseStrandedBody_Unparseable(t *testing.T) {
	if _, _, _, ok := parseStrandedBody("just some random message body"); ok {
		t.Error("ok=true for non-snapshot body, want false")
	}
}

func mkStranded(t *testing.T, s *store.Store, agent, pane, trigger, content string) string {
	t.Helper()
	res, err := s.InsertMessage(context.Background(), store.InsertParams{
		FromAgent: agent, ToAgent: agent,
		Body: renderStrandedDraftBody(pane, trigger, content),
		Kind: store.KindStrandedDraft,
	})
	if err != nil {
		t.Fatalf("insert stranded: %v", err)
	}
	return res.PublicID
}

func TestListStrandedBookmarks_OnlyStranded(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	mkStranded(t, s, "qm", "%5", "7501", "draft alpha")
	mkStranded(t, s, "qm", "%5", "7777", "draft beta")
	// A normal message to qm must NOT appear.
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "bosun", ToAgent: "qm", Body: "regular msg"})

	bms, err := listStrandedBookmarks(ctx, s, "qm")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bms) != 2 {
		t.Fatalf("bookmarks = %d, want 2", len(bms))
	}
	if bms[0].Pane != "%5" || bms[0].Bytes != len("draft alpha") {
		t.Errorf("bm0 = %+v", bms[0])
	}
}

func TestRunStrandedShowWithStore(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	id := mkStranded(t, s, "qm", "%5", "7501", "recover me\nplease")

	t.Run("prints content to stdout", func(t *testing.T) {
		var out, errb bytes.Buffer
		exit := runStrandedShowWithStore(ctx, s, id, "", &out, &errb)
		if exit != exitOK {
			t.Fatalf("exit = %d (%s)", exit, errb.String())
		}
		if !strings.Contains(out.String(), "recover me\nplease") {
			t.Errorf("out = %q", out.String())
		}
	})

	t.Run("-o writes to file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "recovered.txt")
		var out, errb bytes.Buffer
		exit := runStrandedShowWithStore(ctx, s, id, f, &out, &errb)
		if exit != exitOK {
			t.Fatalf("exit = %d (%s)", exit, errb.String())
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read file: %v", err)
		}
		if string(b) != "recover me\nplease" {
			t.Errorf("file = %q", string(b))
		}
	})

	t.Run("unknown id → data error", func(t *testing.T) {
		var out, errb bytes.Buffer
		if exit := runStrandedShowWithStore(ctx, s, "nope", "", &out, &errb); exit != exitDataErr {
			t.Errorf("exit = %d, want %d", exit, exitDataErr)
		}
	})

	t.Run("non-stranded kind → data error", func(t *testing.T) {
		res, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "a", ToAgent: "b", Body: "hi"})
		var out, errb bytes.Buffer
		if exit := runStrandedShowWithStore(ctx, s, res.PublicID, "", &out, &errb); exit != exitDataErr {
			t.Errorf("exit = %d, want %d", exit, exitDataErr)
		}
	})
}

func TestRunStrandedPruneWithStore(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	now := time.Now().UTC()

	oldID := mkStranded(t, s, "qm", "%5", "old", "ancient draft")
	newID := mkStranded(t, s, "qm", "%5", "new", "fresh draft")
	// Backdate oldID to 8 days ago.
	old := now.Add(-8 * 24 * time.Hour).Format(strandedTimeFormat)
	if _, err := s.DB().ExecContext(ctx, `UPDATE messages SET created_at = ? WHERE public_id = ?`, old, oldID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	t.Run("missing --older-than → usage error", func(t *testing.T) {
		var out, errb bytes.Buffer
		if exit := runStrandedPruneWithStore(ctx, s, "qm", "", now, &out, &errb); exit != exitUsage {
			t.Errorf("exit = %d, want %d", exit, exitUsage)
		}
	})

	t.Run("'all' rejected", func(t *testing.T) {
		var out, errb bytes.Buffer
		if exit := runStrandedPruneWithStore(ctx, s, "qm", "all", now, &out, &errb); exit != exitUsage {
			t.Errorf("exit = %d, want %d", exit, exitUsage)
		}
	})

	t.Run("prunes only the old bookmark", func(t *testing.T) {
		var out, errb bytes.Buffer
		exit := runStrandedPruneWithStore(ctx, s, "qm", "7d", now, &out, &errb)
		if exit != exitOK {
			t.Fatalf("exit = %d (%s)", exit, errb.String())
		}
		if !strings.Contains(out.String(), "pruned 1") {
			t.Errorf("out = %q, want 'pruned 1'", out.String())
		}
		// newID survives, oldID gone.
		if _, err := s.GetMessage(ctx, newID); err != nil {
			t.Errorf("new bookmark should survive: %v", err)
		}
		if _, err := s.GetMessage(ctx, oldID); err == nil {
			t.Errorf("old bookmark should be pruned")
		}
	})
}

func TestRunStrandedListWithStore_Format(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	mkStranded(t, s, "qm", "%5", "7501", "draft")

	t.Run("bad format → usage", func(t *testing.T) {
		var out, errb bytes.Buffer
		if exit := runStrandedListWithStore(ctx, s, "qm", "yaml", &out, &errb); exit != exitUsage {
			t.Errorf("exit = %d, want %d", exit, exitUsage)
		}
	})

	t.Run("json omits content", func(t *testing.T) {
		var out, errb bytes.Buffer
		exit := runStrandedListWithStore(ctx, s, "qm", "json", &out, &errb)
		if exit != exitOK {
			t.Fatalf("exit = %d", exit)
		}
		var rows []map[string]any
		if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
			t.Fatalf("json: %v (%s)", err, out.String())
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if _, hasContent := rows[0]["content"]; hasContent {
			t.Error("list json should omit full content")
		}
		if rows[0]["pane"] != "%5" {
			t.Errorf("pane = %v", rows[0]["pane"])
		}
	})

	t.Run("empty list message", func(t *testing.T) {
		var out, errb bytes.Buffer
		runStrandedListWithStore(ctx, s, "nobody", "text", &out, &errb)
		if !strings.Contains(out.String(), "no stranded-draft bookmarks") {
			t.Errorf("out = %q", out.String())
		}
	})
}
