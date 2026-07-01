package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/mcp"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// maxAncestorWalkDepth bounds the PPid walk the MCP identity resolver uses to
// find an ancestor that still carries the tmux env (#549/#553 Fix-1b). 16 covers
// a realistic Codex → shim/wrapper → MCP-server chain without an unbounded climb
// to init; /proc reads are cheap, but this runs on hot-ish MCP call setup, so the
// walk stays deterministic. A visited-PID set additionally guards a cyclic or
// corrupted /proc (PPid should be acyclic, but the bound + cycle guard keep a
// proc-test stub safe).
const maxAncestorWalkDepth = 16

// procEnvForPID / procPPIDForPID are the /proc readers the ancestor walk uses,
// kept as package vars so the white-box tests can stub a synthetic process tree
// (pid → environ, pid → ppid) without a real /proc.
var (
	procEnvForPID  = realProcEnvForPID
	procPPIDForPID = realProcPPIDForPID
)

// runMCPCLI parses MCP-mode flags, opens the store, and serves on stdio.
//
// Usage: tmux-tell-claude mcp [--db PATH]
//
// Identity is resolved from $TMUX_AGENT_NAME (explicit override) or
// from $TMUX_PANE looked up in the agents registry. The latter means a
// pane that's registered (via `discover` or manual INSERT) just works —
// no per-pane MCP config needed.
func runMCPCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	// Remote MCP mode (#310): when $TMUX_TELL_REMOTE_HOST is set, this MCP server
	// runs on a remote host reached over a reverse-SSH tunnel. Instead of opening
	// a local store, every tool call is forwarded back to the originating bus's
	// tmux-tell-claude via SSH. The env var is the explicit opt-in gesture —
	// never inferred (an SSH session without it just behaves as a local standalone
	// on whatever host it runs on).
	if remoteHost := os.Getenv("TMUX_TELL_REMOTE_HOST"); remoteHost != "" {
		return runRemoteMCP(remoteHost, stdin, stdout, stderr)
	}

	resolvedDB := resolveDBPath(*dbPath)
	fmt.Fprintf(stderr, "mcp: claude_msg_db=%s source=%s\n", resolvedDB, dbPathSource(*dbPath))
	s, err := store.Open(resolvedDB)
	if err != nil {
		fmt.Fprintf(stderr, "open store: %v\n", err)
		return exitInternal
	}
	defer func() { _ = s.Close() }()

	srv := newMCPServer(s)
	if err := srv.Serve(context.Background(), stdin, stdout); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(stderr, "mcp serve: %v\n", err)
		return exitInternal
	}
	return exitOK
}

// registerToolSchema builds the tmux-tell.register input schema. The mailman
// systemd unit name and the inbox / hook-context command references name the
// ACTIVE adapter binary (#314) — so a codex agent registering through the MCP
// surface sees tmux-tell-codex, not the claude literal — and the delivery-mode
// prose is adapter-neutral ("the recipient agent's session"), because the
// register tool describes substrate-general mechanism, not Claude-specific
// behavior (ADR-0009 substrate-vs-adapter boundary). The codex chamber consumes
// these MCP tools, and `delivery_mode=hook-context` is its onboarding path, so
// this schema is genuinely codex-visible, not a Claude-only surface.
//
// Built by string concatenation rather than fmt.Sprintf because the static
// schema carries a literal `%5` pane-id example that a format string would
// misread as a verb.
func registerToolSchema() json.RawMessage {
	bin := active.BinaryName
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name":          {"type": "string", "description": "Agent name (the new identity)"},
			"pane":          {"type": "string", "description": "Pane id like %5 (default: $TMUX_PANE)"},
			"start_mailman": {"type": "boolean", "description": "Run systemctl --user enable --now ` + bin + `-mailman@NAME (default true; default false when delivery_mode=mailbox-only). Note: start_mailman=true with delivery_mode=mailbox-only is allowed but vestigial — the daemon starts, observes mailbox-only at startup, logs the no-work condition, and exits cleanly. The 'mailman: active' field in the response is momentary in this case."},
			"force":         {"type": "boolean", "description": "Overwrite an existing row with the same name (default false)"},
			"alias":         {"type": "string", "description": "Optional alternative name discover should accept for this canonical agent (e.g. 'Master Bosun of Nimbus' for canonical 'bosun'). Append-only; existing aliases preserved."},
			"delivery_mode": {"type": "string", "enum": ["paste-and-enter", "mailbox-only", "hook-context"], "description": "How the mailman delivers to this agent (#116). 'paste-and-enter' (default): tmux paste + Enter into the agent's pane — the existing behavior for CLI-tool-hosting panes. 'mailbox-only': messages stay in state=queued; operator polls via ` + bin + ` inbox. 'hook-context' (#249): no pane paste — the recipient agent's session pulls pending messages as additionalContext via a SessionStart/UserPromptSubmit hook running '` + bin + ` hook-context'."}
		},
		"required": ["name"]
	}`)
}

// newMCPServer wires the tmux-tell.* tools onto an mcp.Server.
// Tools registered: send / resend / ping / agents / whoami / inbox /
// message_status / status / register / control / unregister /
// agent_state.
func newMCPServer(s *store.Store) *mcp.Server {
	srv := mcp.NewServer("tmux-tell", "0.1.0")

	srv.RegisterTool("tmux-tell.send",
		"Queue a message for another agent (sender resolved from $TMUX_AGENT_NAME or $TMUX_PANE→registry). Returns {ok,id,queued,recipient,receipt}: ok:true / receipt.enqueue means the bus accepted and persisted the row; it is NOT a paste-confirmation claim. The recipient sees it once their mailman delivers. The recipient block reports send-time disposition (registered/alive/delivery_mode/mailman_running/pane_status). Confirm delivery synchronously with wait_for_delivered, or after the fact with tmux-tell.message_status; with wait_for_delivered the receipt.dispatch and receipt.paste_confirmed layers report the observed delivery/paste evidence. Set reply_to to thread under an earlier message — when you do, the response adds a \"thread_freshness\" block flagging whether the thread moved since you last spoke (crossed-message guard, #155). Set quick=true to render compact single-line chrome (✓ Sender · [re X ·] body) in the recipient's pane instead of the full bracket-header block — for routine acks where typing-overhead-to-signal ratio is high (#154). Multi-recipient: pass to as an array (e.g. [\"bosun\",\"surveyor\"]) to fan the message to multiple recipients in a single call — each recipient gets its own message id; the response shape changes to {ok,messages:[{to,id,queued,recipient,receipt,...},...]} (#158).",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"to": {
					"description": "Recipient agent name (string) or list of names (array of strings) for fan-out (#158)",
					"oneOf": [
						{"type": "string"},
						{"type": "array", "items": {"type": "string"}, "minItems": 1}
					]
				},
				"body":              {"type": "string", "description": "Message body"},
				"reply_to":          {"type": "string", "description": "Optional public_id of the message this replies to; threads the reply (renders the 'Sender → Recipient · re …' header) and enables the thread_freshness crossed-message check"},
				"no_reply_expected": {"type": "boolean", "description": "Set true to signal the recipient that no acknowledgment is needed — reduces ack-cascade on FYI/status messages (#145). Default false."},
				"quick":             {"type": "boolean", "description": "Render compact single-line chrome (✓ Sender · [re X ·] body) in the recipient's pane instead of the full bracket-header block. For routine acks where typing-overhead-to-signal ratio is high. Default false (#154)."},
				"strict":            {"type": "boolean", "description": "Fail (ok:false) if the recipient is registered but not reachable (pane gone). An UNregistered recipient is always fail-loud regardless of this flag. Default false (#152)."},
				"wait_for_delivered": {"type": "boolean", "description": "Block until the message reaches a terminal delivery state (delivered/failed) or timeout, returning a \"delivery\" block with state + verify_ms and filling receipt.dispatch / receipt.paste_confirmed with observed evidence. Default false (#152/#614)."},
				"timeout":           {"type": "string", "description": "Bound for wait_for_delivered as a Go duration (e.g. \"10s\"). Default 10s."},
				"block_on_stale":    {"type": "boolean", "description": "With reply_to: fail (ok:false) instead of queueing when the thread_freshness check finds the thread moved since you last spoke (newer messages addressed to you arrived after your last message in the chain). Default false — staleness is reported but the send still succeeds (#155)."},
				"deliver_after":     {"type": "string", "description": "Defer delivery until a trigger fires (#227): the message is STAGED (not queued) and delivers only after the trigger. Accepts \"resume\" — post-compaction self-handoff: stage orientation text with deliver_after=\"resume\", then call tmux-tell.flush_deferred{trigger:\"resume\"} in your post-/compact resume routine so it lands in the freshly-resumed context instead of being absorbed by the summarizer — or \"register\" (#258a): a spawn-die session bridge addressed to another agent (\"remember this for its next dispatch\"); it auto-promotes when that agent next (re)registers, no explicit flush needed. Single-recipient only. The response carries deliver_after to confirm staging."},
				"expects_reply":     {"type": "boolean", "description": "Signal that you'd like a reply — lightweight intent-marker WITHOUT the blocking wait of ask/wait_for_reply (#270). Use when you want an answer eventually but aren't blocking on it. Your unanswered sends appear under sent --awaiting-reply; the recipient's owed replies appear under inbox --unanswered. Default false."},
				"priority":          {"type": "string", "enum": ["low", "normal", "high"], "description": "Delivery priority for cross-channel scheduling (#449). Default normal. Within a sender→recipient channel order is always FIFO; priority only decides which channel's head the recipient's mailman delivers next when several are queued. Trust-based — reserve high for genuinely urgent coordination, not routine FYIs."}
			},
			"required": ["to", "body"]
		}`),
		mcpSendHandler(s))

	srv.RegisterTool("tmux-tell.resend",
		"Replay an existing message to its original recipient — the explicit recovery path for a message that landed `delivered_in_input_box` or `failed` (#157). The replay carries a \"Replayed: original sent at <ts>\" chrome marker so the recipient knows it's a re-send, and the response adds a \"replay\" block {original_id, original_sent_at, original_state, forced}. A `failed` or `delivered_in_input_box` (delivered-but-unverified, #169) message replays directly — no force needed. Refuses to replay a confirmed-`delivered` (verified) or pre-#169 delivered (unknown) or still in-flight message unless force=true, to avoid duplicate-spam. The replayed body is byte-identical to the original.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"id":    {"type": "string", "description": "public_id of the message to replay"},
				"force": {"type": "boolean", "description": "Replay even a confirmed-delivered or in-flight message (may duplicate). NOT needed for a delivered_in_input_box (delivered-but-unverified) message — the verified column (#169) recognizes the soft-fail and replays it directly; passing force there is deprecated (#230). Default false."}
			},
			"required": ["id"]
		}`),
		mcpResendHandler(s))

	srv.RegisterTool("tmux-tell.flush_deferred",
		"Promote your own deferred messages for a trigger to delivery (#227). A deferred message (sent with deliver_after) is STAGED — invisible to inbox/mailman — until you flush its trigger. Primary use: POST-COMPACTION SELF-HANDOFF. Before /compact, send yourself orientation with deliver_after=\"resume\"; then call this with trigger=\"resume\" as part of your resume routine, so the staged message lands in your freshly-resumed context instead of being absorbed by the summarizer. Idempotent — calling with no matching deferred messages is a no-op (promoted:0), so it's safe to call unconditionally on resume. You can only flush messages addressed to yourself. Returns {ok, trigger, promoted}. Triggers: \"resume\"; \"register\" also exists but auto-fires on (re)register (#258a) so rarely needs an explicit flush.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"trigger": {"type": "string", "description": "The deferred-delivery trigger to fire. Accepts \"resume\" (default) and \"register\" (though register auto-fires on (re)register, #258a). Default \"resume\"."}
			}
		}`),
		mcpFlushDeferredHandler(s))

	srv.RegisterTool("tmux-tell.ask",
		"Send a question and signal you intend to wait for a reply (#250). Like send, but returns an ask_id (the message id) you pass to wait_for_reply / check_replies, and marks the message so the substrate knows a reply is expected. Single-recipient. The recipient answers by replying to the ask_id (send/ask with reply_to=<ask_id>). Use ask + wait_for_reply for synchronous Q&A (pause until answered); ask + check_replies to poll while doing other work.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"to":       {"type": "string", "description": "Recipient agent name (single recipient)"},
				"body":     {"type": "string", "description": "The question"},
				"reply_to": {"type": "string", "description": "Optional public_id this ask threads under"}
			},
			"required": ["to", "body"]
		}`),
		mcpAskHandler(s))

	srv.RegisterTool("tmux-tell.wait_for_reply",
		"Block until a reply to your ask_id arrives, or timeout_ms elapses (#250). Returns {ok, ask_id, reply, timed_out}. `reply` (when present) is {id, from, body, state, unverified, created_at}: `unverified:true` means the reply landed but its delivery wasn't verify-confirmed (#169) — it's returned anyway, you decide whether to trust it. Does NOT auto-acknowledge the reply (ack stays explicit). Use after ask to pause your turn until the recipient answers.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"ask_id":     {"type": "string", "description": "The id returned by ask (the message you're awaiting a reply to)"},
				"timeout_ms": {"type": "integer", "description": "How long to block before returning timed_out:true. Default 30000 (30s)."}
			},
			"required": ["ask_id"]
		}`),
		mcpWaitForReplyHandler(s))

	srv.RegisterTool("tmux-tell.check_replies",
		"Non-blocking: list the replies to your ask_id that have arrived (#250). Returns {ok, ask_id, replies:[{id, from, body, state, unverified, created_at}]}. Pass `since` (a numeric id) to get only replies newer than one you've already seen — the accumulation pattern: do other work, periodically check_replies(ask_id, since=<highest id seen>). Complements wait_for_reply (block) when you'd rather poll.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"ask_id": {"type": "string", "description": "The id returned by ask"},
				"since":  {"type": "integer", "description": "Only return replies with numeric id > this (0 = all). Track the highest id you've seen for incremental polling."}
			},
			"required": ["ask_id"]
		}`),
		mcpCheckRepliesHandler(s))

	srv.RegisterTool("tmux-tell.agents",
		"List registered agents with pane liveness. Each row carries pane_status / paused / queued / attention_state / stuck, plus mailman_last_delivered_at (#348) — the RFC3339 time of the most recent delivery to that agent, derived from the delivery rows (omitted when none in retained history). A non-zero queued + an empty/old mailman_last_delivered_at is the \"queued but mailman silent\" divergence smell.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"available_only": {"type": "boolean", "description": "Filter to live + not-paused agents"}
			}
		}`),
		mcpAgentsHandler(s))

	srv.RegisterTool("tmux-tell.whoami",
		"Return this session's registration. Identity from $TMUX_AGENT_NAME or $TMUX_PANE→registry.",
		json.RawMessage(`{"type": "object", "properties": {}}`),
		mcpWhoamiHandler(s))

	srv.RegisterTool("tmux-tell.inbox",
		"List the caller's own queued messages, or acknowledge announce-skipped backlog residue (#221). Pass ack_ids to mark specific messages acknowledged; pass ack_all=true to acknowledge all messages ≤ the backlog_epoch (drains the announce-skipped residue left by the don't-flood policy). Acknowledged messages are excluded from the default queued view but remain retrievable via tmux-tell.get. Pass unanswered=true to see only messages where the sender signaled expects_reply AND you haven't replied yet (#270).",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"state": {"type": "string", "enum": ["queued","delivering","delivered","failed","acknowledged"]},
				"limit": {"type": "integer", "minimum": 1, "maximum": 1000},
				"ack_ids": {"type": "array", "items": {"type": "string"}, "description": "Public IDs of queued messages to mark acknowledged. Idempotent."},
				"ack_all": {"type": "boolean", "description": "Mark all queued messages ≤ backlog_epoch_id as acknowledged. Drains announce-skipped backlog residue."},
				"unanswered": {"type": "boolean", "description": "List only messages where the sender set expects_reply AND you haven't replied yet (#270). Use to find what owes a response. Default false."}
			}
		}`),
		mcpInboxHandler(s))

	srv.RegisterTool("tmux-tell.message_status",
		"Look up the delivery state of a sent message by its public_id. Returns created_at + delivered_at + error so the sender can see whether the bus has handed off the row yet.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "Public ID of the message to look up (returned in the send/control response)"}
			},
			"required": ["id"]
		}`),
		mcpMessageStatusHandler(s))

	srv.RegisterTool("tmux-tell.get",
		"Fetch a processed message by ID — recovery path for swallowed deliveries (#111). The bus stores message bodies; if the paste landed in a state that obscured the visible delivery (mid-AskUserQuestion, popup open, recipient mid-compaction), retrieving by ID returns the full body + metadata. Accepts full public_id or short prefix (4-char IDs from delivery headers work). Access: sender OR recipient OR allowlisted agent (`privileged-agents` in /etc/tmux-tell/config.toml). Not-found and not-authorized return the same error class to prevent existence leaks.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "Public ID or short prefix (e.g. '8f54' or 'a2c76333...'); falls back to disambiguation error if multiple authorized matches"}
			},
			"required": ["id"]
		}`),
		mcpGetHandler(s))

	srv.RegisterTool("tmux-tell.status",
		"Return registry overview: paused state + queue depths per agent.",
		json.RawMessage(`{"type": "object", "properties": {}}`),
		mcpStatusHandler(s))

	srv.RegisterTool("tmux-tell.flag_operator",
		"Signal that this chamber needs operator attention (#224). Posts the body to the reserved \"operator-attention\" recipient AND marks this chamber's attention_state as \"awaiting_operator\". The flag clears implicitly on the chamber's next register call (after the operator answered + chamber resumed) or explicitly via tmux-tell.clear_operator_flag. Body is required — it is the question or choice the chamber wants the operator to weigh in on. The recipient \"operator-attention\" MUST be pre-registered by the operator (as a mailbox-only agent) — chambers cannot register it themselves.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"body": {"type": "string", "description": "The question or choice the chamber wants the operator to weigh in on (required)"}
			},
			"required": ["body"]
		}`),
		mcpFlagOperatorHandler(s))

	srv.RegisterTool("tmux-tell.clear_operator_flag",
		"Clear this chamber's awaiting_operator attention signal (#224). Sets attention_state back to \"idle\". Used when the operator answered the chamber's question out of band (typed directly in the pane) and the chamber wants to clear the flag without going through register.",
		json.RawMessage(`{"type": "object", "properties": {}}`),
		mcpClearOperatorFlagHandler(s))

	srv.RegisterTool("tmux-tell.register",
		"Register this (or another) pane on the bus. Pane defaults to $TMUX_PANE; start_mailman defaults true UNLESS delivery_mode is `mailbox-only` (in which case it defaults to false — no daemon needed for the operator-as-bus-participant scenario). The response includes `queued`: the number of messages already waiting for this agent at register time (#151) — a fresh or post-restart session learns it has backlog without a separate inbox poll; check it and run tmux-tell.inbox if >0. When that backlog exists, the don't-flood policy (#204) keeps the mailman from pasting the whole queue at once: by default it leaves the backlog queued and delivers a single `📬 N queued` nudge (the `on-register-backlog` TOML knob can switch to auto-delivering the newest N). The response then also carries `backlog_policy`, `backlog_skipped`, and `backlog_nudge`.",
		registerToolSchema(),
		mcpRegisterHandler(s))

	srv.RegisterTool("tmux-tell.control",
		"Send a whitelisted Claude Code slash-command directly to a pane. Scope-gated: when to==self, the self-whitelist applies; when to is a peer, the peer-whitelist applies — with a third tier of per-edge exceptions for destructive commands. Specifically, /clear is globally denied but Bosun→Pilot and Quartermaster→Pilot are permitted (routine clear-before-each-task dispatch + rescue path when Pilot can't sleep (/compact) out of token exhaustion). Bypasses the chat-message renderer. Optional resume_with (only with command=sleep, only on self) queues a follow-up message that the mailman delivers AFTER the sleep (/compact) has settled — pre-write your continuation instead of going silent post-sleep. command=clear REQUIRES for_task: it synthesises an atomic /clear + /rename \"<Chamber> <task>\" pair (clear THEN rename) so the cleared session is relabelled to its dispatch-time task identity, keeping the chamber's persistent name free for an unambiguous resume (#286). (`sleep` is the bus verb for /compact, #509; `compact` still works as a deprecated alias.)",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"to":          {"type": "string", "description": "Recipient agent name; set to your own name for self-invocation"},
				"command":     {"type": "string", "description": "Whitelisted command (e.g. 'sleep'); leading slash optional"},
				"resume_with": {"type": "string", "description": "Optional continuation prompt delivered after the sleep (/compact) settles. Only valid with command=sleep on self-invocation."},
				"for_task":    {"type": "string", "description": "REQUIRED with command=clear: the dispatch-time task identity (e.g. \"tmux-tell#286\") the cleared session is renamed to. Synthesises an atomic /clear + /rename \"<Chamber> <task>\" pair so the cleared session is relabelled away from the chamber's persistent name and a later resume resolves unambiguously (#286). Constrained single-line token: starts alphanumeric, ≤80 chars of [A-Za-z0-9 #/._-]. Rejected (fail-loud) with any other command."},
				"force_rate_limited": {"type": "boolean", "description": "Bypass the recipient's rate-limit / usage-limit defer for this control macro, delivering even when the pane shows a rate-/usage-limit banner (#573, control arm of #558). Applies to BOTH rows of the restart / sleep+resume / clear+rename macros. Does NOT bypass copy-mode / popup / unknown / compaction paste-safety."}
			},
			"required": ["to", "command"]
		}`),
		mcpControlHandler(s))

	srv.RegisterTool("tmux-tell.unregister",
		"Remove an agent from the registry (#289). Stops the mailman, drops the agent row. Idempotent: absent agent returns removed:false. Force overrides the queued-message guard.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"name":        {"type": "string", "description": "Agent name to remove; required"},
				"purge_queue": {"type": "boolean", "description": "Drop queued messages addressed to this agent (default false — preserved so they deliver if re-registered)"},
				"force":       {"type": "boolean", "description": "Override the queued-message guard (otherwise fails with count when agent has pending mail)"}
			},
			"required": ["name"]
		}`),
		mcpUnregisterHandler(s))

	srv.RegisterTool("tmux-tell.agent_state",
		"Probe an agent's agent-state via read-only capture-pane (#71). Returns one of seven states: idle / working / rate-limited / usage-limited / at-rest-in-compaction / awaiting-operator / unknown. 'Knock at the door without waking the inhabitant' — exactly two capture-pane calls, zero pane mutation, ~200ms latency. Consumers should treat 'unknown' as advisory-not-authoritative per #65's substrate-class-of-claim convention (don't silently roll up an unknown classification to a known state). v1 detects idle/working/unknown reliably; at-rest-in-compaction, awaiting-operator, rate-limited, and usage-limited land when their empirical pane patterns are configured.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"agent": {"type": "string", "description": "Agent name to probe"}
			},
			"required": ["agent"]
		}`),
		mcpAgentStateHandler(s))

	srv.RegisterTool("tmux-tell.ping",
		"Substrate-only reachability probe (#144): is the recipient's mailman daemon up and its pane reachable? Queues a kind=ping row that the mailman picks up (proving the daemon is alive) and answers via substrate-health checks (agent registered, pane live) — it does NOT paste into the recipient's pane or load their context, so it's safe for runbook verification and post-restart sanity. Returns {ok, agent, id, state, class, elapsed_ms}: state is `delivered`, `failed` (registered but unreachable — pane gone), or `timeout` (no mailman answered in time). `class` (#366) is the coarse reachability classification, set on every path — `reachable` (confirmed), `pending` (substrate healthy and making progress but the probe didn't confirm in-bound → retry or wait), or `unreachable` (substrate broken → operator must act). On the failing path (ok=false) the response also carries a fine-grained `reason` (#358) — one of `pane_dead` / `mailman_down` / `stuck` / `blocked_delivery` / `backlog_draining` — plus an `evidence` block {mailman_active, queue_depth, current_state, stuck_reason?} so the caller can route recovery: mailman_down → start the daemon, stuck → `register --force`, pane_dead → re-discover, backlog_draining / blocked_delivery → wait (the mailman is working through a queue or finishing a prior delivery; a ping never reaches the observe-gate, so these are healthy-pending, not broken). Branch on `class` for coarse reachability-routing, `reason` for specific recovery. Pinging a non-registered agent fails loud.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"agent":           {"type": "string", "description": "Agent name to probe for reachability"},
				"timeout_seconds": {"type": "number", "description": "Bound the wait for a terminal state (default 5). A reachable agent answers in well under a second."}
			},
			"required": ["agent"]
		}`),
		mcpPingHandler(s))

	srv.RegisterTool("tmux-tell.whoami_db",
		"Report THIS MCP server's live DB binding (#348): {pid, binary_path, started_at, db_path, db_inode, db_deleted}. Read straight from /proc (the open file handle + exe symlink) — NOT by re-resolving the configured path, so it reveals where the process is *actually* writing even after a deploy moved the DB out from under it (the orphan-inode case: a process spawned pre-deploy keeps writing to the unlinked inode, invisible to sqlite3 on the canonical path). `db_deleted: true` is the orphan smell; a divergent `db_inode` across processes is the cross-surface-divergence `doctor` aggregates. No DB access at all (can't itself be misrouted); safe to expose to peers.",
		json.RawMessage(`{"type": "object", "properties": {}}`),
		mcpWhoamiDBHandler())

	srv.RegisterTool("tmux-tell.set_pane_name",
		"Set THIS chamber's tmux pane title to <name> (#556). The calling chamber asserts its OWN display name: resolves the caller's pane via the same $TMUX_PANE / $TMUX_AGENT_NAME path as whoami, then runs `tmux select-pane -T`. Call it after an in-session rename or session-switch (e.g. a codex `resume`/`fork` that swaps the session under the same pane) so the pane title follows the new identity — the chamber-launch title is set by the launch wrapper; this method covers the in-session changes the wrapper cannot observe. Multi-word names are preserved (\"Master Bosun\"). Returns {ok, agent, pane, title}.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "The display name to set as this chamber's pane title — case- and space-preserved (e.g. \"Lookout\" or \"Master Bosun\")"}
			},
			"required": ["name"]
		}`),
		mcpSetPaneNameHandler(s))

	srv.RegisterTool("tmux-tell.set_metabolism",
		"Self-report THIS chamber's metabolism (#621) — an intentional context-throughput state the auto-observed agent_state probe CANNOT infer. Three values: \"warming\" (just resumed, not yet at full throughput), \"saturating\" (context-load approaching the /compact-need), \"compact-pending\" (intent-to-/compact stated but not yet fired — the stall seam). Pass \"\" to clear the self-report. SELF-ONLY: the calling chamber sets its OWN metabolism (resolved like whoami); there is no target parameter — a third-party write would clobber the target's real signal. ADVISORY only: never gates delivery. compact-pending auto-clears once the mailman observes this chamber actually at-rest-in-compaction. Surfaces in agent_state (alongside the observed state) and the agents listing. Returns {ok, agent, metabolism, metabolism_set_at}.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"value": {"type": "string", "enum": ["warming", "saturating", "compact-pending", ""], "description": "The metabolism to self-report, or \"\" to clear the current self-report"}
			},
			"required": ["value"]
		}`),
		mcpSetMetabolismHandler(s))

	srv.RegisterTool("tmux-tell.set_session_id",
		"Backfill the session_id for a target chamber WITHOUT re-registering it (#644). Writes ONLY the session_id column — it deliberately does NOT clear attention_state (#224) or stuck_reason (#298) the way register does. register's auto-clear is correct only when the chamber ITSELF registers (back + ready by definition); this field-specific backfill is the safe ON-BEHALF path, so an orchestrator can populate a stale chamber's session id without erasing its real signals (a pane sitting at awaiting_operator, a parked mailman with a stuck_reason). Pass {name, session_id} — session_id must be an explicit UUID (this MCP surface does NOT self-discover: the server's own pane is not the target's; use the `set-session-id` CLI for discovery-from-pane). Returns {ok, agent, session_id, discovered}.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "The target chamber to backfill the session id for"},
				"session_id": {"type": "string", "description": "The session id (UUID) to write — field-specific backfill; does NOT register the chamber or clear its attention/stuck signals"}
			},
			"required": ["name", "session_id"]
		}`),
		mcpSetSessionIDHandler(s))

	return srv
}

// mcpPingHandler returns the handler for the tmux-tell.ping MCP tool.
// Resolves the caller's identity (the ping's sender) and runs the shared
// pingProbe core — the same code path as the `tmux-tell-claude ping` CLI
// subcommand — so both surfaces emit the identical pingResult shape.
func mcpPingHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		Agent          string  `json:"agent"`
		TimeoutSeconds float64 `json:"timeout_seconds"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		if in.Agent == "" {
			return nil, fmt.Errorf("agent required")
		}
		from, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		timeout := defaultPingTimeout
		if in.TimeoutSeconds > 0 {
			timeout = time.Duration(in.TimeoutSeconds * float64(time.Second))
		}
		// pingProbe returns a structured pingResult; the MCP framework
		// json.Marshals the handler return (internal/mcp/server.go), and
		// pingResult's JSON tags are the single source of truth for the
		// wire shape — identical to the CLI's --format json output.
		res, err := pingProbe(ctx, s, from, in.Agent, timeout, pingPollInterval)
		if err != nil {
			return nil, err
		}
		return res, nil
	}
}

// mcpAgentStateHandler returns the handler for the
// tmux-tell.agent_state MCP tool. Wraps resolveAgentState (shared
// with the CLI subcommand `tmux-tell-claude state`) so both surfaces emit
// the same JSON schema — durable shape that Binnacle's M6b can
// consume verbatim per #74's carry-forward spec.
func mcpAgentStateHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		Agent string `json:"agent"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("parse args: %w", err)
		}
		if in.Agent == "" {
			return nil, fmt.Errorf("agent required")
		}
		res, err := resolveAgentState(ctx, s, in.Agent)
		// Return the result regardless of error — the consumer sees
		// the Evidence.Reason and can decide. Error surfaces via the
		// MCP error channel for callers that want to gate on success.
		if err != nil {
			return res, err
		}
		return res, nil
	}
}

// mcpSetPaneNameHandler returns the handler for the tmux-tell.set_pane_name MCP
// tool (#556). Always self-assert — the calling chamber sets its OWN pane title
// (no override), so it shares the setPaneName core with the CLI subcommand and
// emits the identical setPaneNameResult shape.
func mcpSetPaneNameHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		Name string `json:"name"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		res, err := setPaneName(ctx, s, "", in.Name)
		if err != nil {
			return nil, err
		}
		return res, nil
	}
}

// mcpSetMetabolismHandler returns the handler for the tmux-tell.set_metabolism
// MCP tool (#621). Always self-report — the caller's identity is resolved via
// resolveMCPIdentity (the same path as whoami), and there is no target field in
// the input, so a chamber can only ever set its OWN metabolism (AC#2). Shares
// the setMetabolism core with the CLI subcommand for a byte-identical result
// shape.
func mcpSetMetabolismHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		Value string `json:"value"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		caller, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		res, err := setMetabolism(ctx, s, caller, in.Value)
		if err != nil {
			return nil, err
		}
		return res, nil
	}
}

// mcpSetSessionIDHandler returns the handler for the tmux-tell.set_session_id
// MCP tool (#644). Unlike set_metabolism (self-only), this legitimately TARGETS
// another chamber by name — its whole purpose is on-behalf backfill by an
// orchestrator (Bosun migrating stale claude chambers, #626 Phase 3). Both
// name and session_id are required: the MCP surface does NOT self-discover
// (the server's own pane is not the target's), so the caller supplies the UUID
// it discovered out-of-band. Shares the setSessionID core with the CLI so the
// two surfaces stay byte-identical; the core writes ONLY session_id, never the
// attention/stuck signals register clears.
func mcpSetSessionIDHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		Name      string `json:"name"`
		SessionID string `json:"session_id"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		res, err := setSessionID(ctx, s,
			strings.TrimSpace(in.Name), strings.TrimSpace(in.SessionID), false)
		if err != nil {
			return nil, err
		}
		return res, nil
	}
}

// --- tool handlers ---

// resolveMCPIdentity is the MCP-side adapter over identity.Resolve. It enforces
// Lookout's #553/#549 precedence: a resolvable pane — the calling process's OWN
// registered $TMUX_PANE, or one carried by an ANCESTOR process — outranks any
// name pin ($TMUX_AGENT_NAME), because the pin can be a stale global-config
// value while a pane is tied to a real tmux pane. The ancestor walk covers codex
// MCP children whose spawn path drops $TMUX_PANE while a parent Codex process
// still has it (#553), and the depth bound (vs #562's immediate-parent-only read)
// handles a shim/wrapper sitting between Codex and the MCP server.
//
// Order: own registered pane (or explicit override) → ancestor registered pane →
// own name pin → ancestor name pin → actionable error. A name pin that disagrees
// with a resolved pane is surfaced via identity.WarnMismatch (the pane wins).
// injectedIdentityKey carries a bus identity supplied out-of-band by the
// remote-MCP receiver (#310). When a remote session forwards a tool call over
// SSH, the receiver runs on the originating host (alcatraz) where this SSH
// session has no $TMUX_PANE — so pane resolution would fail. The receiver
// instead injects the remote session's already-resolved bus identity into the
// context, and resolveMCPIdentity honours it as an explicit override.
type injectedIdentityKey struct{}

// withInjectedIdentity returns a context carrying name as the authoritative
// bus identity for the handlers it reaches.
func withInjectedIdentity(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, injectedIdentityKey{}, name)
}

// injectedIdentity returns the out-of-band identity set by withInjectedIdentity,
// or "" when none was injected (the normal in-process MCP path).
func injectedIdentity(ctx context.Context) string {
	v, _ := ctx.Value(injectedIdentityKey{}).(string)
	return v
}

func resolveMCPIdentity(ctx context.Context, s *store.Store) (string, error) {
	// Remote-MCP receiver path (#310): an injected identity is authoritative —
	// the remote session already resolved its bus name; treat it exactly like a
	// CLI --from override (SourceExplicit), skipping all pane/ancestor lookups
	// (this host's panes are not the remote session's).
	if inj := injectedIdentity(ctx); inj != "" {
		name, _, err := identity.Resolve(ctx, s, inj)
		return name, err
	}
	name, src, err := identity.Resolve(ctx, s, "")
	if err != nil {
		return "", err
	}
	// Own registered pane (or an explicit override) is authoritative.
	if src == identity.SourcePane || src == identity.SourceExplicit {
		return name, nil
	}
	// src is SourceEnv (own name pin) or SourceNone. Consult the ancestor pane
	// walk BEFORE settling for a name pin: an ancestor's REGISTERED pane outranks
	// it.
	ancestorPaneName, unregisteredPane, walkErr := resolveAncestorPaneIdentity(ctx, s)
	if walkErr != nil {
		return "", walkErr
	}
	if ancestorPaneName != "" {
		// `name` (if non-empty) is the own name pin — flag if it disagrees.
		identity.WarnMismatch(name, ancestorPaneName)
		return ancestorPaneName, nil
	}
	// No registered ancestor pane. A valid OWN name pin is the next-best signal —
	// an unregistered ancestor pane must not clobber it (we cannot prove the pin
	// stale without a registered pane to compare against).
	if name != "" {
		return name, nil
	}
	// No own identity at all. If an ancestor carried an unregistered pane, nudge
	// the operator to register it rather than silently falling to a higher name
	// pin (Lookout finding 1).
	if unregisteredPane != "" {
		return "", fmt.Errorf(
			"cannot resolve identity: ancestor $TMUX_PANE=%s is not in the agent registry — "+
				"run `%s register --name <name>` to register this pane",
			unregisteredPane, active.BinaryName)
	}
	if pin := ancestorNamePin(); pin != "" {
		return pin, nil
	}
	return "", mcpIdentityError()
}

// resolveAncestorPaneIdentity walks the PPid chain from this process's parent
// upward (bounded) and inspects the FIRST ancestor that carries $TMUX_PANE — the
// walk stops there rather than climbing to a higher ancestor's (less trustworthy)
// name pin (Lookout's #553 finding). It returns:
//   - (name, "",      nil) — a registered ancestor pane was found.
//   - ("",   paneID,  nil) — a pane-bearing ancestor was found but its pane is
//     unregistered; the caller fails loud only if it has no other identity.
//   - ("",   "",      nil) — no ancestor within the bound carries a pane.
func resolveAncestorPaneIdentity(ctx context.Context, s *store.Store) (string, string, error) {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return "", "", err
	}
	visited := make(map[int]bool)
	pid := os.Getppid()
	for depth := 0; depth < maxAncestorWalkDepth; depth++ {
		if pid <= 1 || visited[pid] {
			break
		}
		visited[pid] = true
		if pane, ok := procEnvForPID(pid, "TMUX_PANE"); ok && pane != "" {
			for _, a := range agents {
				if a.PaneID == pane {
					return a.Name, "", nil
				}
			}
			return "", pane, nil // pane-bearing but unregistered; stop here
		}
		ppid, ok := procPPIDForPID(pid)
		if !ok {
			break
		}
		pid = ppid
	}
	return "", "", nil
}

// ancestorNamePin walks the PPid chain for a $TMUX_AGENT_NAME (or legacy
// $CLAUDE_AGENT_NAME) pin — the last-resort signal used only when neither this
// process nor any ancestor carries a resolvable pane. A name pin is less
// trustworthy than a pane (a shared global config can pin it wrong), which is
// why it is consulted only after the pane walk comes up empty.
func ancestorNamePin() string {
	visited := make(map[int]bool)
	pid := os.Getppid()
	for depth := 0; depth < maxAncestorWalkDepth; depth++ {
		if pid <= 1 || visited[pid] {
			break
		}
		visited[pid] = true
		if v, ok := procEnvForPID(pid, "TMUX_AGENT_NAME"); ok && v != "" {
			return v
		}
		if v, ok := procEnvForPID(pid, "CLAUDE_AGENT_NAME"); ok && v != "" {
			return v
		}
		ppid, ok := procPPIDForPID(pid)
		if !ok {
			break
		}
		pid = ppid
	}
	return ""
}

// realProcEnvForPID reads env var key from /proc/<pid>/environ (NUL-separated).
func realProcEnvForPID(pid int, key string) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return "", false
	}
	prefix := []byte(key + "=")
	for _, item := range bytes.Split(data, []byte{0}) {
		if value, ok := bytes.CutPrefix(item, prefix); ok {
			return string(value), true
		}
	}
	return "", false
}

// realProcPPIDForPID returns pid's parent PID from /proc/<pid>/stat (field 4).
// The comm field (2) is parenthesized and may contain spaces/parens, so the
// scan starts after the last ')' to avoid mis-splitting.
func realProcPPIDForPID(pid int) (int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	rparen := strings.LastIndexByte(string(data), ')')
	if rparen < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data)[rparen+1:])
	// After ')': fields[0]=state, fields[1]=ppid.
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// mcpIdentityError constructs an actionable "cannot resolve identity" error
// for MCP contexts (#355/#553). Distinguishes two cases:
//   - $TMUX_PANE is empty in both child and parent: the MCP spawn substrate lost
//     the pane identity; instruct the operator to relaunch from a tmux pane or
//     inspect the MCP spawn environment.
//   - $TMUX_PANE is set but not in the registry: instructs to run register.
func mcpIdentityError() error {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return fmt.Errorf(
			"cannot resolve identity: $TMUX_AGENT_NAME is unset and $TMUX_PANE is " +
				"empty in the MCP child and parent process — launch Codex from a " +
				"registered tmux pane, or inspect the MCP server spawn environment")
	}
	return fmt.Errorf(
		"cannot resolve identity: $TMUX_PANE=%s is not in the agent registry — "+
			"run `%s register --name <name>` to register this pane",
		pane, active.BinaryName)
}

func mcpSendHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		To               json.RawMessage `json:"to"` // string or []string (#158)
		Body             string          `json:"body"`
		ReplyTo          string          `json:"reply_to"`
		NoReplyExpected  bool            `json:"no_reply_expected"`
		Quick            bool            `json:"quick"`
		Strict           bool            `json:"strict"`
		WaitForDelivered bool            `json:"wait_for_delivered"`
		Timeout          string          `json:"timeout"`
		BlockOnStale     bool            `json:"block_on_stale"`
		DeliverAfter     string          `json:"deliver_after"`
		ExpectsReply     bool            `json:"expects_reply"`
		Priority         string          `json:"priority"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		priority, perr := store.ParsePriority(in.Priority)
		if perr != nil {
			return nil, perr
		}
		toList, err := parseMCPToField(in.To)
		if err != nil {
			return nil, err
		}
		from, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		timeout := defaultDeliveredWaitTimeout
		if in.Timeout != "" {
			d, err := time.ParseDuration(in.Timeout)
			if err != nil {
				return nil, fmt.Errorf("invalid timeout %q: %w", in.Timeout, err)
			}
			timeout = d
		}
		cfg, _ := config.Load()
		maxRPS := config.ResolveInt(cfg, from, "max-recipients-per-send", capMaxRecipientsPerSend)
		p := sendParams{
			From:                 from,
			ReplyTo:              in.ReplyTo,
			Body:                 in.Body,
			NoReplyExpected:      in.NoReplyExpected,
			Quick:                in.Quick,
			MaxRecipient:         capRecipientQueue,
			MaxSender:            capSenderBacklog,
			MaxBody:              capBodyBytes,
			MaxRecipientsPerSend: maxRPS,
			Strict:               in.Strict,
			WaitForDelivered:     in.WaitForDelivered,
			Timeout:              timeout,
			BlockOnStale:         in.BlockOnStale,
			DeliverAfter:         in.DeliverAfter,
			ExpectsReply:         in.ExpectsReply,
			Priority:             priority,
		}
		if len(toList) > 1 {
			p.ToRecipients = toList
			return doMultiSendMCP(ctx, s, p)
		}
		if len(toList) == 1 {
			p.To = toList[0]
		}
		return doSendMCP(ctx, s, p)
	}
}

// parseMCPToField decodes the `to` field which may be a JSON string or a
// JSON array of strings (#158). Returns an error when the field is absent,
// empty, or neither shape.
func parseMCPToField(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("to required")
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		if len(arr) == 0 {
			return nil, fmt.Errorf("to: array must not be empty")
		}
		return arr, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, fmt.Errorf("to required")
		}
		return []string{s}, nil
	}
	return nil, fmt.Errorf("to must be a string or array of strings")
}

// doMultiSendMCP is the MCP-side equivalent of runMultiSendWithStore: runs
// cross-call validation once, then fans the send to each recipient
// independently, collecting per-recipient outcomes. Returns MultiSendResponse.
func doMultiSendMCP(ctx context.Context, s *store.Store, p sendParams) (any, error) {
	// #228: resolve special recipient "operator" in any ToRecipients entry.
	if err := resolveOperatorInSendParams(ctx, s, &p); err != nil {
		return nil, err
	}
	if p.Body == "" {
		return nil, fmt.Errorf("body required")
	}
	if p.MaxBody > 0 && len(p.Body) > p.MaxBody {
		return nil, fmt.Errorf("body too large (%d > %d bytes)", len(p.Body), p.MaxBody)
	}
	if p.MaxRecipientsPerSend > 0 && len(p.ToRecipients) > p.MaxRecipientsPerSend {
		return nil, fmt.Errorf("too many recipients: %d (max %d per send)", len(p.ToRecipients), p.MaxRecipientsPerSend)
	}
	if _, err := s.GetAgent(ctx, p.From); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("unknown sender: %s", p.From)
		}
		return nil, err
	}
	var freshness *ThreadFreshness
	if p.ReplyTo != "" {
		tf, err := resolveThreadFreshness(ctx, s, p.ReplyTo, p.From)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("unknown reply-to id: %s", p.ReplyTo)
			}
			return nil, err
		}
		freshness = tf
		if p.BlockOnStale && tf.Stale {
			return nil, fmt.Errorf("thread has %d newer message(s) addressed to you since you last spoke",
				len(tf.NewerInThread))
		}
	}

	results := make([]MultiSendResult, 0, len(p.ToRecipients))
	anyFailed := false
	// #580: per-pool fan-out stagger — space same-pool inserts so the recipients
	// don't all wake into the same token-quota window (the internal rate-limit
	// layer). Below a pool's threshold this is a no-op (offset 0).
	staggerStart := fanoutNow()
	staggerOffsets := resolveFanoutOffsets(ctx, s, p.ToRecipients)
	for i, to := range p.ToRecipients {
		fanoutStaggerWait(ctx, staggerStart, staggerOffsets[i])
		sp := p
		sp.To = to
		resp, err := sendOneRecipient(ctx, s, sp)
		if err != nil {
			return nil, err
		}
		mr := MultiSendResult{
			To:        to,
			OK:        resp.OK,
			ID:        resp.ID,
			Queued:    resp.Queued,
			Recipient: resp.Recipient,
			Delivery:  resp.Delivery,
			Receipt:   resp.Receipt,
			Freshness: freshness,
			Error:     resp.Error,
		}
		if !resp.OK {
			anyFailed = true
		}
		results = append(results, mr)
	}
	return MultiSendResponse{OK: !anyFailed, Messages: results}, nil
}

// doSendMCP is the MCP-side equivalent of runSendWithStore. We use the
// same validation cascade but return structured Go data instead of writing
// JSON to a Writer.
func doSendMCP(ctx context.Context, s *store.Store, p sendParams) (any, error) {
	// #228: resolve special recipient "operator" first.
	if err := resolveOperatorInSendParams(ctx, s, &p); err != nil {
		return nil, err
	}
	if p.To == "" {
		return nil, fmt.Errorf("to required")
	}
	if p.Body == "" {
		return nil, fmt.Errorf("body required")
	}
	if p.MaxBody > 0 && len(p.Body) > p.MaxBody {
		return nil, fmt.Errorf("body too large (%d > %d bytes)", len(p.Body), p.MaxBody)
	}
	// Deferred delivery (#227): validate the trigger before any insert.
	if p.DeliverAfter != "" {
		if err := validateDeferTrigger(p.DeliverAfter); err != nil {
			return nil, err
		}
	}
	// Recipient status (#152) doubles as the registry-existence check:
	// unknown recipient stays fail-loud (day-one safety, #3/#4/#15).
	rs, err := resolveRecipientStatus(ctx, s, p.To)
	if err != nil {
		return nil, err
	}
	if !rs.Registered {
		return nil, fmt.Errorf("unknown recipient: %s", p.To)
	}
	// --strict additionally rejects a registered-but-unreachable recipient
	// (pane gone). Returned as a structured ok:false result (not a Go error)
	// so the caller still gets the recipient block. Skipped for a deferred
	// send (#227) — future delivery, so current unreachability isn't a failure.
	if p.Strict && !rs.Alive && p.DeliverAfter == "" {
		return SendResponse{
			OK:        false,
			Recipient: rs,
			Error:     fmt.Sprintf("recipient %q registered but not reachable (pane %s)", p.To, rs.PaneStatus),
		}, nil
	}
	if _, err := s.GetAgent(ctx, p.From); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("unknown sender: %s", p.From)
		}
		return nil, err
	}
	// Thread-freshness (#155) — mirrors runSendWithStore. Runs before the
	// insert so block_on_stale can refuse without queueing. A registered-
	// but-stale thread returns a structured ok:false result (not a Go error)
	// so the caller keeps the freshness block.
	var freshness *ThreadFreshness
	if p.ReplyTo != "" {
		tf, err := resolveThreadFreshness(ctx, s, p.ReplyTo, p.From)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("unknown reply-to id: %s", p.ReplyTo)
			}
			return nil, err
		}
		freshness = tf
		if p.BlockOnStale && tf.Stale {
			return SendResponse{
				OK:        false,
				Recipient: rs,
				Freshness: tf,
				Error:     fmt.Sprintf("thread has %d newer message(s) addressed to you since you last spoke", len(tf.NewerInThread)),
			}, nil
		}
	}
	// Cap enforcement lives inside InsertMessage's transaction since
	// #29 — no pre-check needed.
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent:         p.From,
		ToAgent:           p.To,
		ReplyTo:           p.ReplyTo,
		Body:              p.Body,
		NoReplyExpected:   p.NoReplyExpected,
		Quick:             p.Quick,
		DeliverAfter:      p.DeliverAfter,
		ExpectsReply:      p.ExpectsReply,
		Priority:          p.Priority,
		MaxRecipientQueue: p.MaxRecipient,
		MaxSenderBacklog:  p.MaxSender,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("unknown reply-to id: %s", p.ReplyTo)
		}
		return nil, err
	}
	resp := SendResponse{
		OK:           true,
		ID:           res.PublicID,
		Queued:       res.Queued,
		Recipient:    rs,
		Freshness:    freshness,
		DeliverAfter: p.DeliverAfter, // non-empty → staged, not queued (#227)
	}
	// A deferred send never delivers within the wait window (it's staged until
	// flush), so the wait is skipped — it would always time out misleadingly.
	if p.WaitForDelivered && p.DeliverAfter == "" {
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = defaultDeliveredWaitTimeout
		}
		resp.Delivery = waitForDelivery(ctx, s, res.PublicID, p.To, timeout, pingPollInterval)
	}
	resp.Receipt = newSendReceipt(sendCreatedAt(ctx, s, res.PublicID), p.DeliverAfter, resp.Delivery)
	return resp, nil
}

// mcpFlushDeferredHandler returns the handler for the tmux-tell.flush_deferred
// MCP tool (#227). It resolves the caller's identity and promotes that agent's
// deferred messages matching the trigger — the chamber-side signal "I'm at
// <trigger point>, deliver what I staged." Authorization is implicit: the
// caller can only flush messages addressed to itself (doFlushDeferred →
// PromoteDeferred is scoped to to_agent = caller).
func mcpFlushDeferredHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		Trigger string `json:"trigger"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		if in.Trigger == "" {
			in.Trigger = deferTriggerResume
		}
		name, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		return doFlushDeferred(ctx, s, name, in.Trigger)
	}
}

// mcpAskHandler returns the handler for the tmux-tell.ask MCP tool (#250): a
// single-recipient send that marks expects_reply and returns the message id as
// the ask_id to pass to wait_for_reply / check_replies. Routes through the
// shared send path (doSendMCP) so caps / recipient-status / thread-freshness
// behave identically to send.
func mcpAskHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		To      string `json:"to"`
		Body    string `json:"body"`
		ReplyTo string `json:"reply_to"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		from, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		return doSendMCP(ctx, s, sendParams{
			From:         from,
			To:           in.To,
			ReplyTo:      in.ReplyTo,
			Body:         in.Body,
			ExpectsReply: true,
			MaxRecipient: capRecipientQueue,
			MaxSender:    capSenderBacklog,
			MaxBody:      capBodyBytes,
		})
	}
}

// mcpWaitForReplyHandler returns the handler for tmux-tell.wait_for_reply (#250):
// block until a reply to ask_id addressed to the caller arrives or timeout_ms
// elapses. No auto-ack (Q3); an unverified reply is returned with the flag (Q4).
func mcpWaitForReplyHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		AskID     string `json:"ask_id"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		if in.AskID == "" {
			return nil, fmt.Errorf("ask_id required")
		}
		caller, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		timeout := 30 * time.Second
		if in.TimeoutMs > 0 {
			timeout = time.Duration(in.TimeoutMs) * time.Millisecond
		}
		return doWaitForReply(ctx, s, caller, in.AskID, timeout), nil
	}
}

// mcpCheckRepliesHandler returns the handler for tmux-tell.check_replies (#250):
// non-blocking — list replies to ask_id addressed to the caller, optionally
// only those with id > since.
func mcpCheckRepliesHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		AskID string `json:"ask_id"`
		Since int64  `json:"since"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		if in.AskID == "" {
			return nil, fmt.Errorf("ask_id required")
		}
		caller, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		return doCheckReplies(ctx, s, caller, in.AskID, in.Since)
	}
}

// mcpResendHandler returns the handler for the tmux-tell.resend MCP tool.
// Mirrors runResendWithStore via the shared resendGuard + replayRefusal so the
// guard policy can't drift between the CLI and MCP surfaces.
func mcpResendHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		ID    string `json:"id"`
		Force bool   `json:"force"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		return doResendMCP(ctx, s, resendParams{OriginalID: in.ID, Force: in.Force})
	}
}

// doResendMCP is the MCP-side equivalent of runResendWithStore: same fetch +
// guard + insert cascade, returning structured Go data instead of writing JSON.
// A guard refusal is a structured ok:false result (not an MCP error) so the
// caller keeps the replay block; an unknown id / unregistered recipient surface
// as MCP errors.
func doResendMCP(ctx context.Context, s *store.Store, p resendParams) (any, error) {
	if p.OriginalID == "" {
		return nil, fmt.Errorf("id required")
	}
	orig, err := s.GetMessage(ctx, p.OriginalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("unknown message id: %s", p.OriginalID)
		}
		return nil, err
	}
	// #230 (C): deprecation WARN to the daemon log when --force is passed
	// against a delivered_in_input_box message (no longer needed). Once per
	// process — the MCP daemon serves many resends in one lifetime.
	maybeWarnResendForceUnverified(os.Stderr, orig, p.Force)
	if reason, ok := resendGuard(orig, p.Force); !ok {
		return replayRefusal(orig, reason), nil
	}
	rs, err := resolveRecipientStatus(ctx, s, orig.ToAgent)
	if err != nil {
		return nil, err
	}
	if !rs.Registered {
		return nil, fmt.Errorf("original recipient %s is no longer registered", orig.ToAgent)
	}
	var replyTo string
	if orig.ReplyTo.Valid {
		replyTo = orig.ReplyTo.String
	}
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent:         orig.FromAgent,
		ToAgent:           orig.ToAgent,
		ReplyTo:           replyTo,
		Body:              orig.Body,
		NoReplyExpected:   orig.NoReplyExpected,
		Quick:             orig.Quick,
		ReplayOf:          orig.PublicID,
		ReplayOfAt:        orig.CreatedAt,
		MaxRecipientQueue: capRecipientQueue,
		MaxSenderBacklog:  capSenderBacklog,
	})
	if err != nil {
		return nil, err
	}
	return SendResponse{
		OK:        true,
		ID:        res.PublicID,
		Queued:    res.Queued,
		Recipient: rs,
		Replay: &ReplayStatus{
			OriginalID:     orig.PublicID,
			OriginalSentAt: orig.CreatedAt,
			OriginalState:  displayState(*orig),
			Forced:         p.Force,
		},
	}, nil
}

func mcpMessageStatusHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		ID string `json:"id"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		// doTrack returns the struct directly; the MCP framework
		// json.Marshals handler returns (internal/mcp/server.go:212),
		// so the JSON tags on trackResult are the single source of
		// truth for the wire shape.
		return doTrack(ctx, s, in.ID)
	}
}

func mcpGetHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		ID string `json:"id"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		requester, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		cfg, cfgErr := config.Load()
		if cfgErr != nil {
			// Config load failure should not block the access check —
			// without the privileged-agents allowlist, the default rule
			// (sender OR recipient) still applies. doGet handles nil cfg.
			cfg = nil
		}
		return doGet(ctx, s, cfg, requester, in.ID)
	}
}

func mcpControlHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		To               string `json:"to"`
		Command          string `json:"command"`
		ResumeWith       string `json:"resume_with"`
		ForTask          string `json:"for_task"`
		ForceRateLimited bool   `json:"force_rate_limited"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		from, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		res, err := doControl(ctx, s, controlParams{
			From:             from,
			To:               in.To,
			Command:          in.Command,
			ResumeWith:       in.ResumeWith,
			ForTask:          in.ForTask,
			ForceRateLimited: in.ForceRateLimited,
			MaxRecipient:     capRecipientQueue,
			MaxSender:        capSenderBacklog,
			MaxBody:          capBodyBytes,
		})
		if err != nil {
			return nil, err
		}
		// #480: a legacy control-macro alias still runs, but log a greppable
		// deprecation WARN to the MCP server's stderr (journal) so an operator
		// can spot fleet usage of the old name. The agent also sees the
		// `deprecated` field in the response below.
		if res.Deprecated != "" {
			fmt.Fprintf(os.Stderr, "WARN deprecated_control_macro %s\n", res.Deprecated)
		}
		// The MCP framework json.Marshals handler returns
		// (internal/mcp/server.go:212), and controlResult's JSON tags
		// already encode the wire shape — so returning the struct
		// directly produces byte-identical output to the previous
		// map[string]any construction. Both callers now go through the
		// same single source of truth for the wire shape.
		return res, nil
	}
}

func mcpAgentsHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		AvailableOnly bool `json:"available_only"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		_ = json.Unmarshal(args, &in)
		live, err := tmuxio.LivePanes(ctx)
		if err != nil {
			return nil, err
		}
		agents, err := s.ListAgents(ctx)
		if err != nil {
			return nil, err
		}
		conflicts := paneConflicts(agents)
		out := []agentView{}
		for _, a := range agents {
			v := agentView{Name: a.Name, Pane: a.PaneID, Paused: a.Paused, AttentionState: a.AttentionState, Stuck: a.StuckReason, DisplayName: a.DisplayName, PaneConflict: len(conflicts[a.PaneID]) > 0}
			switch {
			case a.PaneID == "":
				v.PaneStatus = "no-pane"
			case live[a.PaneID]:
				v.PaneStatus = "live"
			default:
				v.PaneStatus = "stale"
			}
			depth, err := s.RecipientQueueDepth(ctx, a.Name)
			if err != nil {
				return nil, err
			}
			v.Queued = depth
			if last, ok, err := s.RecipientLastDelivered(ctx, a.Name); err != nil {
				return nil, err
			} else if ok {
				v.MailmanLastDelivered = last
			}
			if in.AvailableOnly && (v.PaneStatus != "live" || v.Paused) {
				continue
			}
			out = append(out, v)
		}
		return out, nil
	}
}

func mcpWhoamiHandler(s *store.Store) mcp.ToolHandler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		name, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		a, err := s.GetAgent(ctx, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return map[string]any{
					"ok":         false,
					"error":      "agent not in registry",
					"name":       name,
					"registered": false,
				}, nil
			}
			return nil, err
		}
		live, _ := tmuxio.LivePanes(ctx)
		var paneStatus string
		switch {
		case a.PaneID == "":
			paneStatus = "no-pane"
		case live[a.PaneID]:
			paneStatus = "live"
		default:
			paneStatus = "stale"
		}
		depth, _ := s.RecipientQueueDepth(ctx, name)
		return map[string]any{
			"ok":          true,
			"name":        a.Name,
			"registered":  true,
			"pane":        a.PaneID,
			"pane_status": paneStatus,
			"paused":      a.Paused,
			"queued":      depth,
		}, nil
	}
}

func mcpInboxHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		State      string   `json:"state"`
		Limit      int      `json:"limit"`
		AckIDs     []string `json:"ack_ids"`
		AckAll     bool     `json:"ack_all"`
		Unanswered bool     `json:"unanswered"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		_ = json.Unmarshal(args, &in)
		name, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}

		// Ack-all path: drain announce-skipped backlog residue (#221).
		if in.AckAll {
			a, err := s.GetAgent(ctx, name)
			if err != nil {
				return nil, fmt.Errorf("agent %q not registered: %w", name, err)
			}
			var epoch int64
			if a.BacklogEpoch.Valid {
				epoch = a.BacklogEpoch.Int64
			}
			n, err := s.MarkAcknowledgedBatch(ctx, name, epoch)
			if err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "acked": n}, nil
		}

		// Ack-ids path: mark specific messages acknowledged (#221).
		if len(in.AckIDs) > 0 {
			for _, id := range in.AckIDs {
				if err := s.MarkAcknowledged(ctx, name, id); err != nil {
					return nil, err
				}
			}
			return map[string]any{"ok": true, "acked": len(in.AckIDs)}, nil
		}

		// Default: list inbox.
		state := store.State(in.State)
		if state == "" {
			state = store.StateQueued
		}
		limit := in.Limit
		if limit == 0 {
			limit = 50
		}
		msgs, err := s.ListMessages(ctx, store.ListFilter{
			ToAgent:    name,
			State:      state,
			Limit:      limit,
			Unanswered: in.Unanswered,
		})
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, messageToMap(m))
		}
		return out, nil
	}
}

func mcpRegisterHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		Name         string `json:"name"`
		Pane         string `json:"pane"`
		StartMailman *bool  `json:"start_mailman"`
		Force        bool   `json:"force"`
		Alias        string `json:"alias"`
		DeliveryMode string `json:"delivery_mode"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		if in.Name == "" {
			return nil, fmt.Errorf("name required")
		}
		pane := in.Pane
		if pane == "" {
			pane = os.Getenv("TMUX_PANE")
		}
		if pane == "" {
			return nil, fmt.Errorf("pane required (no --pane given and $TMUX_PANE empty)")
		}

		// delivery_mode default + validation.
		deliveryMode := in.DeliveryMode
		if deliveryMode == "" {
			deliveryMode = store.DeliveryModePasteAndEnter
		}
		if !store.ValidDeliveryMode(deliveryMode) {
			return nil, fmt.Errorf("invalid delivery_mode %q (want %q or %q)",
				deliveryMode, store.DeliveryModePasteAndEnter, store.DeliveryModeMailboxOnly)
		}

		// Collision check.
		existing, err := s.GetAgent(ctx, in.Name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if existing != nil && !in.Force {
			return nil, fmt.Errorf("agent %q already registered with pane %s; pass force=true to overwrite",
				in.Name, existing.PaneID)
		}

		if err := s.UpsertAgent(ctx, in.Name, pane); err != nil {
			return nil, err
		}
		if err := s.SetDeliveryMode(ctx, in.Name, deliveryMode); err != nil {
			return nil, fmt.Errorf("set delivery_mode: %w", err)
		}
		// #291: clear any mailman stuck-state on (re)register. A parked mailman
		// re-registered with a corrected pane would otherwise update pane_id
		// yet stay parked, silently never resuming delivery. Best-effort —
		// registration already succeeded above, so a failed clear is
		// operationally awkward but not bus-fatal (and the MCP closure has no
		// stderr for a soft WARN).
		_ = s.ClearStuck(ctx, in.Name)
		// #298: clear any prior attention_state on (re)register, mirroring the
		// CLI auto-clear (#224). A chamber re-registering via MCP — the spawn-
		// die / self-recovery / ad-hoc reset path — is back and ready;
		// whatever it was awaiting is presumed resolved (or answered
		// out-of-band). Closes the CLI/MCP asymmetry surfaced during #297
		// review (the path chambers actually use must clear the same state
		// as the operator-typed CLI does). Best-effort, same rationale as
		// the ClearStuck above.
		_ = s.SetAttentionState(ctx, in.Name, store.AttentionStateIdle)
		// #626 Phase 1b: self-discover the intrinsic session identity from the
		// registering pane's process tree. This is the path chambers actually use
		// — register from INSIDE the live session via MCP, where the session env
		// is reliably present (unlike the wrapper's launch-time CLI register,
		// which can run before the session starts). Best-effort: when not found
		// (non-Claude CLI, or a bare pane) session_id is left untouched, so a
		// prior value is preserved and a never-set one stays empty → name-based
		// fallback (#626 AC6).
		if sid, ok := discover.New().SessionIDForPane(ctx, pane); ok && sid != "" {
			_ = s.SetSessionID(ctx, in.Name, sid)
		}

		// Optional alias append. AddAlias is idempotent on same-agent
		// duplicates, but rejects cross-canonical collisions with
		// ErrAliasCollision (Surveyor Q(a) review of v0.2.0).
		if in.Alias != "" {
			if err := s.AddAlias(ctx, in.Name, in.Alias); err != nil {
				if errors.Is(err, store.ErrAliasCollision) {
					return nil, fmt.Errorf("alias %q rejected: %w", in.Alias, err)
				}
				return nil, fmt.Errorf("add alias: %w", err)
			}
		}

		// Surface the recipient's queued-message backlog at register time
		// (#151) so a fresh or re-registering session learns it has mail
		// waiting without a separate inbox poll. Non-fatal: registration
		// already succeeded, so a count hiccup degrades to a soft
		// `queued_error` field rather than failing the register (an honest
		// 0 must not be confused with "unknown"). Mirrors the CLI register
		// surface so the MCP and CLI responses stay shape-aligned.
		queued, qErr := s.RecipientQueueDepth(ctx, in.Name)

		resp := map[string]any{
			"ok":            true,
			"name":          in.Name,
			"pane":          pane,
			"delivery_mode": deliveryMode,
			"registered":    true,
		}
		if qErr != nil {
			resp["queued_error"] = qErr.Error()
		} else {
			resp["queued"] = queued
			// #204 don't-flood policy: stamp the claim-floor + insert the
			// 📬 nudge per the resolved on-register-backlog policy when this
			// (re)register found a queued backlog. Config load degrades to
			// defaults on error. Gated on qErr == nil (a count hiccup must
			// not read as an empty backlog). Mirrors the CLI register path.
			cfg, _ := config.Load()
			addBacklogPolicyFields(resp, applyBacklogPolicy(ctx, s, cfg, in.Name, deliveryMode, queued))
		}

		// #258(a): promote register-deferred messages AFTER the backlog count +
		// floor, so the register rows aren't folded into the ordinary-backlog
		// 📬 nudge (they deliver via #227's deliver_after floor-exemption, not
		// via the announce policy). See the CLI register path for the full
		// re-evaluation of the AC's "promote before floor" sketch. Best-effort
		// (the MCP closure has no stderr for a soft WARN); non-zero count only.
		if deferredPromoted, dpErr := s.PromoteDeferred(ctx, in.Name, deferTriggerRegister); dpErr != nil {
			resp["deferred_promoted_error"] = dpErr.Error()
		} else if deferredPromoted > 0 {
			resp["deferred_promoted"] = deferredPromoted
		}

		// Default start_mailman to true — UNLESS delivery_mode is
		// mailbox-only, in which case the implicit default is false
		// (no daemon needed; messages stay queued for operator polling
		// per #116). Explicit start_mailman=true overrides the
		// implicit default if operator really wants a daemon running.
		start := deliveryMode != store.DeliveryModeMailboxOnly
		if in.StartMailman != nil {
			start = *in.StartMailman
		}
		if start {
			// #293: refuse start_mailman when the MCP process is running
			// against a non-default DB path. The systemd-managed mailman
			// launches from the unit-file Environment= (default DB), so a
			// sandbox-DB MCP that starts a systemd mailman silently misroutes
			// — agent row in sandbox DB, mailman polling production DB. Skip
			// the mailman start + surface the reason in mailman_error rather
			// than the registration error path, since the upsert above already
			// succeeded; the operator's actionable next step is foreground
			// `serve --agent NAME`, named in the error.
			if mismatched, callerDB := startMailmanWouldMismatchSystemd(resolveDBPath("")); mismatched {
				resp["mailman"] = "skipped"
				resp["mailman_error"] = startMailmanMismatchError(in.Name, callerDB)
				return resp, nil
			}
			// #356: refuse start_mailman when D-Bus / XDG session vars are absent.
			// Codex MCP children don't inherit these from the shell, so systemctl
			// --user can't reach the user bus. Surface as skipped+error rather than
			// a hard MCP error — the registration itself succeeded above.
			if missing := startMailmanMissingEnv(); len(missing) > 0 {
				resp["mailman"] = "skipped"
				resp["mailman_error"] = startMailmanEnvError(in.Name, missing)
				return resp, nil
			}
			if err := startMailman(ctx, in.Name); err != nil {
				resp["mailman"] = "failed"
				resp["mailman_error"] = err.Error()
				return resp, nil
			}
			resp["mailman"] = "active"
		} else {
			resp["mailman"] = "skipped"
		}
		return resp, nil
	}
}

func mcpUnregisterHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		Name       string `json:"name"`
		PurgeQueue bool   `json:"purge_queue"`
		Force      bool   `json:"force"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		if in.Name == "" {
			return nil, fmt.Errorf("name required")
		}

		// Idempotent: absent agent is success with removed:false.
		existing, err := s.GetAgent(ctx, in.Name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("lookup: %w", err)
		}
		if existing == nil {
			return map[string]any{
				"ok":      true,
				"name":    in.Name,
				"removed": false,
			}, nil
		}

		// Queue-depth guard: refuse if messages are queued unless force.
		if !in.Force {
			depth, err := s.RecipientQueueDepth(ctx, in.Name)
			if err != nil {
				return nil, fmt.Errorf("queue depth: %w", err)
			}
			if depth > 0 {
				return nil, fmt.Errorf("agent %q has %d queued message(s); pass force:true to override",
					in.Name, depth)
			}
		}

		// Stop the mailman before removing the row so it doesn't observe a
		// dangling agent reference. Soft-fail per #338: the agents-table row
		// is authoritative; the systemd unit is a downstream consumer, so a
		// systemctl flake must not block the row removal. A surviving unit
		// gets noticed by #340's serve-exit-on-missing-agent path.
		mailmanStatus := "stopped"
		var mailmanErr string
		if err := stopMailman(ctx, in.Name); err != nil {
			mailmanStatus = "warn"
			mailmanErr = err.Error()
		}

		var purged int64
		if in.PurgeQueue {
			n, err := s.DeleteMessages(ctx, in.Name, []store.State{store.StateQueued})
			if err != nil {
				return nil, err
			}
			purged = n
		}

		removed, err := s.DeleteAgent(ctx, in.Name)
		if err != nil {
			return nil, err
		}

		out := map[string]any{
			"ok":      true,
			"name":    in.Name,
			"removed": removed,
			"mailman": mailmanStatus,
			"deleted": purged,
		}
		if mailmanErr != "" {
			out["mailman_error"] = mailmanErr
		}
		return out, nil
	}
}

func mcpStatusHandler(s *store.Store) mcp.ToolHandler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		agents, err := s.ListAgents(ctx)
		if err != nil {
			return nil, err
		}
		rows := []agentStatus{}
		for _, a := range agents {
			st := agentStatus{Name: a.Name, Paused: a.Paused}
			for _, state := range []store.State{
				store.StateQueued, store.StateDelivering,
				store.StateDelivered, store.StateFailed,
			} {
				msgs, err := s.ListMessages(ctx, store.ListFilter{
					ToAgent: a.Name, State: state, Limit: 1000,
				})
				if err != nil {
					return nil, err
				}
				switch state {
				case store.StateQueued:
					st.Queued = len(msgs)
					if len(msgs) > 0 {
						st.OldestQueuedAge = ageOf(msgs[0].CreatedAt)
					} else {
						st.OldestQueuedAge = "-"
					}
				case store.StateDelivering:
					st.Delivering = len(msgs)
				case store.StateDelivered:
					st.Delivered = len(msgs)
				case store.StateFailed:
					st.Failed = len(msgs)
				}
			}
			rows = append(rows, st)
		}
		return rows, nil
	}
}
