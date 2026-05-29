// Package store provides SQLite-backed persistence for cli-semaphore: the
// messages queue and the agents registry. The package is the single source
// of truth for schema and queue-state invariants; the CLI subcommands and
// the mailman daemon both go through it.
//
// Concurrency: in WAL mode SQLite supports concurrent readers and a single
// writer. cli-semaphore's design has at most one mailman per recipient, so
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

	dsn := "file:" + path + "?_pragma=busy_timeout(5000)"
	if path == ":memory:" {
		// shared cache so the sql.DB connection pool sees the same DB
		// across multiple physical connections (relevant for in-mem tests).
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)"
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

	return s, nil
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
