package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// tmux-msg-claude inbox --watch — interactive TUI consumer for mailbox-only
// agents (#149).
//
// mailbox-only agents (#116) don't get paste-and-Enter delivery — the mailman
// deliberately doesn't paste into their (nonexistent) pane — so their queue
// only drains when something marks messages consumed. `inbox` is read-only and
// `inbox --ack`/`--ack-all` (#221) drain by id; neither gives a live, scan-and-
// drain surface. `--watch` is that surface: a full-screen list that refreshes
// as mail lands and acks under the cursor with one keystroke.
//
// Watch mechanism is **rowid-polling**, not SQLite's update_hook — the same
// pinned call tail.go documents (#148): the mailman that writes rows is a
// *separate process*, and update_hook only fires for the connection that
// registered it, so it would never observe those writes. A bubbletea tea.Tick
// re-runs ListMessages(state=queued) each interval; cross-process arrivals show
// up on the next tick.
//
// The ack action composes with #221: `space` calls store.MarkAcknowledged
// (queued→acknowledged), the same transition `inbox --ack` drives. There is no
// separate "delivered"/"read" lifecycle for mailbox-only consumers — acknowledged
// is the drain (issue #149's original "space → delivered" predates #221's
// acknowledged state; we anchor on the #221 vocabulary).

const (
	// inboxWatchDefaultInterval — calmer than tail's 300ms firehose cadence:
	// a human reading their mailbox doesn't need sub-second refresh.
	inboxWatchDefaultInterval = 2 * time.Second
	// inboxWatchFetchCap bounds the queued rows pulled per poll. 500 is far
	// above any realistic mailbox-only backlog — the per-recipient queue cap
	// (capRecipientQueue) is 5, so a queue this deep is itself the anomaly. A
	// documented sanity bound, not a silent truncation (sister to #267's
	// ListReplies cap).
	inboxWatchFetchCap   = 500
	inboxWatchFromColW   = 14 // from-agent column width before truncation
	inboxWatchMinPreview = 24 // body-preview floor when the terminal is narrow
)

// --- bubbletea messages -----------------------------------------------------

// inboxPollMsg carries the result of one rowid-poll of the queued list.
type inboxPollMsg struct {
	msgs []store.Message
	err  error
}

// inboxActionMsg carries the result of an ack (or, later, fail) action.
type inboxActionMsg struct {
	verb string // "acked"
	id   string
	err  error
}

// inboxTickMsg fires on the poll interval; its handler launches the next poll.
type inboxTickMsg time.Time

// --- model ------------------------------------------------------------------

// inboxWatchModel is the bubbletea model. It holds the store directly: the
// action/poll closures (tea.Cmd) call into it, which keeps Update a pure
// state-transition function — and lets tests exercise the full ack path by
// invoking the returned Cmd against a temp-file store, no TTY required.
type inboxWatchModel struct {
	store    *store.Store
	ctx      context.Context
	agent    string
	interval time.Duration

	msgs     []store.Message // queued mail, oldest-first (ListMessages ASC)
	cursor   int             // selected row
	selID    string          // public_id under the cursor, preserved across re-polls
	expanded bool            // is the selected row's full body shown?
	width    int
	height   int

	status   string // transient one-line result of the last action
	loadErr  error  // last poll/action error, surfaced in the footer
	acted    int    // messages drained this session (scrollback summary)
	quitting bool
}

func (m inboxWatchModel) Init() tea.Cmd {
	// Poll immediately (first frame shows real state) AND start the single tick
	// chain. The tick is the sole rescheduler (see Update): poll results never
	// re-arm a tick, so an action-triggered one-shot refresh can't spawn a
	// second timer chain.
	return tea.Batch(m.pollCmd(), inboxTickCmd(m.interval))
}

// pollCmd reads the queued list once. It does not sleep — the tick owns
// cadence — so tests can drive it directly.
func (m inboxWatchModel) pollCmd() tea.Cmd {
	return func() tea.Msg {
		msgs, err := m.store.ListMessages(m.ctx, store.ListFilter{
			ToAgent: m.agent,
			State:   store.StateQueued,
			Limit:   inboxWatchFetchCap,
		})
		return inboxPollMsg{msgs: msgs, err: err}
	}
}

func inboxTickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return inboxTickMsg(t) })
}

// ackCmd transitions the message under the cursor queued→acknowledged (#221).
func (m inboxWatchModel) ackCmd(id string) tea.Cmd {
	return func() tea.Msg {
		err := m.store.MarkAcknowledged(m.ctx, m.agent, id)
		return inboxActionMsg{verb: "acked", id: id, err: err}
	}
}

func (m inboxWatchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case inboxTickMsg:
		// The tick is the sole scheduler: poll once and arm the next tick. Poll
		// results (inboxPollMsg) never reschedule, so an action-triggered refresh
		// stays a one-shot and can't spawn a second tick chain (the original #149
		// loop rescheduled on every poll, so each ack — which re-polls — leaked an
		// extra timer; #268 fix).
		return m, tea.Batch(m.pollCmd(), inboxTickCmd(m.interval))

	case inboxPollMsg:
		if msg.err != nil {
			m.loadErr = msg.err
		} else {
			m.loadErr = nil
			m.msgs = msg.msgs
			m.reconcileCursor()
		}
		return m, nil // never reschedules — the tick owns cadence

	case inboxActionMsg:
		if msg.err != nil {
			m.loadErr = msg.err
			m.status = fmt.Sprintf("error: %v", msg.err)
		} else {
			m.acted++
			m.status = fmt.Sprintf("%s %s", msg.verb, msg.id)
		}
		// Re-poll now so the drained row drops immediately; the store stays the
		// single source of truth (no optimistic local removal to drift from it).
		// Safe post-fix: this poll is a one-shot, it does not re-arm the tick.
		return m, m.pollCmd()

	case inboxReplyMsg:
		switch {
		case msg.err != nil:
			m.loadErr = msg.err
			m.status = "reply failed: " + msg.err.Error()
		case msg.abandoned:
			m.status = "reply abandoned (empty body)"
		default:
			m.loadErr = nil
			m.status = fmt.Sprintf("replied to %s → %s", msg.replyToID, msg.sentID)
		}
		// A reply doesn't drain my queue (the original stays queued; replying ≠
		// acking), so no refresh is needed — the tick keeps the list current.
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey is the keymap. (The `D` mark-failed action from #149's proposal was
// deliberately not built — no queued→failed substrate path; see #268.)
func (m inboxWatchModel) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "q", "ctrl+c", "esc":
		m.quitting = true
		return m, tea.Quit

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.expanded = false
			m.syncSelID()
		}
		return m, nil

	case "down", "j":
		if m.cursor < len(m.msgs)-1 {
			m.cursor++
			m.expanded = false
			m.syncSelID()
		}
		return m, nil

	case "enter":
		if len(m.msgs) > 0 {
			m.expanded = !m.expanded
		}
		return m, nil

	case " ": // space — ack/drain the selected message
		if cur, ok := m.current(); ok {
			return m, m.ackCmd(cur.PublicID)
		}
		return m, nil

	case "r": // reply to the selected message via $EDITOR (#268)
		if cur, ok := m.current(); ok {
			return m, m.startReply(cur)
		}
		return m, nil
	}
	return m, nil
}

// current returns the message under the cursor, ok=false when the list is empty.
func (m inboxWatchModel) current() (store.Message, bool) {
	if m.cursor < 0 || m.cursor >= len(m.msgs) {
		return store.Message{}, false
	}
	return m.msgs[m.cursor], true
}

// syncSelID records the public_id under the cursor so the next poll can restore
// the selection to the same logical message as rows above it drain.
func (m *inboxWatchModel) syncSelID() {
	if cur, ok := m.current(); ok {
		m.selID = cur.PublicID
	} else {
		m.selID = ""
	}
}

// reconcileCursor re-anchors the cursor after a poll replaces the list: stay on
// the previously-selected public_id if it's still queued, else clamp to a valid
// index. Handles rows draining (this session or a concurrent --ack), new
// arrivals, and the list emptying entirely.
func (m *inboxWatchModel) reconcileCursor() {
	if len(m.msgs) == 0 {
		m.cursor = 0
		m.selID = ""
		m.expanded = false
		return
	}
	if m.selID != "" {
		for i, msg := range m.msgs {
			if msg.PublicID == m.selID {
				m.cursor = i
				return
			}
		}
		// Selected row gone — collapse its expansion before clamping.
		m.expanded = false
	}
	if m.cursor >= len(m.msgs) {
		m.cursor = len(m.msgs) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.syncSelID()
}

// --- view -------------------------------------------------------------------

var (
	inboxHelpStyle   = lipgloss.NewStyle().Faint(true)
	inboxCursorStyle = lipgloss.NewStyle().Bold(true)
	inboxStatusStyle = lipgloss.NewStyle().Faint(true)
	inboxErrStyle    = lipgloss.NewStyle().Bold(true)
	inboxBodyStyle   = lipgloss.NewStyle().Faint(true)
)

func (m inboxWatchModel) View() string {
	if m.quitting {
		// Leave the alt-screen clean; runInboxWatch prints the scrollback
		// summary on the normal screen after the program exits.
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "inbox %q · %d pending\n", m.agent, len(m.msgs))
	b.WriteString(inboxHelpStyle.Render("↑/↓ navigate · space ack · enter expand · r reply · q quit"))
	b.WriteByte('\n')
	b.WriteString(m.rule())
	b.WriteByte('\n')

	if len(m.msgs) == 0 {
		b.WriteString(inboxStatusStyle.Render("✓ inbox drained — nothing queued"))
		b.WriteByte('\n')
	} else {
		for i, msg := range m.msgs {
			b.WriteString(m.renderRow(i, msg))
			b.WriteByte('\n')
			if i == m.cursor && m.expanded {
				b.WriteString(m.renderExpanded(msg))
			}
		}
	}

	b.WriteString(m.rule())
	b.WriteByte('\n')
	b.WriteString(m.footer())
	return b.String()
}

func (m inboxWatchModel) rule() string {
	w := m.width
	if w <= 0 {
		w = 60
	}
	return inboxHelpStyle.Render(strings.Repeat("─", w))
}

// renderRow formats one list row. The cursor row gets a "▸" gutter + bold; the
// rest get a blank gutter so columns stay aligned.
func (m inboxWatchModel) renderRow(i int, msg store.Message) string {
	gutter := "  "
	if i == m.cursor {
		gutter = "▸ "
	}
	from := fmt.Sprintf("from=%-*s", inboxWatchFromColW, truncRunes(msg.FromAgent, inboxWatchFromColW))
	clock := hhmmss(msg.CreatedAt)
	line := fmt.Sprintf("%sid=%s  %s  %s  %s",
		gutter, msg.PublicID, clock, from, truncRunes(oneLine(msg.Body), m.previewWidth()))
	if i == m.cursor {
		return inboxCursorStyle.Render(line)
	}
	return line
}

// renderExpanded shows the full body of the selected message, indented and
// soft-wrapped to the terminal width.
func (m inboxWatchModel) renderExpanded(msg store.Message) string {
	w := m.width
	if w <= 0 {
		w = 60
	}
	wrapped := lipgloss.NewStyle().Width(w - 4).Render(msg.Body)
	var b strings.Builder
	for _, ln := range strings.Split(wrapped, "\n") {
		b.WriteString(inboxBodyStyle.Render("    " + ln))
		b.WriteByte('\n')
	}
	return b.String()
}

func (m inboxWatchModel) footer() string {
	if m.loadErr != nil {
		return inboxErrStyle.Render("⚠ " + m.loadErr.Error())
	}
	if m.status != "" {
		return inboxStatusStyle.Render(m.status)
	}
	if m.acted > 0 {
		return inboxStatusStyle.Render(fmt.Sprintf("%d drained this session", m.acted))
	}
	return inboxStatusStyle.Render("ready")
}

// previewWidth budgets the body-preview column from the terminal width, leaving
// room for the gutter + id + clock + from columns. Floors at inboxWatchMinPreview
// so a narrow terminal still shows something.
func (m inboxWatchModel) previewWidth() int {
	if m.width <= 0 {
		return tailPreviewLn // 60, matches tail's preview budget
	}
	// gutter(2) + "id="(3) + public_id(~4) + spaces + clock(8) + from col(~19)
	used := 2 + 3 + 8 + 8 + (len("from=") + inboxWatchFromColW) + 6
	w := m.width - used
	if w < inboxWatchMinPreview {
		return inboxWatchMinPreview
	}
	return w
}

// truncRunes truncates s to at most n runes, appending "…" when it cuts.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// summaryLine is the scrollback-preserving exit message, printed on the normal
// screen after the alt-screen TUI tears down.
func (m inboxWatchModel) summaryLine() string {
	return fmt.Sprintf("inbox --watch: %d drained this session, %d still queued for %q",
		m.acted, len(m.msgs), m.agent)
}

// --- runner -----------------------------------------------------------------

// runInboxWatch launches the interactive TUI. It owns the terminal (alt-screen),
// so it talks to os.Stdin/os.Stdout directly rather than the passed io.Writers
// (those carry the non-interactive JSON/error contract). SIGINT is handled by
// bubbletea (ctrl+c → quit); SIGTERM via the signal-derived context.
func runInboxWatch(ctx context.Context, s *store.Store, agent string, interval time.Duration, stdout, stderr io.Writer) int {
	if interval <= 0 {
		return writeJSONError(stdout, stderr, "--watch-interval must be positive", exitUsage)
	}
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		return writeJSONError(stdout, stderr,
			"inbox --watch requires an interactive terminal (stdout is not a TTY)", exitUsage)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := inboxWatchModel{store: s, ctx: ctx, agent: agent, interval: interval}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	final, err := p.Run()
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("inbox --watch: %v", err), exitInternal)
	}
	if fm, ok := final.(inboxWatchModel); ok {
		fmt.Fprintln(stdout, fm.summaryLine())
	}
	return exitOK
}
