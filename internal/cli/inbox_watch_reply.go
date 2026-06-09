package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// inbox --watch reply action (#268, the `r` key) — open $EDITOR on a temp file,
// send the saved body as a reply threaded under the selected message.
//
// Composition reuses the send substrate: a reply is a normal InsertMessage with
// ReplyTo set to the original's public_id and ToAgent = the original's sender,
// caps enforced in-transaction (the same path `send --reply-to` takes). It does
// NOT replicate the send-CLI's thread-freshness / --strict / operator-resolution
// guards — those are sender-side ergonomics, not reply-from-my-own-inbox needs;
// the message I'm replying to is in front of me and fresh by construction.
//
// The editor seed uses a git-style **scissors** marker rather than per-line `#`
// comment stripping: the reply is everything ABOVE the scissors, preserved
// byte-for-byte. That matters on this bus specifically — replies routinely start
// a line with `#NNN` (issue refs), which naive `#`-comment stripping would eat.

// replyScissors delimits the editable reply (above) from the read-only context
// block (below). Everything from this line onward is discarded on save.
const replyScissors = "# ------------------------ >8 ------------------------"

// inboxReplyMsg is the result of an `r` reply round-trip (editor → send).
type inboxReplyMsg struct {
	replyToID string // public_id of the message replied to
	sentID    string // public_id of the sent reply (success)
	abandoned bool   // editor saved an empty reply area → nothing sent
	err       error
}

// startReply seeds a temp file, then hands the terminal to $EDITOR via
// tea.ExecProcess (which suspends the alt-screen and restores it on exit). The
// callback reads the buffer back and sends — returning an inboxReplyMsg.
func (m inboxWatchModel) startReply(orig store.Message) tea.Cmd {
	f, err := os.CreateTemp("", "tmux-msg-reply-*.md")
	if err != nil {
		return func() tea.Msg {
			return inboxReplyMsg{replyToID: orig.PublicID, err: fmt.Errorf("create reply buffer: %w", err)}
		}
	}
	path := f.Name()
	_, _ = f.WriteString(replyTemplate(orig, m.agent))
	_ = f.Close()

	argv := append(editorArgv(), path)
	c := exec.Command(argv[0], argv[1:]...) //nolint:gosec // editor from operator env, by design
	return tea.ExecProcess(c, func(runErr error) tea.Msg {
		return m.completeReply(orig, path, runErr)
	})
}

// completeReply reads the edited buffer, strips the context block, and sends the
// reply (unless abandoned). Always removes the temp file. Returns the result as
// an inboxReplyMsg for Update to surface in the footer.
func (m inboxWatchModel) completeReply(orig store.Message, path string, runErr error) inboxReplyMsg {
	defer func() { _ = os.Remove(path) }()
	res := inboxReplyMsg{replyToID: orig.PublicID}
	if runErr != nil {
		res.err = fmt.Errorf("editor exited abnormally: %w", runErr)
		return res
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		res.err = fmt.Errorf("read reply buffer: %w", err)
		return res
	}
	body := stripReplyTemplate(string(raw))
	if body == "" {
		res.abandoned = true
		return res
	}
	id, err := composeReply(m.ctx, m.store, m.agent, orig, body)
	if err != nil {
		res.err = err
		return res
	}
	res.sentID = id
	return res
}

// composeReply validates and inserts a reply threaded under orig. Caps are
// enforced in-transaction (mirrors `send`). Returns the new reply's public_id.
func composeReply(ctx context.Context, s *store.Store, fromAgent string, orig store.Message, body string) (string, error) {
	if len(body) > capBodyBytes {
		return "", fmt.Errorf("reply too large (%d > %d bytes)", len(body), capBodyBytes)
	}
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent:         fromAgent,
		ToAgent:           orig.FromAgent,
		ReplyTo:           orig.PublicID,
		Body:              body,
		MaxRecipientQueue: capRecipientQueue,
		MaxSenderBacklog:  capSenderBacklog,
	})
	if err != nil {
		return "", err
	}
	return res.PublicID, nil
}

// replyTemplate seeds the editor: a blank reply area above the scissors, then a
// read-only context block (instructions + quoted original) below it.
func replyTemplate(orig store.Message, fromAgent string) string {
	var b strings.Builder
	b.WriteString("\n\n") // reply area — the user types here
	b.WriteString(replyScissors + "\n")
	fmt.Fprintf(&b, "# Reply from %s to %s (re %s). Write your reply ABOVE the scissors line.\n",
		fromAgent, orig.FromAgent, orig.PublicID)
	b.WriteString("# Everything below the scissors is ignored. Save an empty reply to abandon.\n#\n")
	for _, ln := range strings.Split(strings.TrimRight(orig.Body, "\n"), "\n") {
		b.WriteString("# > " + ln + "\n")
	}
	return b.String()
}

// stripReplyTemplate returns the reply body: everything above the scissors
// marker, trimmed. Content above is preserved verbatim (no per-line stripping),
// so a reply line that starts with `#` survives.
func stripReplyTemplate(raw string) string {
	if i := strings.Index(raw, replyScissors); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

// editorArgv resolves the editor command (and any args) from the environment,
// falling back to vi. Split on whitespace so values like "code --wait" work.
func editorArgv() []string {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			if fields := strings.Fields(v); len(fields) > 0 {
				return fields
			}
		}
	}
	return []string{"vi"}
}
