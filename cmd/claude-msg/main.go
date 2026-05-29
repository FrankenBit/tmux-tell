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
)

const usage = `usage: claude-msg <subcommand> [args]

Subcommands:
  send    Queue a message for an agent (validates caps, returns JSON)
  inbox   List queued messages for an agent
  status  Show paused state + queue depths across all agents
  agents  List registered agents with pane liveness
  whoami  Show this session's registration (uses $CLAUDE_AGENT_NAME)
  serve   Run the mailman daemon for one agent (not yet implemented)
  pause   Halt one or all mailman daemons (not yet implemented)
  resume  Resume paused mailmen (not yet implemented)
  reset   Purge messages (not yet implemented)
  log     Inspect message threads (not yet implemented)
  mcp     Speak MCP over stdio (not yet implemented)

See https://git.frankenbit.de/frankenbit/cli-semaphore for the design notes.
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
	case "send":
		return runSendCLI(args[1:], stdout, stderr)
	case "inbox":
		return runInboxCLI(args[1:], stdout, stderr)
	case "status":
		return runStatusCLI(args[1:], stdout, stderr)
	case "agents":
		return runAgentsCLI(args[1:], stdout, stderr)
	case "whoami":
		return runWhoamiCLI(args[1:], stdout, stderr)
	case "serve", "pause", "resume", "reset", "log", "discover", "mcp":
		fmt.Fprintf(stderr, "claude-msg %s: not yet implemented — see the roadmap epic\n", args[0])
		return exitInternal
	default:
		fmt.Fprintf(stderr, "claude-msg: unknown subcommand %q\n\n%s", args[0], usage)
		return exitUsage
	}
}
