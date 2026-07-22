package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Substrate data-dir names (#440 Phase 3 rename). dataSubdir is the canonical
// tmux-tell name; legacyDataSubdir is the deprecated tmux-msg name kept as a
// lazy-migration fallback through v1.0 per ADR-0008.
const (
	dataSubdir       = "tmux-tell"
	legacyDataSubdir = "tmux-msg"

	// Env-var names (#440 Phase 3). The TMUX_TELL_* form is canonical; the
	// CLAUDE_MSG_* form is the deprecated alias honored through v1.0.
	envDB           = "TMUX_TELL_DB"
	legacyEnvDB     = "CLAUDE_MSG_DB"
	envConfig       = "TMUX_TELL_CONFIG"
	legacyEnvConfig = "CLAUDE_MSG_CONFIG"

	// deprecatedRemovalVersion is the ADR-0008 §Discretion removal boundary for
	// every Phase-3 legacy surface (env vars, paths, binary aliases).
	deprecatedRemovalVersion = "v1.0"
)

// fileExists reports whether path names an existing filesystem entry.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// xdgDataHome resolves $XDG_DATA_HOME (else $HOME/.local/share, else a relative
// fallback) — the per-user data root the bus DB lives under (#308).
func xdgDataHome() string {
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return dataHome
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "share")
	}
	return filepath.Join(".local", "share")
}

// defaultDataDir / legacyDataDir name the canonical + deprecated per-user data
// directories — surfaced verbatim in the migration WARN's `mv` recipe (#440).
func defaultDataDir() string { return filepath.Join(xdgDataHome(), dataSubdir) }
func legacyDataDir() string  { return filepath.Join(xdgDataHome(), legacyDataSubdir) }

// defaultDBLocationResolved resolves the default DB path under user-home,
// honoring the XDG Base Directory spec (#308): `$XDG_DATA_HOME/tmux-tell/
// messages.db` when $XDG_DATA_HOME is set, else `$HOME/.local/share/tmux-tell/
// messages.db`. Override with --db, $TMUX_TELL_DB, or (deprecated) $CLAUDE_MSG_DB.
//
// Lazy migration (#440 Phase 3): an in-place operator who upgraded but hasn't
// moved their data keeps working on the legacy `…/tmux-msg/messages.db` path —
// returned (with legacy=true) only when the tmux-tell path does NOT yet exist
// AND the legacy one does. A fresh install lands on tmux-tell; the operator
// migrates with `mv` (the WARN names it). Hard-cut to new-only at v1.0.
//
// Resolution is a pure function of the filesystem + environment so a process and
// its systemd-managed mailman — both under the same UID — resolve the same path.
// Degenerate fallback (neither $XDG_DATA_HOME nor $HOME set): a relative
// `.local/share/tmux-tell/messages.db` rather than erroring — store.Open
// surfaces a real failure if unwritable, the honest signal.
func defaultDBLocationResolved() (path string, legacy bool) {
	newPath := filepath.Join(defaultDataDir(), "messages.db")
	legacyPath := filepath.Join(legacyDataDir(), "messages.db")
	if !fileExists(newPath) && fileExists(legacyPath) {
		return legacyPath, true
	}
	return newPath, false
}

// defaultDBLocation is the path-only view of defaultDBLocationResolved.
func defaultDBLocation() string {
	p, _ := defaultDBLocationResolved()
	return p
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

// Exit codes follow sysexits.h. See cmd/tmux-tell-claude/main.go for the
// project-wide mapping.
const (
	exitOK          = 0
	exitUsage       = 64
	exitDataErr     = 65
	exitUnavailable = 69
	exitInternal    = 70
	// exitDoctorIdleStaleOnly is the doctor-specific #797 signal that ALL
	// divergences are idle-stale-only (chamber-side MCPs closable by
	// refresh-all-mcps). Distinct from exitUnavailable (real divergence,
	// requires manual intervention) so deploy.yml can `::warning::` this
	// class instead of hard-failing.
	//
	// Sysexits.h assigns 71 = EX_OSERR — semantic fit is loose (this isn't an
	// OS error) but 70 (EX_SOFTWARE / exitInternal) is already taken and a
	// distinct code is required so deploy.yml doesn't conflate idle-stale
	// with an internal doctor error (which must STAY hard-fail). The constant
	// name is the authoritative semantics; the number is the next-unused
	// sysexits slot that avoids the internal-error collision.
	//
	// #797: land this alongside its deploy.yml case-switch consumer per
	// Engineer's `feedback_check_what_consumes_the_emitted_BEFORE_shipping_the_emitter`
	// discipline — no half-shipped emitter.
	exitDoctorIdleStaleOnly = 71
	exitTempFail            = 75
)

// resolveDBPath returns the path to use for store.Open, honouring:
//
//  1. The explicit flag value if non-empty.
//  2. $TMUX_TELL_DB, then the deprecated $CLAUDE_MSG_DB (#440 Phase 3).
//  3. The default location (with lazy legacy fallback).
//
// Pure (no WARN): the deprecation-comm surface for $CLAUDE_MSG_DB + the legacy
// data path lives in Run's warnIf* cluster (deprecation.go), so this stays a
// pure path resolver its ~many store-open callers can use without stderr.
func resolveDBPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv(envDB); env != "" {
		return env
	}
	if env := os.Getenv(legacyEnvDB); env != "" {
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
	if os.Getenv(envDB) != "" {
		return "env(" + envDB + ")"
	}
	if os.Getenv(legacyEnvDB) != "" {
		return "env(" + legacyEnvDB + ")"
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
