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
		"Queue a message for another agent (sender resolved from $TMUX_AGENT_NAME or $TMUX_PANE→registry). Returns {ok,id,queued,recipient}: \"queued\" means the bus accepted it — the recipient sees it once their mailman delivers. The \"recipient\" block reports send-time disposition (registered/alive/delivery_mode/mailman_running/pane_status). Confirm delivery synchronously with wait_for_delivered, or after the fact with tmux-msg.message_status. Set reply_to to thread under an earlier message — when you do, the response adds a \"thread_freshness\" block flagging whether the thread moved since you last spoke (crossed-message guard, #155).",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"to":                {"type": "string", "description": "Recipient agent name"},
				"body":              {"type": "string", "description": "Message body"},
				"reply_to":          {"type": "string", "description": "Optional public_id of the message this replies to; threads the reply (renders the 'Sender → Recipient · re …' header) and enables the thread_freshness crossed-message check"},
				"no_reply_expected": {"type": "boolean", "description": "Set true to signal the recipient that no acknowledgment is needed — reduces ack-cascade on FYI/status messages (#145). Default false."},
				"strict":            {"type": "boolean", "description": "Fail (ok:false) if the recipient is registered but not reachable (pane gone). An UNregistered recipient is always fail-loud regardless of this flag. Default false (#152)."},
				"wait_for_delivered": {"type": "boolean", "description": "Block until the message reaches a terminal delivery state (delivered/failed) or timeout, returning a \"delivery\" block with state + verify_ms. Default false (#152)."},
				"timeout":           {"type": "string", "description": "Bound for wait_for_delivered as a Go duration (e.g. \"10s\"). Default 10s."},
				"block_on_stale":    {"type": "boolean", "description": "With reply_to: fail (ok:false) instead of queueing when the thread_freshness check finds the thread moved since you last spoke (newer messages addressed to you arrived after your last message in the chain). Default false — staleness is reported but the send still succeeds (#155)."}
			},
			"required": ["to", "body"]
		}`),
		mcpSendHandler(s))

	srv.RegisterTool("tmux-msg.resend",
		"Replay an existing message to its original recipient — the explicit recovery path for a message that landed `delivered_unverified` or `failed` (#157). The replay carries a \"Replayed: original sent at <ts>\" chrome marker so the recipient knows it's a re-send, and the response adds a \"replay\" block {original_id, original_sent_at, original_state, forced}. Refuses to replay an already-`delivered` (verified-or-unverified — the substrate can't distinguish, #169) or still in-flight message unless force=true, to avoid duplicate-spam; a `failed` message replays directly. The replayed body is byte-identical to the original.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"id":    {"type": "string", "description": "public_id of the message to replay"},
				"force": {"type": "boolean", "description": "Replay even an already-delivered or in-flight message (may duplicate). Required to recover a delivered-but-unverified message, since the DB can't distinguish it from a clean delivery (#169). Default false."}
			},
			"required": ["id"]
		}`),
		mcpResendHandler(s))

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
		"List the caller's own queued messages.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"state": {"type": "string", "enum": ["queued","delivering","delivered","failed"]},
				"limit": {"type": "integer", "minimum": 1, "maximum": 1000}
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

	srv.RegisterTool("tmux-msg.register",
		"Register this (or another) pane on the bus. Pane defaults to $TMUX_PANE; start_mailman defaults true UNLESS delivery_mode is `mailbox-only` (in which case it defaults to false — no daemon needed for the operator-as-bus-participant scenario). The response includes `queued`: the number of messages already waiting for this agent at register time (#151) — a fresh or post-restart session learns it has backlog without a separate inbox poll; check it and run tmux-msg.inbox if >0.",
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
		To               string `json:"to"`
		Body             string `json:"body"`
		ReplyTo          string `json:"reply_to"`
		NoReplyExpected  bool   `json:"no_reply_expected"`
		Strict           bool   `json:"strict"`
		WaitForDelivered bool   `json:"wait_for_delivered"`
		Timeout          string `json:"timeout"`
		BlockOnStale     bool   `json:"block_on_stale"`
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
		timeout := defaultDeliveredWaitTimeout
		if in.Timeout != "" {
			d, err := time.ParseDuration(in.Timeout)
			if err != nil {
				return nil, fmt.Errorf("invalid timeout %q: %w", in.Timeout, err)
			}
			timeout = d
		}
		p := sendParams{
			From:             from,
			To:               in.To,
			ReplyTo:          in.ReplyTo,
			Body:             in.Body,
			NoReplyExpected:  in.NoReplyExpected,
			MaxRecipient:     capRecipientQueue,
			MaxSender:        capSenderBacklog,
			MaxBody:          capBodyBytes,
			Strict:           in.Strict,
			WaitForDelivered: in.WaitForDelivered,
			Timeout:          timeout,
			BlockOnStale:     in.BlockOnStale,
		}
		// Re-use the validation + cap logic from the CLI by going
		// directly through the store ourselves but mirroring the checks.
		return doSendMCP(ctx, s, p)
	}
}

// doSendMCP is the MCP-side equivalent of runSendWithStore. We use the
// same validation cascade but return structured Go data instead of writing
// JSON to a Writer.
func doSendMCP(ctx context.Context, s *store.Store, p sendParams) (any, error) {
	if p.To == "" {
		return nil, fmt.Errorf("to required")
	}
	if p.Body == "" {
		return nil, fmt.Errorf("body required")
	}
	if p.MaxBody > 0 && len(p.Body) > p.MaxBody {
		return nil, fmt.Errorf("body too large (%d > %d bytes)", len(p.Body), p.MaxBody)
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
	// so the caller still gets the recipient block.
	if p.Strict && !rs.Alive {
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
		OK:        true,
		ID:        res.PublicID,
		Queued:    res.Queued,
		Recipient: rs,
		Freshness: freshness,
	}
	if p.WaitForDelivered {
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = defaultDeliveredWaitTimeout
		}
		resp.Delivery = waitForDelivery(ctx, s, res.PublicID, timeout, pingPollInterval)
	}
	return resp, nil
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
			OriginalState:  string(orig.State),
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
			v := agentView{Name: a.Name, Pane: a.PaneID, Paused: a.Paused}
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
		State string `json:"state"`
		Limit int    `json:"limit"`
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
