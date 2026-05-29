package store

import "database/sql"

// State represents a message's lifecycle position in the queue.
type State string

const (
	StateQueued     State = "queued"
	StateDelivering State = "delivering"
	StateDelivered  State = "delivered"
	StateFailed     State = "failed"
)

// Kind distinguishes a paste-rendered chat message from a control command
// that is typed directly into the recipient pane.
type Kind string

const (
	KindMessage Kind = "message"
	KindControl Kind = "control"
)

// Message mirrors a row in the messages table. Timestamps are kept as
// strings (ISO 8601 UTC) — callers parse to time.Time at presentation time
// if needed; the store layer is timezone-agnostic.
type Message struct {
	ID          int64
	PublicID    string
	FromAgent   string
	ToAgent     string
	ReplyTo     sql.NullString
	Body        string
	Kind        Kind
	State       State
	CreatedAt   string
	DeliveredAt sql.NullString
	Error       sql.NullString
}

// Agent mirrors a row in the agents table.
type Agent struct {
	Name      string
	PaneID    string // empty when pane_id is NULL
	Paused    bool
	UpdatedAt string
}
