package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/render"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// Hook-context delivery (#249, ADR-0009). This is the **adapter-side**
// hook-helper: the substrate (internal/) stays delivery-method-agnostic; "deliver
// via Claude Code hooks" lives entirely here. The operator wires a SessionStart /
// UserPromptSubmit hook in ~/.claude/settings.json to run `tmux-msg-claude
// hook-context`; on each fire this claims the agent's pending messages, renders
// them, marks them delivered (ADR-0009 Q3/3b: delivered = presented), and emits
// them as `hookSpecificOutput.additionalContext` for Claude to inject.

// hookOutput is the Claude Code hook response. An empty value (no
// hookSpecificOutput) is a valid no-op when nothing is pending.
type hookOutput struct {
	HookSpecificOutput *hookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// hookInput is the subset of Claude Code's hook stdin payload we read — just
// the event name, echoed back in the response. Everything else is ignored.
type hookInput struct {
	HookEventName string `json:"hook_event_name"`
}

const defaultHookEventName = "UserPromptSubmit"

// doHookContext claims every pending message for the agent, renders them into
// an additionalContext block, marks them delivered (= presented, ADR-0009 3b),
// and returns the hook output. Returns an empty hookOutput when nothing is
// pending (a no-op hook fire). presented is how many messages were delivered.
//
// It runs RecoverDelivering first: a hook-context agent has NO mailman running
// (serve short-circuits), so nothing else resets a row left in `delivering` by
// a crashed prior hook invocation — this helper owns that recovery itself.
func doHookContext(ctx context.Context, s *store.Store, agent, eventName string) (out hookOutput, presented int, err error) {
	if _, rerr := s.RecoverDelivering(ctx, agent); rerr != nil {
		return hookOutput{}, 0, fmt.Errorf("recover: %w", rerr)
	}
	// Claim every currently-deliverable message (ClaimNext honors the #204
	// backlog floor + #227 deferred exclusion, so the hook respects the same
	// don't-flood / staging rules as the pane path).
	var claimed []store.Message
	for {
		m, cerr := s.ClaimNext(ctx, agent)
		if cerr != nil {
			return hookOutput{}, 0, fmt.Errorf("claim: %w", cerr)
		}
		if m == nil {
			break
		}
		claimed = append(claimed, *m)
	}
	if len(claimed) == 0 {
		return hookOutput{}, 0, nil
	}
	text := renderHookContext(claimed)
	// Mark presented = delivered (verified): additionalContext definitely
	// reaches the recipient's context, so there's no verify-token retry — a
	// hook-presented message is confirmed by construction (ADR-0009 Q3).
	//
	// Substrate-honesty caveat (#249 N2): "by construction" holds at the
	// substrate→hook-helper boundary (we mark before the JSON is written to
	// stdout). It trusts the hook-helper→Claude boundary: if Claude reads the
	// JSON but crashes before injecting additionalContext, the row stays
	// delivered+verified=1 yet was never seen — and RecoverDelivering can't help
	// (the row is `delivered`, not `delivering`). v1 trusts Claude Code's hook
	// contract here; if that trust ever proves shaky, an ack-on-next-turn
	// confirmation (the row stays unverified until the recipient's NEXT hook
	// fire observes the prior context landed) would close the gap.
	for _, m := range claimed {
		if merr := s.MarkDelivered(ctx, m.PublicID); merr != nil {
			// Best-effort: the message is about to be presented regardless; a
			// mark failure leaves it `delivering` and the next hook's
			// RecoverDelivering re-surfaces it (a bounded duplicate, not a loss).
			return out, presented, fmt.Errorf("mark delivered %s: %w", m.PublicID, merr)
		}
		presented++
	}
	return hookOutput{HookSpecificOutput: &hookSpecificOutput{
		HookEventName:     eventName,
		AdditionalContext: text,
	}}, presented, nil
}

// renderHookContext formats claimed messages as an additionalContext block.
// Plain text (not pane chrome): a short header + one labeled line per message.
// The recipient's Claude reads this as context on its next turn.
func renderHookContext(msgs []store.Message) string {
	var b strings.Builder
	if len(msgs) == 1 {
		b.WriteString("📨 1 message from the tmux-msg bus:\n")
	} else {
		fmt.Fprintf(&b, "📨 %d messages from the tmux-msg bus:\n", len(msgs))
	}
	for _, m := range msgs {
		b.WriteString("\n")
		// Title-case the sender to match the pane header's chrome convention
		// (stored names are lowercase; #249 N3 — consistency across modes).
		if m.ReplyTo.Valid {
			fmt.Fprintf(&b, "[%s → you · re %s · id %s]\n", render.TitleCase(m.FromAgent), m.ReplyTo.String, m.PublicID)
		} else {
			fmt.Fprintf(&b, "[%s · id %s]\n", render.TitleCase(m.FromAgent), m.PublicID)
		}
		b.WriteString(m.Body)
		b.WriteString("\n")
	}
	return b.String()
}

// runHookContextCLI is the `tmux-msg-claude hook-context` subcommand — the
// entrypoint a Claude Code SessionStart / UserPromptSubmit hook invokes. It
// reads the hook payload from stdin (for the event name), resolves the calling
// agent's identity, presents pending messages, and writes the hook JSON to
// stdout. It is a no-op (empty JSON) when the agent has no pending messages, so
// it is safe to wire unconditionally.
func runHookContextCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hook-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "agent whose pending messages to present (env: TMUX_AGENT_NAME; default: this pane)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	// Read the hook payload (best-effort) for the event name to echo back.
	eventName := defaultHookEventName
	if stdin != nil {
		if raw, rerr := io.ReadAll(io.LimitReader(stdin, 1<<20)); rerr == nil && len(raw) > 0 {
			var in hookInput
			if json.Unmarshal(raw, &in) == nil && in.HookEventName != "" {
				eventName = in.HookEventName
			}
		}
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agent, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if agent == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve identity: pass --from, set $TMUX_AGENT_NAME, or register this pane", exitUsage)
	}

	out, _, err := doHookContext(ctx, s, agent, eventName)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	// Always emit valid JSON — empty {} when nothing was pending, so the hook
	// is a clean no-op.
	_ = writeJSONResult(stdout, out)
	return exitOK
}
