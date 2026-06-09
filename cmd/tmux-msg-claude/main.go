// Package main is the tmux-msg-claude CLI entrypoint — the Claude Code adapter
// for the tmux-msg substrate (renamed from claude-msg per #174 Option 2 / #177;
// `claude-msg` survives as a deprecated alias through v1.0 — extension from the
// v0.11.0 two-minor-floor earliest per ADR-0008 §Discretion clause, operator
// decision 2026-06-08).
//
// Subcommand dispatcher only. Each subcommand handler lives in its own file
// (send.go, inbox.go, status.go, agents.go, whoami.go, …) and is split into
// runFooCLI (flag parsing + store open) and runFooWithStore (pure logic,
// testable). See the README for the project shape.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/version"
)

// Deprecated binary alias (#177 / ADR-0008). The binary renamed claude-msg →
// tmux-msg-claude; install.sh keeps a `claude-msg → tmux-msg-claude` symlink for
// the deprecation cycle. When invoked through the old name, warn so operators
// migrate before the alias is removed.
const (
	deprecatedBinaryAlias   = "claude-msg"
	deprecatedBinaryRemoval = "v1.0" // extended from v0.11.0 per ADR-0008 §Discretion clause (operator decision 2026-06-08)
)

const usage = `usage: tmux-msg-claude <subcommand> [args]

Subcommands:
  send    Queue a message for an agent (validates caps, returns JSON)
  resend  Replay an existing message to its recipient (recovery; #157)
  flush   Promote your deferred messages for a trigger (e.g. post-/compact: flush --trigger=resume; mirrors tmux-msg.flush_deferred; #227)
  ask     Send a question and get an ask_id to wait on (mirrors tmux-msg.ask; #250)
  wait-for-reply  Block until a reply to <ask_id> arrives or --timeout (mirrors tmux-msg.wait_for_reply; #250)
  check-replies   Non-blocking: list replies to <ask_id> (mirrors tmux-msg.check_replies; #250)
  ping    Substrate-only reachability probe — daemon up + agent reachable, no pane paste (mirrors tmux-msg.ping)
  control Send a whitelisted slash-command to a pane (mirrors tmux-msg.control)
  track   Show the delivery state of a single message by its public_id
  get     Fetch a processed message by ID (recovery for swallowed deliveries, #111)
  inbox   List queued messages for an agent
  sent    List messages sent by this agent (outbox view)
  status  Show paused state + queue depths across all agents (--today for journal-sourced today counts)
  stats   On-demand bus-traffic aggregates from the local DB (per-agent counts, latency, top pairs)
  digest  Campaign-arc narrative summary: by-counterparty threads + in-flight follow-ups (#161)
  tail    Live cross-chamber firehose with compositional filters (#148)
  health  One-command per-agent health audit from journalctl + systemd (#42)
  config  Read/show the host-level config (#54). Subcommands: show
  agents  List registered agents with pane liveness
  whoami  Show this session's registration (auto-resolves identity)
  register Register this (or another) pane on the bus (mirrors tmux-msg.register; #116)
  serve   Run the mailman daemon for one agent
  pause   Halt one or all mailman daemons
  resume  Resume paused mailmen
  reset   Purge messages (requires --confirm)
  log     Inspect a reply thread flat-chronological (--thread <id>)
  thread  Render a reply thread as a parent→child tree (#141)
  stranded Recover operator paste-buffer snapshots: list|show|prune (#142)
  discover Re-derive agents.pane_id from current tmux state
  state   Probe a agent's current activity via read-only capture-pane (#71)
  refresh-all-mcps  Bulk-fire mcp-restart-tmux-msg to every registered agent (#62)
  mcp     Speak MCP over stdio (Claude Code tools)
  hook-context  Present pending messages as additionalContext for a hook-context agent — invoked by a Claude Code SessionStart/UserPromptSubmit hook (#249)

See https://git.frankenbit.de/frankenbit/tmux-msg for the design notes.
`

func main() {
	warnIfDeprecatedName(os.Args[0], os.Stderr)
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// warnIfDeprecatedName emits the ADR-0008 deprecation WARN when the binary is
// invoked through the legacy `claude-msg` alias (the symlink install.sh keeps
// for the deprecation cycle). The string matches ADR-0008's worked-example
// format verbatim so it's greppable across the fleet. Canonical
// `tmux-msg-claude` invocations are silent.
func warnIfDeprecatedName(argv0 string, stderr io.Writer) {
	if filepath.Base(argv0) == deprecatedBinaryAlias {
		fmt.Fprintf(stderr,
			"WARN deprecated_surface_used name=%s removal=%s — invoke tmux-msg-claude instead (ADR-0008)\n",
			deprecatedBinaryAlias, deprecatedBinaryRemoval)
	}
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
		fmt.Fprintf(stdout, "tmux-msg-claude %s\n", version.Version)
		return exitOK
	case "send":
		return runSendCLI(args[1:], stdout, stderr)
	case "resend":
		return runResendCLI(args[1:], stdout, stderr)
	case "flush":
		return runFlushCLI(args[1:], stdout, stderr)
	case "ask":
		return runAskCLI(args[1:], stdout, stderr)
	case "wait-for-reply":
		return runWaitForReplyCLI(args[1:], stdout, stderr)
	case "check-replies":
		return runCheckRepliesCLI(args[1:], stdout, stderr)
	case "ping":
		return runPingCLI(args[1:], stdout, stderr)
	case "control":
		return runControlCLI(args[1:], stdout, stderr)
	case "track":
		return runTrackCLI(args[1:], stdout, stderr)
	case "get":
		return runGetCLI(args[1:], stdout, stderr)
	case "inbox":
		return runInboxCLI(args[1:], stdout, stderr)
	case "sent":
		return runSentCLI(args[1:], stdout, stderr)
	case "status":
		return runStatusCLI(args[1:], stdout, stderr)
	case "stats":
		return runStatsCLI(args[1:], stdout, stderr)
	case "digest":
		return runDigestCLI(args[1:], stdout, stderr)
	case "tail":
		return runTailCLI(args[1:], stdout, stderr)
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
	case "thread":
		return runThreadCLI(args[1:], stdout, stderr)
	case "stranded":
		return runStrandedCLI(args[1:], stdout, stderr)
	case "discover":
		return runDiscoverCLI(args[1:], stdout, stderr)
	case "state":
		return runStateCLI(args[1:], stdout, stderr)
	case "refresh-all-mcps":
		return runRefreshAllMcpsCLI(args[1:], stdout, stderr)
	case "flag-operator":
		return runFlagOperatorCLI(args[1:], stdout, stderr)
	case "clear-operator-flag":
		return runClearOperatorFlagCLI(args[1:], stdout, stderr)
	case "mcp":
		return runMCPCLI(args[1:], os.Stdin, stdout, stderr)
	case "hook-context":
		return runHookContextCLI(args[1:], os.Stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "tmux-msg-claude: unknown subcommand %q\n\n%s", args[0], usage)
		return exitUsage
	}
}
