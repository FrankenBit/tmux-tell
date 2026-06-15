package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func TestBuildThreadTree(t *testing.T) {
	t.Run("links parent → children, detects root", func(t *testing.T) {
		// root ← c1 ← gc ; root ← c2  (id-asc order as GetThread returns)
		msgs := []store.Message{
			{ID: 1, PublicID: "root", FromAgent: "a", ToAgent: "b", Kind: store.KindMessage, State: store.StateDelivered},
			{ID: 2, PublicID: "c1", ReplyTo: ns("root"), FromAgent: "b", ToAgent: "a", Kind: store.KindMessage, State: store.StateDelivered},
			{ID: 3, PublicID: "c2", ReplyTo: ns("root"), FromAgent: "b", ToAgent: "a", Kind: store.KindMessage, State: store.StateFailed},
			{ID: 4, PublicID: "gc", ReplyTo: ns("c1"), FromAgent: "a", ToAgent: "b", Kind: store.KindMessage, State: store.StateQueued},
		}
		root, err := buildThreadTree(msgs)
		if err != nil {
			t.Fatalf("buildThreadTree: %v", err)
		}
		if root.ID != "root" {
			t.Fatalf("root = %s, want root", root.ID)
		}
		if len(root.Children) != 2 {
			t.Fatalf("root children = %d, want 2", len(root.Children))
		}
		// id-asc order preserved: c1 before c2
		if root.Children[0].ID != "c1" || root.Children[1].ID != "c2" {
			t.Errorf("child order = [%s,%s], want [c1,c2]", root.Children[0].ID, root.Children[1].ID)
		}
		if len(root.Children[0].Children) != 1 || root.Children[0].Children[0].ID != "gc" {
			t.Errorf("c1 children = %+v, want [gc]", root.Children[0].Children)
		}
		if len(root.Children[1].Children) != 0 {
			t.Errorf("c2 should be a leaf, got %d children", len(root.Children[1].Children))
		}
	})

	t.Run("root via reply_to pointing outside the set", func(t *testing.T) {
		// 'orphan' replies to 'gone' which isn't in the set → orphan is the root.
		msgs := []store.Message{
			{ID: 5, PublicID: "orphan", ReplyTo: ns("gone"), FromAgent: "a", ToAgent: "b", Kind: store.KindMessage, State: store.StateDelivered},
		}
		root, err := buildThreadTree(msgs)
		if err != nil {
			t.Fatalf("buildThreadTree: %v", err)
		}
		if root.ID != "orphan" {
			t.Errorf("root = %s, want orphan", root.ID)
		}
	})

	t.Run("empty thread errors", func(t *testing.T) {
		if _, err := buildThreadTree(nil); err == nil {
			t.Fatal("want error for empty thread")
		}
	})

	t.Run("multiple roots errors", func(t *testing.T) {
		msgs := []store.Message{
			{ID: 1, PublicID: "r1", FromAgent: "a", ToAgent: "b", Kind: store.KindMessage, State: store.StateDelivered},
			{ID: 2, PublicID: "r2", FromAgent: "a", ToAgent: "b", Kind: store.KindMessage, State: store.StateDelivered},
		}
		if _, err := buildThreadTree(msgs); err == nil {
			t.Fatal("want error for multiple roots")
		}
	})
}

func TestStateGlyph(t *testing.T) {
	cases := map[string]string{
		string(store.StateDelivered):  glyphDelivered,
		string(store.StateFailed):     glyphFailed,
		string(store.StateQueued):     glyphInFlight,
		string(store.StateDelivering): glyphInFlight,
		"weird_unknown_state":         glyphUnknown,
	}
	for state, want := range cases {
		if got := stateGlyph(state); got != want {
			t.Errorf("stateGlyph(%q) = %s, want %s", state, got, want)
		}
	}
}

func TestThreadBodyPreview(t *testing.T) {
	t.Run("collapses whitespace to single line", func(t *testing.T) {
		got := threadBodyPreview("hello\n\n  world\t  again")
		if got != "hello world again" {
			t.Errorf("preview = %q, want %q", got, "hello world again")
		}
	})
	t.Run("truncates long bodies", func(t *testing.T) {
		long := strings.Repeat("x", 100)
		got := threadBodyPreview(long)
		if len([]rune(got)) > threadPreviewLn+1 { // +1 for the … ellipsis rune
			t.Errorf("preview rune-len = %d, want <= %d", len([]rune(got)), threadPreviewLn+1)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("expected ellipsis suffix on truncated preview, got %q", got)
		}
	})
}

// buildDBThread inserts a real reply chain and sets a couple of states so
// the integration test exercises GetThread → buildThreadTree → render
// end-to-end. Returns the four public_ids.
func buildDBThread(t *testing.T, s *store.Store) (root, c1, c2, gc string) {
	t.Helper()
	ctx := context.Background()
	mk := func(from, to, replyTo, body string) string {
		res, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: from, ToAgent: to, ReplyTo: replyTo, Body: body,
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		return res.PublicID
	}
	root = mk("a", "b", "", "root msg")
	c1 = mk("b", "a", root, "reply one")
	// deliver c1
	if _, err := s.ClaimNext(ctx, "a"); err != nil {
		t.Fatalf("claim c1: %v", err)
	}
	if err := s.MarkDelivered(ctx, c1); err != nil {
		t.Fatalf("mark c1 delivered: %v", err)
	}
	c2 = mk("b", "a", root, "reply two")
	// fail c2
	if _, err := s.ClaimNext(ctx, "a"); err != nil {
		t.Fatalf("claim c2: %v", err)
	}
	if err := s.MarkFailed(ctx, c2, "pane gone"); err != nil {
		t.Fatalf("mark c2 failed: %v", err)
	}
	gc = mk("a", "b", c1, "grandchild") // left queued
	return root, c1, c2, gc
}

func TestRunThreadWithStore_TreeRender(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	root, c1, c2, gc := buildDBThread(t, s)

	var out, errb bytes.Buffer
	// Query from a leaf id to prove root-walk works from anywhere in the chain.
	exit := runThreadWithStore(context.Background(), s, gc, "tree", &out, &errb)
	if exit != exitOK {
		t.Fatalf("exit = %d (stderr=%q)", exit, errb.String())
	}
	text := out.String()

	// Root line: ○ glyph + root id.
	if !strings.Contains(text, glyphRoot+" id="+root) {
		t.Errorf("missing root line; got:\n%s", text)
	}
	// c1 delivered → ✓, branch connector, body preview.
	if !strings.Contains(text, glyphDelivered+" id="+c1) || !strings.Contains(text, "reply one") {
		t.Errorf("missing/incorrect c1 line; got:\n%s", text)
	}
	// c2 failed → ✗.
	if !strings.Contains(text, glyphFailed+" id="+c2) {
		t.Errorf("missing/incorrect c2 line; got:\n%s", text)
	}
	// gc queued → … and nested under c1 (continuation bar present).
	if !strings.Contains(text, glyphInFlight+" id="+gc) {
		t.Errorf("missing gc line; got:\n%s", text)
	}
	if !strings.Contains(text, "└─ ") || !strings.Contains(text, "├─ ") {
		t.Errorf("expected tree connectors; got:\n%s", text)
	}
}

func TestRunThreadWithStore_JSON(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	root, _, _, _ := buildDBThread(t, s)

	var out, errb bytes.Buffer
	exit := runThreadWithStore(context.Background(), s, root, "json", &out, &errb)
	if exit != exitOK {
		t.Fatalf("exit = %d (stderr=%q)", exit, errb.String())
	}
	var tree threadNode
	if err := json.Unmarshal(out.Bytes(), &tree); err != nil {
		t.Fatalf("json: %v (out=%q)", err, out.String())
	}
	if tree.ID != root {
		t.Errorf("json root = %s, want %s", tree.ID, root)
	}
	if len(tree.Children) != 2 {
		t.Errorf("json root children = %d, want 2", len(tree.Children))
	}
}

func TestRunThreadWithStore_Errors(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })

	t.Run("unknown id", func(t *testing.T) {
		var out, errb bytes.Buffer
		exit := runThreadWithStore(context.Background(), s, "nope", "tree", &out, &errb)
		if exit != exitDataErr {
			t.Errorf("exit = %d, want %d", exit, exitDataErr)
		}
	})

	t.Run("bad format", func(t *testing.T) {
		root, _, _, _ := buildDBThread(t, s)
		var out, errb bytes.Buffer
		exit := runThreadWithStore(context.Background(), s, root, "yaml", &out, &errb)
		if exit != exitUsage {
			t.Errorf("exit = %d, want %d", exit, exitUsage)
		}
	})
}
