-- tmux-msg SQLite schema.
--
-- Applied idempotently by the store package on open. The store also runs
-- the runtime PRAGMAs (WAL + synchronous=NORMAL + foreign_keys) before any
-- other statement.
--
-- Timestamps are stored as ISO 8601 UTC text with millisecond resolution
-- (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')) so they sort lexically and are
-- driver-portable.
--
-- See the README for the column-by-column design rationale.

CREATE TABLE IF NOT EXISTS messages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    public_id     TEXT NOT NULL UNIQUE,           -- 7f3a — short, copy-pastable
    from_agent    TEXT NOT NULL,
    to_agent      TEXT NOT NULL,
    reply_to      TEXT REFERENCES messages(public_id),
    body          TEXT NOT NULL,
    kind          TEXT NOT NULL DEFAULT 'message',-- message | control
    no_reply_expected INTEGER NOT NULL DEFAULT 0, -- 1 = sender requests no ack (#145)
    quick         INTEGER NOT NULL DEFAULT 0,      -- 1 = render compact single-line chrome (#154)
    state         TEXT NOT NULL DEFAULT 'queued', -- queued|delivering|delivered|failed
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    delivered_at  TEXT,
    error         TEXT
);

-- Queue-head reads filter by (to_agent, state) and order by id; this index
-- makes that a clustered range scan.
CREATE INDEX IF NOT EXISTS ix_msg_queue ON messages(to_agent, state, id);

CREATE TABLE IF NOT EXISTS agents (
    name        TEXT PRIMARY KEY,
    pane_id     TEXT,                              -- "%3" — refreshed by boot-time discovery
    paused      INTEGER NOT NULL DEFAULT 0,        -- the kill switch
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    aliases     TEXT NOT NULL DEFAULT '[]'         -- #38: JSON-encoded list of alt names discover matches against
);

-- #918: per-sender communication budget. Deliberately NOT a column on `agents`
-- and deliberately WITHOUT a foreign key to it.
--
-- AC: "a budget survives re-registration". `register` writes `agents` only, so
-- keeping the balance in its own table satisfies that BY CONSTRUCTION rather
-- than by anyone remembering to preserve it — there is no code path that could
-- forget. An FK with ON DELETE CASCADE would actively defeat the AC; an FK
-- without one buys nothing here. The cost is an orphan row per agent name ever
-- used, which is bounded by the number of names and is the cheaper side.
--
-- Only the balance is stored. The WINDOW state (which recipients this sender
-- has already paid breadth for) is DERIVED from `messages` at charge time —
-- see internal/store/budget.go. Nothing to keep consistent, nothing to
-- reconcile after a crash.
CREATE TABLE IF NOT EXISTS budgets (
    agent       TEXT PRIMARY KEY,
    balance     REAL NOT NULL,
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
