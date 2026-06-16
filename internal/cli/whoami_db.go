package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/mcp"
)

// processStart records when this process entered cli.Run — the substrate-honest
// "started_at" for whoami_db (#348). A long-lived MCP server spawned at session
// start holds this for its lifetime; comparing it against a deploy timestamp is
// how an operator spots a pre-deploy process still bound to a stale inode. Set
// once in Run; the zero value (in-package tests that bypass Run) renders as an
// omitted field rather than a bogus epoch.
var processStart time.Time

// dbBinding is the substrate-honest answer to "where is this process actually
// writing?" (#348). It is read from /proc — the live open file handle and the
// exe symlink — NOT by re-resolving the DB path, because re-resolution would
// mask the exact divergence this surfaces: a process that opened the DB before
// a deploy `mv`'d it keeps writing to the orphaned inode, reachable only
// through its own fd, invisible to `sqlite3` on the canonical path.
type dbBinding struct {
	PID        int    `json:"pid"`
	BinaryPath string `json:"binary_path"`
	StartedAt  string `json:"started_at,omitempty"`
	DBPath     string `json:"db_path"`
	DBInode    uint64 `json:"db_inode,omitempty"`
	// DBDeleted is true when the open DB handle's dirent is gone (the readlink
	// target carries the kernel's " (deleted)" marker) — the orphan-inode smell.
	DBDeleted bool `json:"db_deleted,omitempty"`
	// Note carries a human-readable caveat when a field couldn't be
	// substantiated (e.g. no open *.db handle — an in-memory or not-yet-opened
	// store), so the absence reads as "couldn't determine" not "none".
	Note string `json:"note,omitempty"`
}

// procExe reads /proc/<pid>/exe — the resolved binary path. The kernel appends
// " (deleted)" when the on-disk binary was replaced/removed since exec (a
// process still running a pre-deploy binary), which is itself a divergence
// signal, so the marker is preserved in the returned path.
func procExe(pid int) string {
	target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return ""
	}
	return target
}

// openDBHandle walks /proc/<pid>/fd looking for the process's open SQLite
// database handle and returns the path it was opened as, its real inode, and
// whether the dirent has been removed. It matches the first fd whose target
// basename ends in ".db" (the bus DB is messages.db; the -wal / -shm / -journal
// siblings end in other suffixes and are skipped).
//
// The inode comes from syscall.Stat on the /proc fd symlink, which follows to
// the actual open file — so it resolves the live inode even when the file has
// been unlinked (the orphan case). found=false means no .db handle is open
// (e.g. an in-memory store, or the process hasn't opened the DB).
func openDBHandle(pid int) (path string, inode uint64, deleted, found bool) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return "", 0, false, false
	}
	for _, e := range entries {
		link := filepath.Join(fdDir, e.Name())
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		del := strings.HasSuffix(target, " (deleted)")
		clean := strings.TrimSuffix(target, " (deleted)")
		if !strings.HasSuffix(clean, ".db") {
			continue
		}
		var st syscall.Stat_t
		var ino uint64
		if err := syscall.Stat(link, &st); err == nil {
			ino = st.Ino
		}
		return clean, ino, del, true
	}
	return "", 0, false, false
}

// collectBinding assembles the dbBinding for a pid by reading /proc. withStart
// includes the in-process start timestamp (only meaningful for self, where the
// processStart var is populated; for peer pids the caller leaves it false).
func collectBinding(pid int, withStart bool) dbBinding {
	b := dbBinding{PID: pid, BinaryPath: procExe(pid)}
	if withStart && !processStart.IsZero() {
		b.StartedAt = processStart.UTC().Format(time.RFC3339)
	}
	if path, inode, deleted, found := openDBHandle(pid); found {
		b.DBPath, b.DBInode, b.DBDeleted = path, inode, deleted
	} else {
		b.Note = "no open *.db handle (in-memory store, or DB not yet opened)"
	}
	return b
}

// mcpWhoamiDBHandler returns the handler for the tmux-tell.whoami_db MCP tool
// (#348): the live MCP server reports its own DB binding so an operator can ask
// "where are you actually writing?" without /proc archeology. Read-only — no DB
// access at all, just /proc on self — so it's safe to expose to peers and can't
// itself be misrouted by a stale binding.
func mcpWhoamiDBHandler() mcp.ToolHandler {
	return func(_ context.Context, _ json.RawMessage) (any, error) {
		return collectBinding(os.Getpid(), true), nil
	}
}
