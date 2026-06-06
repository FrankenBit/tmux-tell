// Package main is the claude-msg CLI entrypoint.
//
// Subcommand dispatcher only. Each subcommand handler lives in its own file
// (send.go, inbox.go, status.go, agents.go, whoami.go, …) and is split into
// runFooCLI (flag parsing + store open) and runFooWithStore (pure logic,
// testable). See the README for the project shape.
package main

import (
	"fmt"
	"os"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/version"
)

const usage = `usage: claude-msg <subcommand> [args]

Subcommands:
  send    Queue a message for an agent (validates caps, returns JSON)
  control Send a whitelisted slash-command to a pane (mirrors tmux-msg.control)
  track   Show the delivery state of a single message by its public_id
  get     Fetch a processed message by ID (recovery for swallowed deliveries, #111)
  inbox   List queued messages for an agent
  status  Show paused state + queue depths across all agents (--today for journal-sourced today counts)
  stats   On-demand bus-traffic aggregates from the local DB (per-agent counts, latency, top pairs)
  health  One-command per-agent health audit from journalctl + systemd (#42)
  config  Read/show the host-level config (#54). Subcommands: show
  agents  List registered agents with pane liveness
  whoami  Show this session's registration (auto-resolves identity)
  register Register this (or another) pane on the bus (mirrors tmux-msg.register; #116)
  serve   Run the mailman daemon for one agent
  pause   Halt one or all mailman daemons
  resume  Resume paused mailmen
  reset   Purge messages (requires --confirm)
  log     Inspect message threads
  discover Re-derive agents.pane_id from current tmux state
  state   Probe a agent's current activity via read-only capture-pane (#71)
  refresh-all-mcps  Bulk-fire mcp-restart-tmux-msg to every registered agent (#62)
  mcp     Speak MCP over stdio (Claude Code tools)

See https://git.frankenbit.de/frankenbit/tmux-msg for the design notes.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entrypoint. It returns the exit code.
func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}

	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case "-v", "--version", "version":
		fmt.Fprintf(stdout, "claude-msg %s\n", version.Version)
		return exitOK
	case "send":
		return runSendCLI(args[1:], stdout, stderr)
	case "control":
		return runControlCLI(args[1:], stdout, stderr)
	case "track":
		return runTrackCLI(args[1:], stdout, stderr)
	case "get":
		return runGetCLI(args[1:], stdout, stderr)
	case "inbox":
		return runInboxCLI(args[1:], stdout, stderr)
	case "status":
		return runStatusCLI(args[1:], stdout, stderr)
	case "stats":
		return runStatsCLI(args[1:], stdout, stderr)
	case "health":
		return runHealthCLI(args[1:], stdout, stderr)
	case "config":
		return runConfigCLI(args[1:], stdout, stderr)
	case "agents":
		return runAgentsCLI(args[1:], stdout, stderr)
	case "whoami":
		return runWhoamiCLI(args[1:], stdout, stderr)
	case "register":
		return runRegisterCLI(args[1:], stdout, stderr)
	case "serve":
		return runServeCLI(args[1:], stdout, stderr)
	case "pause":
		return runPauseCLI(args[1:], true, stdout, stderr)
	case "resume":
		return runPauseCLI(args[1:], false, stdout, stderr)
	case "reset":
		return runResetCLI(args[1:], stdout, stderr)
	case "log":
		return runLogCLI(args[1:], stdout, stderr)
	case "discover":
		return runDiscoverCLI(args[1:], stdout, stderr)
	case "state":
		return runStateCLI(args[1:], stdout, stderr)
	case "refresh-all-mcps":
		return runRefreshAllMcpsCLI(args[1:], stdout, stderr)
	case "mcp":
		return runMCPCLI(args[1:], os.Stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "claude-msg: unknown subcommand %q\n\n%s", args[0], usage)
		return exitUsage
	}
}
