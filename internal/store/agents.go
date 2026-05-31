package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
		a       Agent
		pane    sql.NullString
		paused  int
		aliases string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT name, pane_id, paused, updated_at, aliases FROM agents WHERE name = ?`,
		name).Scan(&a.Name, &pane, &paused, &a.UpdatedAt, &aliases)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if pane.Valid {
		a.PaneID = pane.String
	}
	a.Paused = paused != 0
	a.Aliases = decodeAliases(aliases)
	return &a, nil
}

// ListAgents returns every registered agent, ordered by name ASC.
func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, pane_id, paused, updated_at, aliases FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Agent
	for rows.Next() {
		var (
			a       Agent
			pane    sql.NullString
			paused  int
			aliases string
		)
		if err := rows.Scan(&a.Name, &pane, &paused, &a.UpdatedAt, &aliases); err != nil {
			return nil, err
		}
		if pane.Valid {
			a.PaneID = pane.String
		}
		a.Paused = paused != 0
		a.Aliases = decodeAliases(aliases)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ErrAliasCollision is returned by SetAliases/AddAlias when the
// requested alias is already claimed by another canonical agent (as
// that agent's name or as one of its aliases). Surveyor #38-Q(a)
// review: catch collisions at registration time so the resolver
// never has to choose between two canonicals at delivery time.
var ErrAliasCollision = errors.New("store: alias collides with an existing canonical agent")

// SetAliases replaces the alias list for an agent. Empty slice removes
// all aliases. Returns ErrNotFound if no agent with that name exists,
// or ErrAliasCollision if any requested alias is already claimed by
// another agent.
func (s *Store) SetAliases(ctx context.Context, name string, aliases []string) error {
	if err := s.checkAliasCollisions(ctx, name, aliases); err != nil {
		return err
	}
	encoded, err := encodeAliases(aliases)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents
		 SET aliases = ?,
		     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE name = ?`,
		encoded, name)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: agent %q: %w", name, ErrNotFound)
	}
	return nil
}

// AddAlias appends an alias to the agent's list (idempotent — duplicate
// aliases on the SAME agent are silently ignored). Returns ErrNotFound
// for missing agents, ErrAliasCollision if another canonical agent
// already claims the alias.
func (s *Store) AddAlias(ctx context.Context, name, alias string) error {
	a, err := s.GetAgent(ctx, name)
	if err != nil {
		return err
	}
	for _, existing := range a.Aliases {
		if existing == alias {
			return nil
		}
	}
	return s.SetAliases(ctx, name, append(a.Aliases, alias))
}

// checkAliasCollisions returns ErrAliasCollision if any of `aliases`
// collides with another agent's canonical name OR with one of another
// agent's aliases. Self-collisions (the agent's own name/aliases) are
// allowed — that's just rebinding.
func (s *Store) checkAliasCollisions(ctx context.Context, name string, aliases []string) error {
	if len(aliases) == 0 {
		return nil
	}
	all, err := s.ListAgents(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range aliases {
		for _, other := range all {
			if other.Name == name {
				continue // self
			}
			if other.Name == candidate {
				return fmt.Errorf("%w: alias %q is the canonical name of agent %q",
					ErrAliasCollision, candidate, other.Name)
			}
			for _, otherAlias := range other.Aliases {
				if otherAlias == candidate {
					return fmt.Errorf("%w: alias %q is already an alias of agent %q",
						ErrAliasCollision, candidate, other.Name)
				}
			}
		}
	}
	return nil
}

func decodeAliases(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// Corrupted aliases column shouldn't break callers; treat as
		// empty so the rest of the row is usable.
		return nil
	}
	return out
}

func encodeAliases(aliases []string) (string, error) {
	if len(aliases) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(aliases)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
