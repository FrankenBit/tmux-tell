package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func tailInsert(t *testing.T, s *store.Store, from, to, kind string) string {
	t.Helper()
	r, err := s.InsertMessage(context.Background(), store.InsertParams{
		FromAgent: from, ToAgent: to, Body: "hello " + to, Kind: store.Kind(kind),
	})
	if err != nil {
		t.Fatalf("insert %s→%s: %v", from, to, err)
	}
	return r.PublicID
}

// allTail tails everything from the beginning (no since floor).
func allTailOpts(format string) tailOpts {
	return tailOpts{format: format, interval: tailDefaultInterval}
}

// tailDeliver drives the real lifecycle queued→delivering→delivered for the
// next queued message to `to` (ClaimNext then MarkDelivered).
func tailDeliver(t *testing.T, s *store.Store, to string) {
	t.Helper()
	m, err := s.ClaimNext(context.Background(), to)
	if err != nil || m == nil {
		t.Fatalf("ClaimNext(%s): m=%v err=%v", to, m, err)
	}
	if err := s.MarkDelivered(context.Background(), m.PublicID); err != nil {
		t.Fatalf("MarkDelivered(%s): %v", m.PublicID, err)
	}
}

func TestTailPoll_NewRowsInOrder(t *testing.T) {
	s := newCmdTestStore(t)
	a := tailInsert(t, s, "alice", "bob", "message")
	b := tailInsert(t, s, "bob", "carol", "message")

	ts := newTailState()
	var out bytes.Buffer
	if err := ts.poll(context.Background(), s, allTailOpts("text"), &out); err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, a) || !strings.Contains(got, b) {
		t.Errorf("output missing ids %s/%s:\n%s", a, b, got)
	}
	// alice→bob must appear before bob→carol (id-asc).
	if strings.Index(got, a) > strings.Index(got, b) {
		t.Errorf("rows out of order:\n%s", got)
	}
	// A second poll with no new rows emits nothing new.
	out.Reset()
	_ = ts.poll(context.Background(), s, allTailOpts("text"), &out)
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("second poll re-emitted rows: %q", out.String())
	}
}

func TestTailPoll_StateTransition(t *testing.T) {
	s := newCmdTestStore(t)
	id := tailInsert(t, s, "alice", "bob", "message")

	ts := newTailState()
	var out bytes.Buffer
	// First poll: surfaces the queued row.
	_ = ts.poll(context.Background(), s, allTailOpts("text"), &out)
	if !strings.Contains(out.String(), "queued") {
		t.Fatalf("first poll missing queued line:\n%s", out.String())
	}

	// Deliver it, then poll again: a queued→delivered transition on the same id.
	tailDeliver(t, s, "bob")
	out.Reset()
	_ = ts.poll(context.Background(), s, allTailOpts("text"), &out)
	line := out.String()
	if !strings.Contains(line, "queued→delivered") || !strings.Contains(line, id) {
		t.Errorf("transition line wrong:\n%s", line)
	}
	// Once terminal, it drops from pending — a further poll is silent.
	out.Reset()
	_ = ts.poll(context.Background(), s, allTailOpts("text"), &out)
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("delivered row re-polled: %q", out.String())
	}
}

func TestTailPoll_FilterFrom(t *testing.T) {
	s := newCmdTestStore(t)
	tailInsert(t, s, "alice", "bob", "message")
	keep := tailInsert(t, s, "bosun", "bob", "message")

	opts := allTailOpts("text")
	opts.filter = store.TailFilter{From: "bosun"}
	ts := newTailState()
	var out bytes.Buffer
	_ = ts.poll(context.Background(), s, opts, &out)
	got := out.String()
	if !strings.Contains(got, keep) || strings.Contains(got, "alice") {
		t.Errorf("from=bosun filter leaked:\n%s", got)
	}
}

func TestTailPoll_StateRenderGate(t *testing.T) {
	s := newCmdTestStore(t)
	qID := tailInsert(t, s, "alice", "bob", "message") // stays queued

	opts := allTailOpts("text")
	opts.state = "delivered" // render only delivered
	ts := newTailState()
	var out bytes.Buffer
	// queued row is tracked (for transitions) but NOT rendered under the gate.
	_ = ts.poll(context.Background(), s, opts, &out)
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("state=delivered gate showed a queued row: %q", out.String())
	}
	// On delivery the transition INTO delivered renders.
	tailDeliver(t, s, "bob")
	out.Reset()
	_ = ts.poll(context.Background(), s, opts, &out)
	if !strings.Contains(out.String(), "delivered") || !strings.Contains(out.String(), qID) {
		t.Errorf("state gate hid the transition into delivered:\n%s", out.String())
	}
}

func TestTailPoll_JSONLines(t *testing.T) {
	s := newCmdTestStore(t)
	id := tailInsert(t, s, "alice", "bob", "message")

	ts := newTailState()
	var out bytes.Buffer
	_ = ts.poll(context.Background(), s, allTailOpts("json"), &out)
	line := strings.TrimSpace(out.String())
	var ev tailEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("json decode %q: %v", line, err)
	}
	if ev.Event != "new" || ev.ID != id || ev.From != "alice" || ev.State != "queued" {
		t.Errorf("event = %+v, want new/%s/alice/queued", ev, id)
	}
}

func TestTailLoop_CleanExitOnCancel(t *testing.T) {
	s := newCmdTestStore(t)
	tailInsert(t, s, "alice", "bob", "message")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled → loop must terminate cleanly, not spin
	var out, errb bytes.Buffer
	if exit := tailLoop(ctx, s, allTailOpts("text"), &out, &errb); exit != exitOK {
		t.Errorf("cancelled loop exit = %d, want exitOK (%d)", exit, exitOK)
	}
}
