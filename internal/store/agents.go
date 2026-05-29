package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// UpsertAgent creates or updates an agent registry entry. If paneID is
// empty the existing pane_id (if any) is preserved — useful for `pause`
// or other operations that shouldn't touch discovery state.
func (s *Store) UpsertAgent(ctx context.Context, name, paneID string) error {
	if name == "" {
		return errors.New("store: agent name required")
	}

	if paneID == "" {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO agents (name, pane_id, updated_at)
			 VALUES (?, NULL, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			 ON CONFLICT(name) DO UPDATE SET
			   updated_at = excluded.updated_at`,
			name)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (name, pane_id, updated_at)
		 VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		 ON CONFLICT(name) DO UPDATE SET
		   pane_id = excluded.pane_id,
		   updated_at = excluded.updated_at`,
		name, paneID)
	return err
}

// SetPaused updates the paused flag for an existing agent. Returns
// ErrNotFound if no agent with that name is registered.
func (s *Store) SetPaused(ctx context.Context, name string, paused bool) error {
	p := 0
	if paused {
		p = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents
		 SET paused = ?,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE name = ?`,
		p, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// SetPausedAll flips the paused flag for every registered agent in one
// transaction. Returns the number of rows touched.
func (s *Store) SetPausedAll(ctx context.Context, paused bool) (int64, error) {
	p := 0
	if paused {
		p = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents
		 SET paused = ?,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		p)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetAgent returns the agent by name, or ErrNotFound.
func (s *Store) GetAgent(ctx context.Context, name string) (*Agent, error) {
	var (
		a      Agent
		pane   sql.NullString
		paused int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT name, pane_id, paused, updated_at FROM agents WHERE name = ?`,
		name).Scan(&a.Name, &pane, &paused, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if pane.Valid {
		a.PaneID = pane.String
	}
	a.Paused = paused != 0
	return &a, nil
}

// ListAgents returns every registered agent, ordered by name ASC.
func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, pane_id, paused, updated_at FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Agent
	for rows.Next() {
		var (
			a      Agent
			pane   sql.NullString
			paused int
		)
		if err := rows.Scan(&a.Name, &pane, &paused, &a.UpdatedAt); err != nil {
			return nil, err
		}
		if pane.Valid {
			a.PaneID = pane.String
		}
		a.Paused = paused != 0
		out = append(out, a)
	}
	return out, rows.Err()
}
