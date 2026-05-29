// Package main is the claude-msg CLI entrypoint.
//
// Subcommand dispatcher only. Each subcommand handler will land in its own
// file (send.go, inbox.go, serve.go, …) once the corresponding milestone
// issue is picked up. The shape mirrors the README's "CLI shape" section.
package main

import (
	"fmt"
	"os"
)

// sysexits-style exit codes used by send + future subcommands.
const (
	exitOK          = 0
	exitUsage       = 64 // bad arguments / unknown subcommand
	exitDataErr     = 65 // malformed input (e.g. empty body)
	exitUnavailable = 69 // recipient unknown / agent registry miss
	exitTempFail    = 75 // queue cap exceeded (transient — retry later)
	exitNotImpl     = 70 // stub
)

const usage = `usage: claude-msg <subcommand> [args]

Subcommands:
  send    Queue a message for an agent (validates caps, returns JSON)
  inbox   List queued messages for an agent
  serve   Run the mailman daemon for one agent
  status  Show paused state + queue depths
  pause   Halt one or all mailman daemons
  resume  Resume paused mailmen
  reset   Purge messages (requires --confirm)
  log     Inspect message threads

See https://git.frankenbit.de/frankenbit/cli-semaphore for the design notes.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitUsage)
	}

	switch os.Args[1] {
	case "send":
		os.Exit(runSend(os.Args[2:]))
	case "inbox", "serve", "status", "pause", "resume", "reset", "log":
		fmt.Fprintf(os.Stderr, "claude-msg %s: not yet implemented — see the roadmap epic\n", os.Args[1])
		os.Exit(exitNotImpl)
	case "-h", "--help", "help":
		fmt.Print(usage)
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "claude-msg: unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(exitUsage)
	}
}

// runSend is the stub for the M2.3 implementation. It validates argument
// shape so the contract surfaces in `claude-msg send --help` even before
// the store backend lands.
func runSend(args []string) int {
	// TODO(M2.3): flag parsing (--from, --to, --reply-to, --body), store insert,
	// caps validation, JSON response on stdout. See the roadmap epic.
	fmt.Fprintln(os.Stderr, `{"ok":false,"error":"send is not yet implemented — see issue M2.3"}`)
	return exitNotImpl
}
