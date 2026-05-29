package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Default DB path for production installs. Override with --db or
// $CLAUDE_MSG_DB. Tests use a temp file or :memory:.
const defaultDBLocation = "/var/lib/cli-semaphore/messages.db"

// Caps — operator-chosen 2026-05-29 (see roadmap epic #1).
const (
	capRecipientQueue = 5
	capSenderBacklog  = 2
	capBodyBytes      = 16 * 1024
)

// Exit codes follow sysexits.h. See cmd/claude-msg/main.go for the
// project-wide mapping.
const (
	exitOK          = 0
	exitUsage       = 64
	exitDataErr     = 65
	exitUnavailable = 69
	exitInternal    = 70
	exitTempFail    = 75
)

// resolveDBPath returns the path to use for store.Open, honouring:
//
//  1. The explicit flag value if non-empty.
//  2. The CLAUDE_MSG_DB env var.
//  3. The hard-coded default.
func resolveDBPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("CLAUDE_MSG_DB"); env != "" {
		return env
	}
	return defaultDBLocation
}

// resolveAgentName picks a sender / self identity from (in order):
// the explicit flag, the $CLAUDE_AGENT_NAME env var. Returns "" if neither
// is set; the caller decides whether that's a usage error.
func resolveAgentName(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("CLAUDE_AGENT_NAME")
}

// writeJSONResult writes the given value to w as a single line of JSON.
// Returns the error from the encoder, if any (caller usually ignores it
// since we're at the end of a CLI run).
func writeJSONResult(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// writeJSONError writes a {"ok":false,"error":msg} object to stdout and a
// human-readable line to stderr, returning the supplied exit code.
//
// Centralises the {ok:false} contract so every subcommand returns the same
// shape on failure.
func writeJSONError(stdout, stderr io.Writer, msg string, exit int) int {
	_ = writeJSONResult(stdout, map[string]any{"ok": false, "error": msg})
	fmt.Fprintln(stderr, msg)
	return exit
}

// renderTextTable writes a tab-separated table to w. The first row is the
// header, subsequent rows are the data. Callers run the result through
// `column -t` for pretty alignment.
func renderTextTable(w io.Writer, header []string, rows [][]string) {
	fmt.Fprintln(w, joinTabs(header))
	for _, r := range rows {
		fmt.Fprintln(w, joinTabs(r))
	}
}

func joinTabs(cells []string) string {
	out := ""
	for i, c := range cells {
		if i > 0 {
			out += "\t"
		}
		out += c
	}
	return out
}

// shortBody returns at most n runes of body for table display.
func shortBody(body string, n int) string {
	runes := []rune(body)
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n]) + "…"
}

// ensureParentDir is a small helper for the (rare) case where the CLI
// itself wants to mkdir the DB parent — Open already does this, but keeping
// the helper around documents the assumption.
func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	return os.MkdirAll(dir, 0o755)
}

var _ = ensureParentDir // unused right now; keeps the symbol for callers
