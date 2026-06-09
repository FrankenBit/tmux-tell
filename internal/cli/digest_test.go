package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// seedMsg inserts a message row with full control over reply_to / kind /
// no_reply_expected so the digest thread-classification can be exercised.
// created_at is derived from seq so created_at ordering tracks id ordering.
func seedMsg(t *testing.T, s *store.Store, seq int, id, from, to, replyTo, kind, state string, noReply bool, body string) {
	t.Helper()
	var rt any
	if replyTo != "" {
		rt = replyTo
	}
	nre := 0
	if noReply {
		nre = 1
	}
	createdAt := fmt.Sprintf("2026-06-06T12:%02d:00.000Z", seq)
	_, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO messages (public_id, from_agent, to_agent, reply_to, body, kind, no_reply_expected, state, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		id, from, to, rt, body, kind, nre, state, createdAt)
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// seedDigestFixture lays down three conversational threads + one system notice:
//   - thread m1: alice→bob, bob replies with 🔕 → CLOSED
//   - thread m3: carol→bob, no reply, no 🔕 → IN-FLIGHT (awaiting bob)
//   - thread m4: dave→bob FYI with 🔕 → CLOSED
//   - m5: delivery_failure_notice → excluded from thread analysis
func seedDigestFixture(t *testing.T, s *store.Store) {
	seedMsg(t, s, 1, "m1", "alice", "bob", "", "message", "delivered", false, "need review on the PR")
	seedMsg(t, s, 2, "m2", "bob", "alice", "m1", "message", "delivered", true, "done, merged")
	seedMsg(t, s, 3, "m3", "carol", "bob", "", "message", "queued", false, "can you check the deploy?")
	seedMsg(t, s, 4, "m4", "dave", "bob", "", "message", "delivered", true, "FYI tagged v1")
	seedMsg(t, s, 5, "m5", "bob", "alice", "", "delivery_failure_notice", "delivered", false, "notice")
}

func TestDigestCLI_JSON(t *testing.T) {
	s := newCmdTestStore(t)
	seedDigestFixture(t, s)

	var stdout, stderr bytes.Buffer
	exit := runDigestWithStore(context.Background(), s, store.StatsWindow{All: true}, "all", "", "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	var res digestResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
		t.Fatalf("decode: %v; out=%s", err, stdout.String())
	}

	by := map[string]counterpartyDigest{}
	for _, c := range res.Counterparties {
		by[c.Agent] = c
	}
	// bob is in all three threads: 2 closed (m1, m4) + 1 in-flight (m3).
	if b := by["bob"]; b.Threads != 3 || b.Closed != 2 || b.InFlight != 1 {
		t.Errorf("bob = %+v, want Threads 3 Closed 2 InFlight 1", b)
	}
	// sent/received reuse StatsPerAgent (counts all kinds incl. the notice m5).
	if b := by["bob"]; b.Sent != 2 || b.Received != 3 {
		t.Errorf("bob counts = Sent %d Received %d, want Sent 2 Received 3", b.Sent, b.Received)
	}
	if c := by["carol"]; c.Threads != 1 || c.InFlight != 1 || c.Closed != 0 {
		t.Errorf("carol = %+v, want Threads 1 InFlight 1 Closed 0", c)
	}
	// exactly one in-flight thread: carol→bob (root m3), awaiting bob.
	if len(res.InFlight) != 1 {
		t.Fatalf("in-flight = %d threads, want 1: %+v", len(res.InFlight), res.InFlight)
	}
	f := res.InFlight[0]
	if f.RootID != "m3" || f.From != "carol" || f.Awaiting != "bob" {
		t.Errorf("in-flight[0] = %+v, want root m3 carol→bob", f)
	}
}

func TestDigestCLI_Text(t *testing.T) {
	s := newCmdTestStore(t)
	seedDigestFixture(t, s)

	var stdout, stderr bytes.Buffer
	exit := runDigestWithStore(context.Background(), s, store.StatsWindow{All: true}, "all", "", "text", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"COUNTERPARTY", "bob", "carol",
		"In-flight threads", "carol → bob awaits reply", "m3",
		"heuristic", "🔕",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q; got:\n%s", want, out)
		}
	}
}

func TestDigestCLI_CounterpartyScope(t *testing.T) {
	s := newCmdTestStore(t)
	seedDigestFixture(t, s)

	var stdout, stderr bytes.Buffer
	exit := runDigestWithStore(context.Background(), s, store.StatsWindow{All: true}, "all", "carol", "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var res digestResult
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res)
	if len(res.Counterparties) != 1 || res.Counterparties[0].Agent != "carol" {
		t.Errorf("scoped rows = %+v, want only carol", res.Counterparties)
	}
	// carol participates in exactly the one in-flight thread.
	if len(res.InFlight) != 1 || res.InFlight[0].RootID != "m3" {
		t.Errorf("scoped in-flight = %+v, want only m3", res.InFlight)
	}

	// bob scope: bob is in all 3 threads but only m3 is in-flight.
	stdout.Reset()
	_ = runDigestWithStore(context.Background(), s, store.StatsWindow{All: true}, "all", "bob", "json", &stdout, &stderr)
	var res2 digestResult
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res2)
	if len(res2.InFlight) != 1 || res2.InFlight[0].RootID != "m3" {
		t.Errorf("bob in-flight = %+v, want only m3", res2.InFlight)
	}
}

func TestClassifyThreads_ClosedVsInFlight(t *testing.T) {
	s := newCmdTestStore(t)
	seedDigestFixture(t, s)

	msgs, err := s.MessagesInWindow(context.Background(), store.StatsWindow{All: true})
	if err != nil {
		t.Fatalf("MessagesInWindow: %v", err)
	}
	threads, err := classifyThreads(msgs)
	if err != nil {
		t.Fatalf("classifyThreads: %v", err)
	}
	// 3 conversational threads (the notice m5 is excluded).
	if len(threads) != 3 {
		t.Fatalf("threads = %d, want 3 (notice excluded)", len(threads))
	}
	byRoot := map[string]threadInfo{}
	for _, tinfo := range threads {
		byRoot[tinfo.rootID] = tinfo
	}
	if ti, ok := byRoot["m1"]; !ok || !ti.closed {
		t.Errorf("thread m1 = %+v, want closed (bob's 🔕 reply was last word)", ti)
	}
	if ti, ok := byRoot["m3"]; !ok || ti.closed {
		t.Errorf("thread m3 = %+v, want in-flight (carol's question un-answered)", ti)
	}
	if ti, ok := byRoot["m4"]; !ok || !ti.closed {
		t.Errorf("thread m4 = %+v, want closed (dave's 🔕 FYI)", ti)
	}
	// the in-flight thread's latest is carol's root message itself.
	if ti := byRoot["m3"]; ti.latest.PublicID != "m3" || ti.latest.ToAgent != "bob" {
		t.Errorf("m3 latest = %+v, want m3 awaiting bob", ti.latest)
	}
}

func TestDigest_EmptyWindow(t *testing.T) {
	s := newCmdTestStore(t)
	var stdout, stderr bytes.Buffer
	exit := runDigestWithStore(context.Background(), s, store.StatsWindow{All: true}, "all", "", "text", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	out := stdout.String()
	if !strings.Contains(out, "no conversational traffic") || !strings.Contains(out, "(none") {
		t.Errorf("empty digest missing empty-state lines; got:\n%s", out)
	}
}
