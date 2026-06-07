package store

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// Bus-traffic aggregation primitives (#147 `claude-msg stats`). These are the
// reusable store-layer seam: the CLI (and #161 `digest`) consume them rather
// than re-writing the SQL. All aggregates are sourced from the messages table
// and bounded by a StatsWindow.
//
// Note on the verified/unverified split: a delivered message carries a durable
// `verified` bit since #169 (1 = verify-token observed, 0 = delivered_unverified
// soft-fail, NULL = delivered before the marker existed). The scalar aggregates
// here still count `Delivered` as a whole (state='delivered'); the breakdown is
// available via DeliveredVerificationCounts, the seam #147 re-consumes.

// StatsWindow bounds an aggregation to messages created within a time window.
// All=true selects every message regardless of age (the `--window all` case);
// otherwise only messages with created_at >= Since are counted.
type StatsWindow struct {
	Since time.Time
	All   bool
}

// sqliteTimeFormat matches the schema's strftime('%Y-%m-%dT%H:%M:%fZ','now')
// — ISO 8601 UTC with millisecond resolution, lexically sortable.
const sqliteTimeFormat = "2006-01-02T15:04:05.000Z"

// whereSince returns the created_at clause + arg for w. For All it returns an
// always-true clause and no arg, so callers can compose uniformly.
func (w StatsWindow) whereSince() (clause string, args []any) {
	if w.All {
		// Compile-time constant for --window all; no user input is
		// interpolated here — safe to compose directly into a WHERE clause.
		return "1=1", nil
	}
	return "created_at >= ?", []any{w.Since.UTC().Format(sqliteTimeFormat)}
}

// AgentStat is one agent's per-window traffic summary. Sent counts messages
// the agent originated (from_agent); the remaining counts are recipient-side
// (to_agent) — Received is everything addressed to the agent, with Delivered/
// Failed/Queued the recipient-side outcomes, and the latency percentiles
// measured created_at→delivered_at for messages delivered to the agent.
type AgentStat struct {
	Agent        string `json:"agent"`
	Sent         int    `json:"sent"`
	Received     int    `json:"received"`
	Delivered    int    `json:"delivered"`
	Failed       int    `json:"failed"`
	Queued       int    `json:"queued"`
	Delivering   int    `json:"delivering"`
	P50LatencyMs int    `json:"p50_latency_ms"` // 0 when no delivered messages
	P95LatencyMs int    `json:"p95_latency_ms"`
}

// PairStat is a sender→recipient pair's message count + median latency.
type PairStat struct {
	From         string `json:"from"`
	To           string `json:"to"`
	Count        int    `json:"count"`
	P50LatencyMs int    `json:"p50_latency_ms"`
}

// Totals is the window-wide aggregate across all agents.
type Totals struct {
	Total      int `json:"total"`
	Delivered  int `json:"delivered"`
	Failed     int `json:"failed"`
	Queued     int `json:"queued"`
	Delivering int `json:"delivering"`
}

// statRow is one scanned message reduced to the fields the aggregates need.
type statRow struct {
	from, to, state string
	latencyMs       int
	hasLatency      bool
}

// scanStats runs the single window-bounded scan the aggregates share. Keeping
// it one query keeps the aggregation logic pure-Go (testable) and lets #161
// reuse the same material. Scale is an on-demand operator query over a
// retention-bounded table, so loading the window into memory is acceptable.
func (s *Store) scanStats(ctx context.Context, w StatsWindow) ([]statRow, error) {
	clause, args := w.whereSince()
	// latency in ms via julianday delta (days) × 86_400_000; NULL when not
	// delivered. ROUND (not bare CAST) because the julianday float product
	// lands just under the integer (3s → 2999.9996); truncation would
	// systematically under-report by ~1ms, so round to nearest.
	rows, err := s.db.QueryContext(ctx,
		`SELECT from_agent, to_agent, state,
		        CAST(ROUND((julianday(delivered_at) - julianday(created_at)) * 86400000) AS INTEGER) AS latency_ms
		 FROM messages WHERE `+clause+` ORDER BY id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []statRow
	for rows.Next() {
		var r statRow
		var lat sql.NullInt64
		if err := rows.Scan(&r.from, &r.to, &r.state, &lat); err != nil {
			return nil, err
		}
		if lat.Valid && lat.Int64 >= 0 {
			r.latencyMs = int(lat.Int64)
			r.hasLatency = true
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StatsPerAgent returns per-agent traffic summaries for the window, sorted by
// agent name. Every agent appearing as a sender or recipient in-window gets a
// row.
func (s *Store) StatsPerAgent(ctx context.Context, w StatsWindow) ([]AgentStat, error) {
	rows, err := s.scanStats(ctx, w)
	if err != nil {
		return nil, err
	}
	type acc struct {
		stat      AgentStat
		latencies []int
	}
	m := map[string]*acc{}
	get := func(name string) *acc {
		a := m[name]
		if a == nil {
			a = &acc{stat: AgentStat{Agent: name}}
			m[name] = a
		}
		return a
	}
	for _, r := range rows {
		get(r.from).stat.Sent++
		a := get(r.to)
		a.stat.Received++
		switch State(r.state) {
		case StateDelivered:
			a.stat.Delivered++
		case StateFailed:
			a.stat.Failed++
		case StateQueued:
			a.stat.Queued++
		case StateDelivering:
			a.stat.Delivering++
		}
		if r.hasLatency && State(r.state) == StateDelivered {
			a.latencies = append(a.latencies, r.latencyMs)
		}
	}
	out := make([]AgentStat, 0, len(m))
	for _, a := range m {
		a.stat.P50LatencyMs = percentile(a.latencies, 50)
		a.stat.P95LatencyMs = percentile(a.latencies, 95)
		out = append(out, a.stat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out, nil
}

// StatsTopPairs returns the top-N sender→recipient pairs by message count
// (descending; ties broken by from then to for determinism), each with its
// median delivered-latency. limit <= 0 returns all pairs.
func (s *Store) StatsTopPairs(ctx context.Context, w StatsWindow, limit int) ([]PairStat, error) {
	rows, err := s.scanStats(ctx, w)
	if err != nil {
		return nil, err
	}
	type key struct{ from, to string }
	type acc struct {
		count     int
		latencies []int
	}
	m := map[key]*acc{}
	for _, r := range rows {
		k := key{r.from, r.to}
		a := m[k]
		if a == nil {
			a = &acc{}
			m[k] = a
		}
		a.count++
		if r.hasLatency && State(r.state) == StateDelivered {
			a.latencies = append(a.latencies, r.latencyMs)
		}
	}
	out := make([]PairStat, 0, len(m))
	for k, a := range m {
		out = append(out, PairStat{From: k.from, To: k.to, Count: a.count, P50LatencyMs: percentile(a.latencies, 50)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// StatsTotals returns the window-wide aggregate across all agents.
func (s *Store) StatsTotals(ctx context.Context, w StatsWindow) (Totals, error) {
	rows, err := s.scanStats(ctx, w)
	if err != nil {
		return Totals{}, err
	}
	var t Totals
	for _, r := range rows {
		t.Total++
		switch State(r.state) {
		case StateDelivered:
			t.Delivered++
		case StateFailed:
			t.Failed++
		case StateQueued:
			t.Queued++
		case StateDelivering:
			t.Delivering++
		}
	}
	return t, nil
}

// MessagesInWindow returns every message row created within w, ordered by id
// ASC. It reuses the #147 whereSince window-bounding seam so digest (#161) and
// stats share one definition of "in this window"; unlike scanStats (which
// reduces each row to the aggregate-only fields) this returns full Message
// rows, because thread-structure analysis needs reply_to / public_id / kind /
// no_reply_expected. Scale is the same on-demand, retention-bounded query
// stats runs, so loading the window into memory is acceptable.
func (s *Store) MessagesInWindow(ctx context.Context, w StatsWindow) ([]Message, error) {
	clause, args := w.whereSince()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
		        no_reply_expected, state, created_at, delivered_at, error, replay_of, replay_of_at
		 FROM messages WHERE `+clause+` ORDER BY id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var nre int
		if err := rows.Scan(
			&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
			&nre, &m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error, &m.ReplayOf, &m.ReplayOfAt); err != nil {
			return nil, err
		}
		m.NoReplyExpected = nre != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// VerificationCounts splits delivered messages by their #169 `verified` bit.
// Verified = confirmed (verify-token observed); Unverified = delivered_unverified
// soft-fail; Unknown = delivered before the marker existed (verified IS NULL) —
// never retroactively guessed. Only state='delivered' rows are counted; the
// three buckets sum to the total delivered count in the window.
type VerificationCounts struct {
	Verified   int
	Unverified int
	Unknown    int
}

// DeliveredVerificationCounts splits delivered messages in the window by their
// verified bit (#169). This is the DB-only seam #147 (`stats`) re-consumes for
// the verified/unverified breakdown it previously had to leave to journal
// scraping. The window bounds on created_at, matching the other StatsWindow
// aggregates (so a stats run reports a consistent denominator across counts).
func (s *Store) DeliveredVerificationCounts(ctx context.Context, w StatsWindow) (VerificationCounts, error) {
	clause, args := w.whereSince()
	q := `SELECT verified, COUNT(*) FROM messages
	      WHERE state = ? AND ` + clause + `
	      GROUP BY verified`
	rows, err := s.db.QueryContext(ctx, q, append([]any{StateDelivered}, args...)...)
	if err != nil {
		return VerificationCounts{}, err
	}
	defer rows.Close()

	var vc VerificationCounts
	for rows.Next() {
		var verified sql.NullInt64
		var n int
		if err := rows.Scan(&verified, &n); err != nil {
			return VerificationCounts{}, err
		}
		switch {
		case !verified.Valid:
			vc.Unknown += n
		case verified.Int64 == 1:
			vc.Verified += n
		default:
			vc.Unverified += n
		}
	}
	return vc, rows.Err()
}

// percentile returns the p-th percentile (nearest-rank) of vs in ms, or 0 for
// an empty slice. Mirrors internal/healthscan.percentileMs's nearest-rank
// convention so the two latency surfaces agree.
func percentile(vs []int, p int) int {
	if len(vs) == 0 {
		return 0
	}
	sorted := make([]int, len(vs))
	copy(sorted, vs)
	sort.Ints(sorted)
	// nearest-rank: index = ceil(p/100 * N) - 1, clamped.
	idx := (p*len(sorted) + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}
