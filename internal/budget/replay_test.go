// Calibration replay (tmux-tell#950, methodology from #927).
//
// refill.go's calibration table is a claim about real traffic, and re-deriving
// it used to mean rebuilding this harness from that comment's prose. This is
// the harness, so the numbers are re-runnable instead of re-derivable.
//
// 🔑 IT DRIVES budget.Cost AND budget.Refilled DIRECTLY rather than
// re-implementing them, so the calibration cannot drift from the code it
// calibrates.
//
// ⚠️ IT SKIPS WITHOUT `REPLAY_DB`, so CI never runs the replay itself — which
// would leave the file rotting silently. TestGroupFoldsAFanOutIntoOneSend below
// runs ALWAYS and covers the one piece of logic this file adds (the grouping),
// which refill.go names as "the first question you must answer, not the last".
//
// ⚠️ POINT IT AT A COPY. The live messages.db is the running bus.
package budget_test

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/budget"
	_ "modernc.org/sqlite"
)

type row struct {
	from, to string
	at       time.Time
	bytes    int
}

type sendGroup struct {
	from       string
	at         time.Time
	recipients []string
	bytes      int
}

// group folds delivery rows into send calls: same sender, same body length,
// each row within gap of the group's LAST row (chain, not first — a staggered
// fan-out walks forward).
func group(rows []row, gap time.Duration) []sendGroup {
	bySender := map[string][]row{}
	for _, r := range rows {
		bySender[r.from] = append(bySender[r.from], r)
	}
	var out []sendGroup
	for from, rs := range bySender {
		sort.Slice(rs, func(i, j int) bool { return rs[i].at.Before(rs[j].at) })
		var cur *sendGroup
		var last time.Time
		for _, r := range rs {
			if cur != nil && cur.bytes == r.bytes && r.at.Sub(last) <= gap {
				cur.recipients = append(cur.recipients, r.to)
				last = r.at
				continue
			}
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &sendGroup{from: from, at: r.at, recipients: []string{r.to}, bytes: r.bytes}
			last = r.at
		}
		if cur != nil {
			out = append(out, *cur)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// replay drives the SHIPPED Cost/Refilled per sender. Returns refusals, sends,
// and each sender's ending balance.
func replay(groups []sendGroup, capacity, refill float64) (refused, total int, end map[string]float64) {
	type st struct {
		bal   float64
		last  time.Time
		seen  map[string][]time.Time
		first bool
	}
	agents := map[string]*st{}
	end = map[string]float64{}
	floor := budget.Overdraft(capacity)
	for _, g := range groups {
		a := agents[g.from]
		if a == nil {
			a = &st{bal: capacity, seen: map[string][]time.Time{}, first: true}
			agents[g.from] = a
		}
		if !a.first {
			a.bal = budget.Refilled(a.bal, capacity, refill, g.at.Sub(a.last))
		}
		a.first = false
		a.last = g.at

		prior := 0
		for _, ts := range a.seen {
			for _, t := range ts {
				if g.at.Sub(t) <= budget.DefaultWindow {
					prior++
					break
				}
			}
		}
		newDistinct := prior
		for _, to := range uniq(g.recipients) {
			fresh := true
			for _, t := range a.seen[to] {
				if g.at.Sub(t) <= budget.DefaultWindow {
					fresh = false
					break
				}
			}
			if fresh {
				newDistinct++
			}
		}
		c := budget.Cost(prior, newDistinct, len(g.recipients), g.bytes, budget.DefaultAlpha)
		total++
		if a.bal-c < floor {
			refused++
			continue
		}
		a.bal -= c
		for _, to := range g.recipients {
			a.seen[to] = append(a.seen[to], g.at)
		}
	}
	for k, v := range agents {
		end[k] = v.bal
	}
	return refused, total, end
}

func uniq(in []string) []string {
	m := map[string]bool{}
	var out []string
	for _, s := range in {
		if !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	return out
}

func load(t *testing.T, db *sql.DB, from, to string) []row {
	t.Helper()
	q := `SELECT from_agent, to_agent, created_at, length(body)
	      FROM messages WHERE created_at >= ? AND created_at < ? ORDER BY created_at`
	rs, err := db.Query(q, from, to)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var f, tt, at string
		var n int
		if err := rs.Scan(&f, &tt, &at, &n); err != nil {
			t.Fatal(err)
		}
		ts, err := time.Parse("2006-01-02T15:04:05.000Z", at)
		if err != nil {
			ts, err = time.Parse(time.RFC3339, at)
			if err != nil {
				continue
			}
		}
		out = append(out, row{from: f, to: tt, at: ts, bytes: n})
	}
	return out
}

func TestReplayAgainstRealTraffic(t *testing.T) {
	path := os.Getenv("REPLAY_DB")
	if path == "" {
		t.Skip("set REPLAY_DB=/path/to/a/COPY/of/messages.db to re-derive the calibration table in refill.go")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	days := []struct{ name, from, to string }{
		{"08-19 incident", "2026-08-19T00:00", "2026-08-20T00:00"},
		{"08-24 quiet   ", "2026-08-24T00:00", "2026-08-25T00:00"},
		{"08-25 quiet   ", "2026-08-25T00:00", "2026-08-26T00:00"},
		{"08-26 busy    ", "2026-08-26T00:00", "2026-08-27T00:00"},
		{"09-04 TODAY   ", "2026-09-04T00:00", "2026-09-05T00:00"},
	}
	settings := []struct {
		name          string
		cap_, refill_ float64
	}{
		{"500/80", 500, 80},
		{"150/20", 150, 20},
		{"120/16", 120, 16},
		{"100/14", 100, 14},
	}
	for _, gap := range []time.Duration{2 * time.Second, 12 * time.Second} {
		fmt.Printf("\n===== grouping gap = %v =====\n", gap)
		fmt.Printf("%-16s %7s %7s  %s\n", "day", "rows", "sends", "refusal% by cap/refill")
		for _, d := range days {
			rows := load(t, db, d.from, d.to)
			gs := group(rows, gap)
			line := fmt.Sprintf("%-16s %7d %7d ", d.name, len(rows), len(gs))
			for _, s := range settings {
				ref, tot, _ := replay(gs, s.cap_, s.refill_)
				pct := 0.0
				if tot > 0 {
					pct = 100 * float64(ref) / float64(tot)
				}
				line += fmt.Sprintf("  %s=%5.1f%%", s.name, pct)
			}
			fmt.Println(line)
		}
	}

	// Ending balances for today at the shipped setting.
	rows := load(t, db, "2026-09-04T00:00", "2026-09-05T00:00")
	gs := group(rows, 2*time.Second)
	ref, tot, end := replay(gs, 120, 16)
	fmt.Printf("\n=== TODAY at shipped 120/16: %d refused of %d sends (%.1f%%) ===\n",
		ref, tot, 100*float64(ref)/float64(tot))
	type kv struct {
		k string
		v float64
	}
	var all []kv
	for k, v := range end {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v < all[j].v })
	for _, e := range all {
		fmt.Printf("  %-14s end balance %8.1f\n", e.k, e.v)
	}
}

// TestGroupFoldsAFanOutIntoOneSend is the always-on control for the grouping
// rule the replay depends on. Ungrouped, every fan-out leg prices as a
// 1-recipient send, the breadth term never engages, and the same cap/refill
// reads far gentler — on 08-19 grouping removes 54.3% of the rows.
func TestGroupFoldsAFanOutIntoOneSend(t *testing.T) {
	t0 := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	rows := []row{
		// one fan-out: same sender, same body length, legs 400ms apart (#580's
		// per-pool stagger spaces them; they are still ONE send).
		{from: "a", to: "x", at: t0, bytes: 100},
		{from: "a", to: "y", at: t0.Add(400 * time.Millisecond), bytes: 100},
		{from: "a", to: "z", at: t0.Add(800 * time.Millisecond), bytes: 100},
		// a separate send: far enough out to be its own group.
		{from: "a", to: "x", at: t0.Add(time.Minute), bytes: 100},
		// a different sender never joins another's group.
		{from: "b", to: "x", at: t0.Add(200 * time.Millisecond), bytes: 100},
	}
	got := group(rows, 2*time.Second)
	if len(got) != 3 {
		t.Fatalf("got %d groups, want 3 (one 3-leg fan-out, one later single, one other sender)", len(got))
	}
	byLen := map[int]int{}
	for _, g := range got {
		byLen[len(g.recipients)]++
	}
	if byLen[3] != 1 || byLen[1] != 2 {
		t.Errorf("group sizes = %v, want one group of 3 and two of 1", byLen)
	}

	// The negative control: a body of a DIFFERENT length is a different send even
	// when it lands inside the window, so grouping cannot swallow one. Changing
	// the MIDDLE leg breaks the chain in both directions — x, y and z each become
	// their own group — so 3 groups replace 1 and the total goes 3 → 5, not 4.
	// (Written as 4 first; the arm caught it.)
	rows[1].bytes = 101
	if got := group(rows, 2*time.Second); len(got) != 5 {
		t.Errorf("got %d groups after changing one body length, want 5 — the 3-leg fan-out must split into 3", len(got))
	}
}
