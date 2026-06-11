package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// thread renders a reply-chain as a structural tree (#141). It is a thin
// sibling to `log`: both resolve the same single-chain via the shared
// store.GetThread seam (walk reply_to→root, then BFS all descendants),
// but `log` renders the chain flat-chronological (an audit view) while
// `thread` renders it as a parent→child tree (a navigation/diagnostic
// view). The walk is NOT duplicated here — only the tree rendering is new.

// threadGlyphs. The root carries ○; every other node's glyph maps from
// its delivery state. The `verified` column (#169) is now wired through
// (#230): a delivered-but-unverified node (`delivered_in_input_box`, the
// soft-fail where paste+Enter landed but the verify token never surfaced)
// renders with the `⚠` glyph from #141's example, distinct from a confirmed
// delivery's `✓`. The node carries the display-state (via displayState), so
// the glyph and the `state=` field agree. A pre-#169 row (verified=NULL)
// stays `✓` — the column can't claim a soft-fail it doesn't know about.
const (
	glyphRoot         = "○"
	glyphDelivered    = "✓"
	glyphUnverified   = "⚠"
	glyphFailed       = "✗"
	glyphInFlight     = "…"
	glyphAcknowledged = "·"
	glyphUnknown      = "?"
	threadPreviewLn   = 50
)

// threadNode is one node in the rendered reply-tree. It mirrors a
// store.Message plus its children so `--format json` emits the tree
// structure directly, rather than a flat list the caller must re-thread.
type threadNode struct {
	ID              string        `json:"id"`
	From            string        `json:"from"`
	To              string        `json:"to"`
	Kind            string        `json:"kind"`
	State           string        `json:"state"`
	NoReplyExpected bool          `json:"no_reply_expected,omitempty"`
	ReplyTo         string        `json:"reply_to,omitempty"`
	CreatedAt       string        `json:"created_at"`
	Body            string        `json:"body"`
	Children        []*threadNode `json:"children,omitempty"`
}

// buildThreadTree turns the flat chronological slice from store.GetThread
// into a parent→children tree. The root is the message whose reply_to is
// empty or points outside the returned set. GetThread guarantees a single
// connected chain rooted at one message; we detect the root defensively
// rather than assuming slice order, and surface a multi-root or no-root
// result as an error instead of rendering a misleading partial tree.
//
// Children retain ascending-id (chronological) order: msgs arrives id-asc
// from GetThread and we append in that order, so no per-level re-sort.
func buildThreadTree(msgs []store.Message) (*threadNode, error) {
	if len(msgs) == 0 {
		return nil, errors.New("empty thread")
	}
	nodes := make(map[string]*threadNode, len(msgs))
	inSet := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		inSet[m.PublicID] = true
	}
	for _, m := range msgs {
		n := &threadNode{
			ID:              m.PublicID,
			From:            m.FromAgent,
			To:              m.ToAgent,
			Kind:            string(m.Kind),
			State:           displayState(m),
			NoReplyExpected: m.NoReplyExpected,
			CreatedAt:       m.CreatedAt,
			Body:            m.Body,
		}
		if m.ReplyTo.Valid {
			n.ReplyTo = m.ReplyTo.String
		}
		nodes[m.PublicID] = n
	}

	var root *threadNode
	for _, m := range msgs {
		n := nodes[m.PublicID]
		parentID := ""
		if m.ReplyTo.Valid {
			parentID = m.ReplyTo.String
		}
		if parentID == "" || !inSet[parentID] {
			if root != nil {
				return nil, fmt.Errorf("thread has multiple roots (%s, %s) — not a single chain", root.ID, n.ID)
			}
			root = n
			continue
		}
		parent := nodes[parentID]
		parent.Children = append(parent.Children, n)
	}
	if root == nil {
		return nil, errors.New("thread has no root (cycle?)")
	}
	return root, nil
}

// stateGlyph maps a node's display-state to its tree glyph. The node's
// State is the displayState synthesis (#230), so the soft-fail
// `delivered_in_input_box` arrives here as its own string and maps to ⚠,
// distinct from a confirmed `delivered`'s ✓. Unknown states render as `?`
// (defensive — advisory-not-authoritative, per the substrate-class-of-claim
// convention) rather than being silently rolled up to a known glyph.
func stateGlyph(state string) string {
	switch state {
	case string(store.StateDelivered):
		return glyphDelivered
	case displayStateDeliveredInInputBox:
		return glyphUnverified
	case string(store.StateFailed):
		return glyphFailed
	case string(store.StateQueued), string(store.StateDelivering):
		return glyphInFlight
	case string(store.StateAcknowledged):
		return glyphAcknowledged
	default:
		return glyphUnknown
	}
}

// threadBodyPreview collapses a body to a single whitespace-normalized
// line capped at threadPreviewLn runes, for the per-node summary.
func threadBodyPreview(body string) string {
	oneLine := strings.Join(strings.Fields(body), " ")
	return shortBody(oneLine, threadPreviewLn)
}

// threadNodeDesc is the per-node description shared by root and children
// (glyph is prepended by the caller, since the root's glyph is ○ rather
// than a state glyph).
func threadNodeDesc(n *threadNode) string {
	desc := fmt.Sprintf("id=%s from=%s to=%s kind=%s state=%s",
		n.ID, n.From, n.To, n.Kind, n.State)
	if n.NoReplyExpected {
		desc += " 🔕"
	}
	if preview := threadBodyPreview(n.Body); preview != "" {
		desc += fmt.Sprintf("  (%s)", preview)
	}
	return desc
}

// renderThreadTree writes the ASCII reply-tree. The root line carries the
// ○ glyph and no connector; descendants use ├─ / └─ box-drawing with │
// continuation bars per depth.
func renderThreadTree(w io.Writer, root *threadNode) {
	fmt.Fprintf(w, "%s %s\n", glyphRoot, threadNodeDesc(root))
	renderThreadChildren(w, root.Children, "")
}

func renderThreadChildren(w io.Writer, children []*threadNode, prefix string) {
	for i, c := range children {
		last := i == len(children)-1
		branch, cont := "├─ ", "│  "
		if last {
			branch, cont = "└─ ", "   "
		}
		fmt.Fprintf(w, "%s%s%s %s\n", prefix, branch, stateGlyph(c.State), threadNodeDesc(c))
		renderThreadChildren(w, c.Children, prefix+cont)
	}
}

// runThreadCLI parses thread-subcommand flags, opens the store, and
// dispatches to runThreadWithStore.
//
// Usage: tmux-msg-claude thread <id> [--format tree|json]
func runThreadCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("thread", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	format := fs.String("format", "tree", "tree|json (tree is the default human view)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: %s thread <id> [--format tree|json]\n", active.BinaryName)
		return exitUsage
	}
	id := fs.Arg(0)

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	return runThreadWithStore(context.Background(), s, id, *format, stdout, stderr)
}

// runThreadWithStore is the pure-logic core: resolves the thread via the
// shared store.GetThread seam, builds the tree, and renders it. Designed
// to be table-tested.
func runThreadWithStore(ctx context.Context, s *store.Store, id, format string, stdout, stderr io.Writer) int {
	switch format {
	case "", "tree", "json":
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}

	msgs, err := s.GetThread(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("unknown id: %s", id), exitDataErr)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	root, err := buildThreadTree(msgs)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	if format == "json" {
		_ = writeJSONResult(stdout, root)
		return exitOK
	}
	renderThreadTree(stdout, root)
	return exitOK
}
