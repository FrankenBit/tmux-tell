package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// claude-msg tail — live diagnostic firehose (#148).
//
// The cross-chamber read-only view the per-mailman journals + single-message
// `track` couldn't give: all bus traffic, live, filtered to what you care
// about. The watch mechanism is **rowid-polling**, not SQLite's update_hook:
// the mailman(s) that write rows are *separate processes* from this CLI, and
// update_hook only fires for the connection that registered it (per-connection,
// same-process), so it would never see their writes. Polling MAX(id) since-last-
// seen is the viable cross-process mechanism (#148 refinement, the pinned call).
// State transitions don't move the id, so in-flight rows are re-read by id for
// queued→delivered/failed lifecycle rendering.

const (
	tailDefaultInterval = 300 * time.Millisecond
	tailFetchCap        = 1000 // per-tick row cap; overflow rolls to next tick via afterID
	tailPreviewLn       = 60
	// tailTimeFormat mirrors the schema's strftime('%Y-%m-%dT%H:%M:%fZ') so the
	// --since floor compares lexically against stored created_at values. (The
	// store keeps its own unexported copy; this is the CLI-side mirror.)
	tailTimeFormat = "2006-01-02T15:04:05.000Z"
)

type tailOpts struct {
	filter   store.TailFilter
	state    string // render-time gate only (see TailFilter doc); "" = any
	interval time.Duration
	format   string // text|json
}

// runTailCLI parses tail-subcommand flags and starts the watch loop.
//
// Usage: claude-msg tail [--from X] [--to Y] [--kind K] [--state S]
//
//	[--since now|5m|today|all] [--interval 300ms] [--format text|json]
func runTailCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "only messages from this agent")
	to := fs.String("to", "", "only messages to this agent")
	kind := fs.String("kind", "", "only this kind (message, delivery_failure_notice, ping, …)")
	state := fs.String("state", "", "only render rows in this state (queued|delivering|delivered|failed)")
	since := fs.String("since", "now", "backfill floor before tailing: now (default) | a duration like 5m | today | all")
	interval := fs.Duration("interval", tailDefaultInterval, "poll cadence")
	format := fs.String("format", "text", "text|json (json = one object per line)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	if *state != "" && !validTailState(*state) {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("invalid --state %q (want queued|delivering|delivered|failed)", *state), exitUsage)
	}
	if *interval <= 0 {
		return writeJSONError(stdout, stderr, "--interval must be positive", exitUsage)
	}

	w, err := parseWindow(*since, time.Now())
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	sinceFloor := ""
	if !w.All {
		sinceFloor = w.Since.UTC().Format(tailTimeFormat)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	opts := tailOpts{
		filter:   store.TailFilter{From: *from, To: *to, Kind: *kind, SinceCreatedAt: sinceFloor},
		state:    *state,
		interval: *interval,
		format:   *format,
	}
	return runTailLoop(s, opts, stdout, stderr)
}

func validTailState(s string) bool {
	switch store.State(s) {
	case store.StateQueued, store.StateDelivering, store.StateDelivered, store.StateFailed:
		return true
	}
	return false
}

// tailState carries the watcher's cross-tick memory: the highest id seen (the
// rowid-poll cursor) and the last-known state of in-flight rows already
// surfaced (so a transition renders once, on change). Pending is keyed by
// numeric id; entries drop when the row reaches a terminal state.
type tailState struct {
	lastID  int64
	pending map[int64]store.State
}

func newTailState() *tailState {
	return &tailState{pending: map[int64]store.State{}}
}

// poll runs one watch tick: surface new rows (id > cursor), then re-read
// in-flight rows for state transitions. Pure w.r.t. wall-clock — the loop owns
// timing — so tests drive it directly by seeding rows between poll calls.
func (ts *tailState) poll(ctx context.Context, s *store.Store, opts tailOpts, out io.Writer) error {
	rows, err := s.TailRows(ctx, ts.lastID, opts.filter, tailFetchCap)
	if err != nil {
		return err
	}
	for _, m := range rows {
		if m.ID > ts.lastID {
			ts.lastID = m.ID
		}
		if opts.state == "" || string(m.State) == opts.state {
			renderTailEvent(out, opts.format, "new", "", m)
		}
		if !isTerminalState(string(m.State)) {
			ts.pending[m.ID] = m.State
		}
	}

	if len(ts.pending) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(ts.pending))
	for id := range ts.pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	cur, err := s.MessagesByIDs(ctx, ids)
	if err != nil {
		return err
	}
	for _, m := range cur {
		prev, ok := ts.pending[m.ID]
		if !ok || m.State == prev {
			continue
		}
		if opts.state == "" || string(m.State) == opts.state {
			renderTailEvent(out, opts.format, "transition", string(prev), m)
		}
		if isTerminalState(string(m.State)) {
			delete(ts.pending, m.ID)
		} else {
			ts.pending[m.ID] = m.State
		}
	}
	return nil
}

// runTailLoop polls on opts.interval until SIGINT/SIGTERM, then exits cleanly
// (exitOK). Mirrors track.go's --watch signal+ticker shape. The signal wiring
// lives here; tailLoop carries the testable loop body.
func runTailLoop(s *store.Store, opts tailOpts, stdout, stderr io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return tailLoop(ctx, s, opts, stdout, stderr)
}

// tailLoop is the poll-until-cancelled body. A cancelled ctx — whether from a
// signal or a test — ends the loop with exitOK (clean Ctrl-C), distinguishing
// that from a genuine store error (exitInternal).
func tailLoop(ctx context.Context, s *store.Store, opts tailOpts, stdout, stderr io.Writer) int {
	ts := newTailState()
	for {
		if err := ts.poll(ctx, s, opts, stdout); err != nil {
			if ctx.Err() != nil {
				return exitOK // cancelled mid-poll — clean exit
			}
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(opts.interval):
		}
	}
}

// tailEvent is one JSON-line record (`--format json`).
type tailEvent struct {
	Event       string `json:"event"` // "new" | "transition"
	ID          string `json:"id"`
	From        string `json:"from"`
	To          string `json:"to"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	PrevState   string `json:"prev_state,omitempty"`
	CreatedAt   string `json:"created_at"`
	DeliveredAt string `json:"delivered_at,omitempty"`
	Body        string `json:"body,omitempty"`
}

func renderTailEvent(w io.Writer, format, event, prevState string, m store.Message) {
	if format == "json" {
		ev := tailEvent{
			Event: event, ID: m.PublicID, From: m.FromAgent, To: m.ToAgent,
			Kind: string(m.Kind), State: string(m.State), PrevState: prevState,
			CreatedAt: m.CreatedAt, Body: m.Body,
		}
		if m.DeliveredAt.Valid {
			ev.DeliveredAt = m.DeliveredAt.String
		}
		if b, err := json.Marshal(ev); err == nil {
			fmt.Fprintln(w, string(b))
		}
		return
	}
	renderTailText(w, event, prevState, m)
}

func renderTailText(w io.Writer, event, prevState string, m store.Message) {
	// Event time: when the change happened — delivered_at for a completed
	// delivery, else created_at.
	tstamp := m.CreatedAt
	if m.DeliveredAt.Valid && m.DeliveredAt.String != "" {
		tstamp = m.DeliveredAt.String
	}
	clock := hhmmss(tstamp)

	statusCol := string(m.State)
	if event == "transition" {
		statusCol = prevState + "→" + string(m.State)
	}

	route := m.FromAgent + "→" + m.ToAgent
	line := fmt.Sprintf("%s  %-18s  %s %s", clock, statusCol, m.PublicID, route)
	if m.Kind != store.KindMessage {
		line += " [" + string(m.Kind) + "]"
	}
	if m.NoReplyExpected {
		line += " 🔕"
	}
	if event == "new" {
		if preview := shortBody(oneLine(m.Body), tailPreviewLn); preview != "" {
			line += "  (" + preview + ")"
		}
	}
	fmt.Fprintln(w, line)
}

// hhmmss extracts the HH:MM:SS clock from an ISO-8601 store timestamp
// ("2006-01-02T15:04:05.000Z"). Falls back to the raw string if the shape is
// unexpected, so a malformed timestamp degrades to visible-but-ugly rather than
// panicking.
func hhmmss(iso string) string {
	for i := 0; i < len(iso); i++ {
		if iso[i] == 'T' && i+9 <= len(iso) {
			return iso[i+1 : i+9]
		}
	}
	return iso
}

// oneLine collapses internal whitespace so a multi-line body renders on the
// single tail row.
func oneLine(body string) string {
	return strings.Join(strings.Fields(body), " ")
}
