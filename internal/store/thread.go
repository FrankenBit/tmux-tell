package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// GetThread returns every message in the same reply chain as anyID, in
// chronological order (ascending id).
//
// "Same chain" = same root: walk reply_to backwards from anyID until we
// find a message with no reply_to (the root); then DFS forward over
// children (rows where reply_to = currentNode.public_id).
//
// Cycle-safe: a visited set prevents loops even though reply_to is
// supposed to be a once-set DAG edge.
func (s *Store) GetThread(ctx context.Context, anyID string) ([]Message, error) {
	// 1. Find the root by walking reply_to backwards.
	root, err := s.findThreadRoot(ctx, anyID)
	if err != nil {
		return nil, err
	}

	// 2. BFS forward from root, collecting all descendants.
	visited := map[string]bool{root.PublicID: true}
	out := []Message{*root}
	queue := []string{root.PublicID}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		rows, err := s.db.QueryContext(ctx,
			`SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
			        state, created_at, delivered_at, error
			 FROM messages WHERE reply_to = ?
			 ORDER BY id ASC`, current)
		if err != nil {
			return nil, fmt.Errorf("store: thread children: %w", err)
		}
		var children []Message
		for rows.Next() {
			var m Message
			if err := rows.Scan(
				&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
				&m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error); err != nil {
				rows.Close()
				return nil, err
			}
			children = append(children, m)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		for _, c := range children {
			if visited[c.PublicID] {
				continue
			}
			visited[c.PublicID] = true
			out = append(out, c)
			queue = append(queue, c.PublicID)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) findThreadRoot(ctx context.Context, startID string) (*Message, error) {
	current := startID
	visited := map[string]bool{}
	for {
		if visited[current] {
			return nil, fmt.Errorf("store: cycle detected at %s", current)
		}
		visited[current] = true

		var m Message
		err := s.db.QueryRowContext(ctx,
			`SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
			        state, created_at, delivered_at, error
			 FROM messages WHERE public_id = ?`, current).Scan(
			&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
			&m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		} else if err != nil {
			return nil, err
		}
		if !m.ReplyTo.Valid || m.ReplyTo.String == "" {
			return &m, nil
		}
		current = m.ReplyTo.String
	}
}
