package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// runDBCLI is the umbrella for the `db` subcommand family. Today there's
// one verb (`migrate`); future verbs (`vacuum`, `dump`, `restore`) layer
// in here.
//
// Usage: tmux-msg-claude db <verb> [args]
func runDBCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "usage: %s db <verb> [args]\n", active.BinaryName)
		fmt.Fprintln(stderr, "verbs:")
		fmt.Fprintln(stderr, "  migrate   Move the canonical DB to a new path (#349)")
		return exitUsage
	}
	switch args[0] {
	case "migrate":
		return runDBMigrateCLI(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "%s db: unknown verb %q\n", active.BinaryName, args[0])
		return exitUsage
	}
}

// dbMigrateResult is the structured return shape for `db migrate`.
//
// The JSON tags drive the wire shape; the text formatter renders the same
// struct as a step-prefixed transcript. Don't reconstruct this shape by
// hand in either path or the two outputs will drift.
type dbMigrateResult struct {
	OK            bool     `json:"ok"`
	DryRun        bool     `json:"dry_run"`
	Source        string   `json:"source"`
	Dest          string   `json:"dest"`
	AgentsStopped int      `json:"agents_stopped"`
	AgentsStarted int      `json:"agents_started"`
	Agents        int      `json:"agents"`
	Messages      int      `json:"messages"`
	Warnings      []string `json:"warnings,omitempty"`
}

// runDBMigrateCLI implements `tmux-msg-claude db migrate <new-path>`.
//
// The 8-step atomic migration helper (#349 Fix 3):
//
//  1. Validate destination parent dir exists + writable
//  2. systemctl --user stop 'tmux-msg-*-mailman@*.service' (per-agent)
//  3. PRAGMA wal_checkpoint(TRUNCATE) on the source DB
//  4. mv source → destination
//  5. Delete source -wal and -shm sidecars
//  6. systemctl --user start 'tmux-msg-*-mailman@*.service' (per-agent)
//  7. tmux-msg-claude refresh-all-mcps (against the dest DB)
//  8. Self-verify: open destination DB, count rows
//
// With --dry-run the command prints the plan it would execute and exits 0
// without touching the filesystem or systemd. The plan reflects the source
// + destination paths the runtime would actually use.
//
// Caveat (operator-facing): if `$CLAUDE_MSG_DB` is set in the operator's
// shell profile, the command emits a "update your shell profile" warning
// at the end — it can't rewrite shell rc files itself.
//
// Bespoke-destination handling: if the destination doesn't match
// `defaultDBLocation()`, steps 6 + 7 are skipped with a warning. The
// systemd-managed mailmen + chamber MCPs both resolve the default DB path
// on their own (#308 dropped the unit-file Environment= override), so
// pointing them at a bespoke path requires foreground `serve` + manual
// MCP restart on the operator side.
func runDBMigrateCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("db migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "source DB path (env: CLAUDE_MSG_DB; default: user-home XDG path)")
	dryRun := fs.Bool("dry-run", false, "print the migration plan and exit without executing")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return writeJSONError(stdout, stderr,
			"db migrate requires exactly one positional argument: <new-path>",
			exitUsage)
	}
	dest, err := filepath.Abs(rest[0])
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("resolve destination path: %v", err), exitUsage)
	}
	source, err := filepath.Abs(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("resolve source path: %v", err), exitUsage)
	}

	if source == dest {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("source and destination are the same path: %s", source),
			exitUsage)
	}

	result := dbMigrateResult{DryRun: *dryRun, Source: source, Dest: dest}

	// Step 1: validate destination.
	if err := validateMigrateDest(dest); err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitDataErr)
	}

	// Read agents up-front so steps 2 + 6 iterate the same set even if the
	// destination DB is somehow stale.
	ctx := context.Background()
	srcStore, err := store.Open(source)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open source DB: %v", err), exitInternal)
	}
	agents, err := srcStore.ListAgents(ctx)
	if err != nil {
		_ = srcStore.Close()
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("list agents: %v", err), exitInternal)
	}
	result.Agents = len(agents)

	// Filter to mailman-bearing agents (skip hook-context — their mailman
	// unit isn't enabled by design).
	mailmanAgents := make([]store.Agent, 0, len(agents))
	for _, a := range agents {
		if a.DeliveryMode != store.DeliveryModeHookContext {
			mailmanAgents = append(mailmanAgents, a)
		}
	}

	// Whether the destination matches the default the binary computes on
	// its own. Drives the warning + skip-restart behavior below.
	destIsDefault := dest == defaultDBLocation()
	if !destIsDefault {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("destination %s does not match default %s — skipping mailman restart + refresh-all-mcps (#308 dropped the unit-file Environment= override, so systemd-managed mailmen resolve only the default path). Start mailmen as foreground `%s serve --agent NAME` and restart chamber MCPs manually.",
				dest, defaultDBLocation(), active.BinaryName))
	}
	if os.Getenv("CLAUDE_MSG_DB") != "" {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("$CLAUDE_MSG_DB is set in this shell — update your shell profile to %s (this command cannot rewrite rc files).",
				dest))
	}

	if *dryRun {
		_ = srcStore.Close()
		result.OK = true
		return emitDBMigrateResult(stdout, result, *format, true)
	}

	// Close the source store before systemctl-stop + mv so we don't hold a
	// connection on the inode we're about to rename.
	if err := srcStore.Close(); err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("close source DB before move: %v", err), exitInternal)
	}

	// Step 2: stop mailmen. Iterates the per-agent surface so the systemd
	// glob doesn't depend on shell behavior. stopMailman is idempotent for
	// not-loaded / not-running, so the count is best-effort.
	for _, a := range mailmanAgents {
		if err := stopMailman(ctx, a.Name); err != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("stop mailman for %s: %v", a.Name, err),
				exitUnavailable)
		}
		result.AgentsStopped++
	}

	// Step 3: WAL checkpoint TRUNCATE on the source.
	if err := checkpointTruncate(ctx, source); err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("wal_checkpoint(TRUNCATE) on source: %v", err),
			exitInternal)
	}

	// Step 4: move the file. Falls back to copy+delete if rename fails with
	// EXDEV (cross-volume).
	if err := moveFile(source, dest); err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("move %s → %s: %v", source, dest, err),
			exitInternal)
	}

	// Step 5: remove sidecars. Best-effort — absence is fine.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(source + suffix)
	}

	if !destIsDefault {
		// Skip steps 6 + 7 per the warning above. The operator has the
		// surface to drive both manually.
		result.OK = true
		return emitDBMigrateResult(stdout, result, *format, true)
	}

	// Step 6: restart mailmen against the destination (which IS the
	// default systemd resolves).
	for _, a := range mailmanAgents {
		if err := startMailman(ctx, a.Name); err != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("start mailman for %s: %v", a.Name, err),
				exitUnavailable)
		}
		result.AgentsStarted++
	}

	// Step 7: refresh chamber MCPs against the new DB. Open the dest
	// store + reuse refresh-all-mcps's testable core.
	destStore, err := store.Open(dest)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open dest DB for refresh-all-mcps: %v", err),
			exitInternal)
	}
	// Self-verify (step 8) runs against the open dest store; do it before
	// the refresh-all-mcps emission so a corrupt move surfaces sooner.
	if err := destStore.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages").Scan(&result.Messages); err != nil {
		_ = destStore.Close()
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("verify dest message count: %v", err),
			exitInternal)
	}

	// refresh-all-mcps needs a sender identity. Use a synthetic sender
	// that's clearly attributable to this command — operator-explicit, not
	// an existing chamber. The control rows the macro creates carry this
	// in their sender field, so the chamber-side journal makes the source
	// obvious.
	if rc := runRefreshAllMcpsWithStore(ctx, destStore, "db-migrate", "json", io.Discard, stderr); rc != exitOK {
		_ = destStore.Close()
		return writeJSONError(stdout, stderr,
			"refresh-all-mcps after move failed; see stderr for per-agent detail",
			exitInternal)
	}
	if err := destStore.Close(); err != nil {
		// Non-fatal: the move + refresh-all-mcps emission succeeded.
		fmt.Fprintf(stderr, "WARN db migrate: close dest DB: %v\n", err)
	}

	result.OK = true
	return emitDBMigrateResult(stdout, result, *format, false)
}

// validateMigrateDest checks that the destination path is usable for the
// migrate target: parent dir exists, parent is writable, no existing file
// at the destination.
//
// Why no MkdirAll: an operator-typo destination ("/srvm/...") shouldn't
// silently materialize a new tree. Refuse + surface the path. Operator
// can pre-create the parent dir if they meant it.
func validateMigrateDest(dest string) error {
	parent := filepath.Dir(dest)
	info, err := os.Stat(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("destination parent dir does not exist: %s", parent)
		}
		return fmt.Errorf("stat destination parent dir %s: %w", parent, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("destination parent path is not a directory: %s", parent)
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("destination already exists, refusing to overwrite: %s", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat destination %s: %w", dest, err)
	}
	// Writability probe: create + remove a tempfile in parent.
	probe, err := os.CreateTemp(parent, ".tmux-msg-migrate-probe-*")
	if err != nil {
		return fmt.Errorf("destination parent %s not writable: %w", parent, err)
	}
	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)
	return nil
}

// checkpointTruncate opens the DB at path long enough to issue
// `PRAGMA wal_checkpoint(TRUNCATE)` and then closes it. The pragma
// flushes the WAL into the main file and shrinks the WAL file to zero
// length, so the subsequent rename moves a self-contained DB.
func checkpointTruncate(ctx context.Context, path string) error {
	s, err := store.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer s.Close()
	if _, err := s.DB().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("exec pragma: %w", err)
	}
	return nil
}

// moveFile renames source → dest, falling back to copy+remove if the
// rename crosses a filesystem boundary (EXDEV). The fallback preserves
// permissions; the source is removed only after the copy completes
// cleanly so a mid-copy failure leaves the original intact.
func moveFile(source, dest string) error {
	if err := os.Rename(source, dest); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) && !strings.Contains(err.Error(), "cross-device") {
		return err
	}
	// Cross-device fallback.
	src, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	srcStat, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}
	dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, srcStat.Mode())
	if err != nil {
		return fmt.Errorf("create dest: %w", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("copy: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("close dest: %w", err)
	}
	if err := os.Remove(source); err != nil {
		return fmt.Errorf("remove source after cross-device copy: %w", err)
	}
	return nil
}

// emitDBMigrateResult renders the result in the requested format.
// Returns exitOK on success.
func emitDBMigrateResult(stdout io.Writer, r dbMigrateResult, format string, partial bool) int {
	switch format {
	case "json":
		_ = writeJSONResult(stdout, r)
	case "text", "":
		if r.DryRun {
			fmt.Fprintln(stdout, "DRY RUN — would execute:")
		}
		fmt.Fprintf(stdout, "SOURCE\t%s\n", r.Source)
		fmt.Fprintf(stdout, "DEST\t%s\n", r.Dest)
		fmt.Fprintf(stdout, "AGENTS\t%d\n", r.Agents)
		if !r.DryRun {
			fmt.Fprintf(stdout, "STOPPED\t%d mailmen\n", r.AgentsStopped)
			if partial {
				fmt.Fprintln(stdout, "STARTED\t0 mailmen (skipped — see warnings)")
			} else {
				fmt.Fprintf(stdout, "STARTED\t%d mailmen\n", r.AgentsStarted)
				fmt.Fprintf(stdout, "MESSAGES\t%d rows in moved DB\n", r.Messages)
			}
		}
		for _, w := range r.Warnings {
			fmt.Fprintf(stdout, "WARN\t%s\n", w)
		}
		if r.OK && !r.DryRun {
			fmt.Fprintln(stdout, "OK\tmigration complete")
		}
	}
	return exitOK
}
