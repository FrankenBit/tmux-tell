package store

import (
	"context"
	"fmt"
	"strings"
)

// DeleteMessages removes messages matching the given states, optionally
// scoped to one recipient. It returns the count of rows deleted.
//
// The function is used by the `reset` subcommand. `agents` table rows are
// never touched.
func (s *Store) DeleteMessages(ctx context.Context, toAgent string, states []State) (int64, error) {
	if len(states) == 0 {
		return 0, fmt.Errorf("store: at least one state required")
	}
	// Build the IN (?, ?, ?) placeholder list dynamically.
	placeholders := make([]string, len(states))
	args := make([]any, 0, len(states)+1)
	for i, st := range states {
		placeholders[i] = "?"
		args = append(args, st)
	}
	q := fmt.Sprintf(`DELETE FROM messages WHERE state IN (%s)`,
		strings.Join(placeholders, ","))
	if toAgent != "" {
		q += " AND to_agent = ?"
		args = append(args, toAgent)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("store: delete messages: %w", err)
	}
	return res.RowsAffected()
}

// DeleteMessagesBefore removes messages whose created_at is strictly older than
// cutoff and whose state matches one of the provided states, optionally scoped
// to one recipient (empty toAgent = all agents). Returns the count deleted.
// Used by `reset --older-than`.
//
// created_at is stored as ISO8601 UTC ('%Y-%m-%dT%H:%M:%fZ'), which sorts
// lexicographically — so a cutoff string in the same format compares
// correctly with a plain `<`.
func (s *Store) DeleteMessagesBefore(ctx context.Context, toAgent, cutoff string, states []State) (int64, error) {
	if cutoff == "" {
		return 0, fmt.Errorf("store: cutoff required")
	}
	if len(states) == 0 {
		return 0, fmt.Errorf("store: at least one state required")
	}
	placeholders := make([]string, len(states))
	args := make([]any, 0, 1+len(states)+1)
	args = append(args, cutoff)
	for i, st := range states {
		placeholders[i] = "?"
		args = append(args, st)
	}
	q := fmt.Sprintf(`DELETE FROM messages WHERE created_at < ? AND state IN (%s)`,
		strings.Join(placeholders, ","))
	if toAgent != "" {
		q += " AND to_agent = ?"
		args = append(args, toAgent)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("store: delete messages before: %w", err)
	}
	return res.RowsAffected()
}

// DeleteStrandedDraftsBefore removes stranded_draft bookmark rows whose
// created_at is strictly older than cutoff, optionally scoped to one
// recipient (empty toAgent = all agents). Returns the count deleted.
// Used by `claude-msg stranded prune --older-than`.
//
// created_at is stored as ISO8601 UTC ('%Y-%m-%dT%H:%M:%fZ'), which sorts
// lexicographically — so a cutoff string in the same format compares
// correctly with a plain `<`.
func (s *Store) DeleteStrandedDraftsBefore(ctx context.Context, toAgent, cutoff string) (int64, error) {
	if cutoff == "" {
		return 0, fmt.Errorf("store: cutoff required")
	}
	q := `DELETE FROM messages WHERE kind = ? AND created_at < ?`
	args := []any{KindStrandedDraft, cutoff}
	if toAgent != "" {
		q += " AND to_agent = ?"
		args = append(args, toAgent)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("store: prune stranded: %w", err)
	}
	return res.RowsAffected()
}
