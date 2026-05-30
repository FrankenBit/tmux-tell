package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// InsertParams is the input to InsertMessage. ReplyTo may be empty for new
// (non-reply) threads. Kind defaults to KindMessage when empty.
//
// MaxRecipientQueue and MaxSenderBacklog opt into in-transaction cap
// enforcement. Zero disables that cap (the store inserts unconditionally).
// Non-zero gives the caller a hard ceiling: the cap is checked inside the
// same BEGIN IMMEDIATE transaction that does the INSERT, so two concurrent
// senders to the same recipient can no longer race past it (#29).
type InsertParams struct {
	FromAgent string
	ToAgent   string
	ReplyTo   string
	Body      string
	Kind      Kind

	MaxRecipientQueue int // 0 = no cap check
	MaxSenderBacklog  int // 0 = no cap check
}

// InsertResult is the output of InsertMessage. Queued is the recipient's
// queue depth *after* the insert.
type InsertResult struct {
	PublicID string
	Queued   int
}

// ErrRecipientQueueFull is returned by InsertMessage / InsertMessagePair
// when MaxRecipientQueue > 0 and the in-transaction depth check would
// be violated. The error message carries the agent name + observed
// depth + cap so the caller can surface a precise reason.
var ErrRecipientQueueFull = errors.New("store: recipient queue full")

// ErrSenderBacklogFull is the symmetric sentinel for the from-side cap.
var ErrSenderBacklogFull = errors.New("store: sender backlog full")

// publicIDRetryAttempts caps the collision-retry loop in InsertMessage.
// At 4 hex chars (~65 K namespace) and a few thousand outstanding rows,
// 20 attempts is comfortably overkill.
const publicIDRetryAttempts = 20

// InsertMessage inserts a queued message and returns its assigned public_id
// plus the recipient's new queue depth. Public IDs are generated server-side
// with collision retry.
//
// When p.MaxRecipientQueue or p.MaxSenderBacklog are non-zero, the cap
// is enforced inside the BEGIN IMMEDIATE transaction — the SELECT
// COUNT(*) and the INSERT see consistent data, and concurrent writers
// from other connections can't race past the cap (resolved by #29).
// Cross-process write contention is bounded by the busy_timeout PRAGMA
// configured in Open().
func (s *Store) InsertMessage(ctx context.Context, p InsertParams) (InsertResult, error) {
	if err := validateInsertParams(p); err != nil {
		return InsertResult{}, err
	}
	if err := s.validateReplyTo(ctx, p.ReplyTo); err != nil {
		return InsertResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InsertResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := checkCapsInTx(ctx, tx, p, 1); err != nil {
		return InsertResult{}, err
	}

	res, err := insertOneInTx(ctx, tx, p)
	if err != nil {
		return InsertResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return InsertResult{}, err
	}
	return res, nil
}

// InsertMessagePair inserts two messages in a single BEGIN IMMEDIATE
// transaction. Both rows land or neither does — the atomicity guarantee
// the restart macro and resume_with sugar need so neither can leave the
// recipient half-actioned (e.g. /mcp disable without the matching
// enable, or /compact without the follow-up).
//
// Caps are checked once for +2 slots up front, so the call fails fast
// without inserting either row when the budget can't accommodate both.
// p1 and p2 must share FromAgent and ToAgent — the call paths that use
// this (restart, resume_with) always do.
//
// linkP2ToP1 controls the audit thread between the two rows. When
// true, the function sets p2.ReplyTo to p1's assigned public_id; this
// gives the two rows the same thread relationship the existing callers
// established before #29.
//
// Precondition: when linkP2ToP1 is true, p2.ReplyTo MUST be empty. The
// call returns an error otherwise — passing both is a caller bug we
// surface rather than silently overwrite (Surveyor #29 review).
func (s *Store) InsertMessagePair(ctx context.Context, p1, p2 InsertParams, linkP2ToP1 bool) (InsertResult, InsertResult, error) {
	if p1.FromAgent != p2.FromAgent || p1.ToAgent != p2.ToAgent {
		return InsertResult{}, InsertResult{}, errors.New("store: pair must share from/to")
	}
	if linkP2ToP1 && p2.ReplyTo != "" {
		return InsertResult{}, InsertResult{}, errors.New(
			"store: linkP2ToP1 requires p2.ReplyTo to be empty (caller is asking the store to set it from p1)")
	}
	if err := validateInsertParams(p1); err != nil {
		return InsertResult{}, InsertResult{}, err
	}
	if err := validateInsertParams(p2); err != nil {
		return InsertResult{}, InsertResult{}, err
	}
	// reply_to on p1 must still resolve. On p2 we either link to p1
	// (no pre-validation needed) or honour the caller's explicit value.
	if err := s.validateReplyTo(ctx, p1.ReplyTo); err != nil {
		return InsertResult{}, InsertResult{}, err
	}
	if !linkP2ToP1 {
		if err := s.validateReplyTo(ctx, p2.ReplyTo); err != nil {
			return InsertResult{}, InsertResult{}, err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InsertResult{}, InsertResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := checkCapsInTx(ctx, tx, p1, 2); err != nil {
		return InsertResult{}, InsertResult{}, err
	}

	res1, err := insertOneInTx(ctx, tx, p1)
	if err != nil {
		return InsertResult{}, InsertResult{}, err
	}
	if linkP2ToP1 {
		p2.ReplyTo = res1.PublicID
	}
	res2, err := insertOneInTx(ctx, tx, p2)
	if err != nil {
		return InsertResult{}, InsertResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return InsertResult{}, InsertResult{}, err
	}
	return res1, res2, nil
}

// validateInsertParams pulls the shape checks out so InsertMessage and
// InsertMessagePair don't drift.
func validateInsertParams(p InsertParams) error {
	if p.Body == "" {
		return errors.New("store: body must be non-empty")
	}
	if p.FromAgent == "" || p.ToAgent == "" {
		return errors.New("store: from and to are required")
	}
	return nil
}

// validateReplyTo is a thin wrapper over the existence-check pattern so
// both single and pair inserts share the wording on ErrNotFound.
func (s *Store) validateReplyTo(ctx context.Context, replyTo string) error {
	if replyTo == "" {
		return nil
	}
	var dummy int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM messages WHERE public_id = ? LIMIT 1`,
		replyTo).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: reply_to %q: %w", replyTo, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("store: validate reply_to: %w", err)
	}
	return nil
}

// checkCapsInTx enforces the recipient-queue and sender-backlog caps
// inside an existing transaction. addedRows is how many rows the
// caller is about to insert (1 for InsertMessage, 2 for
// InsertMessagePair); the cap is satisfied when (current_depth +
// addedRows) ≤ cap.
//
// Atomicity guarantee: the BEGIN IMMEDIATE tx (from the _txlock=immediate
// DSN parameter set in Open) has held the RESERVED lock since BEGIN, so
// the COUNT(*) reads here are consistent with the subsequent INSERTs in
// the same transaction.
func checkCapsInTx(ctx context.Context, tx *sql.Tx, p InsertParams, addedRows int) error {
	if p.MaxRecipientQueue > 0 {
		var depth int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM messages WHERE to_agent = ? AND state = ?`,
			p.ToAgent, StateQueued).Scan(&depth); err != nil {
			return fmt.Errorf("store: cap check recipient: %w", err)
		}
		if depth+addedRows > p.MaxRecipientQueue {
			return fmt.Errorf("%w: %s (%d/%d, need %d slot(s))",
				ErrRecipientQueueFull, p.ToAgent, depth, p.MaxRecipientQueue, addedRows)
		}
	}
	if p.MaxSenderBacklog > 0 {
		var backlog int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM messages WHERE from_agent = ? AND state = ?`,
			p.FromAgent, StateQueued).Scan(&backlog); err != nil {
			return fmt.Errorf("store: cap check sender: %w", err)
		}
		if backlog+addedRows > p.MaxSenderBacklog {
			return fmt.Errorf("%w: %s (%d/%d, need %d slot(s))",
				ErrSenderBacklogFull, p.FromAgent, backlog, p.MaxSenderBacklog, addedRows)
		}
	}
	return nil
}

// insertOneInTx is the shared INSERT-with-collision-retry helper used
// by InsertMessage and InsertMessagePair. It assumes the caller has
// already opened the transaction and performed cap checks.
func insertOneInTx(ctx context.Context, tx *sql.Tx, p InsertParams) (InsertResult, error) {
	var replyToArg any
	if p.ReplyTo != "" {
		replyToArg = p.ReplyTo
	}
	kind := p.Kind
	if kind == "" {
		kind = KindMessage
	}

	var publicID string
	for i := 0; i < publicIDRetryAttempts; i++ {
		candidate, err := generatePublicID()
		if err != nil {
			return InsertResult{}, fmt.Errorf("store: generate public_id: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO messages (public_id, from_agent, to_agent, reply_to, body, kind)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			candidate, p.FromAgent, p.ToAgent, replyToArg, p.Body, kind)
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
		`SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
		        state, created_at, delivered_at, error
		 FROM messages
		 WHERE to_agent = ? AND state = ?
		 ORDER BY id
		 LIMIT 1`,
		toAgent, StateQueued).Scan(
		&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
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
		`SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
		        state, created_at, delivered_at, error
		 FROM messages WHERE public_id = ?`,
		publicID).Scan(
		&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
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

	q := `SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
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
			&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
			&m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
