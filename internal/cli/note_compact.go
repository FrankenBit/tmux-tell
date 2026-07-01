package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// Self-compact signalling (#285 PR2). This is the **adapter-side** hook-helper
// for the post-compaction event, sibling to hook-context (#249): the substrate
// (internal/store) stays delivery-method-agnostic; "a self-/compact just happened"
// enters the substrate here. The operator wires their adapter's post-compaction
// hook (Claude Code's `PostCompact`, matcher `auto|manual`, catches BOTH
// auto-compaction and manual /compact) to run `tmux-tell-claude note-compact`; on
// each fire this stamps last_self_compact_at for the calling agent. The mailman
// edge-detects that signal on its self-observation cadence and counts it toward the
// #285 respawn threshold (see serve.go / store.CountSelfCompactIfNew).
//
// Why a signal + mailman-side count, not a direct increment here: the counter's
// race-freedom (PR1) rests on the single-flight mailman being its SOLE writer. This
// helper runs as a SEPARATE process (spawned by the adapter's hook), so it must not
// touch respawn_shrink_count. It writes only the signal timestamp (a blind
// overwrite); the mailman does the increment. See SetSelfCompactSignal.

// noteCompactResult is the wire shape emitted by `note-compact`. Small + stable so
// a future MCP surface (or a debugging operator) can consume it; JSON tags are the
// source of truth.
type noteCompactResult struct {
	OK    bool   `json:"ok"`
	Agent string `json:"agent"`           // the chamber whose self-compact was recorded
	Event string `json:"event,omitempty"` // the hook event name, if the adapter passed one on stdin
}

// runNoteCompactCLI is the `tmux-tell-claude note-compact` subcommand — the
// entrypoint a post-compaction hook invokes. It resolves the calling agent's
// identity (like hook-context: --from / $TMUX_AGENT_NAME / this pane), stamps the
// self-compact signal, and writes a small JSON result. It reads (and ignores the
// body of) stdin so a hook that pipes its payload doesn't hit a broken pipe; if the
// payload carries a hook_event_name it is echoed back for observability only.
//
// Usage:
//
//	tmux-tell-claude note-compact                 # self-target from $TMUX_PANE
//	tmux-tell-claude note-compact --from pilot     # explicit target (must be registered)
func runNoteCompactCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("note-compact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	from := fs.String("from", "", "agent whose self-compact to record (env: TMUX_AGENT_NAME; default: this pane)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	// Drain stdin best-effort (the hook may pipe its JSON payload) and pull the
	// event name out for the echoed result — purely observational; the signal is
	// the same regardless of auto-vs-manual compaction.
	var eventName string
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
	defer s.Close() //nolint:errcheck // best-effort close

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
	// A misconfigured hook (wrong --from, or a pane whose registration lapsed)
	// must fail loud rather than silently dropping the signal — an unrecorded
	// self-compact silently defeats the respawn the operator opted into. This
	// mirrors hook-context's explicit-target registration check (#361), extended
	// to the pane-resolved case too because SetSelfCompactSignal is the sole
	// substrate touch (there is no later ClaimNext to surface the typo).
	if _, gerr := s.GetAgent(ctx, agent); gerr != nil {
		hint := "omit --from to resolve from $TMUX_PANE"
		if src != identity.SourceExplicit {
			hint = fmt.Sprintf("run `%s register --name %s` to register this pane", active.BinaryName, agent)
		}
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("agent %q is not registered — %s", agent, hint), exitUsage)
	}

	if err := s.SetSelfCompactSignal(ctx, agent); err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	_ = writeJSONResult(stdout, noteCompactResult{OK: true, Agent: agent, Event: eventName})
	return exitOK
}
