package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/config"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/mcp"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// runMCPCLI parses MCP-mode flags, opens the store, and serves on stdio.
//
// Usage: tmux-msg-claude mcp [--db PATH]
//
// Identity is resolved from $TMUX_AGENT_NAME (explicit override) or
// from $TMUX_PANE looked up in the agents registry. The latter means a
// pane that's registered (via `discover` or manual INSERT) just works —
// no per-pane MCP config needed.
func runMCPCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		fmt.Fprintf(stderr, "open store: %v\n", err)
		return exitInternal
	}
	defer s.Close()

	srv := newMCPServer(s)
	if err := srv.Serve(context.Background(), stdin, stdout); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(stderr, "mcp serve: %v\n", err)
		return exitInternal
	}
	return exitOK
}

// newMCPServer wires the tmux-msg.* tools onto an mcp.Server.
// Tools registered: send / resend / ping / agents / whoami / inbox /
// message_status / status / register / control / unregister /
// agent_state.
func newMCPServer(s *store.Store) *mcp.Server {
	srv := mcp.NewServer("tmux-msg", "0.1.0")

	srv.RegisterTool("tmux-msg.send",
		"Queue a message for another agent (sender resolved from $TMUX_AGENT_NAME or $TMUX_PANE→registry). Returns {ok,id,queued,recipient}: \"queued\" means the bus accepted it — the recipient sees it once their mailman delivers. The \"recipient\" block reports send-time disposition (registered/alive/delivery_mode/mailman_running/pane_status). Confirm delivery synchronously with wait_for_delivered, or after the fact with tmux-msg.message_status. Set reply_to to thread under an earlier message — when you do, the response adds a \"thread_freshness\" block flagging whether the thread moved since you last spoke (crossed-message guard, #155). Set quick=true to render compact single-line chrome (✓ Sender · [re X ·] body) in the recipient's pane instead of the full bracket-header block — for routine acks where typing-overhead-to-signal ratio is high (#154). Multi-recipient: pass to as an array (e.g. [\"bosun\",\"surveyor\"]) to fan the message to multiple recipients in a single call — each recipient gets its own message id; the response shape changes to {ok,messages:[{to,id,queued,recipient,...},...]} (#158).",
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
				"wait_for_delivered": {"type": "boolean", "description": "Block until the message reaches a terminal delivery state (delivered/failed) or timeout, returning a \"delivery\" block with state + verify_ms. Default false (#152)."},
				"timeout":           {"type": "string", "description": "Bound for wait_for_delivered as a Go duration (e.g. \"10s\"). Default 10s."},
				"block_on_stale":    {"type": "boolean", "description": "With reply_to: fail (ok:false) instead of queueing when the thread_freshness check finds the thread moved since you last spoke (newer messages addressed to you arrived after your last message in the chain). Default false — staleness is reported but the send still succeeds (#155)."},
				"deliver_after":     {"type": "string", "description": "Defer delivery until a trigger fires (#227): the message is STAGED (not queued) and delivers only after a matching flush_deferred call. Primary use: post-compaction self-handoff — send yourself orientation text with deliver_after=\"resume\", then call tmux-msg.flush_deferred{trigger:\"resume\"} as part of your post-/compact resume routine so it lands in the freshly-resumed context rather than being absorbed by the summarizer. v1 accepts only \"resume\". Single-recipient only. The response carries deliver_after to confirm staging."}
			},
			"required": ["to", "body"]
		}`),
		mcpSendHandler(s))

	srv.RegisterTool("tmux-msg.resend",
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

	srv.RegisterTool("tmux-msg.flush_deferred",
		"Promote your own deferred messages for a trigger to delivery (#227). A deferred message (sent with deliver_after) is STAGED — invisible to inbox/mailman — until you flush its trigger. Primary use: POST-COMPACTION SELF-HANDOFF. Before /compact, send yourself orientation with deliver_after=\"resume\"; then call this with trigger=\"resume\" as part of your resume routine, so the staged message lands in your freshly-resumed context instead of being absorbed by the summarizer. Idempotent — calling with no matching deferred messages is a no-op (promoted:0), so it's safe to call unconditionally on resume. You can only flush messages addressed to yourself. Returns {ok, trigger, promoted}. v1 trigger: \"resume\".",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"trigger": {"type": "string", "description": "The deferred-delivery trigger to fire. v1 accepts \"resume\". Default \"resume\"."}
			}
		}`),
		mcpFlushDeferredHandler(s))

	srv.RegisterTool("tmux-msg.ask",
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

	srv.RegisterTool("tmux-msg.wait_for_reply",
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

	srv.RegisterTool("tmux-msg.check_replies",
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

	srv.RegisterTool("tmux-msg.agents",
		"List registered agents with pane liveness.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"available_only": {"type": "boolean", "description": "Filter to live + not-paused agents"}
			}
		}`),
		mcpAgentsHandler(s))

	srv.RegisterTool("tmux-msg.whoami",
		"Return this session's registration. Identity from $TMUX_AGENT_NAME or $TMUX_PANE→registry.",
		json.RawMessage(`{"type": "object", "properties": {}}`),
		mcpWhoamiHandler(s))

	srv.RegisterTool("tmux-msg.inbox",
		"List the caller's own queued messages, or acknowledge announce-skipped backlog residue (#221). Pass ack_ids to mark specific messages acknowledged; pass ack_all=true to acknowledge all messages ≤ the backlog_epoch (drains the announce-skipped residue left by the don't-flood policy). Acknowledged messages are excluded from the default queued view but remain retrievable via tmux-msg.get.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"state": {"type": "string", "enum": ["queued","delivering","delivered","failed","acknowledged"]},
				"limit": {"type": "integer", "minimum": 1, "maximum": 1000},
				"ack_ids": {"type": "array", "items": {"type": "string"}, "description": "Public IDs of queued messages to mark acknowledged. Idempotent."},
				"ack_all": {"type": "boolean", "description": "Mark all queued messages ≤ backlog_epoch_id as acknowledged. Drains announce-skipped backlog residue."}
			}
		}`),
		mcpInboxHandler(s))

	srv.RegisterTool("tmux-msg.message_status",
		"Look up the delivery state of a sent message by its public_id. Returns created_at + delivered_at + error so the sender can see whether the bus has handed off the row yet.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "Public ID of the message to look up (returned in the send/control response)"}
			},
			"required": ["id"]
		}`),
		mcpMessageStatusHandler(s))

	srv.RegisterTool("tmux-msg.get",
		"Fetch a processed message by ID — recovery path for swallowed deliveries (#111). The bus stores message bodies; if the paste landed in a state that obscured the visible delivery (mid-AskUserQuestion, popup open, recipient mid-compaction), retrieving by ID returns the full body + metadata. Accepts full public_id or short prefix (4-char IDs from delivery headers work). Access: sender OR recipient OR allowlisted agent (`privileged-agents` in /etc/tmux-msg/config.toml). Not-found and not-authorized return the same error class to prevent existence leaks.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "Public ID or short prefix (e.g. '8f54' or 'a2c76333...'); falls back to disambiguation error if multiple authorized matches"}
			},
			"required": ["id"]
		}`),
		mcpGetHandler(s))

	srv.RegisterTool("tmux-msg.status",
		"Return registry overview: paused state + queue depths per agent.",
		json.RawMessage(`{"type": "object", "properties": {}}`),
		mcpStatusHandler(s))

	srv.RegisterTool("tmux-msg.flag_operator",
		"Signal that this chamber needs operator attention (#224). Posts the body to the reserved \"operator-attention\" recipient AND marks this chamber's attention_state as \"awaiting_operator\". The flag clears implicitly on the chamber's next register call (after the operator answered + chamber resumed) or explicitly via tmux-msg.clear_operator_flag. Body is required — it is the question or choice the chamber wants the operator to weigh in on. The recipient \"operator-attention\" MUST be pre-registered by the operator (as a mailbox-only agent) — chambers cannot register it themselves.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"body": {"type": "string", "description": "The question or choice the chamber wants the operator to weigh in on (required)"}
			},
			"required": ["body"]
		}`),
		mcpFlagOperatorHandler(s))

	srv.RegisterTool("tmux-msg.clear_operator_flag",
		"Clear this chamber's awaiting_operator attention signal (#224). Sets attention_state back to \"idle\". Used when the operator answered the chamber's question out of band (typed directly in the pane) and the chamber wants to clear the flag without going through register.",
		json.RawMessage(`{"type": "object", "properties": {}}`),
		mcpClearOperatorFlagHandler(s))

	srv.RegisterTool("tmux-msg.register",
		"Register this (or another) pane on the bus. Pane defaults to $TMUX_PANE; start_mailman defaults true UNLESS delivery_mode is `mailbox-only` (in which case it defaults to false — no daemon needed for the operator-as-bus-participant scenario). The response includes `queued`: the number of messages already waiting for this agent at register time (#151) — a fresh or post-restart session learns it has backlog without a separate inbox poll; check it and run tmux-msg.inbox if >0. When that backlog exists, the don't-flood policy (#204) keeps the mailman from pasting the whole queue at once: by default it leaves the backlog queued and delivers a single `📬 N queued` nudge (the `on-register-backlog` TOML knob can switch to auto-delivering the newest N). The response then also carries `backlog_policy`, `backlog_skipped`, and `backlog_nudge`.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"name":          {"type": "string", "description": "Agent name (the new identity)"},
				"pane":          {"type": "string", "description": "Pane id like %5 (default: $TMUX_PANE)"},
				"start_mailman": {"type": "boolean", "description": "Run systemctl --user enable --now tmux-msg-claude-mailman@NAME (default true; default false when delivery_mode=mailbox-only). Note: start_mailman=true with delivery_mode=mailbox-only is allowed but vestigial — the daemon starts, observes mailbox-only at startup, logs the no-work condition, and exits cleanly. The 'mailman: active' field in the response is momentary in this case."},
				"force":         {"type": "boolean", "description": "Overwrite an existing row with the same name (default false)"},
				"alias":         {"type": "string", "description": "Optional alternative name discover should accept for this canonical agent (e.g. 'Master Bosun of Nimbus' for canonical 'bosun'). Append-only; existing aliases preserved."},
				"delivery_mode": {"type": "string", "enum": ["paste-and-enter", "mailbox-only"], "description": "How the mailman delivers to this agent (#116). 'paste-and-enter' (default): tmux paste + Enter into the agent's pane — the existing behavior for CLI-tool-hosting panes. 'mailbox-only': messages stay in state=queued; operator polls via tmux-msg-claude inbox. Use 'mailbox-only' to register an operator-shell pane as a bus destination (per ADR-0005 wheel-reinvention check)."}
			},
			"required": ["name"]
		}`),
		mcpRegisterHandler(s))

	srv.RegisterTool("tmux-msg.control",
		"Send a whitelisted Claude Code slash-command directly to a pane. Scope-gated: when to==self, the self-whitelist applies; when to is a peer, the peer-whitelist applies — with a third tier of per-edge exceptions for destructive commands. Specifically, /clear is globally denied but Bosun→Pilot and Quartermaster→Pilot are permitted (routine clear-before-each-task dispatch + rescue path when Pilot can't /compact out of token exhaustion). Bypasses the chat-message renderer. Optional resume_with (only with command=compact, only on self) queues a follow-up message that the mailman delivers AFTER /compact has settled — pre-write your continuation instead of going silent post-compact.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"to":          {"type": "string", "description": "Recipient agent name; set to your own name for self-invocation"},
				"command":     {"type": "string", "description": "Whitelisted command (e.g. 'compact'); leading slash optional"},
				"resume_with": {"type": "string", "description": "Optional continuation prompt delivered after /compact settles. Only valid with command=compact on self-invocation."}
			},
			"required": ["to", "command"]
		}`),
		mcpControlHandler(s))

	srv.RegisterTool("tmux-msg.unregister",
		"Remove an agent from the registry. stop_mailman defaults true.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"name":           {"type": "string"},
				"stop_mailman":   {"type": "boolean", "description": "Run systemctl --user disable --now (default true)"},
				"purge_messages": {"type": "boolean", "description": "Also delete delivered/failed audit rows (default false)"}
			},
			"required": ["name"]
		}`),
		mcpUnregisterHandler(s))

	srv.RegisterTool("tmux-msg.agent_state",
		"Probe an agent's agent-state via read-only capture-pane (#71). Returns one of five states: idle / working / at-rest-in-compaction / awaiting-operator / unknown. 'Knock at the door without waking the inhabitant' — exactly two capture-pane calls, zero pane mutation, ~200ms latency. Consumers should treat 'unknown' as advisory-not-authoritative per #65's substrate-class-of-claim convention (don't silently roll up an unknown classification to a known state). v1 detects idle/working/unknown reliably; at-rest-in-compaction and awaiting-operator land when #70's empirical capture populates the marker constants.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"agent": {"type": "string", "description": "Agent name to probe"}
			},
			"required": ["agent"]
		}`),
		mcpAgentStateHandler(s))

	srv.RegisterTool("tmux-msg.ping",
		"Substrate-only reachability probe (#144): is the recipient's mailman daemon up and its pane reachable? Queues a kind=ping row that the mailman picks up (proving the daemon is alive) and answers via substrate-health checks (agent registered, pane live) — it does NOT paste into the recipient's pane or load their context, so it's safe for runbook verification and post-restart sanity. Returns {ok, agent, id, state, elapsed_ms}: state is `delivered` (reachable), `failed` (registered but unreachable — pane gone), or `timeout` (no mailman answered in time — daemon down/paused/backlogged). Pinging a non-registered agent fails loud.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"agent":           {"type": "string", "description": "Agent name to probe for reachability"},
				"timeout_seconds": {"type": "number", "description": "Bound the wait for a terminal state (default 5). A reachable agent answers in well under a second."}
			},
			"required": ["agent"]
		}`),
		mcpPingHandler(s))

	return srv
}

// mcpPingHandler returns the handler for the tmux-msg.ping MCP tool.
// Resolves the caller's identity (the ping's sender) and runs the shared
// pingProbe core — the same code path as the `tmux-msg-claude ping` CLI
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
		if from == "" {
			return nil, fmt.Errorf("cannot resolve sender identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
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
// tmux-msg.agent_state MCP tool. Wraps resolveAgentState (shared
// with the CLI subcommand `tmux-msg-claude state`) so both surfaces emit
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

// --- tool handlers ---

// resolveMCPIdentity is the MCP-side adapter over identity.Resolve. The
// MCP path has no `--from` analogue, so it always passes "" as override.
// Kept as a thin wrapper so handler call-sites stay readable.
func resolveMCPIdentity(ctx context.Context, s *store.Store) (string, error) {
	name, _, err := identity.Resolve(ctx, s, "")
	return name, err
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
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		toList, err := parseMCPToField(in.To)
		if err != nil {
			return nil, err
		}
		from, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		if from == "" {
			return nil, fmt.Errorf("cannot resolve sender identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
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
	for _, to := range p.ToRecipients {
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
		resp.Delivery = waitForDelivery(ctx, s, res.PublicID, timeout, pingPollInterval)
	}
	return resp, nil
}

// mcpFlushDeferredHandler returns the handler for the tmux-msg.flush_deferred
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
		if name == "" {
			return nil, fmt.Errorf("cannot resolve identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
		}
		return doFlushDeferred(ctx, s, name, in.Trigger)
	}
}

// mcpAskHandler returns the handler for the tmux-msg.ask MCP tool (#250): a
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
		if from == "" {
			return nil, fmt.Errorf("cannot resolve sender identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
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

// mcpWaitForReplyHandler returns the handler for tmux-msg.wait_for_reply (#250):
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
		if caller == "" {
			return nil, fmt.Errorf("cannot resolve identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
		}
		timeout := 30 * time.Second
		if in.TimeoutMs > 0 {
			timeout = time.Duration(in.TimeoutMs) * time.Millisecond
		}
		return doWaitForReply(ctx, s, caller, in.AskID, timeout), nil
	}
}

// mcpCheckRepliesHandler returns the handler for tmux-msg.check_replies (#250):
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
		if caller == "" {
			return nil, fmt.Errorf("cannot resolve identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
		}
		return doCheckReplies(ctx, s, caller, in.AskID, in.Since)
	}
}

// mcpResendHandler returns the handler for the tmux-msg.resend MCP tool.
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
		if requester == "" {
			return nil, fmt.Errorf("cannot resolve requester identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
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
		To         string `json:"to"`
		Command    string `json:"command"`
		ResumeWith string `json:"resume_with"`
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
		if from == "" {
			return nil, fmt.Errorf("cannot resolve sender identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
		}
		res, err := doControl(ctx, s, controlParams{
			From:         from,
			To:           in.To,
			Command:      in.Command,
			ResumeWith:   in.ResumeWith,
			MaxRecipient: capRecipientQueue,
			MaxSender:    capSenderBacklog,
			MaxBody:      capBodyBytes,
		})
		if err != nil {
			return nil, err
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
		out := []agentView{}
		for _, a := range agents {
			v := agentView{Name: a.Name, Pane: a.PaneID, Paused: a.Paused, AttentionState: a.AttentionState}
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
		if name == "" {
			return nil, fmt.Errorf("cannot resolve identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
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
		paneStatus := "no-pane"
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
		State  string   `json:"state"`
		Limit  int      `json:"limit"`
		AckIDs []string `json:"ack_ids"`
		AckAll bool     `json:"ack_all"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		_ = json.Unmarshal(args, &in)
		name, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		if name == "" {
			return nil, fmt.Errorf("cannot resolve identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
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
			ToAgent: name, State: state, Limit: limit,
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

		// Default start_mailman to true — UNLESS delivery_mode is
		// mailbox-only, in which case the implicit default is false
		// (no daemon needed; messages stay queued for operator polling
		// per #116). Explicit start_mailman=true overrides the
		// implicit default if operator really wants a daemon running.
		start := true
		if deliveryMode == store.DeliveryModeMailboxOnly {
			start = false
		}
		if in.StartMailman != nil {
			start = *in.StartMailman
		}
		if start {
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
		Name          string `json:"name"`
		StopMailman   *bool  `json:"stop_mailman"`
		PurgeMessages bool   `json:"purge_messages"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		if in.Name == "" {
			return nil, fmt.Errorf("name required")
		}

		// Stop the mailman first so it doesn't try to deliver to a soon-
		// to-be-deleted agent.
		stop := true
		if in.StopMailman != nil {
			stop = *in.StopMailman
		}
		mailmanState := "skipped"
		if stop {
			if err := stopMailman(ctx, in.Name); err != nil {
				return nil, err
			}
			mailmanState = "stopped"
		}

		var deleted int64
		if in.PurgeMessages {
			n, err := s.DeleteMessages(ctx, in.Name,
				[]store.State{store.StateQueued, store.StateDelivering,
					store.StateDelivered, store.StateFailed})
			if err != nil {
				return nil, err
			}
			deleted = n
		}

		// Drop the agent row.
		if _, err := s.DB().ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, in.Name); err != nil {
			return nil, err
		}

		return map[string]any{
			"ok":           true,
			"name":         in.Name,
			"mailman":      mailmanState,
			"deleted":      deleted,
			"unregistered": true,
		}, nil
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
