package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// runDigestCLI parses digest-subcommand flags and dispatches.
//
// Usage: tmux-msg-claude digest [--since 24h|7d|today|yesterday|week|all]
//
//	[--counterparty NAME] [--format text|json]
//
// digest is the qualitative campaign-arc view (#161): which conversations were
// active, which threads closed cleanly, who's owed a reply. It is the narrative
// sibling to `stats` (#147, quantitative metrics-aggregate) and reuses #147's
// aggregation layer (StatsPerAgent for sent/received counts, the parseWindow
// helper for `--since`) plus #141's buildThreadTree for the reply-tree walk —
// none of that SQL/walk is re-implemented here.
func runDigestCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	format := fs.String("format", "text", "text|json")
	since := fs.String("since", "24h", "time window: today|yesterday|week | all | <N>d | a duration like 4h")
	counterparty := fs.String("counterparty", "", "scope to conversations involving one agent")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	w, err := parseWindow(*since, time.Now())
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	return runDigestWithStore(context.Background(), s, w, *since, *counterparty, *format, stdout, stderr)
}

// counterpartyDigest is one agent's conversational summary for the window:
// quantitative sent/received (from #147's StatsPerAgent) plus thread
// participation (how many reply-threads it took part in, and how many of those
// are closed vs still in-flight per the heuristic).
type counterpartyDigest struct {
	Agent    string `json:"agent"`
	Sent     int    `json:"sent"`
	Received int    `json:"received"`
	Threads  int    `json:"threads"`
	Closed   int    `json:"closed"`
	InFlight int    `json:"in_flight"`
}

// inFlightThread is one reply-thread whose latest message is awaiting a reply
// (see the close heuristic in classifyThreads). Listed in the "needs
// follow-up" section so an operator can see what's still owed at day's end.
type inFlightThread struct {
	RootID   string `json:"root_id"`
	LatestID string `json:"latest_id"`
	From     string `json:"from"`     // who sent the un-acked latest message
	Awaiting string `json:"awaiting"` // recipient of that message — the party owing a reply
	LatestAt string `json:"latest_at"`
	Preview  string `json:"preview"`
}

type digestResult struct {
	Window         string               `json:"window"`
	Counterparties []counterpartyDigest `json:"counterparties"`
	InFlight       []inFlightThread     `json:"in_flight_threads"`
}

// threadInfo is a classified reply-thread within the window.
type threadInfo struct {
	rootID       string
	participants map[string]bool // distinct from/to agents across the thread
	closed       bool
	latest       store.Message // highest-id message in the thread
}

func runDigestWithStore(ctx context.Context, s *store.Store, w store.StatsWindow,
	sinceSpec, counterparty, format string, stdout, stderr io.Writer,
) int {
	// Quantitative counts reuse #147's aggregation primitive verbatim.
	agentStats, err := s.StatsPerAgent(ctx, w)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	// Full rows for thread-structure analysis (window-bounded via the same
	// whereSince seam StatsPerAgent uses).
	msgs, err := s.MessagesInWindow(ctx, w)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	threads, err := classifyThreads(msgs)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	res := assembleDigest(sinceSpec, agentStats, threads, counterparty)

	if format == "json" {
		if err := writeJSONResult(stdout, res); err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		return exitOK
	}
	renderDigestText(stdout, res, counterparty)
	return exitOK
}

// classifyThreads groups the window's conversational messages into reply-threads
// and classifies each as closed or in-flight.
//
// Only KindMessage rows participate — system chrome (delivery_failure_notice,
// dedupe_notice, stranded_draft, ping, control) is substrate plumbing, not
// narrative, so it is excluded from thread analysis.
//
// Close heuristic (the architectural core, pinned in #161's ACs — a heuristic,
// not ground truth, because the substrate cannot know if a conversation is
// *semantically* done): a thread is **in-flight / likely-needs-follow-up** when
// its latest message awaits a reply — i.e. it was actually sent (state not
// failed) and the sender did NOT mark it `no_reply_expected` (🔕). The awaited
// party is that message's recipient. A thread is **closed** otherwise: the last
// word carried 🔕 (an explicit "no ack needed" / terminal ack), or the latest
// send failed (broken, not awaiting). The 🔕 flag (#170) is the cleanest
// substrate signal for "this needs no response"; before it, every un-replied
// latest message would read as in-flight, which is why the heuristic leans on it.
//
// "Root" is resolved *within the window*: a message whose reply_to points to a
// pre-window parent is treated as the in-window thread root. The per-thread tree
// is assembled by #141's buildThreadTree (reused, not re-implemented) — that
// both validates the group is a single connected chain and yields the canonical
// root id.
func classifyThreads(msgs []store.Message) ([]threadInfo, error) {
	inSet := make(map[string]store.Message, len(msgs))
	for _, m := range msgs {
		if m.Kind == store.KindMessage {
			inSet[m.PublicID] = m
		}
	}

	// rootOf walks reply_to up until it leaves the in-window conversational
	// set; memoized so a deep chain is walked once. Cycle-guarded defensively
	// even though reply_to is a once-set edge.
	rootCache := map[string]string{}
	var rootOf func(id string) string
	rootOf = func(id string) string {
		if r, ok := rootCache[id]; ok {
			return r
		}
		m := inSet[id]
		seen := map[string]bool{id: true}
		cur := m
		for cur.ReplyTo.Valid && cur.ReplyTo.String != "" {
			parent, ok := inSet[cur.ReplyTo.String]
			if !ok || seen[parent.PublicID] {
				break // parent outside window (or cycle) → cur is the in-window root
			}
			seen[parent.PublicID] = true
			cur = parent
		}
		rootCache[id] = cur.PublicID
		return cur.PublicID
	}

	groups := map[string][]store.Message{}
	for _, m := range inSet {
		r := rootOf(m.PublicID)
		groups[r] = append(groups[r], m)
	}

	out := make([]threadInfo, 0, len(groups))
	for _, g := range groups {
		// Reuse #141's tree assembler: validates single-chain + roots it.
		root, err := buildThreadTree(g)
		if err != nil {
			return nil, fmt.Errorf("digest: thread %s: %w", g[0].PublicID, err)
		}
		info := threadInfo{rootID: root.ID, participants: map[string]bool{}}
		for _, m := range g {
			info.participants[m.FromAgent] = true
			info.participants[m.ToAgent] = true
			if info.latest.ID == 0 || m.ID > info.latest.ID {
				info.latest = m
			}
		}
		l := info.latest
		awaitsReply := !l.NoReplyExpected && l.State != store.StateFailed
		info.closed = !awaitsReply
		out = append(out, info)
	}
	return out, nil
}

// assembleDigest merges the quantitative per-agent counts with thread
// participation into the per-counterparty rows, builds the in-flight list, and
// applies the optional --counterparty scope.
func assembleDigest(sinceSpec string, agentStats []store.AgentStat,
	threads []threadInfo, counterparty string,
) digestResult {
	rows := map[string]*counterpartyDigest{}
	row := func(name string) *counterpartyDigest {
		r := rows[name]
		if r == nil {
			r = &counterpartyDigest{Agent: name}
			rows[name] = r
		}
		return r
	}
	for _, a := range agentStats {
		r := row(a.Agent)
		r.Sent = a.Sent
		r.Received = a.Received
	}
	for _, t := range threads {
		for agent := range t.participants {
			r := row(agent)
			r.Threads++
			if t.closed {
				r.Closed++
			} else {
				r.InFlight++
			}
		}
	}

	var inflight []inFlightThread
	for _, t := range threads {
		if t.closed {
			continue
		}
		if counterparty != "" && !t.participants[counterparty] {
			continue
		}
		l := t.latest
		inflight = append(inflight, inFlightThread{
			RootID:   t.rootID,
			LatestID: l.PublicID,
			From:     l.FromAgent,
			Awaiting: l.ToAgent,
			LatestAt: l.CreatedAt,
			Preview:  threadBodyPreview(l.Body),
		})
	}
	// Newest-first: the most recent un-acked threads are the most actionable.
	sort.Slice(inflight, func(i, j int) bool {
		if inflight[i].LatestAt != inflight[j].LatestAt {
			return inflight[i].LatestAt > inflight[j].LatestAt
		}
		return inflight[i].RootID < inflight[j].RootID
	})

	out := make([]counterpartyDigest, 0, len(rows))
	for _, r := range rows {
		if counterparty != "" && r.Agent != counterparty {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })

	return digestResult{Window: sinceSpec, Counterparties: out, InFlight: inflight}
}

func renderDigestText(w io.Writer, res digestResult, counterparty string) {
	scope := ""
	if counterparty != "" {
		scope = fmt.Sprintf(" — %s", counterparty)
	}
	fmt.Fprintf(w, "Bus digest%s — window %s\n\n", scope, res.Window)

	header := []string{"COUNTERPARTY", "SENT", "RECEIVED", "THREADS", "CLOSED", "IN-FLIGHT"}
	rows := make([][]string, 0, len(res.Counterparties))
	for _, c := range res.Counterparties {
		rows = append(rows, []string{
			c.Agent, itoa(c.Sent), itoa(c.Received),
			itoa(c.Threads), itoa(c.Closed), itoa(c.InFlight),
		})
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "  (no conversational traffic in window)")
	} else {
		renderTextTable(w, header, rows)
	}

	fmt.Fprintln(w, "\nIn-flight threads (likely need follow-up):")
	if len(res.InFlight) == 0 {
		fmt.Fprintln(w, "  (none — every thread's last word needed no reply)")
	} else {
		for _, t := range res.InFlight {
			fmt.Fprintf(w, "  • %s  %s → %s awaits reply  (latest %s)\n",
				t.RootID, t.From, t.Awaiting, t.LatestID)
			if t.Preview != "" {
				fmt.Fprintf(w, "      %s\n", t.Preview)
			}
		}
	}
	// The close/in-flight split is a heuristic keyed on the 🔕 no-reply marker;
	// the substrate can't know if a conversation is semantically done (#161).
	fmt.Fprintln(w, "\n(in-flight = latest message not marked 🔕 no-reply-expected; a heuristic, not ground truth)")
}
