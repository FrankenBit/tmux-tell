package store

import (
	"context"
	"database/sql"
	"errors"
)

// Presence keys. Today there is only one — the operator-last-seen slot
// (#228). Defined as a const so callers don't typo the key string.
const (
	// PresenceKeyOperatorLastSeenIn is the substrate's record of which
	// chamber pane the operator was most recently observed attached to.
	// Updated whenever the substrate observes operator-attached-to-
	// chamber-X (via the tmuxio active-client poll). Read by the
	// send-to-operator routing path to resolve "operator" → chamber name.
	PresenceKeyOperatorLastSeenIn = "operator.last_seen_in"
)

// SetPresence upserts a value into the single-key presence slot (#228).
// Empty key is rejected; empty value is allowed (an explicit "clear"
// gesture), though callers typically delete via DeletePresence instead.
func (s *Store) SetPresence(ctx context.Context, key, value string) error {
	if key == "" {
		return errors.New("store: presence key required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO presence (key, value, updated_at)
		 VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		 ON CONFLICT(key) DO UPDATE SET
		   value = excluded.value,
		   updated_at = excluded.updated_at`,
		key, value)
	return err
}

// GetPresence returns the value at the named key. Returns ErrNotFound
// when the key has never been set; callers distinguish that from an
// empty value via the error.
func (s *Store) GetPresence(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM presence WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return value, err
}
