package store

import (
	"context"
	"time"
)

// observedStateWorking is the agents.observed_state value that counts toward the
// #448 per-provider concurrency cap. It mirrors tmuxio.State.String() for
// StateWorking; kept as a store-local const so the cap query doesn't import
// internal/tmuxio (the mailman writes the string, the store compares it).
const observedStateWorking = "working"

// SetProvider records the agent's adapter-declared upstream LLM provider
// (#448), written once at serve start. The per-provider cap scopes its
// working-count to this value. Additive: an agent whose adapter declares no
// provider keeps the empty default and is never gated or counted.
func (s *Store) SetProvider(ctx context.Context, agent, provider string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET provider = ? WHERE name = ?`, provider, agent)
	return err
}

// SetObservedState records the mailman's most recent live AgentState
// observation of its own pane (#448), stamped with observedAt so a crashed
// mailman's stale "working" ages out of the cap count. Called on a throttled
// cadence from the serve loop (not every iteration — bounds the extra
// capture-pane probes the cross-mailman count needs).
func (s *Store) SetObservedState(ctx context.Context, agent, state string, observedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET observed_state = ?, observed_state_at = ? WHERE name = ?`,
		state, observedAt.UTC().Format(sqliteTimeFormat), agent)
	return err
}

// CountWorkingOnProvider returns how many agents on the given provider have a
// fresh observed_state of "working" (#448) — the concurrency the cap gates
// against. "Fresh" means observed_state_at is within ttl of now: a mailman that
// crashed mid-work stops refreshing its row, so its stale "working" ages out
// and stops pinning a slot. provider == "" returns 0 (the opt-out path).
//
// now is passed in (not read from the clock) so the TTL boundary is unit-
// testable deterministically.
func (s *Store) CountWorkingOnProvider(ctx context.Context, provider string, ttl time.Duration, now time.Time) (int, error) {
	if provider == "" {
		return 0, nil
	}
	cutoff := now.UTC().Add(-ttl).Format(sqliteTimeFormat)
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents
		 WHERE provider = ? AND observed_state = ? AND observed_state_at > ?`,
		provider, observedStateWorking, cutoff).Scan(&n)
	return n, err
}
