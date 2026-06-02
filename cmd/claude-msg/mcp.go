package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/identity"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/mcp"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

// runMCPCLI parses MCP-mode flags, opens the store, and serves on stdio.
//
// Usage: claude-msg mcp [--db PATH]
//
// Identity is resolved from $CLAUDE_AGENT_NAME (explicit override) or
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

// newMCPServer wires the five semaphore.* tools onto an mcp.Server.
func newMCPServer(s *store.Store) *mcp.Server {
	srv := mcp.NewServer("semaphore", "0.1.0")

	srv.RegisterTool("semaphore.send",
		"Queue a message for another agent. Sender is resolved from $CLAUDE_AGENT_NAME or $TMUX_PANE→registry.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"to":       {"type": "string", "description": "Recipient agent name"},
				"body":     {"type": "string", "description": "Message body"},
				"reply_to": {"type": "string", "description": "Optional public_id of the message this is a reply to"}
			},
			"required": ["to", "body"]
		}`),
		mcpSendHandler(s))

	srv.RegisterTool("semaphore.agents",
		"List registered agents with pane liveness.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"available_only": {"type": "boolean", "description": "Filter to live + not-paused agents"}
			}
		}`),
		mcpAgentsHandler(s))

	srv.RegisterTool("semaphore.whoami",
		"Return this session's registration. Identity from $CLAUDE_AGENT_NAME or $TMUX_PANE→registry.",
		json.RawMessage(`{"type": "object", "properties": {}}`),
		mcpWhoamiHandler(s))

	srv.RegisterTool("semaphore.inbox",
		"List the caller's own queued messages.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"state": {"type": "string", "enum": ["queued","delivering","delivered","failed"]},
				"limit": {"type": "integer", "minimum": 1, "maximum": 1000}
			}
		}`),
		mcpInboxHandler(s))

	srv.RegisterTool("semaphore.message_status",
		"Look up the delivery state of a sent message by its public_id. Returns created_at + delivered_at + error so the sender can see whether the bus has handed off the row yet.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "Public ID of the message to look up (returned in the send/control response)"}
			},
			"required": ["id"]
		}`),
		mcpMessageStatusHandler(s))

	srv.RegisterTool("semaphore.status",
		"Return registry overview: paused state + queue depths per agent.",
		json.RawMessage(`{"type": "object", "properties": {}}`),
		mcpStatusHandler(s))

	srv.RegisterTool("semaphore.register",
		"Register this (or another) pane on the bus. Pane defaults to $TMUX_PANE; start_mailman defaults true.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"name":          {"type": "string", "description": "Agent name (the new identity)"},
				"pane":          {"type": "string", "description": "Pane id like %5 (default: $TMUX_PANE)"},
				"start_mailman": {"type": "boolean", "description": "Run systemctl --user enable --now claude-mailman@NAME (default true)"},
				"force":         {"type": "boolean", "description": "Overwrite an existing row with the same name (default false)"},
				"alias":         {"type": "string", "description": "Optional alternative name discover should accept for this canonical agent (e.g. 'Master Bosun of Nimbus' for canonical 'bosun'). Append-only; existing aliases preserved."}
			},
			"required": ["name"]
		}`),
		mcpRegisterHandler(s))

	srv.RegisterTool("semaphore.control",
		"Send a whitelisted Claude Code slash-command directly to a pane. Scope-gated: when to==self, the self-whitelist applies; when to is a peer, the peer-whitelist applies — with a third tier of per-edge exceptions for destructive commands. Specifically, /clear is globally denied but Bosun→Pilot is permitted (rescue path when Pilot can't /compact out of token exhaustion). Bypasses the chat-message renderer. Optional resume_with (only with command=compact, only on self) queues a follow-up message that the mailman delivers AFTER /compact has settled — pre-write your continuation instead of going silent post-compact.",
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

	srv.RegisterTool("semaphore.unregister",
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

	return srv
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
			return nil, fmt.Errorf("cannot resolve sender identity: set $CLAUDE_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
		}
		p := sendParams{
			From:         from,
			To:           in.To,
			ReplyTo:      in.ReplyTo,
			Body:         in.Body,
			MaxRecipient: capRecipientQueue,
			MaxSender:    capSenderBacklog,
			MaxBody:      capBodyBytes,
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
	if _, err := s.GetAgent(ctx, p.To); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("unknown recipient: %s", p.To)
		}
		return nil, err
	}
	if _, err := s.GetAgent(ctx, p.From); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("unknown sender: %s", p.From)
		}
		return nil, err
	}
	// Cap enforcement lives inside InsertMessage's transaction since
	// #29 — no pre-check needed.
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent:         p.From,
		ToAgent:           p.To,
		ReplyTo:           p.ReplyTo,
		Body:              p.Body,
		MaxRecipientQueue: p.MaxRecipient,
		MaxSenderBacklog:  p.MaxSender,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("unknown reply-to id: %s", p.ReplyTo)
		}
		return nil, err
	}
	return map[string]any{
		"ok":     true,
		"id":     res.PublicID,
		"queued": res.Queued,
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
			return nil, fmt.Errorf("cannot resolve sender identity: set $CLAUDE_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
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
			return nil, fmt.Errorf("cannot resolve identity: set $CLAUDE_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
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
			return nil, fmt.Errorf("cannot resolve identity: set $CLAUDE_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
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

		// Default start_mailman to true.
		start := true
		if in.StartMailman != nil {
			start = *in.StartMailman
		}
		mailmanState := "skipped"
		if start {
			if err := startMailman(ctx, in.Name); err != nil {
				return map[string]any{
					"ok":             true,
					"name":           in.Name,
					"pane":           pane,
					"mailman":        "failed",
					"mailman_error":  err.Error(),
					"registered":     true,
				}, nil
			}
			mailmanState = "active"
		}
		return map[string]any{
			"ok":         true,
			"name":       in.Name,
			"pane":       pane,
			"mailman":    mailmanState,
			"registered": true,
		}, nil
	}
}

func mcpUnregisterHandler(s *store.Store) mcp.ToolHandler {
	type input struct {
		Name           string `json:"name"`
		StopMailman    *bool  `json:"stop_mailman"`
		PurgeMessages  bool   `json:"purge_messages"`
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
			"ok":              true,
			"name":            in.Name,
			"mailman":         mailmanState,
			"deleted":         deleted,
			"unregistered":    true,
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
