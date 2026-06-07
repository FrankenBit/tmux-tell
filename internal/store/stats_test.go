package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedStat inserts one message row with fully-controlled fields so the
// aggregation tests can assert counts, latency, and window filtering
// deterministically. deliveredAt "" → NULL.
func seedStat(t *testing.T, s *Store, id, from, to, state, createdAt, deliveredAt string) {
	t.Helper()
	var dat any
	if deliveredAt != "" {
		dat = deliveredAt
	}
	_, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at, delivered_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		id, from, to, "body", "message", state, createdAt, dat)
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// ts renders an offset from a fixed base in the schema's timestamp format.
var statBase = time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

func ts(offset time.Duration) string {
	return statBase.Add(offset).UTC().Format(sqliteTimeFormat)
}

func allWindow() StatsWindow { return StatsWindow{All: true} }

func TestStatsPerAgent_Counts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// bob receives: 2 delivered, 1 failed, 1 queued. alice sends 3, carol sends 1.
	seedStat(t, s, "a1", "alice", "bob", "delivered", ts(0), ts(1*time.Second))
	seedStat(t, s, "a2", "alice", "bob", "delivered", ts(0), ts(2*time.Second))
	seedStat(t, s, "a3", "alice", "bob", "failed", ts(0), "")
	seedStat(t, s, "a4", "carol", "bob", "queued", ts(0), "")

	got, err := s.StatsPerAgent(ctx, allWindow())
	if err != nil {
		t.Fatalf("StatsPerAgent: %v", err)
	}
	by := map[string]AgentStat{}
	for _, a := range got {
		by[a.Agent] = a
	}
	if a := by["alice"]; a.Sent != 3 || a.Received != 0 {
		t.Errorf("alice = %+v, want Sent 3 Received 0", a)
	}
	if a := by["carol"]; a.Sent != 1 {
		t.Errorf("carol Sent = %d, want 1", a.Sent)
	}
	b := by["bob"]
	if b.Received != 4 || b.Delivered != 2 || b.Failed != 1 || b.Queued != 1 {
		t.Errorf("bob = %+v, want Received 4 Delivered 2 Failed 1 Queued 1", b)
	}
	// sorted by agent name
	if got[0].Agent != "alice" {
		t.Errorf("first row = %q, want alice (sorted)", got[0].Agent)
	}
}

func TestStatsPerAgent_LatencyPercentiles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// 5 delivered to bob with latencies 1,2,3,4,5 s. p50 (nearest-rank) = 3s = 3000ms; p95 = 5s.
	for i := 1; i <= 5; i++ {
		seedStat(t, s, fmt.Sprintf("L%d", i), "alice", "bob", "delivered",
			ts(0), ts(time.Duration(i)*time.Second))
	}
	got, err := s.StatsPerAgent(ctx, allWindow())
	if err != nil {
		t.Fatalf("StatsPerAgent: %v", err)
	}
	var bob AgentStat
	for _, a := range got {
		if a.Agent == "bob" {
			bob = a
		}
	}
	if bob.P50LatencyMs != 3000 {
		t.Errorf("bob P50 = %dms, want 3000", bob.P50LatencyMs)
	}
	if bob.P95LatencyMs != 5000 {
		t.Errorf("bob P95 = %dms, want 5000", bob.P95LatencyMs)
	}
}

func TestStatsWindow_FiltersByCreatedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedStat(t, s, "old", "alice", "bob", "delivered", ts(-48*time.Hour), ts(-48*time.Hour+time.Second))
	seedStat(t, s, "new", "alice", "bob", "delivered", ts(0), ts(time.Second))

	// Window since base-1h includes only "new".
	w := StatsWindow{Since: statBase.Add(-1 * time.Hour)}
	tot, err := s.StatsTotals(ctx, w)
	if err != nil {
		t.Fatalf("StatsTotals: %v", err)
	}
	if tot.Total != 1 {
		t.Errorf("windowed total = %d, want 1 (old excluded)", tot.Total)
	}
	// All includes both.
	allTot, _ := s.StatsTotals(ctx, allWindow())
	if allTot.Total != 2 {
		t.Errorf("all total = %d, want 2", allTot.Total)
	}
}

func TestStatsTopPairs_OrderingAndLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// alice→bob: 3; alice→carol: 2; dave→bob: 1.
	for i := 0; i < 3; i++ {
		seedStat(t, s, fmt.Sprintf("ab%d", i), "alice", "bob", "delivered", ts(0), ts(time.Second))
	}
	for i := 0; i < 2; i++ {
		seedStat(t, s, fmt.Sprintf("ac%d", i), "alice", "carol", "delivered", ts(0), ts(time.Second))
	}
	seedStat(t, s, "db0", "dave", "bob", "delivered", ts(0), ts(time.Second))

	pairs, err := s.StatsTopPairs(ctx, allWindow(), 2)
	if err != nil {
		t.Fatalf("StatsTopPairs: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("top-2 returned %d pairs", len(pairs))
	}
	if pairs[0].From != "alice" || pairs[0].To != "bob" || pairs[0].Count != 3 {
		t.Errorf("top pair = %+v, want alice→bob count 3", pairs[0])
	}
	if pairs[1].From != "alice" || pairs[1].To != "carol" || pairs[1].Count != 2 {
		t.Errorf("2nd pair = %+v, want alice→carol count 2", pairs[1])
	}
	// limit 0 = all (3 distinct pairs).
	all, _ := s.StatsTopPairs(ctx, allWindow(), 0)
	if len(all) != 3 {
		t.Errorf("limit 0 returned %d pairs, want 3", len(all))
	}
}

func TestStatsTotals_Empty(t *testing.T) {
	s := newTestStore(t)
	tot, err := s.StatsTotals(context.Background(), allWindow())
	if err != nil {
		t.Fatalf("StatsTotals: %v", err)
	}
	if tot.Total != 0 {
		t.Errorf("empty total = %d, want 0", tot.Total)
	}
	agents, _ := s.StatsPerAgent(context.Background(), allWindow())
	if len(agents) != 0 {
		t.Errorf("empty per-agent = %d rows, want 0", len(agents))
	}
}

func TestMessagesInWindow_FullRowsAndFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedStat(t, s, "old", "alice", "bob", "delivered", ts(-48*time.Hour), ts(-48*time.Hour+time.Second))
	seedStat(t, s, "new", "alice", "bob", "delivered", ts(0), ts(time.Second))

	// All returns full rows (public_id populated, not the reduced statRow).
	all, err := s.MessagesInWindow(ctx, allWindow())
	if err != nil {
		t.Fatalf("MessagesInWindow: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %d rows, want 2", len(all))
	}
	if all[0].PublicID != "old" || all[1].PublicID != "new" {
		t.Errorf("rows = %q,%q, want old,new (id-asc, full rows)", all[0].PublicID, all[1].PublicID)
	}

	// Window bounds on created_at, sharing the whereSince seam with stats.
	win, err := s.MessagesInWindow(ctx, StatsWindow{Since: statBase.Add(-1 * time.Hour)})
	if err != nil {
		t.Fatalf("MessagesInWindow windowed: %v", err)
	}
	if len(win) != 1 || win[0].PublicID != "new" {
		t.Errorf("windowed = %+v, want only 'new'", win)
	}
}

func TestPercentile(t *testing.T) {
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("empty p50 = %d, want 0", got)
	}
	if got := percentile([]int{42}, 50); got != 42 {
		t.Errorf("single p50 = %d, want 42", got)
	}
	// 1..10 nearest-rank p50 = index ceil(0.5*10)=5 → value 5; p90 → idx 9 → 9; p100 → 10.
	vs := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(vs, 50); got != 5 {
		t.Errorf("p50 = %d, want 5", got)
	}
	if got := percentile(vs, 100); got != 10 {
		t.Errorf("p100 = %d, want 10", got)
	}
}
