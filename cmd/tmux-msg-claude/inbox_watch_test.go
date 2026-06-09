package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// step feeds one tea.Msg through Update and returns the concrete model + cmd.
// Keeps the model tests free of repetitive type-assertions.
func step(t *testing.T, m inboxWatchModel, msg tea.Msg) (inboxWatchModel, tea.Cmd) {
	t.Helper()
	updated, cmd := m.Update(msg)
	out, ok := updated.(inboxWatchModel)
	if !ok {
		t.Fatalf("Update returned %T, want inboxWatchModel", updated)
	}
	return out, cmd
}

// newWatchModel builds a model wired to a seeded store with `n` queued messages
// from "sender" to "mailbox", then primes it via one poll so msgs/cursor are set.
func newWatchModel(t *testing.T, n int) (inboxWatchModel, *store.Store) {
	t.Helper()
	s := newCmdTestStore(t, "sender", "mailbox")
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: "sender", ToAgent: "mailbox", Body: "body " + string(rune('A'+i)),
		}); err != nil {
			t.Fatalf("seed msg %d: %v", i, err)
		}
	}
	m := inboxWatchModel{store: s, ctx: ctx, agent: "mailbox", interval: inboxWatchDefaultInterval}
	// Prime: run the poll cmd and feed its result through Update.
	poll := m.pollCmd()
	m, _ = step(t, m, poll())
	return m, s
}

func TestInboxWatch_PollLoadsQueued(t *testing.T) {
	m, _ := newWatchModel(t, 3)
	if len(m.msgs) != 3 {
		t.Fatalf("msgs = %d, want 3", len(m.msgs))
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
	// Oldest-first (ListMessages ASC): first row is the first-inserted body.
	if m.msgs[0].Body != "body A" {
		t.Errorf("first row body = %q, want %q", m.msgs[0].Body, "body A")
	}
}

// The tick is the SOLE rescheduler (the #268 anti-multiplication fix). A poll
// result must NOT re-arm a tick — otherwise an action-triggered re-poll spawns a
// second tick chain and the poll rate compounds on every ack.
func TestInboxWatch_PollDoesNotReschedule(t *testing.T) {
	m, _ := newWatchModel(t, 1)
	_, cmd := step(t, m, inboxPollMsg{msgs: m.msgs})
	if cmd != nil {
		t.Fatal("poll handler returned a cmd; only the tick may reschedule (tick-multiplication regression)")
	}
}

func TestInboxWatch_TickReschedules(t *testing.T) {
	m, _ := newWatchModel(t, 1)
	// The tick polls AND arms the next tick — so the loop continues from here,
	// not from the poll result.
	_, cmd := step(t, m, inboxTickMsg{})
	if cmd == nil {
		t.Fatal("tick handler returned nil cmd; watch loop would stall")
	}
}

// An ack triggers a one-shot refresh poll; that poll must not reschedule, so the
// single tick chain is preserved (no compounding).
func TestInboxWatch_AckRefreshPollDoesNotReschedule(t *testing.T) {
	m, _ := newWatchModel(t, 2)
	_, ackCmd := step(t, m, tea.KeyMsg{Type: tea.KeySpace})
	res, ok := ackCmd().(inboxActionMsg)
	if !ok {
		t.Fatalf("ack cmd returned %T", ackCmd())
	}
	// Feed the action result: it re-polls (one-shot). That poll result must not
	// re-arm a tick.
	_, afterAction := step(t, m, res)
	if afterAction == nil {
		t.Fatal("action handler should return a one-shot refresh poll")
	}
	if pollMsg, isPoll := afterAction().(inboxPollMsg); isPoll {
		_, afterPoll := step(t, m, pollMsg)
		if afterPoll != nil {
			t.Fatal("refresh poll rescheduled a tick — multiplication regression")
		}
	} else {
		t.Fatalf("action refresh returned %T, want inboxPollMsg", afterAction())
	}
}

func TestInboxWatch_Navigation(t *testing.T) {
	m, _ := newWatchModel(t, 3)

	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 1 {
		t.Fatalf("after down: cursor = %d, want 1", m.cursor)
	}
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 2 {
		t.Fatalf("after down×2: cursor = %d, want 2", m.cursor)
	}
	// Down at the last row is a no-op (no wrap).
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 2 {
		t.Errorf("down at last row: cursor = %d, want 2 (clamped)", m.cursor)
	}
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 1 {
		t.Fatalf("after up: cursor = %d, want 1", m.cursor)
	}
	// Up at the top row is a no-op.
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("up past top: cursor = %d, want 0 (clamped)", m.cursor)
	}
}

func TestInboxWatch_ExpandToggleAndCollapseOnMove(t *testing.T) {
	m, _ := newWatchModel(t, 2)
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.expanded {
		t.Fatal("enter did not expand")
	}
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.expanded {
		t.Fatal("second enter did not collapse")
	}
	// Expanding then moving collapses (the expansion belonged to the old row).
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.expanded {
		t.Error("moving cursor did not collapse the expansion")
	}
}

func TestInboxWatch_SpaceAcksUnderCursor(t *testing.T) {
	m, s := newWatchModel(t, 2)
	ctx := context.Background()
	target := m.msgs[0].PublicID

	// space returns a cmd; running it performs the ack and yields an action msg.
	_, cmd := step(t, m, tea.KeyMsg{Type: tea.KeySpace})
	if cmd == nil {
		t.Fatal("space returned nil cmd")
	}
	res, ok := cmd().(inboxActionMsg)
	if !ok {
		t.Fatalf("ack cmd returned %T, want inboxActionMsg", cmd())
	}
	if res.err != nil {
		t.Fatalf("ack errored: %v", res.err)
	}
	if res.verb != "acked" || res.id != target {
		t.Errorf("action = %+v, want acked %s", res, target)
	}

	// Store really transitioned queued→acknowledged (#221 compose).
	got, err := s.GetMessage(ctx, target)
	if err != nil {
		t.Fatalf("get acked msg: %v", err)
	}
	if got.State != store.StateAcknowledged {
		t.Errorf("state = %q, want %q", got.State, store.StateAcknowledged)
	}

	// Feeding the action result updates the session counter + status, and
	// returns a re-poll cmd so the drained row drops.
	m2, pollCmd := step(t, m, res)
	if m2.acted != 1 {
		t.Errorf("acted = %d, want 1", m2.acted)
	}
	if !strings.Contains(m2.status, "acked") {
		t.Errorf("status = %q, want it to mention acked", m2.status)
	}
	if pollCmd == nil {
		t.Error("action handler returned nil cmd; expected a re-poll")
	}
}

func TestInboxWatch_AckErrorSurfacesInFooter(t *testing.T) {
	m, _ := newWatchModel(t, 1)
	m, _ = step(t, m, inboxActionMsg{verb: "acked", id: "deadbeef", err: errAckTest})
	if m.acted != 0 {
		t.Errorf("acted = %d, want 0 on error", m.acted)
	}
	if m.loadErr == nil {
		t.Error("loadErr not set on action error")
	}
	if !strings.Contains(m.View(), "⚠") {
		t.Error("footer did not surface the error marker")
	}
}

func TestInboxWatch_ReconcileKeepsSelectionWhenRowAboveDrains(t *testing.T) {
	m, _ := newWatchModel(t, 3)
	// Select the middle row, recording its public_id.
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	keep := m.msgs[1].PublicID
	if m.selID != keep {
		t.Fatalf("selID = %q, want %q", m.selID, keep)
	}
	// Simulate a poll where the top row drained (acked elsewhere): list is now
	// [keep, third]. Cursor should follow `keep` to its new index 0.
	newList := []store.Message{m.msgs[1], m.msgs[2]}
	m, _ = step(t, m, inboxPollMsg{msgs: newList})
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (followed selID)", m.cursor)
	}
	if m.msgs[m.cursor].PublicID != keep {
		t.Errorf("cursor landed on %q, want %q", m.msgs[m.cursor].PublicID, keep)
	}
}

func TestInboxWatch_EmptyListClampsCursor(t *testing.T) {
	m, _ := newWatchModel(t, 2)
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyDown}) // cursor 1
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = step(t, m, inboxPollMsg{msgs: nil})
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0 on empty list", m.cursor)
	}
	if m.selID != "" {
		t.Errorf("selID = %q, want empty on empty list", m.selID)
	}
	if m.expanded {
		t.Error("expanded should reset on empty list")
	}
	if !strings.Contains(m.View(), "drained") {
		t.Error("empty view should show the drained message")
	}
}

func TestInboxWatch_QuitKeys(t *testing.T) {
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyCtrlC},
		{Type: tea.KeyEsc},
	} {
		m, _ := newWatchModel(t, 1)
		m2, cmd := step(t, m, k)
		if !m2.quitting {
			t.Errorf("%s: quitting not set", k.String())
		}
		if cmd == nil {
			t.Fatalf("%s: quit returned nil cmd", k.String())
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s: cmd did not yield tea.QuitMsg", k.String())
		}
	}
}

func TestInboxWatch_ViewHasHeaderAndCursor(t *testing.T) {
	m, _ := newWatchModel(t, 2)
	v := m.View()
	if !strings.Contains(v, `inbox "mailbox"`) {
		t.Errorf("view missing titled header:\n%s", v)
	}
	if !strings.Contains(v, "2 pending") {
		t.Errorf("view missing pending count:\n%s", v)
	}
	if !strings.Contains(v, "space ack") {
		t.Errorf("view missing help line:\n%s", v)
	}
	if !strings.Contains(v, "▸") {
		t.Errorf("view missing cursor gutter:\n%s", v)
	}
}

func TestInboxWatch_ViewExpandedShowsBody(t *testing.T) {
	s := newCmdTestStore(t, "sender", "mailbox")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "sender", ToAgent: "mailbox",
		Body: "the full multi word body that should appear when expanded",
	})
	m := inboxWatchModel{store: s, ctx: ctx, agent: "mailbox", interval: inboxWatchDefaultInterval, width: 80}
	m, _ = step(t, m, m.pollCmd()())
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.View(), "full multi word body") {
		t.Errorf("expanded view missing full body:\n%s", m.View())
	}
}

func TestInboxWatch_QuittingViewIsEmpty(t *testing.T) {
	m, _ := newWatchModel(t, 1)
	m.quitting = true
	if m.View() != "" {
		t.Errorf("quitting view = %q, want empty (clean alt-screen exit)", m.View())
	}
}

func TestInboxWatch_SummaryLine(t *testing.T) {
	m, _ := newWatchModel(t, 2)
	m.acted = 3
	got := m.summaryLine()
	if !strings.Contains(got, "3 drained") || !strings.Contains(got, "2 still queued") {
		t.Errorf("summary = %q", got)
	}
}

// --- CLI-surface guards (no TTY required: all fire before the TUI launches) --

func TestInboxWatch_RejectsNonPositiveInterval(t *testing.T) {
	exit := runInboxWatch(context.Background(), nil, "mailbox", 0, &bytes.Buffer{}, &bytes.Buffer{})
	if exit != exitUsage {
		t.Errorf("exit = %d, want exitUsage for interval=0", exit)
	}
}

// TestInboxWatch_FlagGuards exercises the runInboxCLI guards that reject
// incoherent --watch combinations before any TUI launch (so no TTY needed).
// Uses a temp-file DB because runInboxCLI does its own store.Open.
func TestInboxWatch_FlagGuards(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	seed, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	if err := seed.UpsertAgent(context.Background(), "mailbox", "%9"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	_ = seed.Close()
	t.Setenv("CLAUDE_MSG_DB", dbPath)

	cases := []struct {
		name string
		args []string
	}{
		{"watch+json", []string{"mailbox", "--watch", "--format", "json"}},
		{"watch+ack", []string{"mailbox", "--watch", "--ack", "1a2b"}},
		{"watch+ack-all", []string{"mailbox", "--watch", "--ack-all"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := runInboxCLI(tc.args, &stdout, &stderr)
			if exit != exitUsage {
				t.Errorf("exit = %d, want exitUsage; stderr=%s", exit, stderr.String())
			}
		})
	}
}

var errAckTest = &ackTestErr{}

type ackTestErr struct{}

func (*ackTestErr) Error() string { return "synthetic ack failure" }
