// Package store provides SQLite-backed persistence for tmux-msg: the
// messages queue and the agents registry. The package is the single source
// of truth for schema and queue-state invariants; the CLI subcommands and
// the mailman daemon both go through it.
//
// Concurrency: in WAL mode SQLite supports concurrent readers and a single
// writer. tmux-msg's design has at most one mailman per recipient, so
// writes to a given to_agent are naturally serial. Reads (status, inbox,
// caps) are concurrent and lock-free.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the database handle. Use Open to construct one and Close to
// release it.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path, applies the embedded
// schema idempotently, and configures the runtime PRAGMAs that the rest of
// the package assumes (WAL, NORMAL sync, foreign keys on).
//
// path may be ":memory:" for tests.
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("store: create parent dir: %w", err)
		}
	}

	// _txlock=immediate makes BeginTx issue `BEGIN IMMEDIATE`, acquiring
	// the RESERVED lock at transaction start. This is what makes the
	// in-transaction cap checks in InsertMessage / InsertMessagePair
	// safe against cross-process write races (#29) — two concurrent
	// senders to the same recipient can no longer both read N, both
	// decide N+1 ≤ cap, and both insert past the cap. With IMMEDIATE,
	// the second BeginTx waits for the first to COMMIT (bounded by
	// busy_timeout above) before seeing the post-insert state.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_txlock=immediate"
	if path == ":memory:" {
		// shared cache so the sql.DB connection pool sees the same DB
		// across multiple physical connections (relevant for in-mem tests).
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)&_txlock=immediate"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}

	// Pin to a single connection so PRAGMA state is consistent and so the
	// in-memory cache stays alive for the test lifetime.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, p := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := s.db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: %s: %w", p, err)
		}
	}

	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	// Idempotent column-adds for databases created before the column
	// existed. SQLite doesn't support ALTER TABLE ADD COLUMN IF NOT
	// EXISTS, so we swallow the "duplicate column" error explicitly.
	for _, m := range migrations {
		if _, err := s.db.ExecContext(ctx, m); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				_ = db.Close()
				return nil, fmt.Errorf("store: migrate %q: %w", m, err)
			}
		}
	}

	return s, nil
}

// migrations are idempotent schema patches. Each must be safe to re-run
// (i.e. either inherently idempotent or — for ALTER TABLE ADD COLUMN —
// fails with "duplicate column name" which Open() ignores).
var migrations = []string{
	`ALTER TABLE messages ADD COLUMN kind TEXT NOT NULL DEFAULT 'message'`,
	`ALTER TABLE agents ADD COLUMN aliases TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE agents ADD COLUMN delivery_mode TEXT NOT NULL DEFAULT 'paste-and-enter'`,
	`ALTER TABLE messages ADD COLUMN no_reply_expected INTEGER NOT NULL DEFAULT 0`,
	// #157 PR1: replay linkage for `resend`. replay_of holds the original
	// message's public_id; replay_of_at holds its created_at (so the pure
	// render layer can show "Replayed: original sent at <ts>" without a
	// store lookup). Both NULL for normal (non-replay) messages.
	`ALTER TABLE messages ADD COLUMN replay_of TEXT`,
	`ALTER TABLE messages ADD COLUMN replay_of_at TEXT`,
	// #169: durable verified/unverified delivery marker. 1 = verify-token
	// observed (confirmed delivery), 0 = delivered_in_input_box soft-fail
	// (paste landed, token never surfaced), NULL = unknown (pre-migration
	// delivered rows, or any non-delivered state). Orthogonal to `state`,
	// which keeps `delivered` for both 1 and 0 — the bit is the only
	// distinction, where previously only a journal line carried it.
	`ALTER TABLE messages ADD COLUMN verified INTEGER`,
	// #154: compact single-line chrome. 1 = recipient's pane renders the
	// message as one line (✓ Sender · [re X ·] body) instead of the full
	// bracket-header block. Opt-in per message; default false.
	`ALTER TABLE messages ADD COLUMN quick INTEGER NOT NULL DEFAULT 0`,
	// #204: backlog epoch. The highest message id that counts as
	// "pre-existing backlog" at the agent's last (re)register — stamped by
	// the register handler when queued > 0. The mailman's don't-flood policy
	// keys on it: messages with id <= backlog_epoch_id are the backlog that
	// existed when the session (re)started, distinct from new arrivals
	// (id > epoch) that deliver normally. NULL = never registered with a
	// backlog (no epoch in effect → all messages deliver normally).
	`ALTER TABLE agents ADD COLUMN backlog_epoch_id INTEGER`,
	// #224: chamber → operator attention signal. Three values:
	//   - "idle"               — default; chamber is reachable, no operator action pending
	//   - "busy"               — chamber is mid-tool-call (informational; future hook)
	//   - "awaiting_operator"  — chamber has presented a choice / question and needs the
	//                            operator to weigh in before continuing
	// Set explicitly by chambers via the flag_operator / clear_operator_flag
	// tools. Auto-cleared to "idle" on the chamber's next register call (the
	// chamber resumed; whatever it was waiting on is presumed resolved).
	`ALTER TABLE agents ADD COLUMN attention_state TEXT NOT NULL DEFAULT 'idle'`,
	// #291: mailman stuck-state. Empty (default) = healthy. A non-empty
	// reason (currently only "pane-not-found") means the mailman hit N
	// consecutive pane-probe failures on a persistent failure (a stale /
	// wrong-server pane registration) and has parked itself: it stops
	// probing tmux for this agent entirely so the retry storm can't wedge
	// the tmux server (the 2026-06-10 17:54 tmux death). Messages stay
	// queued (no loss). Cleared by `register --force` (the operator fixes
	// the registration), which resumes normal delivery on the next loop.
	`ALTER TABLE agents ADD COLUMN stuck_reason TEXT NOT NULL DEFAULT ''`,
	// #228: presence slot for operator-presence routing. Single-key K/V table
	// recording substrate observations of where the operator currently is or
	// was last attached. The send_to_operator path resolves the special
	// recipient "operator" via this slot: look up the chamber the operator is
	// at right now (or was last attached to). Single-row-per-key shape;
	// today the only key in use is "operator.last_seen_in".
	`CREATE TABLE IF NOT EXISTS presence (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	)`,
	// #227: deferred-delivery trigger. Non-NULL only on rows in state
	// 'deferred' — names the trigger that promotes the row to 'queued' (v1:
	// "resume", fired by a flush_deferred call). NULL for every normal
	// message. Additive; deferred rows are invisible to ClaimNext/inbox/
	// mailman until promoted, so existing flows are unaffected.
	`ALTER TABLE messages ADD COLUMN deliver_after TEXT`,
	// #250: request-reply marker. 1 = sender intends a reply (set by `ask` or
	// `send --expects-reply`, #270). 0 = normal send. Read through
	// Message.ExpectsReply and the Unanswered / AwaitingReply ListFilter
	// booleans (#270). Default 0 — every existing send is unaffected.
	`ALTER TABLE messages ADD COLUMN expects_reply INTEGER NOT NULL DEFAULT 0`,
	// #348: covering index for RecipientLastDelivered's per-agent
	// MAX(delivered_at) WHERE to_agent=? AND state=?. Read-only forward-design:
	// alcatraz scale doesn't need it today, but the per-recipient delivery-recency
	// query becomes load-bearing as the (infinite-retention-by-default) messages
	// table grows over the substrate's lifetime — index-now beats scan-then-add.
	`CREATE INDEX IF NOT EXISTS idx_messages_recipient_state_delivered ON messages(to_agent, state, delivered_at)`,
	// #448: provider-aware concurrency cap. provider is the adapter-declared
	// upstream LLM provider ("anthropic" / "openai"), written at serve start.
	// observed_state mirrors the mailman's most recent live AgentState
	// observation of its own pane ("working" / "idle" / …) so any mailman can
	// count how many same-provider chambers are currently working;
	// observed_state_at stamps that write so a crashed mailman's stale
	// "working" ages out of the count (TTL guard) instead of pinning a slot.
	// All additive with empty/NULL defaults — an agent with no provider is
	// never gated and never counted.
	`ALTER TABLE agents ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN observed_state TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN observed_state_at TEXT`,
	// #449: per-message priority weight (low=10 / normal=20 / high=30). Drives
	// the cross-channel scheduler in ClaimNext (within-channel FIFO preserved).
	// Default 20 (normal) so every existing + un-prioritized message is normal.
	`ALTER TABLE messages ADD COLUMN priority INTEGER NOT NULL DEFAULT 20`,
	// #507: the per-provider concurrency cap the agent's mailman serves with,
	// persisted at serve start alongside provider (#448) so a *separate* process
	// (`inbox`) can live-derive whether a recipient's queued message is being
	// held by the provider cap without knowing the mailman's serve flags. 0 (the
	// default) means "no cap configured" — never gated, never surfaced as deferred.
	`ALTER TABLE agents ADD COLUMN provider_cap INTEGER NOT NULL DEFAULT 0`,
	// #556: the chamber-asserted display name (case-/space-preserved, e.g.
	// "Lookout", "Master Bosun") shown in agents listings + status outputs.
	// Distinct from the canonical `name` PK (the bus routing key, possibly
	// lowercase) — display_name is render-only, never a routing key. Empty
	// default: an agent that never asserted one falls back to its name in the UI.
	`ALTER TABLE agents ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`,
	// #558: operator --force-rate-limited override. 1 = the mailman bypasses the
	// rate-limit / usage-limit defer gates for this one message (escape-hatch for
	// a false-positive pattern, or when the operator knows the limit cleared);
	// paste-unsafe states OTHER than rate/usage (copy-mode, awaiting-operator,
	// unknown, compaction) are still honored. Default 0 = normal deferral.
	`ALTER TABLE messages ADD COLUMN force_rate_limited INTEGER NOT NULL DEFAULT 0`,
	// #626 Phase 1b: intrinsic session identity. The Claude session UUID
	// (CLAUDE_CODE_SESSION_ID), self-discovered from the registering pane's
	// process tree at register time. The primary, exact match key for
	// session-as-addressee delivery (vs the fuzzy `claude --resume <name>`
	// argv match). Empty default = a legacy / not-yet-discovered registration
	// -> delivery falls back to the name-based discover path (#626 AC6).
	`ALTER TABLE agents ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
	// #621: first-class self-reported metabolism. A chamber self-reports an
	// intentional context-throughput state that the auto-observer (observed_state,
	// #448) CANNOT infer from pane chrome: "warming" (just resumed, not yet at
	// full context throughput), "saturating" (context-load approaching the
	// /compact-need, not yet idle nor at-rest), "compact-pending" (intent-to-
	// /compact stated but not yet executed — the chamber-stall seam Bosun's
	// metabolism-judgment pin addresses). Orthogonal to attention_state (#224, the
	// operator-flag axis) and observed_state (#448, the auto-observed axis): a
	// self-report the chamber owns and only it can set. Empty default = no
	// self-report. metabolism_set_at stamps when the current value was set so a
	// consumer can discount a stale self-report; NULL whenever metabolism is empty
	// (the cleared state). Advisory only — NEVER gates delivery (IsPasteUnsafe
	// stays observed-state-only).
	`ALTER TABLE agents ADD COLUMN metabolism TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN metabolism_set_at TEXT`,
	// #285: bounded post-shrink chamber respawn. respawn_after_shrinks is the
	// per-chamber threshold N — the mailman respawns the chamber's process
	// after N in-substrate context-shrink events (bus-delivered clear in PR1;
	// self-compact detection layers on in PR2). 0 (the default) = DISABLED:
	// opt-in per chamber via set-respawn-after-shrinks, so an install never
	// auto-restarts live processes (the memory-cap wrapper, alcatraz-infra#50,
	// already covers host-protection; this is graceful-vs-abrupt hygiene).
	// respawn_shrink_count is the running counter, incremented on each counted
	// shrink and reset to 0 after a respawn fires. Both additive, default 0.
	`ALTER TABLE agents ADD COLUMN respawn_after_shrinks INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE agents ADD COLUMN respawn_shrink_count INTEGER NOT NULL DEFAULT 0`,
	// #285 PR2: self-compact detection. A self-/compact is chamber-driven, not
	// bus-delivered, so the mailman can't count it on delivery like the clear
	// (PR1). Instead the adapter's post-compaction hook writes a signal
	// (last_self_compact_at, via the note-compact helper) and the mailman
	// edge-detects it on its self-observation cadence. self_compact_counted_at is
	// the mailman-owned watermark of the last signal it counted — advanced
	// atomically with the increment so a signal is never double-counted. The hook
	// only ever writes last_self_compact_at; the mailman stays the SOLE writer of
	// respawn_shrink_count, preserving PR1's single-flight race-freedom. Both
	// nullable (NULL until the first compact / first count).
	`ALTER TABLE agents ADD COLUMN last_self_compact_at TEXT`,
	`ALTER TABLE agents ADD COLUMN self_compact_counted_at TEXT`,
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw *sql.DB. Subcommand code should prefer the typed
// methods on Store; this hatch exists for testing and the rare ad-hoc
// query.
func (s *Store) DB() *sql.DB { return s.db }

// generatePublicID returns a random 4-hex-character identifier
// (16 bits, ~65 K namespace). InsertMessage's UNIQUE collision
// retry covers birthday-paradox risk at this size.
func generatePublicID() (string, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ErrNotFound is returned when a row lookup misses.
var ErrNotFound = errors.New("not found")
