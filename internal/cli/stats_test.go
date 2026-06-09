package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// seedStatRow inserts a fully-controlled message row for CLI-level stats tests
// (window=all is used, so created_at only needs to be valid, not precise).
func seedStatRow(t *testing.T, s *store.Store, id, from, to, state, deliveredAt string) {
	t.Helper()
	var dat any
	if deliveredAt != "" {
		dat = deliveredAt
	}
	_, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at, delivered_at)
		 VALUES (?,?,?,?,?,?, '2026-06-06T12:00:00.000Z', ?)`,
		id, from, to, "body", "message", state, dat)
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func seedStatsFixture(t *testing.T, s *store.Store) {
	// alice → bob: 2 delivered + 1 queued; alice → carol: 1 delivered; dave → bob: 1 failed.
	seedStatRow(t, s, "s1", "alice", "bob", "delivered", "2026-06-06T12:00:01.000Z")
	seedStatRow(t, s, "s2", "alice", "bob", "delivered", "2026-06-06T12:00:02.000Z")
	seedStatRow(t, s, "s3", "alice", "bob", "queued", "")
	seedStatRow(t, s, "s4", "alice", "carol", "delivered", "2026-06-06T12:00:01.000Z")
	seedStatRow(t, s, "s5", "dave", "bob", "failed", "")
}

func allSpec() store.StatsWindow { return store.StatsWindow{All: true} }

func TestStatsCLI_JSON(t *testing.T) {
	s := newCmdTestStore(t)
	seedStatsFixture(t, s)

	var stdout, stderr bytes.Buffer
	exit := runStatsWithStore(context.Background(), s, allSpec(), "all", "json", "", false, 10, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	var res statsResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
		t.Fatalf("decode: %v; out=%s", err, stdout.String())
	}
	by := map[string]store.AgentStat{}
	for _, a := range res.Agents {
		by[a.Agent] = a
	}
	if a := by["alice"]; a.Sent != 4 { // 3 to bob + 1 to carol
		t.Errorf("alice Sent = %d, want 4", a.Sent)
	}
	if b := by["bob"]; b.Received != 4 || b.Delivered != 2 || b.Failed != 1 || b.Queued != 1 {
		t.Errorf("bob = %+v, want Received 4 Delivered 2 Failed 1 Queued 1", b)
	}
	if res.Totals.Total != 5 || res.Totals.Delivered != 3 || res.Totals.Failed != 1 {
		t.Errorf("totals = %+v, want Total 5 Delivered 3 Failed 1", res.Totals)
	}
}

func TestStatsCLI_Text(t *testing.T) {
	s := newCmdTestStore(t)
	seedStatsFixture(t, s)

	var stdout, stderr bytes.Buffer
	exit := runStatsWithStore(context.Background(), s, allSpec(), "all", "text", "", false, 10, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"AGENT", "alice", "bob", "Totals: 5 messages", "Delivered split:"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q; got:\n%s", want, out)
		}
	}
}

func TestStatsCLI_AgentFilter(t *testing.T) {
	s := newCmdTestStore(t)
	seedStatsFixture(t, s)

	var stdout, stderr bytes.Buffer
	exit := runStatsWithStore(context.Background(), s, allSpec(), "all", "json", "bob", false, 10, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var res statsResult
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res)
	if len(res.Agents) != 1 || res.Agents[0].Agent != "bob" {
		t.Errorf("agent-filtered = %+v, want only bob", res.Agents)
	}
}

func TestStatsCLI_Pairs(t *testing.T) {
	s := newCmdTestStore(t)
	seedStatsFixture(t, s)

	var stdout, stderr bytes.Buffer
	exit := runStatsWithStore(context.Background(), s, allSpec(), "all", "json", "", true, 10, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var res statsResult
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res)
	if len(res.TopPairs) == 0 {
		t.Fatal("--pair returned no pairs")
	}
	top := res.TopPairs[0]
	if top.From != "alice" || top.To != "bob" || top.Count != 3 {
		t.Errorf("top pair = %+v, want alice→bob count 3", top)
	}
	// agent filter on pairs: only pairs touching carol.
	stdout.Reset()
	_ = runStatsWithStore(context.Background(), s, allSpec(), "all", "json", "carol", true, 10, &stdout, &stderr)
	var res2 statsResult
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res2)
	for _, p := range res2.TopPairs {
		if p.From != "carol" && p.To != "carol" {
			t.Errorf("agent-filtered pair %+v doesn't touch carol", p)
		}
	}
}
