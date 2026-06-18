package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// runSentCLI parses sent-subcommand flags and dispatches.
//
// Usage: tmux-tell-claude sent [--since DUR] [--state STATE] [--to AGENT]
//
//	[--limit N] [--format text|json]
//
// STATE may be any of the standard store states (queued, delivering,
// delivered, failed) or the synthetic "delivered_in_input_box" which maps to
// state=delivered AND verified=0.
func runSentCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	since := fs.String("since", "24h",
		"time window: 24h (default) | 1h | today | all | any duration")
	stateFlag := fs.String("state", "",
		"queued|delivering|delivered|failed|delivered_in_input_box (empty = all); delivered_unverified accepted as deprecated alias")
	to := fs.String("to", "", "only messages sent to this agent")
	limit := fs.Int("limit", 50, "maximum rows to return")
	deferred := fs.Bool("deferred", false,
		"list your deferred (staged-but-not-yet-delivered) messages instead — the pre-staged context awaiting a flush_deferred trigger (#227). Overrides --state.")
	awaitingReply := fs.Bool("awaiting-reply", false,
		"list only messages you sent with --expects-reply where the recipient hasn't replied yet (#270). Complements `inbox --unanswered` on the recipient side.")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage: %s sent [flags]\n", active.BinaryName)
		return exitUsage
	}

	// Normalize deprecated --state alias before validation.
	if *stateFlag == "delivered_unverified" {
		fmt.Fprintf(stderr,
			"WARN deprecated_surface_used name=--state delivered_unverified removal=v1.0 — use --state delivered_in_input_box instead (ADR-0008)\n")
		*stateFlag = "delivered_in_input_box"
	}

	// Validate --state before opening the store.
	if err := validateSentState(*stateFlag); err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}

	w, err := parseWindow(*since, time.Now())
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	sinceFloor := ""
	if !w.All {
		sinceFloor = w.Since.UTC().Format("2006-01-02T15:04:05.000Z")
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	ctx := context.Background()
	agent, _, err := identity.Resolve(ctx, s, "")
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if agent == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve identity: set $TMUX_AGENT_NAME or register this pane",
			exitUsage)
	}

	return runSentWithStore(ctx, s, agent, *stateFlag, *to, *limit, *since, sinceFloor, *deferred, *awaitingReply, *format, stdout, stderr)
}

// validateSentState returns an error if state is not a known value for --state.
// The empty string (all states) is always valid.
func validateSentState(state string) error {
	switch state {
	case "", "queued", "delivering", "delivered", "failed", "delivered_in_input_box", "acknowledged":
		return nil
	default:
		return fmt.Errorf("unknown --state %q (want queued|delivering|delivered|failed|delivered_in_input_box|acknowledged)", state)
	}
}

func runSentWithStore(ctx context.Context, s *store.Store,
	agent, stateFilter, toAgent string,
	limit int, sinceSpec, sinceFloor string,
	deferred bool,
	awaitingReply bool,
	format string,
	stdout, stderr io.Writer,
) int {
	f := store.ListFilter{
		FromAgent:      agent,
		ToAgent:        toAgent,
		SinceCreatedAt: sinceFloor,
		Limit:          limit,
		OrderDesc:      true,
	}

	switch {
	case deferred:
		// --deferred opts into the otherwise-hidden staged rows (#227) and
		// overrides any --state filter (the two are contradictory).
		f.Deferred = true
	case awaitingReply:
		// --awaiting-reply: expects_reply=1 AND recipient hasn't replied yet
		// (#270). Overrides --state (the filter makes sense across all states).
		f.AwaitingReply = true
	case stateFilter == "delivered_in_input_box":
		f.Unverified = true
	case stateFilter == "":
		// no state filter (deferred rows stay hidden by ListMessages default)
	default:
		f.State = store.State(stateFilter)
	}

	msgs, err := s.ListMessages(ctx, f)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	switch format {
	case "json":
		out := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			row := messageToMap(m)
			row["display_state"] = displayState(m)
			out = append(out, row)
		}
		_ = writeJSONResult(stdout, out)
		return exitOK

	case "text", "":
		windowDesc := sinceSpec
		if sinceSpec == "" {
			windowDesc = "24h"
		}
		fmt.Fprintf(stdout, "Recent sent (last %s, %d total)\n\n", windowDesc, len(msgs))
		header := []string{"ID", "TO", "STATE", "CREATED", "DELIVERED", "BODY"}
		rows := make([][]string, 0, len(msgs))
		for _, m := range msgs {
			del := "—"
			if m.DeliveredAt.Valid {
				del = wallTime(m.DeliveredAt.String)
			}
			rows = append(rows, []string{
				m.PublicID,
				m.ToAgent,
				displayState(m),
				wallTime(m.CreatedAt),
				del,
				shortBody(m.Body, 60),
			})
		}
		renderTextTable(stdout, header, rows)

		// Summary footer for actionable states.
		var nUnverified, nFailed int
		for _, m := range msgs {
			switch displayState(m) {
			case "delivered_in_input_box":
				nUnverified++
			case "failed":
				nFailed++
			}
		}
		if nUnverified > 0 || nFailed > 0 {
			fmt.Fprintln(stdout)
		}
		if nUnverified > 0 {
			fmt.Fprintf(stdout,
				"%d message(s) in delivered_in_input_box — run `%s resend <id>` to recover.\n",
				nUnverified, active.BinaryName)
		}
		if nFailed > 0 {
			fmt.Fprintf(stdout,
				"%d message(s) failed — run `%s resend <id>` to retry.\n",
				nFailed, active.BinaryName)
		}
		return exitOK

	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

// displayStateDeliveredInInputBox is the synthetic display-state label for a
// delivered message whose verify-token never surfaced (verified=0, #169). It is
// not a stored State value — the row's State stays `delivered`; this label only
// distinguishes the soft-fail at the presentation layer. Single-sourced here so
// every consumer surface (sent / inbox / track / get / thread / sendstatus)
// renders the same string (#230).
const displayStateDeliveredInInputBox = "delivered_in_input_box"

// displayState synthesises the human-facing state label. For delivered messages
// with verified=0, it returns "delivered_in_input_box" instead of "delivered" so
// operators can distinguish confirmed deliveries from soft-fails. A delivered
// message with verified=1 (confirmed) or verified=NULL (pre-#169 row) renders as
// plain "delivered" — the column can't claim a soft-fail it doesn't know about.
func displayState(m store.Message) string {
	if m.State == store.StateDelivered && m.Verified.Valid && m.Verified.Int64 == 0 {
		return displayStateDeliveredInInputBox
	}
	return string(m.State)
}

// wallTime parses an ISO 8601 UTC timestamp and returns the HH:MM:SS portion
// formatted in local time. Returns "-" if the timestamp doesn't parse.
func wallTime(iso string) string {
	t, err := time.Parse("2006-01-02T15:04:05.000Z", iso)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05Z", iso)
		if err != nil {
			return "-"
		}
	}
	return t.Local().Format("15:04:05")
}
