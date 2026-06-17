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
	FromAgent       string
	ToAgent         string
	ReplyTo         string
	Body            string
	Kind            Kind
	NoReplyExpected bool // true → sender requests no ack (#145)
	Quick           bool // true → compact single-line chrome on delivery (#154)

	// Replay linkage (#157 PR1). Set by `resend` to mark a message as a
	// replay of an earlier one: ReplayOf is the original public_id,
	// ReplayOfAt the original created_at. Empty on a normal send → both
	// columns stored NULL. The replayed body stays byte-identical to the
	// original (the marker is metadata, not body content) so PR2's
	// body-hash dedupe can match a replay against its original.
	ReplayOf   string
	ReplayOfAt string

	// DeliverAfter, when non-empty, inserts the row in StateDeferred carrying
	// this trigger name (#227) instead of the normal StateQueued. The row is
	// invisible to ClaimNext / inbox / mailman until a flush_deferred call
	// promotes it. A deferred insert bypasses the recipient-queue and
	// sender-backlog caps (it is not in the live queue); InsertMessage forces
	// the cap args to 0 when this is set, so a caller can't accidentally cap a
	// pre-queue row.
	DeliverAfter string

	// ExpectsReply marks the row as an `ask` (#250): the sender intends to
	// wait for a reply (sets the expects_reply column to 1). Pure marker — it
	// does not change delivery; the request-reply wait happens via the
	// reply-query seams (FindReply / ListReplies / WaitForReply). Default
	// false = a normal send.
	//
	// Deliberately NOT threaded through `resend`: a replayed ask is not
	// re-marked, which is correct — wait_for_reply filters on reply_to = the
	// original ask's id, so a marker on the replay would be redundant.
	ExpectsReply bool

	// Priority is the #449 scheduling weight (low=10 / normal=20 / high=30).
	// Zero is normalized to PriorityNormal at insert, so every existing call
	// site that doesn't set it gets the default unchanged.
	Priority int

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
	// #227: a deferred row is not in the live queue, so it neither counts
	// against nor is gated by the recipient-queue / sender-backlog caps.
	// Force the cap args off here so a caller can't accidentally cap a
	// pre-queue row (mirrors InsertNotice's cap-bypass discipline).
	if p.DeliverAfter != "" {
		p.MaxRecipientQueue = 0
		p.MaxSenderBacklog = 0
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

// InsertNotice inserts a system-generated notice message bypassing
// the recipient-queue and sender-backlog caps. Used by the mailman's
// delivery-failure hook (#53) to surface failed deliveries back to
// the original sender even when the recipient's pane is congested.
//
// Operationally-critical signals shouldn't be silently dropped on cap;
// losing a failure-notice because the sender's queue is full would
// defeat the point. The cap-exemption is a deliberate commitment
// worth pinning if the discipline matters across the codebase's life.
//
// The caller is responsible for setting p.Kind to a notice-shaped
// value (typically KindDeliveryFailureNotice). This method does NOT
// validate the kind — the kind discipline lives at the call site,
// not at the store boundary.
func (s *Store) InsertNotice(ctx context.Context, p InsertParams) (InsertResult, error) {
	if err := validateInsertParams(p); err != nil {
		return InsertResult{}, err
	}
	if err := s.validateReplyTo(ctx, p.ReplyTo); err != nil {
		return InsertResult{}, err
	}
	// Force cap-bypass: zero MaxRecipientQueue and MaxSenderBacklog
	// so checkCapsInTx is a no-op. Callers can't accidentally cap
	// notices by passing non-zero values.
	p.MaxRecipientQueue = 0
	p.MaxSenderBacklog = 0

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InsertResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

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
// Both caps are scoped to the destination recipient (#296): the
// recipient-queue cap counts all queued rows for to_agent, the
// sender-backlog cap counts queued rows for the (from_agent, to_agent)
// pair. The sender cap is therefore a per-sender fairness slice of one
// recipient's queue (no single sender may occupy more than
// MaxSenderBacklog of the recipient's MaxRecipientQueue slots), NOT a
// global ceiling on a sender's total outbound — a slow recipient can no
// longer block a sender's traffic to unrelated healthy recipients.
// Both InsertMessagePair rows share to_agent (enforced in
// InsertMessagePair), so the addedRows=2 pair check is well-defined.
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
			`SELECT COUNT(*) FROM messages WHERE from_agent = ? AND to_agent = ? AND state = ?`,
			p.FromAgent, p.ToAgent, StateQueued).Scan(&backlog); err != nil {
			return fmt.Errorf("store: cap check sender: %w", err)
		}
		if backlog+addedRows > p.MaxSenderBacklog {
			return fmt.Errorf("%w: %s→%s (%d/%d, need %d slot(s))",
				ErrSenderBacklogFull, p.FromAgent, p.ToAgent, backlog, p.MaxSenderBacklog, addedRows)
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
	var replayOfArg, replayOfAtArg any
	if p.ReplayOf != "" {
		replayOfArg = p.ReplayOf
	}
	if p.ReplayOfAt != "" {
		replayOfAtArg = p.ReplayOfAt
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
		nre := 0
		if p.NoReplyExpected {
			nre = 1
		}
		q := 0
		if p.Quick {
			q = 1
		}
		// #227: a non-empty DeliverAfter inserts the row in StateDeferred with
		// the trigger name in deliver_after; otherwise the row takes the schema
		// default state ('queued') and a NULL deliver_after.
		state := StateQueued
		var deliverAfterArg any
		if p.DeliverAfter != "" {
			state = StateDeferred
			deliverAfterArg = p.DeliverAfter
		}
		// #250: ask-marker.
		er := 0
		if p.ExpectsReply {
			er = 1
		}
		// #449: priority weight. 0 (the un-set zero value) normalizes to
		// PriorityNormal so existing callers are unchanged.
		priority := p.Priority
		if priority == 0 {
			priority = PriorityNormal
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO messages (public_id, from_agent, to_agent, reply_to, body, kind, no_reply_expected, quick, replay_of, replay_of_at, state, deliver_after, expects_reply, priority)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			candidate, p.FromAgent, p.ToAgent, replyToArg, p.Body, kind, nre, q, replayOfArg, replayOfAtArg, state, deliverAfterArg, er, priority)
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

// ClaimNext atomically transitions the next-to-deliver queued message for
// toAgent from 'queued' to 'delivering' and returns it. Returns nil (no error)
// if no queued message is available — the mailman loop's idle case.
//
// Uses StrategyMaxPriority (the default scheduler, #449): under uniform priority
// it reduces to plain FIFO, so every caller that doesn't care about priority
// (and the 67 existing tests) observe the historical oldest-first behavior. The
// mailman threads its configured strategy via ClaimNextWithStrategy.
func (s *Store) ClaimNext(ctx context.Context, toAgent string) (*Message, error) {
	return s.ClaimNextWithStrategy(ctx, toAgent, StrategyMaxPriority)
}

// ClaimNextWithStrategy is ClaimNext with an explicit cross-channel scheduling
// strategy (#449). It scans the eligible queued rows, lets selectScheduled pick
// which sender-channel's head fires next (within-channel FIFO preserved), and
// claims that row — all inside one BEGIN IMMEDIATE transaction so the pick is
// consistent with the claim.
//
// Cost shape: this materializes every eligible queued row for the recipient
// (id, from_agent, priority) into memory each call, so one claim is O(N) in the
// recipient's queue depth rather than the O(1) "claim the indexed head" a pure
// FIFO would allow. That is intentional and bounded: cross-channel priority
// selection (#449) needs to see all channel heads at once, and a single
// recipient's undelivered backlog is small (the mailman drains continuously;
// depth is the queue-depth gauge, normally low single digits). If a recipient's
// steady-state depth ever grew large enough to matter, the fix is to push the
// per-channel-head reduction into SQL (a window function over from_agent)
// rather than scanning in Go — not a reason to revert to id-only FIFO.
func (s *Store) ClaimNextWithStrategy(ctx context.Context, toAgent string, strategy SchedulerStrategy) (*Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Scan eligible queued candidates — lightweight (id, from_agent, priority).
	// The WHERE preserves the #204 backlog floor + the #227 promoted-deferred
	// exemption exactly as before; only the SELECTION among eligible rows
	// changes (priority-weighted across channels instead of pure id-FIFO). The
	// floor subquery runs inside the IMMEDIATE tx, so the read is consistent
	// with the claim it gates.
	rows, err := tx.QueryContext(ctx,
		`SELECT id, from_agent, priority
		 FROM messages
		 WHERE to_agent = ? AND state = ?
		   AND (deliver_after IS NOT NULL
		        OR id > COALESCE((SELECT backlog_epoch_id FROM agents WHERE name = ?), 0))
		 ORDER BY id`,
		toAgent, StateQueued, toAgent)
	if err != nil {
		return nil, fmt.Errorf("store: scan queued candidates: %w", err)
	}
	var cands []claimCandidate
	for rows.Next() {
		var c claimCandidate
		if err := rows.Scan(&c.ID, &c.FromAgent, &c.Priority); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: scan candidate: %w", err)
		}
		cands = append(cands, c)
	}
	if cerr := rows.Close(); cerr != nil {
		return nil, cerr
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, rerr
	}

	chosenID, ok := selectScheduled(cands, strategy)
	if !ok {
		return nil, nil // idle: nothing eligible
	}

	// Fetch the full chosen row + claim it, same tx (it is still queued — the
	// IMMEDIATE write lock is held, so no other writer changed it since the scan).
	var m Message
	var nre, q, er int
	err = tx.QueryRowContext(ctx,
		`SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
		        no_reply_expected, quick, state, created_at, delivered_at, error, replay_of, replay_of_at, verified, deliver_after, expects_reply, priority
		 FROM messages WHERE id = ?`,
		chosenID).Scan(
		&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
		&nre, &q, &m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error, &m.ReplayOf, &m.ReplayOfAt, &m.Verified, &m.DeliverAfter, &er, &m.Priority)
	if err != nil {
		return nil, fmt.Errorf("store: fetch chosen message: %w", err)
	}
	m.NoReplyExpected = nre != 0
	m.Quick = q != 0
	m.ExpectsReply = er != 0

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

// MarkDelivered transitions a delivering message to 'delivered' (verify-token
// observed) and stamps delivered_at, writing verified=1. Returns ErrNotFound if
// no row matches (e.g. the message was reset by RecoverDelivering between Claim
// and Mark).
func (s *Store) MarkDelivered(ctx context.Context, publicID string) error {
	return s.markDelivered(ctx, publicID, 1)
}

// MarkDeliveredInInputBox transitions a delivering message to 'delivered' but
// records verified=0 — the paste+Enter landed mechanically (the message is in
// the recipient's input box), but the verify token never surfaced in budget
// (#169). The DB state stays `delivered`; only the `verified` bit distinguishes
// it from a confirmed delivery, where previously the sole signal was a mailman
// journal line. Same not-found semantics as MarkDelivered.
func (s *Store) MarkDeliveredInInputBox(ctx context.Context, publicID string) error {
	return s.markDelivered(ctx, publicID, 0)
}

// markDelivered is the shared core: transition delivering → delivered, stamp
// delivered_at, and write the verified bit. The verified value is the only
// difference between the confirmed and unverified paths.
func (s *Store) markDelivered(ctx context.Context, publicID string, verified int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages
		 SET state = ?, verified = ?,
		     delivered_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE public_id = ? AND state = ?`,
		StateDelivered, verified, publicID, StateDelivering)
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

// PromoteDeferred transitions every StateDeferred row addressed to toAgent
// whose deliver_after matches trigger to StateQueued, returning the count
// promoted (#227). It is the store half of flush_deferred: a promoted row,
// now queued, becomes eligible for the mailman's eager delivery on its next
// loop. Idempotent — a second call with no remaining matching deferred rows
// returns (0, nil), not an error (the `state = deferred` guard means an
// already-promoted row is never re-touched). Scoped to toAgent; the handler
// enforces that the caller may only flush messages addressed to itself.
func (s *Store) PromoteDeferred(ctx context.Context, toAgent, trigger string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET state = ?
		 WHERE to_agent = ? AND state = ? AND deliver_after = ?`,
		StateQueued, toAgent, StateDeferred, trigger)
	if err != nil {
		return 0, fmt.Errorf("store: promote deferred: %w", err)
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

// RecipientLastDelivered returns the timestamp (RFC3339) of the most recent
// delivery to toAgent and ok=false when there is none in retained history
// (#348). A delivery is any row that reached state=delivered — both the
// verify-confirmed and the delivered_in_input_box soft-fail paths set that
// state + stamp delivered_at, so this counts every paste that reached the pane;
// failed (pane-gone) rows are excluded.
//
// Source-of-truth-derived from messages.delivered_at rather than a denormalized
// agents column: the delivery rows ARE the truth, so deriving can't drift from
// them and adds no write to the mailman's delivery hot path. Under the default
// infinite retention this is exact-forever; under a finite retention window,
// deliveries older than the window have been pruned, so a long-quiet mailman
// returns ok=false — which reads as "idle ≥ retention window", itself the
// divergence smell the operator wants (substrate-honest about what the
// retained substrate still knows).
func (s *Store) RecipientLastDelivered(ctx context.Context, toAgent string) (string, bool, error) {
	var ts sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(delivered_at) FROM messages WHERE to_agent = ? AND state = ?`,
		toAgent, StateDelivered).Scan(&ts)
	if err != nil {
		return "", false, err
	}
	if !ts.Valid || ts.String == "" {
		return "", false, nil
	}
	return ts.String, true, nil
}

// QueuedBacklogFloor computes the claim-floor for a (re)registering agent's
// pre-existing queued backlog under the #204 don't-flood policy. It keeps the
// `keepNewest` highest-id queued messages deliverable and reports the floor
// at or below which the remaining (older) queued rows are announce-skipped,
// plus how many rows that is.
//
// The single primitive serves both register modes:
//
//   - announce mode passes keepNewest = 0 → floor = the highest queued id,
//     skipped = M (the whole backlog stays queued; one nudge is enough).
//   - auto-deliver mode passes keepNewest = cap → if the backlog M is within
//     cap, everything delivers (floor 0, skipped 0, no nudge); otherwise the
//     newest `keepNewest` deliver and the oldest M−keepNewest are skipped.
//
// Returns (0, 0, nil) whenever there is nothing to skip (empty backlog, or
// keepNewest ≥ M): floor 0 means "no floor" to ClaimNext, and skipped 0 tells
// the caller to leave the epoch untouched and insert no nudge. The floor is
// id-ordinal: it is the (keepNewest+1)-th highest queued id, so ClaimNext's
// `id > floor` predicate keeps exactly the newest `keepNewest` rows.
//
// Note on residue interaction (#221): M counts ALL currently-queued rows,
// including any residue an earlier epoch already skipped. That is deliberate —
// the nudge's "N queued" then reports the true inbox depth the operator sees.
// A consequence is that re-registering with a low cap can re-admit a
// previously-skipped residue row if its id ranks within the newest
// `keepNewest`; that is consistent with the id-ordinal model and bounded by
// the cap. Draining residue wholesale is the deferred follow-up #221.
func (s *Store) QueuedBacklogFloor(ctx context.Context, toAgent string, keepNewest int) (floor int64, skipped int, err error) {
	if keepNewest < 0 {
		keepNewest = 0
	}
	depth, err := s.RecipientQueueDepth(ctx, toAgent)
	if err != nil {
		return 0, 0, fmt.Errorf("store: backlog floor depth: %w", err)
	}
	if depth == 0 || keepNewest >= depth {
		return 0, 0, nil // nothing to skip → no floor, no nudge
	}
	// floor = the (keepNewest+1)-th highest queued id. Ordering by id DESC
	// and skipping the newest `keepNewest` rows lands on the highest id that
	// must be skipped; ClaimNext's `id > floor` then keeps exactly the newest
	// keepNewest rows deliverable. keepNewest == 0 ⇒ OFFSET 0 ⇒ the max id.
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM messages
		 WHERE to_agent = ? AND state = ?
		 ORDER BY id DESC
		 LIMIT 1 OFFSET ?`,
		toAgent, StateQueued, keepNewest).Scan(&floor)
	if err != nil {
		return 0, 0, fmt.Errorf("store: backlog floor id: %w", err)
	}
	return floor, depth - keepNewest, nil
}

// SenderBacklog returns the number of queued messages originated by
// fromAgent across all recipients. This is a global per-sender
// diagnostic; it is NOT the cap predicate — the send-side sender cap is
// enforced per-(sender, recipient) in checkCapsInTx since #296, via an
// inline COUNT scoped to to_agent. Kept global because "how much does
// this sender have in flight overall" is still a meaningful question.
func (s *Store) SenderBacklog(ctx context.Context, fromAgent string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE from_agent = ? AND state = ?`,
		fromAgent, StateQueued).Scan(&n)
	return n, err
}

// MarkAcknowledged transitions a single queued message to acknowledged (#221).
// Only affects messages addressed to toAgent (auth-scope guard). Idempotent:
// if the message is already acknowledged the call succeeds with no state change.
// Returns ErrNotFound if no queued or acknowledged message with publicID exists
// addressed to toAgent.
func (s *Store) MarkAcknowledged(ctx context.Context, toAgent, publicID string) error {
	// Update only if the row is still queued (idempotent for already-acknowledged).
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET state = ?
		 WHERE public_id = ? AND to_agent = ? AND state = ?`,
		StateAcknowledged, publicID, toAgent, StateQueued)
	if err != nil {
		return fmt.Errorf("store: mark acknowledged: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows > 0 {
		return nil // transitioned successfully
	}
	// Either already acknowledged (idempotent) or genuinely not found/wrong agent.
	// Distinguish by checking existence.
	var count int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		 WHERE public_id = ? AND to_agent = ? AND state = ?`,
		publicID, toAgent, StateAcknowledged).Scan(&count)
	if err != nil {
		return fmt.Errorf("store: mark acknowledged: check: %w", err)
	}
	if count > 0 {
		return nil // already acknowledged — idempotent success
	}
	return fmt.Errorf("store: message %q for agent %q: %w", publicID, toAgent, ErrNotFound)
}

// MarkAcknowledgedBatch transitions all queued messages with id ≤ epochID
// addressed to toAgent to acknowledged (#221). Scope matches the backlog_epoch_id
// claim-floor so operators drain exactly the announce-skipped residue without
// touching newly-arrived messages (id > epoch). Returns the number of rows updated.
// When epochID is 0 (no epoch in effect) the call is a no-op that returns 0.
func (s *Store) MarkAcknowledgedBatch(ctx context.Context, toAgent string, epochID int64) (int64, error) {
	if epochID <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET state = ?
		 WHERE to_agent = ? AND state = ? AND id <= ?`,
		StateAcknowledged, toAgent, StateQueued, epochID)
	if err != nil {
		return 0, fmt.Errorf("store: mark acknowledged batch: %w", err)
	}
	return res.RowsAffected()
}

// staleQueuedPredicate is the #390 "pre-flip orphan" set: queued rows that will
// NOT auto-deliver once a delivery_mode flip fences them below the new backlog
// floor. Promoted-deferred rows (deliver_after IS NOT NULL) are EXCLUDED — they
// bypass the floor in ClaimNext (`deliver_after IS NOT NULL OR id > epoch`) and
// deliver regardless of the flip, so they are not orphans. Single-sourced so the
// count (CountStaleQueued) and the purge (AckStaleQueued) can never diverge.
const staleQueuedPredicate = `to_agent = ? AND state = ? AND deliver_after IS NULL`

// CountStaleQueued returns the number of queued, non-deferred messages addressed
// to toAgent — the rows a delivery_mode flip would orphan (#390). The register
// flip-gate uses this count to decide whether to require an explicit disposition.
func (s *Store) CountStaleQueued(ctx context.Context, toAgent string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE `+staleQueuedPredicate,
		toAgent, StateQueued).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count stale queued: %w", err)
	}
	return n, nil
}

// AckStaleQueued transitions every queued, non-deferred message addressed to
// toAgent to acknowledged — the `--purge-stale-queue` disposition for a
// delivery_mode flip (#390). Returns the number of rows updated. Unlike
// MarkAcknowledgedBatch (epoch-scoped, for announce-skipped residue), this is
// scoped by the orphan predicate, not by an id floor: at flip time the new
// floor isn't set yet, and every currently-queued non-deferred row predates the
// flip by definition.
func (s *Store) AckStaleQueued(ctx context.Context, toAgent string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET state = ? WHERE `+staleQueuedPredicate,
		StateAcknowledged, toAgent, StateQueued)
	if err != nil {
		return 0, fmt.Errorf("store: ack stale queued: %w", err)
	}
	return res.RowsAffected()
}

// FindDedupeMatch returns the most recent delivered+unverified message from
// fromAgent to toAgent whose body matches exactly and whose created_at is
// newer than cutoff. Returns nil (no error) when no match exists.
//
// Used by the mailman's dedupe path (#157 PR2): before delivering a message,
// check whether a prior delivery attempt for the same body landed as
// delivered_in_input_box (verified=0) within the dedupe window. If found,
// the mailman re-verifies the original in pane scrollback and either confirms
// it (absorbing the duplicate) or delivers the replay.
func (s *Store) FindDedupeMatch(ctx context.Context, fromAgent, toAgent, body, cutoff string) (*Message, error) {
	var m Message
	var nre, q int
	var er int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
		        no_reply_expected, quick, state, created_at, delivered_at, error, replay_of, replay_of_at, verified, deliver_after, expects_reply, priority
		 FROM messages
		 WHERE from_agent = ? AND to_agent = ? AND body = ?
		   AND state = ? AND verified = 0 AND created_at > ?
		 ORDER BY id DESC LIMIT 1`,
		fromAgent, toAgent, body, StateDelivered, cutoff).Scan(
		&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
		&nre, &q, &m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error, &m.ReplayOf, &m.ReplayOfAt, &m.Verified, &m.DeliverAfter, &er, &m.Priority)
	m.NoReplyExpected = nre != 0
	m.Quick = q != 0
	m.ExpectsReply = er != 0
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find dedupe match: %w", err)
	}
	return &m, nil
}

// MarkVerifiedByDedupe upgrades a delivered_in_input_box row to confirmed-
// delivered: sets verified=1 on a delivered row that currently has verified=0.
// Used by the mailman's dedupe path (#157 PR2) when a scrollback re-verify
// confirms the original message was actually processed by the recipient.
// Returns ErrNotFound when no matching delivered+unverified row exists.
func (s *Store) MarkVerifiedByDedupe(ctx context.Context, publicID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET verified = 1
		 WHERE public_id = ? AND state = ? AND verified = 0`,
		publicID, StateDelivered)
	if err != nil {
		return fmt.Errorf("store: mark verified by dedupe: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: mark verified by dedupe %q: %w", publicID, ErrNotFound)
	}
	return nil
}

// GetMessage returns one message by its public_id, or ErrNotFound.
func (s *Store) GetMessage(ctx context.Context, publicID string) (*Message, error) {
	var m Message
	var nre, q, er int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
		        no_reply_expected, quick, state, created_at, delivered_at, error, replay_of, replay_of_at, verified, deliver_after, expects_reply, priority
		 FROM messages WHERE public_id = ?`,
		publicID).Scan(
		&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
		&nre, &q, &m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error, &m.ReplayOf, &m.ReplayOfAt, &m.Verified, &m.DeliverAfter, &er, &m.Priority)
	m.NoReplyExpected = nre != 0
	m.Quick = q != 0
	m.ExpectsReply = er != 0
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return &m, nil
}

// FindMessagesByPrefix returns all messages whose public_id starts with the
// given prefix. Caller is responsible for any access-filtering and for
// handling the disambiguation case (len(result) > 1) appropriately. Empty
// result is returned as nil + nil error — distinct from ErrNotFound, which
// the GetMessage exact-match path returns; the prefix path uses a SELECT
// (not QueryRowContext) and "no rows" is a valid result, not an error.
//
// Used by the get-by-id surface (#111) for short-prefix lookups (the 4-char
// IDs that appear in delivery headers). The store does not enforce a
// minimum prefix length — at very-short prefixes the result set can be
// large; callers should reject prefixes that would return too many rows
// before exposing the surface.
//
// LIKE-wildcard escape: the prefix is treated as a literal string match,
// not a SQL LIKE pattern. `%` and `_` within the prefix are escaped via
// backslash (matched by SQLite's ESCAPE clause below) so a caller can't
// turn `get %` into a list-all-my-messages enumeration. Per Surveyor's
// PR #128 S1: validation belongs where LIKE happens, not at every caller.
func (s *Store) FindMessagesByPrefix(ctx context.Context, prefix string) ([]Message, error) {
	if prefix == "" {
		return nil, errors.New("store: prefix required")
	}
	escaped := escapeLikePrefix(prefix)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
		        no_reply_expected, quick, state, created_at, delivered_at, error, replay_of, replay_of_at, verified, deliver_after, expects_reply, priority
		 FROM messages WHERE public_id LIKE ? ESCAPE '\' ORDER BY id ASC`,
		escaped+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var nre, q, er int
		if err := rows.Scan(&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent,
			&m.ReplyTo, &m.Body, &m.Kind, &nre, &q, &m.State, &m.CreatedAt,
			&m.DeliveredAt, &m.Error, &m.ReplayOf, &m.ReplayOfAt, &m.Verified, &m.DeliverAfter, &er, &m.Priority); err != nil {
			return nil, err
		}
		m.NoReplyExpected = nre != 0
		m.Quick = q != 0
		m.ExpectsReply = er != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// escapeLikePrefix backslash-escapes the three characters SQLite's LIKE
// treats specially when an ESCAPE clause is in effect: the backslash
// itself, `%` (zero-or-more chars wildcard), and `_` (single-char
// wildcard). Order matters — backslash must be escaped first, otherwise
// a subsequent `%` → `\%` replacement would itself get re-escaped.
func escapeLikePrefix(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// ListFilter narrows the rows returned by ListMessages. Zero-value fields
// mean "no filter on that column".
type ListFilter struct {
	ToAgent        string
	FromAgent      string
	ReplyTo        string // #250: filter to rows whose reply_to == this public_id
	State          State
	Kind           Kind
	Limit          int    // 0 → 100; capped at 1000.
	SinceCreatedAt string // ISO 8601 UTC floor; "" = no floor
	Unverified     bool   // true → only state=delivered AND verified=0 rows
	OrderDesc      bool   // true → ORDER BY id DESC (newest-first); false = id ASC
	// Deferred controls visibility of #227 deferred rows. false (default):
	// deferred rows are EXCLUDED unless an explicit State filter asks for them
	// — so default list/inbox/audit views never leak pre-queue rows. true:
	// return ONLY state=deferred rows (the `sent --deferred` opt-in). When a
	// caller sets State explicitly, that filter wins and Deferred is ignored.
	Deferred bool
	// Unanswered, when true, restricts to rows where expects_reply=1 AND the
	// ToAgent has not sent a reply (no row with reply_to=this.public_id and
	// from_agent=ToAgent exists). The inbox --unanswered filter (#270):
	// messages in the recipient's inbox where the sender flagged intent and
	// the recipient hasn't replied. Requires ToAgent to be set.
	Unanswered bool
	// AwaitingReply, when true, restricts to rows where expects_reply=1 AND
	// the original recipient has not replied (no row with reply_to=this.public_id
	// and from_agent=this.to_agent exists). The sent --awaiting-reply filter
	// (#270): messages the sender marked expects_reply and hasn't heard back on.
	AwaitingReply bool
}

// ListMessages returns messages matching the filter, ordered by id ASC by
// default (or id DESC when f.OrderDesc is true).
func (s *Store) ListMessages(ctx context.Context, f ListFilter) ([]Message, error) {
	if f.Unverified && f.State != "" && f.State != StateDelivered {
		return nil, fmt.Errorf("store: ListFilter: Unverified=true requires State to be empty or %q", StateDelivered)
	}
	if f.Unanswered && f.ToAgent == "" {
		return nil, fmt.Errorf("store: ListFilter: Unanswered=true requires ToAgent to be set")
	}
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
	if f.ReplyTo != "" {
		wheres = append(wheres, "reply_to = ?")
		args = append(args, f.ReplyTo)
	}
	if f.State != "" {
		wheres = append(wheres, "state = ?")
		args = append(args, f.State)
	} else if f.Deferred {
		// Opt-in: only deferred rows (#227 `sent --deferred`).
		wheres = append(wheres, "state = ?")
		args = append(args, StateDeferred)
	} else {
		// Default: hide deferred rows from the all-states view so pre-queue
		// rows never leak into inbox / audit / list surfaces (#227).
		wheres = append(wheres, "state != ?")
		args = append(args, StateDeferred)
	}
	if f.Kind != "" {
		wheres = append(wheres, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.SinceCreatedAt != "" {
		wheres = append(wheres, "created_at >= ?")
		args = append(args, f.SinceCreatedAt)
	}
	if f.Unverified {
		wheres = append(wheres, "state = ?")
		args = append(args, StateDelivered)
		wheres = append(wheres, "verified = 0")
	}
	// #270: inbox --unanswered: expects_reply=1 AND ToAgent hasn't replied.
	if f.Unanswered {
		wheres = append(wheres, "expects_reply = 1")
		wheres = append(wheres, "NOT EXISTS (SELECT 1 FROM messages AS r WHERE r.reply_to = messages.public_id AND r.from_agent = ?)")
		args = append(args, f.ToAgent)
	}
	// #270: sent --awaiting-reply: expects_reply=1 AND recipient hasn't replied.
	if f.AwaitingReply {
		wheres = append(wheres, "expects_reply = 1")
		wheres = append(wheres, "NOT EXISTS (SELECT 1 FROM messages AS r WHERE r.reply_to = messages.public_id AND r.from_agent = messages.to_agent)")
	}
	switch {
	case f.Limit <= 0:
		f.Limit = 100
	case f.Limit > 1000:
		f.Limit = 1000
	}
	args = append(args, f.Limit)

	ord := "ASC"
	if f.OrderDesc {
		ord = "DESC"
	}
	qry := `SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
	             no_reply_expected, quick, state, created_at, delivered_at, error, replay_of, replay_of_at, verified, deliver_after, expects_reply, priority
	      FROM messages`
	if len(wheres) > 0 {
		qry += " WHERE " + strings.Join(wheres, " AND ")
	}
	qry += " ORDER BY id " + ord + " LIMIT ?"

	rows, err := s.db.QueryContext(ctx, qry, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// TailFilter narrows TailRows by *immutable* columns only (from / to / kind /
// created-at floor). State is deliberately excluded: a live tail (#148) tracks
// state transitions on rows it has already surfaced, so filtering on state at
// the query level would hide a row until it entered the wanted state and lose
// the transition. The CLI applies any --state gate at render time instead.
type TailFilter struct {
	From           string
	To             string
	Kind           string
	SinceCreatedAt string // sqliteTimeFormat (T…Z); "" = no floor
}

// TailRows returns messages with id > afterID matching f, ordered id ASC and
// capped at limit. It is the cross-process rowid-poll primitive behind
// `claude-msg tail` (#148): the CLI re-calls it each tick with afterID = the
// highest id it has seen, so a separate-process mailman's INSERTs surface
// incrementally. WAL mode (set in Open) makes these reads safe concurrent with
// mailman writes. State changes on already-seen rows do NOT move the id, so the
// CLI re-reads in-flight ids via MessagesByIDs for transition rendering.
func (s *Store) TailRows(ctx context.Context, afterID int64, f TailFilter, limit int) ([]Message, error) {
	wheres := []string{"id > ?"}
	args := []any{afterID}
	if f.From != "" {
		wheres = append(wheres, "from_agent = ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		wheres = append(wheres, "to_agent = ?")
		args = append(args, f.To)
	}
	if f.Kind != "" {
		wheres = append(wheres, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.SinceCreatedAt != "" {
		wheres = append(wheres, "created_at >= ?")
		args = append(args, f.SinceCreatedAt)
	}
	if limit <= 0 {
		limit = 1000
	}
	args = append(args, limit)

	q := `SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
	             no_reply_expected, quick, state, created_at, delivered_at, error, replay_of, replay_of_at, verified, deliver_after, expects_reply, priority
	      FROM messages WHERE ` + strings.Join(wheres, " AND ") +
		` ORDER BY id ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: tail rows: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// MessagesByIDs returns the current rows for the given numeric ids, ordered id
// ASC. `tail` uses it to re-read the live state of in-flight rows it has
// already surfaced, so queued→delivering→delivered/failed transitions render on
// the same id. Empty ids → empty result, no query.
func (s *Store) MessagesByIDs(ctx context.Context, ids []int64) ([]Message, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT id, public_id, from_agent, to_agent, reply_to, body, kind,
	             no_reply_expected, quick, state, created_at, delivered_at, error, replay_of, replay_of_at, verified, deliver_after, expects_reply, priority
	      FROM messages WHERE id IN (` + strings.Join(placeholders, ",") + `) ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: messages by ids: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// scanMessages drains a full-column message query into a slice. Shared by the
// readers above (the column order matches the SELECT lists verbatim).
func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var m Message
		var nre, q, er int
		if err := rows.Scan(
			&m.ID, &m.PublicID, &m.FromAgent, &m.ToAgent, &m.ReplyTo, &m.Body, &m.Kind,
			&nre, &q, &m.State, &m.CreatedAt, &m.DeliveredAt, &m.Error, &m.ReplayOf, &m.ReplayOfAt, &m.Verified, &m.DeliverAfter, &er, &m.Priority); err != nil {
			return nil, err
		}
		m.NoReplyExpected = nre != 0
		m.Quick = q != 0
		m.ExpectsReply = er != 0
		out = append(out, m)
	}
	return out, rows.Err()
}
