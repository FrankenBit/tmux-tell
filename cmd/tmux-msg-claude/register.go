package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// runRegisterCLI parses register-subcommand flags and dispatches to the
// shared register pipeline. Mirrors the `tmux-msg.register` MCP tool so
// operators-at-a-bare-shell can register their own pane without needing
// an MCP client (load-bearing for the operator-as-bus-participant use
// case per #116).
//
// Usage: claude-msg register --name <name> [--pane <pane>]
//
//	[--delivery-mode paste-and-enter|mailbox-only]
//	[--start-mailman=true|false] [--force]
//	[--alias <alias>]
//
// Mailman lifecycle default: start_mailman defaults true UNLESS
// delivery_mode is `mailbox-only` (then default false — no daemon
// needed since mailbox-only agents never receive a paste). Explicit
// --start-mailman=true overrides.
func runRegisterCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	name := fs.String("name", "", "agent name (the new identity); required")
	pane := fs.String("pane", "", "pane id like %5 (default: $TMUX_PANE)")
	deliveryMode := fs.String("delivery-mode", store.DeliveryModePasteAndEnter,
		"how the mailman delivers to this agent: 'paste-and-enter' (default) or 'mailbox-only' (operator-as-bus-participant per #116; messages stay queued, operator polls via inbox)")
	startMailmanFlag := fs.String("start-mailman", "",
		"true|false — start the mailman daemon for this agent. Default: true (mailbox-only defaults to false; explicit true overrides). Note: --start-mailman=true combined with --delivery-mode=mailbox-only is allowed but vestigial — the daemon will start, observe the mailbox-only mode at startup, log the no-work condition, and exit cleanly with Result=success. The 'mailman: active' field in the response is momentary in this case.")
	force := fs.Bool("force", false,
		"overwrite an existing registration with the same name")
	alias := fs.String("alias", "",
		"optional alternative name the discover walker should accept for this canonical agent")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	if *name == "" {
		return writeJSONError(stdout, stderr, "--name required", exitUsage)
	}
	resolvedPane := *pane
	if resolvedPane == "" {
		resolvedPane = os.Getenv("TMUX_PANE")
	}
	if resolvedPane == "" {
		return writeJSONError(stdout, stderr,
			"pane required: pass --pane or run inside tmux with $TMUX_PANE set",
			exitUsage)
	}
	if !store.ValidDeliveryMode(*deliveryMode) {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("invalid --delivery-mode %q (want %q or %q)",
				*deliveryMode, store.DeliveryModePasteAndEnter, store.DeliveryModeMailboxOnly),
			exitUsage)
	}

	// Mailman-start default depends on delivery_mode. Explicit flag
	// override beats the implicit default.
	start := true
	if *deliveryMode == store.DeliveryModeMailboxOnly {
		start = false
	}
	if *startMailmanFlag != "" {
		switch *startMailmanFlag {
		case "true":
			start = true
		case "false":
			start = false
		default:
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("invalid --start-mailman %q (want true|false)", *startMailmanFlag),
				exitUsage)
		}
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	existing, err := s.GetAgent(ctx, *name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("lookup: %v", err), exitInternal)
	}
	if existing != nil && !*force {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("agent %q already registered with pane %s; pass --force to overwrite",
				*name, existing.PaneID), exitDataErr)
	}

	if err := s.UpsertAgent(ctx, *name, resolvedPane); err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("upsert: %v", err), exitInternal)
	}
	if err := s.SetDeliveryMode(ctx, *name, *deliveryMode); err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("set delivery_mode: %v", err), exitInternal)
	}
	if *alias != "" {
		if err := s.AddAlias(ctx, *name, *alias); err != nil {
			if errors.Is(err, store.ErrAliasCollision) {
				return writeJSONError(stdout, stderr,
					fmt.Sprintf("alias %q rejected: %v", *alias, err),
					exitDataErr)
			}
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("add alias: %v", err), exitInternal)
		}
	}

	out := map[string]any{
		"ok":            true,
		"name":          *name,
		"pane":          resolvedPane,
		"delivery_mode": *deliveryMode,
		"registered":    true,
	}
	if start {
		if err := startMailman(ctx, *name); err != nil {
			out["mailman"] = "failed"
			out["mailman_error"] = err.Error()
		} else {
			out["mailman"] = "active"
		}
	} else {
		out["mailman"] = "skipped"
	}
	_ = writeJSONResult(stdout, out)
	return exitOK
}
