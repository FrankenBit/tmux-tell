package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/version"
)

// usageText renders the top-level usage string for the active adapter. The
// binary name and the adapter display-label ("Claude Code" / "Codex") are
// interpolated from the profile so each adapter binary prints its own name and
// names its own host tool; the subcommand list is shared (#248 PR2, #280).
func usageText() string {
	return fmt.Sprintf(`usage: %s <subcommand> [args]

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
  doctor  Walk live tmux-msg processes + flag MCP/mailman DB-binding divergence (#348); exits non-zero on divergence
  config  Read/show the host-level config (#54). Subcommands: show
  agents  List registered agents with pane liveness
  whoami  Show this session's registration (auto-resolves identity)
  register   Register this (or another) pane on the bus (mirrors tmux-msg.register; #116)
  unregister Remove an agent from the registry + stop its mailman (mirrors tmux-msg.unregister; #289)
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
  restart-mailmen  Restart this adapter's running mailman units so a freshly-installed binary takes effect (#436)
  db      DB-housekeeping verbs (#349). Subcommands: migrate
  bootstrap  Substrate-honest install hard-cut: discover + enable mailmen + orphan walk + refresh (#349)
  codex-install  Codex-adapter bootstrap: set hook-context delivery mode + write hook blocks + MCP env to ~/.codex/config.toml (#384)
  mcp     Speak MCP over stdio (%s tools)
  hook-context  Present pending messages as additionalContext for a hook-context agent — invoked by a %s SessionStart/UserPromptSubmit hook (#249)

See https://git.frankenbit.de/frankenbit/tmux-tell for the design notes.
`, active.BinaryName, active.DisplayLabel, active.DisplayLabel)
}

// warnIfDeprecatedName emits the ADR-0008 deprecation WARN when the binary is
// invoked through the active adapter's legacy alias (the symlink install.sh
// keeps for the deprecation cycle). The string matches ADR-0008's worked-example
// format verbatim so it's greppable across the fleet. Canonical invocations —
// and adapters with no legacy alias (DeprecatedAlias == "") — are silent.
func warnIfDeprecatedName(argv0 string, stderr io.Writer) {
	if active.DeprecatedAlias == "" {
		return
	}
	if filepath.Base(argv0) == active.DeprecatedAlias {
		fmt.Fprintf(stderr,
			"WARN deprecated_surface_used name=%s removal=%s — invoke %s instead (ADR-0008)\n",
			active.DeprecatedAlias, active.DeprecatedRemoval, active.BinaryName)
	}
}

// Run is the shared, adapter-agnostic CLI entrypoint. Each adapter binary
// (cmd/tmux-tell-claude, cmd/tmux-tell-codex) is a thin wrapper that builds its
// Profile and calls Run; the subcommand dispatch + every handler live here in
// internal/cli — the #248 substrate-vs-adapter boundary (ADR-0009).
//
// argv0 is os.Args[0] (the invoked binary path), used only for the
// deprecated-alias warning; args is os.Args[1:]. Returns the process exit code.
func Run(p Profile, argv0 string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	active = p
	// Stamp process start for whoami_db's started_at (#348) — a long-lived MCP
	// server holds this for its lifetime so an operator can spot a pre-deploy
	// process still bound to a stale inode.
	if processStart.IsZero() {
		processStart = time.Now()
	}
	// Install the adapter's pane-observation snippets into the tmuxio
	// classifier's process-global before any subcommand (notably serve's
	// mailman) starts observing panes (#322). The CLI binary serves exactly
	// one adapter for its lifetime, so a single install at entry mirrors the
	// `active` Profile global above.
	tmuxio.SetActivePaneProfile(p.Pane)
	warnIfDeprecatedName(argv0, stderr)

	if len(args) == 0 {
		fmt.Fprint(stderr, usageText())
		return exitUsage
	}

	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usageText())
		return exitOK
	case "-v", "--version", "version":
		fmt.Fprintf(stdout, "%s %s\n", active.BinaryName, version.Version)
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
	case "doctor":
		return runDoctorCLI(args[1:], stdout, stderr)
	case "config":
		return runConfigCLI(args[1:], stdout, stderr)
	case "agents":
		return runAgentsCLI(args[1:], stdout, stderr)
	case "whoami":
		return runWhoamiCLI(args[1:], stdout, stderr)
	case "register":
		return runRegisterCLI(args[1:], stdout, stderr)
	case "unregister":
		return runUnregisterCLI(args[1:], stdout, stderr)
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
	case "restart-mailmen":
		return runRestartMailmenCLI(args[1:], stdout, stderr)
	case "db":
		return runDBCLI(args[1:], stdout, stderr)
	case "bootstrap":
		return runBootstrapCLI(args[1:], stdout, stderr)
	case "codex-install":
		return runCodexInstallCLI(args[1:], stdout, stderr)
	case "flag-operator":
		return runFlagOperatorCLI(args[1:], stdout, stderr)
	case "clear-operator-flag":
		return runClearOperatorFlagCLI(args[1:], stdout, stderr)
	case "mcp":
		return runMCPCLI(args[1:], stdin, stdout, stderr)
	case "hook-context":
		return runHookContextCLI(args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "%s: unknown subcommand %q\n\n%s", active.BinaryName, args[0], usageText())
		return exitUsage
	}
}
