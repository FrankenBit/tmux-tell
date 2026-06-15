package cli

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
func doHookContext(ctx context.Context, s *store.Store, agent, eventName string, stderr io.Writer) (out hookOutput, presented int, err error) {
	// #443 Obs1: the hook-context path is the canonical delivery for a
	// hook-context agent ONLY. If this agent's delivery_mode was flipped to
	// paste-and-enter (or mailbox-only) but a stale codex hook block still fires
	// this command, deliver nothing — the mailman's paste is the single delivery.
	// Reading delivery_mode here makes the DB the single source of truth and
	// demotes the toml hook block to a trigger that defers to it; without this
	// guard both paths claim the same message in the race window before the first
	// marks it delivered, double-arriving at the chamber surface (the bus DB shows
	// one clean delivered_at, so the duplicate is visible only at the chamber).
	// The structural alternative (rewrite the toml on every flip) is a follow-up:
	// after this guard a stale toml block is harmless, so it is cosmetic hygiene.
	a, gerr := s.GetAgent(ctx, agent)
	if gerr != nil {
		return hookOutput{}, 0, fmt.Errorf("get agent %q: %w", agent, gerr)
	}
	if a.DeliveryMode != store.DeliveryModeHookContext {
		// User-silent (the operator deliberately flipped delivery_mode and doesn't
		// want hook noise in their session) but substrate-observable: a greppable
		// WARN lets them discover a stale toml block from the journal. Same shape as
		// serve.go's WARN control_command_unsupported (#419) — silent to the chamber,
		// observable to the substrate (#443 Obs1, greenlit).
		fmt.Fprintf(stderr, "WARN hook_context_skipped_paste_mode agent=%s delivery_mode=%s — "+
			"stale codex hook block fired for a non-hook-context agent; the mailman paste is the "+
			"single delivery (toml may need cleanup) (#443 Obs1 / #438)\n", agent, a.DeliveryMode)
		return hookOutput{}, 0, nil
	}
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
	// --event-name pins the hookEventName echoed in the output, overriding the
	// stdin-derived value. The output's hookEventName must match the firing
	// event (some CLIs — e.g. Codex — reject a mismatch), but not every CLI
	// documents whether (or under what key) it sends the event name on stdin.
	// Pinning it in the hook command itself makes the output deterministic
	// regardless of the CLI's stdin schema:
	//   command = "tmux-msg-codex hook-context --from bob --event-name UserPromptSubmit"
	eventNameOverride := fs.String("event-name", "",
		"override the echoed hookEventName (default: read from stdin hook_event_name, else "+defaultHookEventName+")")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	// Resolve the event name to echo back: explicit --event-name wins; else the
	// stdin payload's hook_event_name; else the default.
	eventName := defaultHookEventName
	if stdin != nil {
		if raw, rerr := io.ReadAll(io.LimitReader(stdin, 1<<20)); rerr == nil && len(raw) > 0 {
			var in hookInput
			if json.Unmarshal(raw, &in) == nil && in.HookEventName != "" {
				eventName = in.HookEventName
			}
		}
	}
	if *eventNameOverride != "" {
		eventName = *eventNameOverride
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agent, src, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if agent == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve identity: pass --from, set $TMUX_AGENT_NAME, or register this pane", exitUsage)
	}
	// When --from was supplied explicitly, verify the name is registered (#361).
	// identity.Resolve returns the override verbatim without a registry check, so
	// a typo silently no-ops (ClaimNext finds nothing) rather than surfacing the
	// misconfiguration. Hooks run in the operator's shell, where $TMUX_PANE IS
	// set — omitting --from and letting the pane-lookup resolve dynamically is
	// the preferred wiring; --from is retained for edge cases but must name a
	// registered agent.
	if src == identity.SourceExplicit {
		if _, gerr := s.GetAgent(ctx, agent); gerr != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("agent %q is not registered — check --from value, or omit --from to resolve from $TMUX_PANE (run `%s register --name %s` to register)",
					agent, active.BinaryName, agent),
				exitUsage)
		}
	}

	out, _, err := doHookContext(ctx, s, agent, eventName, stderr)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	// Always emit valid JSON — empty {} when nothing was pending, so the hook
	// is a clean no-op.
	_ = writeJSONResult(stdout, out)
	return exitOK
}
