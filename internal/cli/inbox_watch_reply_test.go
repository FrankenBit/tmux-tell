package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func TestStripReplyTemplate_PreservesHashLines(t *testing.T) {
	// A reply that starts a line with #NNN (issue ref — common on this bus) must
	// survive verbatim. This is why we use a scissors cut, not #-line stripping.
	raw := "#268 is done, see the PR\nsecond line\n\n" + replyScissors +
		"\n# Reply from a to b (re 1a2b). Write above.\n# > original body\n"
	got := stripReplyTemplate(raw)
	want := "#268 is done, see the PR\nsecond line"
	if got != want {
		t.Errorf("stripReplyTemplate = %q, want %q", got, want)
	}
}

func TestStripReplyTemplate_EmptyAboveScissors(t *testing.T) {
	raw := "\n\n" + replyScissors + "\n# context\n# > quoted\n"
	if got := stripReplyTemplate(raw); got != "" {
		t.Errorf("empty reply area should strip to %q, got %q", "", got)
	}
}

func TestStripReplyTemplate_WhitespaceOnlyIsEmpty(t *testing.T) {
	raw := "   \n\t\n" + replyScissors + "\n# context\n"
	if got := stripReplyTemplate(raw); got != "" {
		t.Errorf("whitespace-only reply area should strip to empty, got %q", got)
	}
}

func TestReplyTemplate_HasScissorsAndQuotedBody(t *testing.T) {
	orig := store.Message{PublicID: "1a2b", FromAgent: "bosun", Body: "line one\nline two"}
	tpl := replyTemplate(orig, "mailbox")
	if !strings.Contains(tpl, replyScissors) {
		t.Error("template missing scissors marker")
	}
	if !strings.Contains(tpl, "re 1a2b") || !strings.Contains(tpl, "to bosun") {
		t.Errorf("template missing reply context:\n%s", tpl)
	}
	if !strings.Contains(tpl, "# > line one") || !strings.Contains(tpl, "# > line two") {
		t.Errorf("template missing quoted original:\n%s", tpl)
	}
	// The quoted body must be BELOW the scissors so it's stripped, not sent.
	if stripReplyTemplate(tpl) != "" {
		t.Errorf("fresh template should strip to empty reply, got %q", stripReplyTemplate(tpl))
	}
}

func TestComposeReply_ThreadsAndSends(t *testing.T) {
	s := newCmdTestStore(t, "sender", "mailbox")
	ctx := context.Background()
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "sender", ToAgent: "mailbox", Body: "the question",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	orig, _ := s.GetMessage(ctx, res.PublicID)

	replyID, err := composeReply(ctx, s, "mailbox", *orig, "the answer")
	if err != nil {
		t.Fatalf("composeReply: %v", err)
	}
	got, err := s.GetMessage(ctx, replyID)
	if err != nil {
		t.Fatalf("get reply: %v", err)
	}
	if got.FromAgent != "mailbox" || got.ToAgent != "sender" {
		t.Errorf("reply routing = %s→%s, want mailbox→sender", got.FromAgent, got.ToAgent)
	}
	if !got.ReplyTo.Valid || got.ReplyTo.String != orig.PublicID {
		t.Errorf("reply_to = %v, want %s (threaded under original)", got.ReplyTo, orig.PublicID)
	}
	if got.Body != "the answer" {
		t.Errorf("body = %q, want %q", got.Body, "the answer")
	}
}

func TestComposeReply_RejectsOversizeBody(t *testing.T) {
	s := newCmdTestStore(t, "sender", "mailbox")
	orig := store.Message{PublicID: "1a2b", FromAgent: "sender"}
	big := strings.Repeat("x", capBodyBytes+1)
	if _, err := composeReply(context.Background(), s, "mailbox", orig, big); err == nil {
		t.Fatal("expected oversize body to be rejected")
	}
}

func TestEditorArgv_Precedence(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	if got := editorArgv(); len(got) != 1 || got[0] != "vi" {
		t.Errorf("fallback = %v, want [vi]", got)
	}
	t.Setenv("EDITOR", "nano")
	if got := editorArgv(); len(got) != 1 || got[0] != "nano" {
		t.Errorf("EDITOR = %v, want [nano]", got)
	}
	t.Setenv("VISUAL", "code --wait")
	got := editorArgv()
	if len(got) != 2 || got[0] != "code" || got[1] != "--wait" {
		t.Errorf("VISUAL takes precedence + splits args; got %v", got)
	}
}

func TestInboxWatch_ReplyKeyStartsReply(t *testing.T) {
	m, _ := newWatchModel(t, 1)
	_, cmd := step(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Fatal("r on a selected message should return an editor cmd")
	}
}

func TestInboxWatch_ReplyKeyNoopOnEmpty(t *testing.T) {
	// Construct an explicitly-empty model (msgs nil) rather than priming from a
	// store: r's no-op-on-empty is a current()/cursor invariant, independent of
	// store state. (And `store.Open(":memory:")` shares one DB across opens
	// alive in the same test, so a second "empty" store would not be empty.)
	m := inboxWatchModel{agent: "mailbox", interval: inboxWatchDefaultInterval}
	if _, c := step(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); c != nil {
		t.Error("r on an empty list should be a no-op")
	}
	// space is likewise a no-op with nothing selected.
	if _, c := step(t, m, tea.KeyMsg{Type: tea.KeySpace}); c != nil {
		t.Error("space on an empty list should be a no-op")
	}
}

func TestInboxWatch_CompleteReply_Abandoned(t *testing.T) {
	m, s := newWatchModel(t, 1)
	orig := m.msgs[0]
	// A buffer with an empty reply area (only the template) → abandoned, no send.
	path := filepath.Join(t.TempDir(), "buf.md")
	if err := os.WriteFile(path, []byte(replyTemplate(orig, "mailbox")), 0o600); err != nil {
		t.Fatal(err)
	}
	res := m.completeReply(orig, path, nil)
	if !res.abandoned || res.sentID != "" || res.err != nil {
		t.Errorf("expected abandoned, got %+v", res)
	}
	// No reply landed in the sender's queue.
	replies, _ := s.ListMessages(context.Background(), store.ListFilter{ToAgent: "sender"})
	if len(replies) != 0 {
		t.Errorf("abandoned reply should send nothing; found %d", len(replies))
	}
	// Temp file cleaned up.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("completeReply should remove the temp file")
	}
}

func TestInboxWatch_CompleteReply_Sends(t *testing.T) {
	m, s := newWatchModel(t, 1)
	orig := m.msgs[0]
	path := filepath.Join(t.TempDir(), "buf.md")
	buf := "my actual reply\n\n" + replyTemplate(orig, "mailbox")
	if err := os.WriteFile(path, []byte(buf), 0o600); err != nil {
		t.Fatal(err)
	}
	res := m.completeReply(orig, path, nil)
	if res.err != nil || res.abandoned || res.sentID == "" {
		t.Fatalf("expected a sent reply, got %+v", res)
	}
	got, err := s.GetMessage(context.Background(), res.sentID)
	if err != nil {
		t.Fatalf("get sent reply: %v", err)
	}
	if got.Body != "my actual reply" || got.ToAgent != "sender" {
		t.Errorf("sent reply = %q to %s, want %q to sender", got.Body, got.ToAgent, "my actual reply")
	}
}

func TestInboxWatch_CompleteReply_EditorError(t *testing.T) {
	m, _ := newWatchModel(t, 1)
	orig := m.msgs[0]
	path := filepath.Join(t.TempDir(), "buf.md")
	_ = os.WriteFile(path, []byte("x"), 0o600)
	res := m.completeReply(orig, path, context.Canceled) // simulate editor failure
	if res.err == nil {
		t.Error("editor error should surface as res.err")
	}
}

func TestInboxWatch_ReplyMsgUpdatesFooter(t *testing.T) {
	base, _ := newWatchModel(t, 1)

	sent, _ := step(t, base, inboxReplyMsg{replyToID: "1a2b", sentID: "9f9f"})
	if !strings.Contains(sent.status, "replied") || !strings.Contains(sent.status, "9f9f") {
		t.Errorf("sent status = %q", sent.status)
	}

	ab, _ := step(t, base, inboxReplyMsg{replyToID: "1a2b", abandoned: true})
	if !strings.Contains(ab.status, "abandoned") {
		t.Errorf("abandoned status = %q", ab.status)
	}

	bad, _ := step(t, base, inboxReplyMsg{replyToID: "1a2b", err: errAckTest})
	if bad.loadErr == nil {
		t.Error("reply error should set loadErr")
	}
}
