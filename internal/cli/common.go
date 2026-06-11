package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// defaultDBLocation resolves the default DB path under user-home, honoring the
// XDG Base Directory spec (#308): `$XDG_DATA_HOME/tmux-msg/messages.db` when
// $XDG_DATA_HOME is set, else `$HOME/.local/share/tmux-msg/messages.db`.
// Override with --db or $CLAUDE_MSG_DB. Tests use a temp file or :memory:.
//
// Moved from the system-global `/var/lib/tmux-msg/messages.db` (#308): tmux is
// per-user by design (sockets, panes, identity all run under the operator's
// UID), so the bus's trust boundary belongs under user-home — congruent with
// tmux's per-user model, no install-time shared-space chown, and writable by
// sandbox-by-default adapters (codex) without per-write escalation.
//
// Resolution is a pure function of the environment so a process and its
// systemd-managed mailman — both running under the same UID — resolve the same
// path. Caveat (#308): if the operator's interactive shell exports
// $XDG_DATA_HOME but the `systemctl --user` manager does not inherit it, the
// CLI and the mailman could diverge; in the common case (XDG_DATA_HOME unset,
// $HOME set everywhere) both land on `~/.local/share/tmux-msg/messages.db`.
//
// Degenerate fallback: if neither $XDG_DATA_HOME nor $HOME is set, returns a
// relative `.local/share/tmux-msg/messages.db` rather than erroring — store.Open
// surfaces a real failure if that path is unwritable, which is the honest signal.
func defaultDBLocation() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		if home := os.Getenv("HOME"); home != "" {
			dataHome = filepath.Join(home, ".local", "share")
		} else {
			dataHome = filepath.Join(".local", "share")
		}
	}
	return filepath.Join(dataHome, "tmux-msg", "messages.db")
}

// Caps — operator-chosen 2026-05-29 (see roadmap epic #1).
//
// Hard ceilings since #29. Enforcement lives inside InsertMessage /
// InsertMessagePair's BEGIN IMMEDIATE transaction, so the depth read
// and the INSERT are atomic with respect to other concurrent writers
// (cross-process safe via _txlock=immediate in the store's DSN, and
// trivially safe within a process via SetMaxOpenConns(1)). N concurrent
// senders can never overshoot the cap — at most `capRecipientQueue`
// inserts succeed regardless of concurrency.
//
// capSenderBacklog is scoped per-(sender, recipient) since #296: it is
// the most queued rows a single sender may hold in one recipient's
// queue, a fairness slice of capRecipientQueue (2 of 5) so one sender
// can't monopolise a mailbox. It is NOT a global per-sender outbound
// ceiling — a sender blocked at one slow recipient can still reach all
// others.
const (
	capRecipientQueue       = 5
	capSenderBacklog        = 2
	capBodyBytes            = 16 * 1024
	capMaxRecipientsPerSend = 10
)

// Exit codes follow sysexits.h. See cmd/tmux-msg-claude/main.go for the
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
	return defaultDBLocation()
}

// dbPathSource returns a label describing how resolveDBPath resolved the
// active DB path — used in the startup observability log (#290) so operators
// can confirm at a glance which DB a process is bound to.
func dbPathSource(flagValue string) string {
	if flagValue != "" {
		return "flag(--db)"
	}
	if os.Getenv("CLAUDE_MSG_DB") != "" {
		return "env(CLAUDE_MSG_DB)"
	}
	return "default(env unset)"
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
