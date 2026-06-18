package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// ackResult is the JSON response shape for --ack and --ack-all.
type ackResult struct {
	OK    bool   `json:"ok"`
	Acked int    `json:"acked"`
	ID    string `json:"id,omitempty"` // set only by --ack <id>
}

// runInboxCLI parses inbox-subcommand flags and dispatches.
//
// Usage: tmux-tell-claude inbox [AGENT] [--state STATE] [--limit N] [--format text|json]
//
//	tmux-tell-claude inbox [AGENT] --ack <id>
//	tmux-tell-claude inbox [AGENT] --ack-all
//
// AGENT defaults to the calling pane's identity (via the same
// resolution rules as tmux-tell.whoami).
func runInboxCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	stateFlag := fs.String("state", "queued",
		"queued|delivering|delivered|failed|acknowledged (empty = all)")
	limit := fs.Int("limit", 50, "maximum rows to return")
	format := fs.String("format", "text", "text|json")
	ackID := fs.String("ack", "", "mark a single queued message as acknowledged (#221)")
	ackAll := fs.Bool("ack-all", false, "mark all queued messages ≤ backlog_epoch as acknowledged (#221)")
	watch := fs.Bool("watch", false, "interactive TUI: live-updating inbox with cursor-nav + inline ack/expand (#149)")
	watchInterval := fs.Duration("watch-interval", inboxWatchDefaultInterval, "poll cadence when --watch is set")
	unanswered := fs.Bool("unanswered", false,
		"list only messages where the sender flagged expects_reply AND you haven't replied yet (#270). Complements `sent --awaiting-reply` on the sender side.")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(stderr, "usage: %s inbox [AGENT] [flags]\n", active.BinaryName)
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	ctx := context.Background()
	var agent string
	if fs.NArg() == 1 {
		agent = fs.Arg(0)
	} else {
		agent, _, err = identity.Resolve(ctx, s, "")
		if err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		if agent == "" {
			return writeJSONError(stdout, stderr,
				"cannot resolve identity: pass AGENT, set $TMUX_AGENT_NAME, or register this pane",
				exitUsage)
		}
	}

	if *watch {
		if *ackID != "" || *ackAll {
			return writeJSONError(stdout, stderr,
				"--watch cannot be combined with --ack/--ack-all", exitUsage)
		}
		if *unanswered {
			return writeJSONError(stdout, stderr,
				"--watch cannot be combined with --unanswered", exitUsage)
		}
		if *format == "json" {
			return writeJSONError(stdout, stderr,
				"--watch is an interactive TUI; --format json is not supported", exitUsage)
		}
		return runInboxWatch(ctx, s, agent, *watchInterval, stdout, stderr)
	}

	if *ackID != "" {
		return runInboxAck(ctx, s, agent, *ackID, stdout, stderr)
	}
	if *ackAll {
		return runInboxAckAll(ctx, s, agent, stdout, stderr)
	}
	return runInboxWithStore(ctx, s,
		agent, store.State(*stateFlag), *limit, *unanswered, *format, stdout, stderr)
}

// runInboxAck marks a single queued message as acknowledged.
func runInboxAck(ctx context.Context, s *store.Store, agent, id string, stdout, stderr io.Writer) int {
	if err := s.MarkAcknowledged(ctx, agent, id); err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUnavailable)
	}
	_ = writeJSONResult(stdout, ackResult{OK: true, Acked: 1, ID: id})
	return exitOK
}

// runInboxAckAll marks all queued messages ≤ the agent's backlog_epoch_id as acknowledged.
func runInboxAckAll(ctx context.Context, s *store.Store, agent string, stdout, stderr io.Writer) int {
	a, err := s.GetAgent(ctx, agent)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("agent %q not registered: %v", agent, err), exitUnavailable)
	}
	var epoch int64
	if a.BacklogEpoch.Valid {
		epoch = a.BacklogEpoch.Int64
	}
	n, err := s.MarkAcknowledgedBatch(ctx, agent, epoch)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	_ = writeJSONResult(stdout, ackResult{OK: true, Acked: int(n)})
	return exitOK
}

func runInboxWithStore(ctx context.Context, s *store.Store,
	agent string, state store.State, limit int, unanswered bool, format string,
	stdout, stderr io.Writer,
) int {
	f := store.ListFilter{
		ToAgent: agent,
		State:   state,
		Limit:   limit,
	}
	if unanswered {
		f.Unanswered = true
	}
	msgs, err := s.ListMessages(ctx, f)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	// #390: a row is "backlog-fenced" — queued but below the agent's backlog
	// floor and not a promoted-deferred row — when the mailman's ClaimNext will
	// silently skip it (`state='queued' AND deliver_after IS NULL AND id <=
	// backlog_epoch_id`). This is exactly the pre-delivery_mode-flip orphan case.
	// Best-effort GetAgent: an unregistered listing target (messages addressed to
	// a not-yet-registered name) leaves epoch=0 → nothing fenced, no false marks.
	var epoch int64
	if a, gerr := s.GetAgent(ctx, agent); gerr == nil && a.BacklogEpoch.Valid {
		epoch = a.BacklogEpoch.Int64
	}
	fenced := func(m store.Message) bool {
		return m.State == store.StateQueued && !m.DeliverAfter.Valid && epoch > 0 && m.ID <= epoch
	}

	// #507: provider-cap deferral, live-derived like backlog_fenced — no per-row
	// stored flag. A queued, claimable (not backlog-fenced) message isn't moving
	// because the recipient's #448 provider cap is currently saturated when the
	// recipient declares a provider + cap (persisted at its mailman's serve start)
	// AND the same-provider working-count is at/over that cap right now. This
	// recomputes the gate predicate from a separate process; it uses the default
	// cap-TTL, so a mailman started with a custom --provider-cap-ttl can make the
	// freshness boundary here slightly off — a display nuance, not a gate change.
	capDeferred := func(store.Message) bool { return false }
	if provider, cap, perr := s.ProviderCapConfig(ctx, agent); perr == nil && provider != "" && cap > 0 {
		if working, cerr := s.CountWorkingOnProvider(ctx, provider, defaultProviderCapTTL, time.Now()); cerr == nil && working >= cap {
			capDeferred = func(m store.Message) bool {
				return m.State == store.StateQueued && !fenced(m)
			}
		}
	}

	// #526: copy-mode deferral, live-derived from the recipient's CURRENT tmux
	// pane state. Unlike backlog_fenced / provider_cap_deferred (both DB-
	// derived), this is a read-only tmux query (`pane_in_mode`): when the
	// recipient has scrolled their pane up into copy-mode, the mailman holds
	// delivery, so its queued, claimable backlog is surfaced as pane-in-copy-
	// mode — the operator's answer to "why isn't my queue draining". Any
	// GetAgent / pane-query error (no pane registered, tmux unreachable from
	// this process) degrades gracefully to false — a display nuance, never a
	// gate change. Ordered after capDeferred so a single message reports one
	// dominant reason.
	copyModeDeferred := func(store.Message) bool { return false }
	if a, gerr := s.GetAgent(ctx, agent); gerr == nil && a.PaneID != "" {
		if inMode, merr := tmuxio.PaneInCopyMode(ctx, a.PaneID); merr == nil && inMode {
			copyModeDeferred = func(m store.Message) bool {
				return m.State == store.StateQueued && !fenced(m) && !capDeferred(m)
			}
		}
	}

	switch format {
	case "json":
		out := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			mm := messageToMap(m)
			// Stable inbox surface (#390): programmatic consumers rely on
			// backlog_fenced to detect the won't-auto-deliver state. Always
			// emitted (true/false) on the inbox listing, not omitted.
			mm["backlog_fenced"] = fenced(m)
			// #507: provider_cap_deferred — queued-but-held-by-the-provider-cap.
			// Always emitted (true/false) so consumers can distinguish a
			// cap-held message from a merely-waiting-its-turn one.
			mm["provider_cap_deferred"] = capDeferred(m)
			// #526: pane_in_copy_mode — queued-but-held because the recipient
			// scrolled their pane up into copy-mode. Always emitted (true/false).
			mm["pane_in_copy_mode"] = copyModeDeferred(m)
			out = append(out, mm)
		}
		_ = writeJSONResult(stdout, out)
		return exitOK

	case "text", "":
		header := []string{"ID", "FROM", "TO", "STATE", "PRIO", "AGE", "BODY"}
		rows := make([][]string, 0, len(msgs))
		for _, m := range msgs {
			state := string(m.State)
			if fenced(m) {
				state = "queued (backlog-fenced)"
			} else if capDeferred(m) {
				state = "queued (provider-cap)"
			} else if copyModeDeferred(m) {
				state = "queued (pane-in-copy-mode)"
			}
			rows = append(rows, []string{
				m.PublicID,
				m.FromAgent,
				m.ToAgent,
				state,
				store.PriorityName(m.Priority), // #449
				ageOf(m.CreatedAt),
				shortBody(m.Body, 60),
			})
		}
		renderTextTable(stdout, header, rows)
		return exitOK

	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

// messageToMap shapes a Message for JSON output. Mirrors the wire format
// the MCP tools will use (#16), so they share one definition.
func messageToMap(m store.Message) map[string]any {
	out := map[string]any{
		"id":            m.PublicID,
		"from":          m.FromAgent,
		"to":            m.ToAgent,
		"body":          m.Body,
		"state":         displayState(m),
		"created_at":    m.CreatedAt,
		"quick":         m.Quick,
		"expects_reply": m.ExpectsReply,
		"priority":      store.PriorityName(m.Priority), // #449
	}
	if m.ReplyTo.Valid {
		out["reply_to"] = m.ReplyTo.String
	}
	if m.DeliveredAt.Valid {
		out["delivered_at"] = m.DeliveredAt.String
	}
	if m.Error.Valid {
		out["error"] = m.Error.String
	}
	return out
}

// ageOf returns "32s", "4m12s", "1h", "3d" given an ISO 8601 UTC timestamp.
// Returns "-" if the input doesn't parse.
func ageOf(iso string) string {
	t, err := time.Parse("2006-01-02T15:04:05.000Z", iso)
	if err != nil {
		// fallback for older sqlite formats without subsecond
		t, err = time.Parse("2006-01-02T15:04:05Z", iso)
		if err != nil {
			return "-"
		}
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) - 60*m
		return fmt.Sprintf("%dm%ds", m, s)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
