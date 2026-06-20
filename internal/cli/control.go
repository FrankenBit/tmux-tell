package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/control"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// controlParams is the resolved input to doControl. Mirrors the MCP
// tool's input plus the cap budget (so unit tests can tighten them).
type controlParams struct {
	From       string
	To         string
	Command    string
	ResumeWith string
	// ForceRateLimited is the #573 control-surface arm of the #558 operator
	// escape-hatch: when true, every row the macro emits carries the
	// force_rate_limited marker, so the recipient's mailman bypasses the
	// rate-limit / usage-limit defer gates for the whole control macro (but
	// NOT copy-mode / popup / unknown / compaction — those still defer per
	// IsPasteUnsafeForced). Applied to BOTH rows of the restart / sleep+resume
	// InsertMessagePair, not just the primary: a forced restart whose enable
	// row deferred would re-create the half-actioned state #29's atomic insert
	// prevents; a forced sleep whose resume row deferred would leave the
	// chamber slept-but-dormant. Default false = normal deferral.
	ForceRateLimited bool
	MaxRecipient     int
	MaxSender        int
	MaxBody          int
}

// controlResult is the structured return from doControl. Both the MCP
// handler and the CLI subcommand serialise it directly — the JSON tags
// (with omitempty on the per-path fields) are the single source of
// truth for the wire shape. Don't reconstruct this shape by hand in
// either caller, or the two outputs will drift the next time a field
// is added.
type controlResult struct {
	OK       bool   `json:"ok"`
	ID       string `json:"id"`
	EnableID string `json:"enable_id,omitempty"`
	ResumeID string `json:"resume_id,omitempty"`
	Macro    string `json:"macro,omitempty"`
	Command  string `json:"command"`
	Queued   int    `json:"queued"`
	// Deprecated names the canonical replacement when the caller invoked a
	// deprecated control-macro alias (#480, e.g. mcp-restart-tmux-msg →
	// mcp-restart-tmux-tell). Empty (omitted) on a canonical invocation. The
	// macro still runs correctly; this is the wire-visible nudge. Both callers
	// also emit a greppable WARN deprecated_control_macro to their log.
	Deprecated string `json:"deprecated,omitempty"`
}

// doControl is the shared validate+insert pipeline behind both the MCP
// tmux-tell.control tool and the new `tmux-tell-claude control` CLI. Returns
// a structured result the caller renders into its preferred shape.
//
// Three execution paths:
//
//  1. mcp-restart-tmux-tell macro → two control rows
//     (/mcp disable tmux-tell, /mcp enable tmux-tell).
//  2. sleep with resume_with → one control row + one message row,
//     reply_to-threaded; the mailman's post-compact pause lets the
//     follow-up land after the /compact slash-command settles. (The bus
//     verb is `sleep` (#509); the emitted CLI primitive stays /compact, so
//     the post-compact-pause machinery keys on it unchanged.)
//  3. plain control → one control row with the resolved text.
//
// The whitelist scope check is performed once at the entry; the inner
// inserts for path (1) bypass the per-row scope check on purpose
// because the macro has already been authorised at the trust boundary.
func doControl(ctx context.Context, s *store.Store, p controlParams) (*controlResult, error) {
	if p.From == "" {
		return nil, errors.New("cannot resolve sender identity")
	}
	if p.To == "" {
		return nil, errors.New("to required")
	}
	if p.Command == "" {
		return nil, errors.New("command required")
	}
	scope := control.ScopePeer
	if p.To == p.From {
		scope = control.ScopeSelf
	}
	text, err := control.Resolve(p.Command, scope, p.From, p.To)
	if err != nil {
		return nil, fmt.Errorf("%w; %s-invokable: %v",
			err, scope, control.NamesForScope(scope, p.From, p.To))
	}
	if _, err := s.GetAgent(ctx, p.To); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("unknown recipient: %s", p.To)
		}
		return nil, err
	}

	// canonName is the whitelist key (lowercased, slash-stripped, trimmed,
	// AND deprecated-alias-resolved per #480). We dispatch path 1 / path 2
	// against this — NOT against the resolved text — so the macro is keyed on
	// the canonical command name rather than its prose form. (If a future
	// whitelist edit added another entry whose Text happened to be `/mcp
	// restart tmux-tell`, dispatching on text would silently route it through
	// the macro; dispatching on name keeps the coupling visible.) Canonicalize
	// follows a legacy `mcp-restart-tmux-msg` alias to `…-tmux-tell` so the old
	// macro name still triggers the restart path.
	canonName, wasAlias := control.Canonicalize(p.Command)
	deprecated := ""
	if wasAlias {
		deprecated = fmt.Sprintf("control macro %q is a deprecated alias for %q (removed v1.0, ADR-0008 §Discretion)",
			strings.ToLower(strings.TrimSpace(p.Command)), canonName)
	}

	// Path 1: restart macro. After #29, both rows land in a single
	// BEGIN IMMEDIATE transaction via InsertMessagePair — atomicity
	// guarantee means we can never leave the recipient half-actioned
	// (disabled but never re-enabled). Cap budget for +2 slots is
	// enforced inside the same transaction.
	if canonName == "mcp-restart-tmux-tell" {
		disableP := store.InsertParams{
			FromAgent: p.From, ToAgent: p.To,
			Body: "/mcp disable tmux-tell", Kind: store.KindControl,
			ForceRateLimited:  p.ForceRateLimited,
			MaxRecipientQueue: p.MaxRecipient,
			MaxSenderBacklog:  p.MaxSender,
		}
		enableP := store.InsertParams{
			FromAgent: p.From, ToAgent: p.To,
			Body: "/mcp enable tmux-tell", Kind: store.KindControl,
			// Both rows forced: a deferred enable would leave the recipient
			// MCP-disabled-but-never-re-enabled — the half-actioned state #29's
			// atomic insert prevents, re-created at delivery time. See #573.
			ForceRateLimited: p.ForceRateLimited,
		}
		disableRes, enableRes, err := s.InsertMessagePair(ctx, disableP, enableP, true)
		if err != nil {
			return nil, err
		}
		return &controlResult{
			OK: true, ID: disableRes.PublicID, EnableID: enableRes.PublicID,
			Queued: enableRes.Queued, Command: text, Macro: "restart",
			Deprecated: deprecated,
		}, nil
	}

	// Path 2: sleep + resume_with. Same atomicity pattern via
	// InsertMessagePair so we can never /compact the recipient and
	// then fail to queue the resume prompt.
	if p.ResumeWith != "" {
		if text != "/compact" {
			return nil, errors.New("resume_with is only valid with command=sleep")
		}
		if scope != control.ScopeSelf {
			return nil, errors.New("resume_with requires self-invocation")
		}
		if p.MaxBody > 0 && len(p.ResumeWith) > p.MaxBody {
			return nil, fmt.Errorf("resume_with too large (%d > %d bytes)",
				len(p.ResumeWith), p.MaxBody)
		}
		compactP := store.InsertParams{
			FromAgent: p.From, ToAgent: p.To,
			Body: text, Kind: store.KindControl,
			ForceRateLimited:  p.ForceRateLimited,
			MaxRecipientQueue: p.MaxRecipient,
			MaxSenderBacklog:  p.MaxSender,
		}
		resumeP := store.InsertParams{
			FromAgent: p.From, ToAgent: p.To,
			Body: p.ResumeWith, Kind: store.KindMessage,
			// Both rows forced: a deferred resume would leave the chamber
			// slept-but-dormant, the failure resume_with exists to prevent.
			// See #573.
			ForceRateLimited: p.ForceRateLimited,
		}
		compactRes, resumeRes, err := s.InsertMessagePair(ctx, compactP, resumeP, true)
		if err != nil {
			return nil, err
		}
		return &controlResult{
			OK: true, ID: compactRes.PublicID, ResumeID: resumeRes.PublicID,
			Queued: resumeRes.Queued, Command: text, Macro: "resume",
			Deprecated: deprecated,
		}, nil
	}

	// Path 3: plain control. Cap enforcement lives inside InsertMessage's
	// transaction since #29; no separate pre-check.
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: p.From, ToAgent: p.To,
		Body: text, Kind: store.KindControl,
		ForceRateLimited:  p.ForceRateLimited,
		MaxRecipientQueue: p.MaxRecipient,
		MaxSenderBacklog:  p.MaxSender,
	})
	if err != nil {
		return nil, err
	}
	return &controlResult{
		OK: true, ID: res.PublicID, Queued: res.Queued, Command: text,
		Deprecated: deprecated,
	}, nil
}

// runControlCLI parses control-subcommand flags and dispatches to
// doControl, writing the result as JSON to stdout.
//
// Usage: tmux-tell-claude control --to AGENT --command NAME [--resume-with TEXT]
func runControlCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("control", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	from := fs.String("from", "", "sender agent name (default: identity-resolved)")
	to := fs.String("to", "", "recipient agent name (required)")
	command := fs.String("command", "",
		fmt.Sprintf("whitelisted command (#583 — receiver-side effects):\n%s",
			control.DescTable()))
	resumeWith := fs.String("resume-with", "",
		"optional continuation prompt; only valid with --command sleep on self")
	forceRateLimited := fs.Bool("force-rate-limited", false,
		"bypass the recipient's rate-limit / usage-limit defer for this control macro, delivering even when the pane shows a rate-/usage-limit banner (#573, control arm of #558). Applies to BOTH rows of the restart / sleep+resume macros. Does NOT bypass copy-mode / popup / unknown / compaction paste-safety.")
	maxRecipient := fs.Int("max-recipient-queue", capRecipientQueue,
		"reject when the recipient's queue depth would exceed this")
	maxSender := fs.Int("max-sender-backlog", capSenderBacklog,
		"reject when the sender's queued backlog would exceed this")
	maxBody := fs.Int("max-body-bytes", capBodyBytes,
		"reject resume_with bodies larger than this many bytes")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	// If --to wasn't specified and exactly one positional remains, treat
	// it as the recipient — operator's natural typing pattern
	// `control alice --command sleep` works without remembering that
	// `--to` is a required flag. This is additive: the existing
	// flag-only form keeps working unchanged.
	if *to == "" && fs.NArg() == 1 {
		*to = fs.Arg(0)
	}
	if *to == "" {
		return writeJSONError(stdout, stderr, "--to required", exitUsage)
	}
	if *command == "" {
		return writeJSONError(stdout, stderr, "--command required", exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	ctx := context.Background()
	fromName, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if fromName == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve sender: pass --from, set $TMUX_AGENT_NAME, or register this pane",
			exitUsage)
	}

	res, err := doControl(ctx, s, controlParams{
		From:             fromName,
		To:               *to,
		Command:          *command,
		ResumeWith:       *resumeWith,
		ForceRateLimited: *forceRateLimited,
		MaxRecipient:     *maxRecipient,
		MaxSender:        *maxSender,
		MaxBody:          *maxBody,
	})
	if err != nil {
		// Cap rejections route via sentinel (post-#29), not string
		// match. Other paths route by error class so callers can branch
		// on exit code or the JSON "error" field.
		msg := err.Error()
		switch {
		case errors.Is(err, store.ErrRecipientQueueFull),
			errors.Is(err, store.ErrSenderBacklogFull):
			return writeJSONError(stdout, stderr, msg, exitTempFail)
		case strings.Contains(msg, "unknown recipient"):
			return writeJSONError(stdout, stderr, msg, exitUnavailable)
		case errors.Is(err, control.ErrNotAllowed),
			errors.Is(err, control.ErrScopeDenied):
			return writeJSONError(stdout, stderr, msg, exitUsage)
		default:
			return writeJSONError(stdout, stderr, msg, exitDataErr)
		}
	}
	if res.Deprecated != "" {
		fmt.Fprintf(stderr, "WARN deprecated_control_macro %s\n", res.Deprecated)
	}
	_ = writeJSONResult(stdout, res)
	return exitOK
}
