package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// runUnregisterCLI implements `tmux-tell-claude unregister`.
//
// Usage: tmux-tell-claude unregister --name <agent>
//
//	[--purge-queue] [--force] [--db <path>]
//
// Stops the agent's mailman (idempotent if not running), then removes the
// agent row. By default, queued messages for the agent are preserved — they
// deliver if the agent is re-registered later. --purge-queue drops them.
// --force bypasses the queued-message guard that otherwise fails loudly when
// the agent has pending mail (#289).
//
// Idempotent: unregistering an already-absent agent returns ok:true with
// removed:false — parallel to mkdir -p for cleanup scripts.
func runUnregisterCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("unregister", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	name := fs.String("name", "", "agent name to remove from the registry; required")
	purgeQueue := fs.Bool("purge-queue", false,
		"drop queued messages addressed to this agent (default: preserve for re-registration)")
	force := fs.Bool("force", false,
		"override the queued-message guard (otherwise fails if the agent has pending mail)")

	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if *name == "" {
		return writeJSONError(stdout, stderr, "--name required", exitUsage)
	}

	resolvedDB := resolveDBPath(*dbPath)
	s, err := store.Open(resolvedDB)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	ctx := context.Background()

	// Idempotent: not-found is success with removed:false.
	existing, err := s.GetAgent(ctx, *name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("lookup: %v", err), exitInternal)
	}
	if existing == nil {
		_ = writeJSONResult(stdout, map[string]any{
			"ok":      true,
			"name":    *name,
			"removed": false,
		})
		return exitOK
	}

	// Queue-depth guard: refuse if messages are queued unless --force.
	if !*force {
		depth, err := s.RecipientQueueDepth(ctx, *name)
		if err != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("queue depth: %v", err), exitInternal)
		}
		if depth > 0 {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("agent %q has %d queued message(s); pass --force to override",
					*name, depth),
				exitDataErr)
		}
	}

	// Stop the mailman before removing the row so it doesn't observe a
	// dangling agent reference. stopMailman runs `systemctl --user disable
	// --now`, which is idempotent for not-loaded/missing units. A hard error
	// here (e.g. systemd-not-available, full disk, broken user manager) is
	// soft-failed so the DB row removal still proceeds — the substrate-honest
	// framing per #338 is that the agents-table row is authoritative state
	// and the systemd unit is a downstream consumer. A surviving unit gets
	// noticed by #340's serve-exit-on-missing-agent path; leaving the DB row
	// behind because systemctl flaked would be worse.
	mailmanStatus := "stopped"
	var mailmanErr string
	if err := stopMailman(ctx, *name); err != nil {
		mailmanStatus = "warn"
		mailmanErr = err.Error()
		fmt.Fprintf(stderr, "WARN unregister: stop mailman: %v\n", err)
	}

	var purged int64
	if *purgeQueue {
		n, err := s.DeleteMessages(ctx, *name, []store.State{store.StateQueued})
		if err != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("purge queue: %v", err), exitInternal)
		}
		purged = n
	}

	removed, err := s.DeleteAgent(ctx, *name)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("delete agent: %v", err), exitInternal)
	}

	out := map[string]any{
		"ok":      true,
		"name":    *name,
		"removed": removed,
		"mailman": mailmanStatus,
		"deleted": purged,
	}
	if mailmanErr != "" {
		out["mailman_error"] = mailmanErr
	}
	_ = writeJSONResult(stdout, out)
	return exitOK
}
