package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// pingBody is the placeholder body carried by a kind=ping row. It is
// never pasted into the recipient's pane (the mailman's ping branch
// short-circuits before delivery) — InsertMessage simply requires a
// non-empty body, and this marker keeps audit/inbox views legible.
const pingBody = "ping"

// defaultPingTimeout bounds the probe wait when the caller doesn't set
// one. A reachable agent answers in well under a second (one ClaimNext +
// one LivePanes shell-out); 5s leaves headroom for a busy daemon working
// through a queue ahead of the ping.
const defaultPingTimeout = 5 * time.Second

// pingPollInterval is how often pingProbe re-reads the row's state while
// waiting for the mailman to process it.
const pingPollInterval = 100 * time.Millisecond

// pingStateTimeout is the synthetic terminal state pingProbe reports when
// the wait elapses before the mailman transitions the row. Distinct from
// the store's "failed": a `failed` ping means the agent is registered but
// unreachable (pane gone); a `timeout` means no mailman answered in time
// (daemon down, paused, or backlogged).
const pingStateTimeout = "timeout"

// pingResult is the structured response shared by the `tmux-tell-claude ping`
// CLI subcommand and the `tmux-tell.ping` MCP tool (#144). OK is true only
// when the probe reached `delivered` (recipient reachable). State is one
// of "delivered", "failed", or "timeout".
//
// Class (#366) is the coarse reachability classification — reachable /
// pending / unreachable — layered over the fine-grained Reason; it is set on
// every path (reachable on the OK path) so tooling can branch on reachability
// without memorizing the reason→class map. On the failing path (OK=false),
// Reason classifies WHY (#358) and Evidence carries the raw substrate signals
// behind that call; both are omitted on the reachable path.
type pingResult struct {
	OK        bool          `json:"ok"`
	Agent     string        `json:"agent"`
	ID        string        `json:"id"`
	State     string        `json:"state"`
	Class     pingClass     `json:"class"`
	ElapsedMs int64         `json:"elapsed_ms"`
	Reason    pingReason    `json:"reason,omitempty"`
	Evidence  *pingEvidence `json:"evidence,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// pingReason classifies an UNREACHABLE ping into the operator-actionable
// condition behind it (#358). It is a CLOSED set of typed constants, not a
// free-form string: the value space is the contract the CLI renderer, the
// operator, and downstream tooling branch on to route recovery, so an
// open-ended reason would defeat the distinguishability the feature exists to
// provide. A new condition is a deliberate addition here, not an emergent
// string.
type pingReason string

const (
	// reasonPaneDead: the mailman claimed the ping and found the recipient's
	// pane not live (the ping reached store-terminal `failed`). The session is
	// gone — re-discover or re-register. Evidence-free beyond the row Error.
	reasonPaneDead pingReason = "pane_dead"
	// reasonMailmanDown: the recipient's mailman unit is not active. Nothing
	// is draining the queue — start the daemon.
	reasonMailmanDown pingReason = "mailman_down"
	// reasonStuck: the mailman is parked in the #291 stuck state (N
	// consecutive pane-probe failures → it wrote agents.stuck_reason and
	// stopped probing tmux). Clear via `register --force`. The specific park
	// reason rides in Evidence.StuckReason.
	reasonStuck pingReason = "stuck"
	// reasonBlockedDelivery: the mailman is running and reachable, but our ping
	// row wasn't claimed within the bound because the daemon is occupied on a
	// PRIOR delivery — classically gated on operator-typing for the head message
	// (the gathered current_state=awaiting-operator reflects that head delivery,
	// not our ping). A ping never traverses the observe-gate itself: serve.go's
	// mailman loop short-circuits kind=ping before the gate, so this names a
	// healthy-but-pending condition (classes pending, #366), not a broken one —
	// wait for the in-flight delivery to clear.
	reasonBlockedDelivery pingReason = "blocked_delivery"
	// reasonBacklogDraining: the mailman is running and working through a real
	// backlog (queue depth exceeds our own probe row); the ping is behind in
	// line. Wait, or query queue depth.
	reasonBacklogDraining pingReason = "backlog_draining"
)

// pingClass is the coarse reachability classification (#366) layered over the
// fine-grained pingReason. Where pingReason answers "what specific condition?"
// (the recovery-routing contract), pingClass answers "is the substrate
// healthy?" (the reachability-routing contract) — the three-way split the flat
// #358 `reachable:false` binary collapsed:
//
//	reachable   — the probe confirmed delivery (mailman up, pane live).
//	pending     — the substrate is healthy and making forward progress, but the
//	              probe didn't confirm within its bound: the mailman is draining
//	              a backlog ahead of our row, or is busy on a prior delivery.
//	              Retrying / waiting helps.
//	unreachable — the substrate is broken: the mailman is down, parked in the
//	              #291 stuck state, or the recipient's pane is gone. Retrying
//	              won't help; the operator must act.
//
// Like pingReason it is a CLOSED set: tooling branches on class for coarse
// reachability-routing and on reason for fine recovery-routing. The mapping is
// single-sourced in reachabilityClass — no render site re-derives it.
type pingClass string

const (
	classReachable   pingClass = "reachable"
	classPending     pingClass = "pending"
	classUnreachable pingClass = "unreachable"
)

// pingEvidence is the raw substrate signal-set gathered on the UNREACHABLE
// path (#358), carried alongside the classified Reason so the operator sees
// the basis for the call — and can still route correctly if the heuristic
// mislabels an edge. Fields are read-only snapshots taken after the probe
// times out / fails.
//
// last_delivered_at / mailman_idle_since (in the #358 example JSON) are
// deliberately ABSENT: there is no store source for them yet — they are
// #348's `agents`-listing extensions. Adding always-empty fields would read
// as supported-but-broken; they join this struct when #348 lands.
type pingEvidence struct {
	// MailmanActive is `systemctl --user is-active <unit>` for the recipient.
	MailmanActive bool `json:"mailman_active"`
	// QueueDepth is the recipient's queued-message count. On the timeout path
	// it INCLUDES this probe's own still-queued row, so depth 1 means "only
	// our ping"; >1 means a real backlog behind/ahead of it.
	QueueDepth int `json:"queue_depth"`
	// CurrentState is the observe-gate / agent_state classification
	// (idle / working / rate-limited / usage-limited /
	// awaiting-operator / at-rest-in-compaction / unknown).
	CurrentState string `json:"current_state"`
	// StuckReason is the #291 park reason (e.g. "pane-not-found") when the
	// mailman has parked itself; empty otherwise.
	StuckReason string `json:"stuck_reason,omitempty"`
}

// pingCLIParams is the resolved input to runPingWithStore, post-flag-parse.
type pingCLIParams struct {
	From    string
	To      string
	Timeout time.Duration
	Format  string
}

// insertPing validates the recipient and inserts a kind=ping row,
// returning its public_id. The recipient MUST be registered: pinging a
// non-registered agent fails loud (per #144 out-of-scope — "should
// fail-loud with a clear error, not silently succeed") rather than
// enqueuing a row no mailman would ever claim. The ping bypasses the
// recipient-queue and sender-backlog caps: a reachability probe must not
// be rejected because the recipient's inbox is momentarily full (the row
// is transient — queued→delivered with no paste).
func insertPing(ctx context.Context, s *store.Store, from, to string) (string, error) {
	if from == "" {
		return "", errors.New("cannot resolve sender: set $TMUX_AGENT_NAME, pass --from, or register this pane")
	}
	if to == "" {
		return "", errors.New("recipient agent required")
	}
	if _, err := s.GetAgent(ctx, to); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("unknown recipient: %s (not registered — ping cannot reach an unregistered agent)", to)
		}
		return "", err
	}
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: from,
		ToAgent:   to,
		Body:      pingBody,
		Kind:      store.KindPing,
		// Caps disabled (0): a probe must not be rejected on a full queue.
		MaxRecipientQueue: 0,
		MaxSenderBacklog:  0,
	})
	if err != nil {
		return "", err
	}
	return res.PublicID, nil
}

// pollPingTerminal polls the row identified by id until it reaches a
// store-terminal state (delivered/failed) or timeout elapses, returning
// the structured pingResult. agent is echoed back in the result for the
// caller's convenience. A GetMessage error aborts the poll and is
// returned. ctx cancellation is reported as a timeout-class result.
func pollPingTerminal(ctx context.Context, s *store.Store, id, agent string, timeout, pollInterval time.Duration) (pingResult, error) {
	if pollInterval <= 0 {
		pollInterval = pingPollInterval
	}
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		m, err := s.GetMessage(ctx, id)
		if err != nil {
			return pingResult{}, err
		}
		if m.State == store.StateDelivered || m.State == store.StateFailed {
			out := pingResult{
				OK:        m.State == store.StateDelivered,
				Agent:     agent,
				ID:        id,
				State:     string(m.State),
				ElapsedMs: time.Since(start).Milliseconds(),
			}
			if m.Error.Valid {
				out.Error = m.Error.String
			}
			return out, nil
		}
		if !time.Now().Before(deadline) {
			return pingResult{
				OK:        false,
				Agent:     agent,
				ID:        id,
				State:     pingStateTimeout,
				ElapsedMs: time.Since(start).Milliseconds(),
				Error:     fmt.Sprintf("no terminal state within %s (mailman down, paused, or backlogged?)", timeout),
			}, nil
		}
		select {
		case <-ctx.Done():
			return pingResult{
				OK:        false,
				Agent:     agent,
				ID:        id,
				State:     pingStateTimeout,
				ElapsedMs: time.Since(start).Milliseconds(),
				Error:     ctx.Err().Error(),
			}, nil
		case <-time.After(pollInterval):
		}
	}
}

// pingProbe is the shared core behind both the CLI and MCP surfaces
// (#144): insert a kind=ping row, then poll for its terminal state. On an
// UNREACHABLE outcome it gathers the substrate evidence and classifies the
// reason (#358) — only on the failing path, so a reachable ping stays one
// ClaimNext + one LivePanes with no extra probes.
func pingProbe(ctx context.Context, s *store.Store, from, to string, timeout, pollInterval time.Duration) (pingResult, error) {
	id, err := insertPing(ctx, s, from, to)
	if err != nil {
		return pingResult{}, err
	}
	res, err := pollPingTerminal(ctx, s, id, to, timeout, pollInterval)
	if err != nil {
		return res, err
	}
	if !res.OK {
		ev := gatherPingEvidence(ctx, s, to)
		res.Evidence = &ev
		res.Reason = classifyPingReason(res.State, ev)
	}
	res.Class = reachabilityClass(res)
	return res, nil
}

// classifyPingReason maps an UNREACHABLE probe outcome to its operator-
// actionable reason (#358). Pure + table-tested: the evidence is gathered by
// the caller (gatherPingEvidence), so this decision tree is exercised for
// every sub-case without a live system.
//
// Decision tree (only reached when the probe did NOT deliver):
//
//	failed                            → pane_dead        (mailman claimed it; pane not live)
//	timeout + StuckReason set         → stuck            (parked per #291)
//	timeout + mailman not active      → mailman_down     (nothing draining the queue)
//	timeout + state=awaiting-operator → blocked_delivery (mailman busy on a prior gated delivery)
//	timeout + queue depth > 1         → backlog_draining (real backlog ahead of our probe row)
//	timeout + (anything else)         → blocked_delivery (mailman alive but hasn't claimed our ping)
//
// The default-to-blocked_delivery tail is the honest fallback: a running,
// non-stuck, non-backlogged mailman that still hadn't claimed our ping row
// within the bound is alive but momentarily occupied — typically finishing a
// prior delivery. The ping never reaches the observe-gate itself (serve.go
// short-circuits kind=ping before it), so this is a healthy-but-pending
// condition, not a paste-blocked one. Evidence travels with the reason so the
// operator sees the raw signals regardless of the label.
func classifyPingReason(pingState string, ev pingEvidence) pingReason {
	if pingState == string(store.StateFailed) {
		return reasonPaneDead
	}
	switch {
	case ev.StuckReason != "":
		return reasonStuck
	case !ev.MailmanActive:
		return reasonMailmanDown
	case ev.CurrentState == tmuxio.StateAwaitingOperator.String():
		return reasonBlockedDelivery
	case ev.QueueDepth > 1:
		return reasonBacklogDraining
	default:
		return reasonBlockedDelivery
	}
}

// reachabilityClass maps a probe outcome to its coarse reachability class
// (#366) — the single canonical site for the reason→class mapping, so no
// render or tooling site re-derives it. A confirmed delivery is reachable; the
// two "healthy but unconfirmed-in-bound" reasons are pending; the three
// "substrate broken" reasons are unreachable.
//
// blocked_delivery maps to pending UNCONDITIONALLY — not decided by
// current_state. A ping never traverses the observe-gate: serve.go's mailman
// loop short-circuits kind=ping immediately after ClaimNext, before the gate,
// the paste-capability check, and the paste itself (handlePing only probes
// pane liveness). So the persistent-broken sub-cases a state-split would guard
// against — a paste-incapable adapter force-defer, a wedged gate — cannot
// produce a ping blocked_delivery; a paste-incapable adapter's mailman marks a
// ping delivered fine. For a ping, blocked_delivery only ever means "the
// mailman is alive but occupied on a prior delivery and hasn't looped back to
// claim our row" — reachable+pending, the same family as backlog_draining. The
// observe-gate's own MaxWait cap (non-disableable, 5min default; deliver-anyway
// on the cap) guarantees the mailman makes forward progress, so there is no
// indefinite park to reclassify as broken.
func reachabilityClass(res pingResult) pingClass {
	if res.OK {
		return classReachable
	}
	switch res.Reason {
	case reasonBacklogDraining, reasonBlockedDelivery:
		return classPending
	default: // reasonPaneDead, reasonMailmanDown, reasonStuck (and any unset)
		return classUnreachable
	}
}

// hint returns the short retryability cue the CLI appends to a PENDING or
// UNREACHABLE headline (#366) so the operator sees the load-bearing
// distinction the class exists to carry: a PENDING probe is worth retrying
// (the substrate is healthy and working), an UNREACHABLE one needs operator
// action (it won't clear on its own). Anchoring the headline on retryability
// was Herald's #366 naming-pre-flight ask. Reachable needs no cue (confirmed).
func (c pingClass) hint() string {
	switch c {
	case classPending:
		return "retry or wait, the mailman is working"
	case classUnreachable:
		return "won't clear on its own, operator action needed"
	default:
		return ""
	}
}

// gatherPingEvidence reads the substrate signals behind an UNREACHABLE ping
// (#358). Best-effort: each probe failure degrades to a zero value rather than
// erroring — the evidence is diagnostic context, not a hard contract, and a
// partial picture still routes the operator better than a bare timeout.
func gatherPingEvidence(ctx context.Context, s *store.Store, agent string) pingEvidence {
	ev := pingEvidence{}
	if a, err := s.GetAgent(ctx, agent); err == nil {
		ev.StuckReason = a.StuckReason
	}
	ev.MailmanActive = mailmanActive(ctx, agent)
	if d, err := s.RecipientQueueDepth(ctx, agent); err == nil {
		ev.QueueDepth = d
	}
	st, _ := resolveAgentState(ctx, s, agent)
	ev.CurrentState = st.State
	return ev
}

// pingReasonSuffix builds the human-readable suffix the CLI appends to an
// UNREACHABLE line (#358): the reason, its phrase, and the load-bearing
// evidence (queue depth + observe-gate state, plus the park reason when
// stuck). Callers gate on res.Reason != "".
func pingReasonSuffix(res pingResult) string {
	suffix := fmt.Sprintf("%s: %s", res.Reason, res.Reason.describe())
	if res.Evidence != nil {
		suffix += fmt.Sprintf("; queue=%d, state=%s", res.Evidence.QueueDepth, res.Evidence.CurrentState)
		if res.Evidence.StuckReason != "" {
			suffix += ", stuck=" + res.Evidence.StuckReason
		}
	}
	return suffix
}

// describe renders a pingReason as a short human phrase for the CLI suffix.
func (r pingReason) describe() string {
	switch r {
	case reasonPaneDead:
		return "recipient pane is gone"
	case reasonMailmanDown:
		return "mailman daemon not running"
	case reasonStuck:
		return "mailman parked in the stuck state"
	case reasonBlockedDelivery:
		return "mailman busy on a prior delivery"
	case reasonBacklogDraining:
		return "mailman is working through a backlog"
	default:
		return string(r)
	}
}

// runPingCLI parses ping-subcommand flags, opens the store, resolves the
// sender identity, and dispatches to runPingWithStore.
//
// Usage: tmux-tell-claude ping <agent> [--timeout D] [--format text|json] [--from NAME]
func runPingCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	from := fs.String("from", "", "sender agent name (env: TMUX_AGENT_NAME)")
	timeout := fs.Duration("timeout", defaultPingTimeout,
		"bound the wait for a terminal delivery state")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: %s ping <agent> [--timeout D] [--format text|json]\n", active.BinaryName)
		return exitUsage
	}
	to := fs.Arg(0)

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	fromName, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	return runPingWithStore(ctx, s, pingCLIParams{
		From:    fromName,
		To:      to,
		Timeout: *timeout,
		Format:  *format,
	}, stdout, stderr)
}

// runPingWithStore is the pure-logic core: validates --format, runs the
// probe, renders the result, and returns the exit code. Designed to be
// table-tested.
func runPingWithStore(ctx context.Context, s *store.Store, p pingCLIParams, stdout, stderr io.Writer) int {
	switch p.Format {
	case "", "text", "json":
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", p.Format), exitUsage)
	}
	res, err := pingProbe(ctx, s, p.From, p.To, p.Timeout, pingPollInterval)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUnavailable)
	}
	renderPingResult(stdout, res, p.Format)
	return pingExitCode(res)
}

// renderPingResult writes the probe outcome in the requested shape.
func renderPingResult(stdout io.Writer, res pingResult, format string) {
	switch format {
	case "json":
		_ = writeJSONResult(stdout, res)
	default: // text / ""
		fmt.Fprintf(stdout, "AGENT\t%s\n", res.Agent)
		switch {
		case res.OK:
			fmt.Fprintf(stdout, "PING\t%s (REACHABLE)\n", res.State)
		case res.Class == classPending:
			// PENDING: substrate healthy + progressing, the probe just didn't
			// confirm in-bound (#366). The trailing hint anchors the operator on
			// the retryable-vs-act distinction that separates PENDING from
			// UNREACHABLE:
			//   timeout — PENDING (backlog_draining: mailman is working through a backlog; queue=7, state=working) — retry or wait, the mailman is working
			fmt.Fprintf(stdout, "PING\t%s — PENDING (%s) — %s\n", res.State, pingReasonSuffix(res), classPending.hint())
		case res.Reason != "":
			// UNREACHABLE: substrate broken (#358/#366), structured reason suffix
			// plus the act-now hint:
			//   timeout — UNREACHABLE (mailman_down: mailman daemon not running; queue=1, state=unknown) — won't clear on its own, operator action needed
			fmt.Fprintf(stdout, "PING\t%s — UNREACHABLE (%s) — %s\n", res.State, pingReasonSuffix(res), classUnreachable.hint())
		default:
			fmt.Fprintf(stdout, "PING\t%s (UNREACHABLE) — %s\n", res.State, classUnreachable.hint())
		}
		fmt.Fprintf(stdout, "ELAPSED\t%dms\n", res.ElapsedMs)
		fmt.Fprintf(stdout, "ID\t%s\n", res.ID)
		if res.Error != "" {
			fmt.Fprintf(stdout, "ERROR\t%s\n", res.Error)
		}
	}
}

// pingExitCode maps a probe outcome to a sysexits-style code so tooling can
// branch on reachability — keyed on the #366 reachability class:
//   - reachable   → exitOK (0)
//   - pending     → exitTempFail (substrate healthy + progressing; retry may help)
//   - unreachable → exitUnavailable (substrate broken; retry won't help)
//
// This refines the pre-#366 state-keyed mapping along the substrate-permanent-
// vs-transient axis: mailman_down and stuck (state=timeout, previously
// exitTempFail) now class unreachable → exitUnavailable, because a down or
// parked mailman won't self-heal on a retry. backlog_draining and
// blocked_delivery stay exitTempFail (now via class=pending); pane_dead stays
// exitUnavailable (now via class=unreachable).
func pingExitCode(res pingResult) int {
	switch res.Class {
	case classReachable:
		return exitOK
	case classPending:
		return exitTempFail
	default: // classUnreachable (and the defensive unset case)
		return exitUnavailable
	}
}
