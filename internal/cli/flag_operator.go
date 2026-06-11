package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// operatorAttentionRecipient is the reserved agent-name a `flag_operator`
// call posts its body to (#224). The operator registers this as a
// `mailbox-only` agent at setup time; chambers post to it whenever they
// surface an `awaiting_operator` signal, and the operator polls / tails
// its inbox to see who needs them.
//
// Reserved-name convention: a chamber may not register itself under
// this name — the substrate enforces it at flag time by requiring the
// recipient agent to exist BEFORE the flag is accepted (mirrors the
// regular send-to-unregistered-recipient fail-loud principle from #152).
const operatorAttentionRecipient = "operator-attention"

// runFlagOperatorCLI is the CLI surface for `tmux-msg-claude flag-operator
// "<body>"` (#224). Posts the body to the reserved operator-attention
// recipient AND marks the calling agent's attention_state as
// "awaiting_operator". The flag clears implicitly on the chamber's next
// register, or explicitly via `clear-operator-flag`.
//
// Body is required: it is the question / choice the chamber wants the
// operator to weigh in on. Empty bodies are rejected before any
// substrate mutation.
func runFlagOperatorCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flag-operator", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "sender name (default: $TMUX_AGENT_NAME or $TMUX_PANE→registry)")
	format := fs.String("format", "text", "output format: text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintf(stderr, "usage: %s flag-operator [--from NAME] \"<body>\"\n", active.BinaryName)
		return exitUsage
	}
	body := strings.Join(rest, " ")
	if body == "" {
		fmt.Fprintln(stderr, "flag-operator: body required (the question or choice for the operator)")
		return exitUsage
	}

	ctx := context.Background()
	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	sender, err := resolveSender(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	out, code := doFlagOperator(ctx, s, sender, body)
	return emitFlagOperatorResult(stdout, stderr, out, code, *format)
}

// runClearOperatorFlagCLI is the CLI surface for `tmux-msg-claude
// clear-operator-flag` (#224). Sets the calling agent's attention_state
// back to "idle". Used when a chamber's question gets answered out of band
// (operator wrote in the pane directly) and the chamber wants to clear the
// flag without going through register.
func runClearOperatorFlagCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("clear-operator-flag", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "sender name (default: $TMUX_AGENT_NAME or $TMUX_PANE→registry)")
	format := fs.String("format", "text", "output format: text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	ctx := context.Background()
	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	sender, err := resolveSender(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	if err := s.SetAttentionState(ctx, sender, store.AttentionStateIdle); err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("clear attention_state: %v", err), exitInternal)
	}

	result := map[string]any{
		"ok":              true,
		"name":            sender,
		"attention_state": store.AttentionStateIdle,
	}
	return emitFlagOperatorResult(stdout, stderr, result, exitOK, *format)
}

// doFlagOperator is the shared core for the CLI + MCP surfaces. Sends the
// body to the operator-attention recipient AND flips the sender's
// attention_state to awaiting_operator. Order matters: the send is
// attempted first, so a failed send (unregistered recipient, oversize body)
// does NOT leak a stale attention_state.
func doFlagOperator(ctx context.Context, s *store.Store, sender, body string) (map[string]any, int) {
	// Verify operator-attention recipient is registered BEFORE the flag
	// flip. The substrate uses the regular send-to-unregistered fail-loud
	// principle here (#152): a typo'd recipient should not silently swallow
	// the attention request.
	if _, err := s.GetAgent(ctx, operatorAttentionRecipient); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return map[string]any{
				"ok":    false,
				"error": fmt.Sprintf("recipient %q not registered — operator needs to set it up via `%s register --name %s --delivery-mode mailbox-only` before chambers can flag for attention", operatorAttentionRecipient, active.BinaryName, operatorAttentionRecipient),
			}, exitDataErr
		}
		return map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("get %q: %v", operatorAttentionRecipient, err),
		}, exitInternal
	}

	// Body cap (operational bound, matches send semantics)
	if capBodyBytes > 0 && len(body) > capBodyBytes {
		return map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("body too large (%d > %d bytes)", len(body), capBodyBytes),
		}, exitDataErr
	}

	row, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: sender,
		ToAgent:   operatorAttentionRecipient,
		Body:      body,
	})
	if err != nil {
		return map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("insert: %v", err),
		}, exitInternal
	}

	// Send succeeded; flip the attention_state. A failure here would leave
	// the message in the inbox but no attention_state set — surface the
	// state-error explicitly so the chamber knows to investigate (rather
	// than a silent partial success).
	if err := s.SetAttentionState(ctx, sender, store.AttentionStateAwaitingOperator); err != nil {
		return map[string]any{
			"ok":          true,
			"name":        sender,
			"id":          row.PublicID,
			"queued":      1,
			"state_error": fmt.Sprintf("set attention_state: %v", err),
		}, exitOK
	}

	return map[string]any{
		"ok":              true,
		"name":            sender,
		"id":              row.PublicID,
		"queued":          1,
		"attention_state": store.AttentionStateAwaitingOperator,
	}, exitOK
}

// emitFlagOperatorResult writes the result map in the chosen format and
// returns the exit code. Mirrors the writeJSONResult / text-table pattern
// the other subcommands use.
func emitFlagOperatorResult(stdout, stderr io.Writer, result map[string]any, code int, format string) int {
	switch format {
	case "json":
		_ = writeJSONResult(stdout, result)
	case "text", "":
		if ok, _ := result["ok"].(bool); ok {
			fmt.Fprintf(stdout, "ok: name=%v id=%v attention=%v\n",
				result["name"], result["id"], result["attention_state"])
		} else {
			fmt.Fprintf(stderr, "error: %v\n", result["error"])
		}
	default:
		fmt.Fprintf(stderr, "unknown --format: %s\n", format)
		return exitUsage
	}
	return code
}

// resolveSender determines the calling agent for flag-operator /
// clear-operator-flag. CLI may pass --from; otherwise the identity
// resolves through the standard $TMUX_AGENT_NAME / $TMUX_PANE→registry
// chain (mirrors the resolveMCPIdentity pattern).
func resolveSender(ctx context.Context, s *store.Store, fromFlag string) (string, error) {
	if fromFlag != "" {
		return fromFlag, nil
	}
	name, err := resolveMCPIdentity(ctx, s)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("cannot resolve sender identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
	}
	return name, nil
}

// mcpFlagOperatorHandler is the MCP-side surface for `tmux-msg.flag_operator`
// (#224). Mirrors doFlagOperator semantics.
func mcpFlagOperatorHandler(s *store.Store) func(ctx context.Context, args json.RawMessage) (any, error) {
	type input struct {
		Body string `json:"body"`
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
		if in.Body == "" {
			return nil, fmt.Errorf("body required (the question or choice for the operator)")
		}
		from, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		if from == "" {
			return nil, fmt.Errorf("cannot resolve sender identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
		}
		result, _ := doFlagOperator(ctx, s, from, in.Body)
		return result, nil
	}
}

// mcpClearOperatorFlagHandler is the MCP-side surface for
// `tmux-msg.clear_operator_flag` (#224).
func mcpClearOperatorFlagHandler(s *store.Store) func(ctx context.Context, args json.RawMessage) (any, error) {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		from, err := resolveMCPIdentity(ctx, s)
		if err != nil {
			return nil, err
		}
		if from == "" {
			return nil, fmt.Errorf("cannot resolve sender identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table", os.Getenv("TMUX_PANE"))
		}
		if err := s.SetAttentionState(ctx, from, store.AttentionStateIdle); err != nil {
			return nil, fmt.Errorf("clear attention_state: %w", err)
		}
		return map[string]any{
			"ok":              true,
			"name":            from,
			"attention_state": store.AttentionStateIdle,
		}, nil
	}
}
