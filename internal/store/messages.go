package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// InsertParams is the input to InsertMessage. ReplyTo may be empty for new
// (non-reply) threads.
type InsertParams struct {
	FromAgent string
	ToAgent   string
	ReplyTo   string
	Body      string
}

// InsertResult is the output of InsertMessage. Queued is the recipient's
// queue depth *after* the insert.
type InsertResult struct {
	PublicID string
	Queued   int
}

// publicIDRetryAttempts caps the collision-retry loop in InsertMessage.
// At 4 hex chars (~65 K namespace) and a few thousand outstanding rows,
// 20 attempts is comfortably overkill.
const publicIDRetryAttempts = 20

// InsertMessage inserts a queued message and returns its assigned public_id
// plus the recipient's new queue depth. Public IDs are generated server-side
// with collision retry.
//
// Cap *enforcement* (queue depth limits, body size limit) is the caller's
// responsibility (#3); InsertMessage only insists on schema invariants
// (non-empty fields, reply_to references an existing message).
func (s *Store) InsertMessage(ctx context.Context, p InsertParams) (InsertResult, error) {
	if p.Body == "" {
		return InsertResult{}, errors.New("store: body must be non-empty")
	}
	if p.FromAgent == "" || p.ToAgent == "" {
		return InsertResult{}, errors.New("store: from and to are required")
	}
	if p.ReplyTo != "" {
		var dummy int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM messages WHERE public_id = ? LIMIT 1`,
			p.ReplyTo).Scan(&dummy)
		if errors.Is(err, sql.ErrNoRows) {
			return InsertResult{}, fmt.Errorf("store: reply_to %q: %w", p.ReplyTo, ErrNotFound)
		} else if err != nil {
			return InsertResult{}, fmt.Errorf("store: validate reply_to: %w", err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InsertResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var replyToArg any
	if p.ReplyTo != "" {
		replyToArg = p.ReplyTo
	}

	var publicID string
	for i := 0; i < publicIDRetryAttempts; i++ {
		candidate, err := generatePublicID()
		if err != nil {
			return InsertResult{}, fmt.Errorf("store: generate public_id: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO messages (public_id, from_agent, to_agent, reply_to, body)
			 VALUES (?, ?, ?, ?, ?)`,
			candidate, p.FromAgent, p.ToAgent, replyToArg, p.Body)
		if err == nil {
			publicID = candidate
			break
		}
		// SQLite returns "UNIQUE constraint failed: messages.public_id"
		// on collision. Retry transparently.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") &&
			strings.Contains(err.Error(), "public_id") {
			continue
		}
		return InsertResult{}, fmt.Errorf("store: insert message: %w", err)
	}
	if publicID == "" {
		return InsertResult{}, fmt.Errorf("store: exhausted %d public_id retries", publicIDRetryAttempts)
	}

	var queued int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE to_agent = ? AND state = ?`,
		p.ToAgent, StateQueued).Scan(&queued); err != nil {
		return InsertResult{}, fmt.Errorf("store: queue depth: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return InsertResult{}, err
	}
	return InsertResult{PublicID: publicID, Queued: queued}, nil
}

// ClaimNext atomically transitions the oldest queued message for toAgent
// from 'queued' to 'delivering' and returns it. Returns nil (no error) if
// no queued message is available — the mailman loop's idle case.
func (s *Store) ClaimNext(ctx context.Context, toAgent string) (*Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var m Message
	err = tx.QueryRowContext(ctx,
		`SELECT id, public_id, from_agent, to_agent, reply_to, body,
		        state, created_at, delivered_at, error
		 FROM messages
		 WHERE to_agent = ? AND state = ?
		 ORDER BY id
		 LIMIT 1`,
		toAgent, StateQueued).Scan(
		&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body,
		&m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("store: select queued: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET state = ? WHERE id = ?`,
		StateDelivering, m.ID); err != nil {
		return nil, fmt.Errorf("store: mark delivering: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.State = StateDelivering
	return &m, nil
}

// MarkDelivered transitions a delivering message to 'delivered' and stamps
// delivered_at. Returns ErrNotFound if no row matches (e.g. the message
// was reset by RecoverDelivering between Claim and Mark).
func (s *Store) MarkDelivered(ctx context.Context, publicID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages
		 SET state = ?,
		     delivered_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE public_id = ? AND state = ?`,
		StateDelivered, publicID, StateDelivering)
	if err != nil {
		return fmt.Errorf("store: mark delivered: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: mark delivered %q: %w", publicID, ErrNotFound)
	}
	return nil
}

// MarkFailed transitions a delivering message to 'failed', recording the
// reason in the error column. Same not-found semantics as MarkDelivered.
func (s *Store) MarkFailed(ctx context.Context, publicID, reason string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages
		 SET state = ?, error = ?,
		     delivered_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE public_id = ? AND state = ?`,
		StateFailed, reason, publicID, StateDelivering)
	if err != nil {
		return fmt.Errorf("store: mark failed: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: mark failed %q: %w", publicID, ErrNotFound)
	}
	return nil
}

// RecoverDelivering resets every 'delivering' message for toAgent back to
// 'queued'. Called at mailman startup so messages that were in flight when
// the daemon crashed are retried. Returns the number of rows recovered.
func (s *Store) RecoverDelivering(ctx context.Context, toAgent string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET state = ?
		 WHERE to_agent = ? AND state = ?`,
		StateQueued, toAgent, StateDelivering)
	if err != nil {
		return 0, fmt.Errorf("store: recover delivering: %w", err)
	}
	return res.RowsAffected()
}

// RecipientQueueDepth returns the number of queued messages addressed to
// toAgent. Used by send-side cap enforcement (#3).
func (s *Store) RecipientQueueDepth(ctx context.Context, toAgent string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE to_agent = ? AND state = ?`,
		toAgent, StateQueued).Scan(&n)
	return n, err
}

// SenderBacklog returns the number of queued messages originated by
// fromAgent across all recipients. Used by send-side cap enforcement (#3).
func (s *Store) SenderBacklog(ctx context.Context, fromAgent string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE from_agent = ? AND state = ?`,
		fromAgent, StateQueued).Scan(&n)
	return n, err
}

// GetMessage returns one message by its public_id, or ErrNotFound.
func (s *Store) GetMessage(ctx context.Context, publicID string) (*Message, error) {
	var m Message
	err := s.db.QueryRowContext(ctx,
		`SELECT id, public_id, from_agent, to_agent, reply_to, body,
		        state, created_at, delivered_at, error
		 FROM messages WHERE public_id = ?`,
		publicID).Scan(
		&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body,
		&m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListFilter narrows the rows returned by ListMessages. Zero-value fields
// mean "no filter on that column".
type ListFilter struct {
	ToAgent   string
	FromAgent string
	State     State
	Limit     int // 0 → 100; capped at 1000.
}

// ListMessages returns messages matching the filter, ordered by id ASC.
func (s *Store) ListMessages(ctx context.Context, f ListFilter) ([]Message, error) {
	var (
		wheres []string
		args   []any
	)
	if f.ToAgent != "" {
		wheres = append(wheres, "to_agent = ?")
		args = append(args, f.ToAgent)
	}
	if f.FromAgent != "" {
		wheres = append(wheres, "from_agent = ?")
		args = append(args, f.FromAgent)
	}
	if f.State != "" {
		wheres = append(wheres, "state = ?")
		args = append(args, f.State)
	}
	switch {
	case f.Limit <= 0:
		f.Limit = 100
	case f.Limit > 1000:
		f.Limit = 1000
	}
	args = append(args, f.Limit)

	q := `SELECT id, public_id, from_agent, to_agent, reply_to, body,
	             state, created_at, delivered_at, error
	      FROM messages`
	if len(wheres) > 0 {
		q += " WHERE " + strings.Join(wheres, " AND ")
	}
	q += " ORDER BY id ASC LIMIT ?"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(
			&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body,
			&m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
