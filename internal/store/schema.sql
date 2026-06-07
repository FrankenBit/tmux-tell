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
