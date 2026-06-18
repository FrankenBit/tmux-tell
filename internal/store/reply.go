package store

import (
	"context"
	"time"
)

// Request-reply seams (#250). A "reply" to an ask is any message whose
// reply_to == the ask's public_id AND to_agent == the asker — i.e. the answer
// the asker is blocked waiting for. These three seams power the ask /
// wait_for_reply / check_replies tools.

// defaultReplyPollInterval is the substrate-side poll cadence for WaitForReply's
// poll-backed notify seam (#250 Q2). Small enough that a reply surfaces
// near-instantly to a blocked asker, large enough that an idle wait costs
// almost nothing.
const defaultReplyPollInterval = 150 * time.Millisecond

// ListReplies returns every reply to askID addressed to caller, ordered id ASC.
// sinceID > 0 restricts to replies with id > sinceID — the accumulation case
// for check_replies, where the caller passes the highest id it has already
// seen. Deferred rows are excluded by ListMessages' default (a reply is never
// deferred). Empty result is (nil, nil), not an error.
//
// Caps at the ListMessages default (1000 rows). A single ask accumulating >1000
// replies is implausible for the Q&A use case; if it ever matters, check_replies'
// `since` pages past the cap by advancing the high-water id.
func (s *Store) ListReplies(ctx context.Context, caller, askID string, sinceID int64) ([]Message, error) {
	msgs, err := s.ListMessages(ctx, ListFilter{ToAgent: caller, ReplyTo: askID, Limit: 1000})
	if err != nil {
		return nil, err
	}
	if sinceID <= 0 {
		return msgs, nil
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.ID > sinceID {
			out = append(out, m)
		}
	}
	return out, nil
}

// FindReply returns the earliest reply to askID addressed to caller, or nil if
// none has arrived yet. sinceID > 0 restricts to replies after that id.
func (s *Store) FindReply(ctx context.Context, caller, askID string, sinceID int64) (*Message, error) {
	msgs, err := s.ListReplies(ctx, caller, askID, sinceID)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	m := msgs[0]
	return &m, nil
}

// WaitForReply blocks until a reply to askID addressed to caller appears, or
// ctx is done (the timeout the caller set). It is the #250 Q2 "push
// subscription" seam — a push-shaped blocking API.
//
// Substrate-honesty on "push": tmux-msg is multi-process — the reply is
// INSERTed by a *different* process (the replier's) than the one waiting (the
// asker's). A literal sqlite update_hook fires only intra-connection, so it
// can't bridge that gap; true cross-process push would need new IPC. So this
// seam is poll-backed at the substrate side (the operator's chosen option (b),
// "sql-based polling at the substrate side", 2026-06-09): the poll lives here
// behind one reusable blocking call, and callers never see it.
//
// #515 made good on the documented escape hatch: when a SetWatcher hook is
// installed (the CLI wires it to the fs-watch doorbell layer), this seam wakes
// on the reply-insert ring instead of waiting out pollInterval — and not a
// single caller changed, exactly as promised. The poll remains as the
// best-effort fallback: a dropped ring just costs one poll interval, never a
// lost reply (the answer always still comes from FindReply against SQLite).
//
// Returns (reply, nil) on a reply, (nil, ctx.Err()) on timeout/cancel.
//
// pollInterval is reserved for tuning — tests pass a small value to keep the
// suite fast. There is no v1 config surface for it; production callers pass 0
// and take defaultReplyPollInterval.
func (s *Store) WaitForReply(ctx context.Context, caller, askID string, sinceID int64, pollInterval time.Duration) (*Message, error) {
	if pollInterval <= 0 {
		pollInterval = defaultReplyPollInterval
	}
	// #515: a reply is a message addressed to caller, so the reply-insert rings
	// caller's doorbell. Subscribe once for the wait's lifetime; nil when no
	// watcher is wired → the select below is pure poll, unchanged.
	notifyCh := watchKey(ctx, caller)
	for {
		// Check first — a reply may already be waiting (no wasted sleep). This
		// also closes the subscribe-vs-insert race: a ring that landed between
		// watchKey and here is caught by this read, not lost.
		m, err := s.FindReply(ctx, caller, askID, sinceID)
		if err != nil {
			return nil, err
		}
		if m != nil {
			return m, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-notifyCh:
			// Fast wake on the reply ring; loop re-reads. nil channel never
			// fires, so this case is inert when no watcher is wired.
		case <-time.After(pollInterval):
		}
	}
}
