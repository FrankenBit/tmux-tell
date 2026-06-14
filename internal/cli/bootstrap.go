package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// legacyDBPath is the pre-#308 system-global DB location. Used by the
// bootstrap's stale-DB-detect step to offer an in-place migration to
// the post-#308 user-home default.
const legacyDBPath = "/var/lib/tmux-msg/messages.db"

// bootstrapResult is the structured return shape for `bootstrap`.
//
// The JSON tags drive the wire shape; the text formatter renders the same
// struct as a step-prefixed transcript. Don't reconstruct this shape by
// hand in either path or the two outputs will drift — same single-source
// rule as dbMigrateResult.
type bootstrapResult struct {
	OK             bool     `json:"ok"`
	Migrated       bool     `json:"migrated"`
	Discovered     int      `json:"discovered"`
	MailmanEnabled int      `json:"mailman_enabled"`
	OrphansFound   int      `json:"orphans_found"`
	OrphansPruned  int      `json:"orphans_pruned"`
	McpsRefreshed  int      `json:"mcps_refreshed"`
	Warnings       []string `json:"warnings,omitempty"`
}

// runBootstrapCLI implements `tmux-msg-<adapter> bootstrap` — the
// substrate-honest hard-cut bootstrap path called by install.sh after
// the binary + systemd template land (#349 Fix 2).
//
// The operator-framing on 2026-06-12 (paraphrased: an install IS a hard
// cut; pieces fall into place, obsolete ones are dropped or demoted)
// shaped this as a one-call bootstrap that picks up every piece rather
// than printing a manual ritual the operator must remember.
//
// Six steps (order matters — each depends on the prior):
//
//  1. systemctl --user daemon-reload — pick up any template change.
//  2. Stale-DB detect + migrate — if the pre-#308 system-global
//     legacyDBPath exists AND the user-home default doesn't, run
//     `db migrate` (Fix 3) to move the data into place. If both exist,
//     abort: operator-resolution required.
//  3. Discover — populate `agents` from the current tmux state.
//  4. Enable + restart mailmen — `systemctl --user enable` + `restart` per
//     registered agent whose delivery_mode != hook-context. Restart (rather
//     than enable --now) is what makes an already-running mailman pick up a
//     freshly-installed binary; first-deploy-lane on alcatraz 2026-06-14
//     surfaced the substrate gap: enable --now is a no-op on an active unit,
//     so the mailman kept running its deleted-inode pre-install binary +
//     doctor flagged DIVERGENCE. See restartMailman in systemctl.go.
//  5. Orphan walk — walk USER_SYSTEMD for `<adapter>-mailman@<NAME>.service`
//     instance units whose <NAME> isn't in the freshly-discovered agents
//     table. Print by default; disable with --prune-orphans.
//  6. refresh-all-mcps — fire mcp-restart-tmux-msg per agent so chamber
//     MCPs rebind to the freshly-installed binary + canonical DB.
//
// Runs as the operator (install.sh drops privs before invoking). The
// systemd-dir override exists for tests + bespoke layouts; defaults to
// $HOME/.config/systemd/user.
func runBootstrapCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	pruneOrphans := fs.Bool("prune-orphans", false,
		"actively `systemctl --user disable --now` orphan mailman units (default: print only)")
	systemdDir := fs.String("systemd-dir", "",
		"override $HOME/.config/systemd/user (for tests + bespoke layouts)")
	format := fs.String("format", "text", "text|json")
	skipReload := fs.Bool("skip-daemon-reload", false,
		"skip the leading `systemctl --user daemon-reload` (for tests that mock systemctl)")
	skipDiscover := fs.Bool("skip-discover", false,
		"skip the discover step (for tests that pre-seed the agents table; reuse the existing rows as the post-discover set)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	ctx := context.Background()
	result := bootstrapResult{}

	// Step 1: daemon-reload.
	if !*skipReload {
		if out, err := systemctlRun(ctx, "daemon-reload"); err != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("systemctl --user daemon-reload: %v: %s", err, strings.TrimSpace(string(out))),
				exitUnavailable)
		}
	}

	// Step 2: stale-DB detect + migrate.
	resolvedDB := resolveDBPath(*dbPath)
	legacyExists := pathExists(legacyDBPath)
	defaultExists := pathExists(resolvedDB)
	switch {
	case legacyExists && defaultExists:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("ambiguous DB state: both legacy %s and default %s exist — operator-resolution required, refusing to choose",
				legacyDBPath, resolvedDB),
			exitDataErr)
	case legacyExists && !defaultExists:
		// Delegate to the db migrate sub-primitive (Fix 3). It handles
		// the WAL-safe stop → checkpoint → mv → sidecars → restart →
		// refresh cycle. The bootstrap's later steps (4 + 6) would be
		// no-ops after a migrate (already done), so we early-return on
		// successful migrate; orphan-walk + final refresh still need
		// running after a fresh discover, but a migrate path means the
		// agents table is already populated from the legacy DB.
		if rc := runDBMigrateCLI(
			[]string{"--db", legacyDBPath, "--format", "json", resolvedDB},
			io.Discard, stderr,
		); rc != exitOK {
			return writeJSONError(stdout, stderr,
				"db migrate from legacy DB failed; see stderr",
				rc)
		}
		result.Migrated = true
		fmt.Fprintf(stderr, "bootstrap: migrated %s → %s\n", legacyDBPath, resolvedDB)
	}

	// Open the canonical store for the remaining steps.
	s, err := store.Open(resolvedDB)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store at %s: %v", resolvedDB, err),
			exitInternal)
	}
	defer s.Close()

	// Step 3: discover.
	if !*skipDiscover {
		if rc := runDiscoverWithStore(ctx, s, discover.New(), false, false, "json", io.Discard, stderr); rc != exitOK {
			return writeJSONError(stdout, stderr,
				"discover failed; see stderr",
				rc)
		}
	}

	agents, err := s.ListAgents(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("list agents post-discover: %v", err),
			exitInternal)
	}
	result.Discovered = len(agents)

	// Step 4: enable + restart mailmen for non-hook-context agents.
	//
	// `systemctl --user enable` (without --now) persists the unit at boot.
	// `restart` then unconditionally cycles the process: starts the unit if
	// inactive, kills + respawns if active. The respawn is what makes an
	// already-running mailman pick up the freshly-installed binary inode
	// (#349 Fix 2 surfaced the gap on alcatraz 2026-06-14 first-deploy
	// smoke: enable --now alone left mailmen on the deleted-inode binary).
	for _, a := range agents {
		if a.DeliveryMode == store.DeliveryModeHookContext {
			continue
		}
		if out, err := systemctlRun(ctx, "enable", mailmanUnit(a.Name)); err != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("enable mailman for %s: %v: %s", a.Name, err, strings.TrimSpace(string(out))),
				exitUnavailable)
		}
		if err := restartMailman(ctx, a.Name); err != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("restart mailman for %s: %v", a.Name, err),
				exitUnavailable)
		}
		result.MailmanEnabled++
	}

	// Step 5: orphan walk.
	dir := resolveSystemdDir(*systemdDir)
	orphans, err := findOrphanMailmanUnits(dir, agents)
	if err != nil {
		// Non-fatal: orphan-walk failure shouldn't block the bootstrap.
		// Surface as a warning + continue.
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("orphan-walk skipped: %v", err))
	} else {
		result.OrphansFound = len(orphans)
		for _, unit := range orphans {
			if *pruneOrphans {
				agent := orphanInstanceName(unit)
				if err := stopMailman(ctx, agent); err != nil {
					return writeJSONError(stdout, stderr,
						fmt.Sprintf("disable orphan %s: %v", unit, err),
						exitUnavailable)
				}
				result.OrphansPruned++
				fmt.Fprintf(stderr, "bootstrap: disabled orphan %s\n", unit)
			} else {
				fmt.Fprintf(stderr, "bootstrap: orphan systemd unit (pass --prune-orphans to disable): %s\n", unit)
			}
		}
	}

	// Step 6: refresh-all-mcps.
	// Use a synthetic sender that's clearly attributable to the
	// bootstrap path (same naming convention as db-migrate's
	// "db-migrate"). Re-uses refresh-all-mcps' testable core; failures
	// surface on stderr.
	captureRefresh := &countingWriter{}
	rc := runRefreshAllMcpsWithStore(ctx, s, "bootstrap", "json", captureRefresh, stderr)
	if rc != exitOK {
		return writeJSONError(stdout, stderr,
			"refresh-all-mcps failed; see stderr",
			rc)
	}
	result.McpsRefreshed = len(agents)

	result.OK = true
	return emitBootstrapResult(stdout, result, *format)
}

// pathExists is a thin os.Stat / errors.Is wrapper for readability.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

// resolveSystemdDir returns the override if non-empty, else the
// operator's standard user-unit dir under $HOME.
func resolveSystemdDir(override string) string {
	if override != "" {
		return override
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "systemd", "user")
	}
	return ""
}

// findOrphanMailmanUnits returns the basenames of mailman instance units
// under dir whose instance-name isn't in agents. Only matches the
// currently-active adapter's prefix (e.g., tmux-msg-claude-mailman@) —
// cross-adapter walking is out of scope for v1.
//
// Skips the template unit itself (tmux-msg-claude-mailman@.service) and
// non-symlinks (template files), since instance enablement plants a
// symlink under default.target.wants and the operator-facing units
// directory holds the actual template file. The combination of
// "has-`@<instance>`-not-empty" + "is a symlink" pinpoints instance
// enablements specifically.
func findOrphanMailmanUnits(dir string, agents []store.Agent) ([]string, error) {
	if dir == "" {
		return nil, fmt.Errorf("systemd dir not set (HOME unset?); pass --systemd-dir to override")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No user-unit dir yet — fresh install, no orphans to find.
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	known := map[string]bool{}
	for _, a := range agents {
		known[a.Name] = true
	}

	prefix := active.BinaryName + "-mailman@"
	const suffix = ".service"
	var orphans []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		instance := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
		if instance == "" {
			// Template unit itself — not an instance, skip.
			continue
		}
		// Only consider entries that look like enablement-side symlinks.
		// The template file is a regular file; an instance enablement
		// shows up as a symlink (or in default.target.wants/, but we
		// don't walk subdirs in v1).
		info, statErr := e.Info()
		if statErr != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if !known[instance] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}

// orphanInstanceName extracts the agent name from a mailman instance
// unit filename: "tmux-msg-claude-mailman@alpha.service" → "alpha".
func orphanInstanceName(unitFile string) string {
	at := strings.Index(unitFile, "@")
	if at == -1 {
		return ""
	}
	rest := unitFile[at+1:]
	return strings.TrimSuffix(rest, ".service")
}

// emitBootstrapResult renders the result in the requested format.
func emitBootstrapResult(stdout io.Writer, r bootstrapResult, format string) int {
	switch format {
	case "json":
		_ = writeJSONResult(stdout, r)
	case "text", "":
		if r.Migrated {
			fmt.Fprintln(stdout, "MIGRATED\tlegacy DB moved into user-home default")
		}
		fmt.Fprintf(stdout, "DISCOVERED\t%d agents\n", r.Discovered)
		fmt.Fprintf(stdout, "MAILMEN\t%d enabled (hook-context skipped)\n", r.MailmanEnabled)
		fmt.Fprintf(stdout, "ORPHANS\t%d found, %d pruned\n", r.OrphansFound, r.OrphansPruned)
		fmt.Fprintf(stdout, "MCPS\t%d refresh macros queued\n", r.McpsRefreshed)
		for _, w := range r.Warnings {
			fmt.Fprintf(stdout, "WARN\t%s\n", w)
		}
		if r.OK {
			fmt.Fprintln(stdout, "OK\tbootstrap complete")
		}
	}
	return exitOK
}

// countingWriter discards bytes but tracks how many were written. Used
// to swallow refresh-all-mcps' structured-output noise on the
// bootstrap's text channel without losing track of "did it actually
// emit anything." Replaces a raw io.Discard for the refresh step so a
// later observability hook has a sniff point.
type countingWriter struct{ n int }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += len(p)
	return len(p), nil
}
