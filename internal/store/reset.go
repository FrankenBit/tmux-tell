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
