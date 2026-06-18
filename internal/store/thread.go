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
			        no_reply_expected, state, created_at, delivered_at, error, replay_of, replay_of_at
			 FROM messages WHERE reply_to = ?
			 ORDER BY id ASC`, current)
		if err != nil {
			return nil, fmt.Errorf("store: thread children: %w", err)
		}
		var children []Message
		for rows.Next() {
			var m Message
			var nre int
			if err := rows.Scan(
				&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
				&nre, &m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error, &m.ReplayOf, &m.ReplayOfAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			m.NoReplyExpected = nre != 0
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
		// Honor cancellation at the loop top (#496) — uniform with every other
		// long-running loop in the substrate. The QueryRowContext below already
		// respects ctx, but the explicit check catches a cancelled context before
		// the DB round-trip and keeps the "every loop checks ctx.Done() near the
		// top" invariant gap-free.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if visited[current] {
			return nil, fmt.Errorf("store: cycle detected at %s", current)
		}
		visited[current] = true

		var m Message
		var nre int
		err := s.db.QueryRowContext(ctx,
			`SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
			        no_reply_expected, state, created_at, delivered_at, error, replay_of, replay_of_at
			 FROM messages WHERE public_id = ?`, current).Scan(
			&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
			&nre, &m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error, &m.ReplayOf, &m.ReplayOfAt)
		m.NoReplyExpected = nre != 0
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
