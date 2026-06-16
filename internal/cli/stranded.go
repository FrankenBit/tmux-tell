package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// Stranded-draft bookmark recovery (#142).
//
// Storage (source-probed per AC1): stranded drafts are `messages` rows
// with kind=stranded_draft, self-addressed (from==to== the agent whose
// pane was flushed). The mailman's observe-gate archives operator-typed
// input as one of these rows before pasting over it (#92, serve.go's
// archiveStrandedDraft). The row body is the human-readable snapshot
// rendered by renderStrandedDraftBody (serve.go).
//
// These marker constants are the single source of truth for that body
// format — shared by the renderer (serve.go) and parseStrandedBody below,
// so the render/parse pair cannot drift (the AC5 recovery-hint line is
// emitted BEFORE the content marker so the trailing block stays the
// cleared content).
const (
	strandedHeaderLine    = ":bookmark: Stranded draft snapshot"
	strandedPanePrefix    = "  Pane: "
	strandedTriggerPrefix = "  Triggered by delivery of: "
	strandedContentMarker = "  Cleared content:"
	strandedBodyIndent    = "    "
	// strandedEmptyMarker is what indentForBody emits for empty content.
	strandedEmptyMarker = "(empty)"
)

// strandedTimeFormat matches the store's created_at ISO8601 layout
// (schema.sql: strftime('%Y-%m-%dT%H:%M:%fZ')), so a prune cutoff string
// compares lexicographically with stored timestamps.
const strandedTimeFormat = "2006-01-02T15:04:05.000Z"

// strandedBookmark is one parsed stranded-draft row for the list/show
// surfaces.
type strandedBookmark struct {
	ID          string `json:"id"`
	Pane        string `json:"pane"`
	TriggeredBy string `json:"triggered_by"`
	CreatedAt   string `json:"created_at"`
	Bytes       int    `json:"bytes"`   // byte length of the recovered content
	Content     string `json:"content"` // the cleared/recovered content
}

// parseStrandedBody is the inverse of renderStrandedDraftBody: it pulls
// the pane, trigger id, and cleared content back out of a stored
// stranded_draft body. ok=false when the body doesn't match the expected
// snapshot shape — surfaced as unparseable rather than mis-rendered.
func parseStrandedBody(body string) (pane, triggeredBy, content string, ok bool) {
	lines := strings.Split(body, "\n")
	if len(lines) == 0 || lines[0] != strandedHeaderLine {
		return "", "", "", false
	}
	contentStart := -1
	for i, ln := range lines {
		if ln == strandedContentMarker {
			contentStart = i + 1
			break
		}
		switch {
		case strings.HasPrefix(ln, strandedPanePrefix):
			pane = strings.TrimPrefix(ln, strandedPanePrefix)
		case strings.HasPrefix(ln, strandedTriggerPrefix):
			triggeredBy = strings.TrimPrefix(ln, strandedTriggerPrefix)
		}
	}
	if contentStart < 0 {
		return pane, triggeredBy, "", false
	}
	var b strings.Builder
	for i := contentStart; i < len(lines); i++ {
		if i > contentStart {
			b.WriteByte('\n')
		}
		b.WriteString(strings.TrimPrefix(lines[i], strandedBodyIndent))
	}
	content = b.String()
	if content == strandedEmptyMarker {
		content = ""
	}
	return pane, triggeredBy, content, true
}

// listStrandedBookmarks returns the parsed stranded-draft rows addressed
// to agent (self-addressed), newest first. Rows whose body doesn't parse
// are still listed (with empty pane/content) so a malformed bookmark is
// visible rather than silently dropped.
func listStrandedBookmarks(ctx context.Context, s *store.Store, agent string) ([]strandedBookmark, error) {
	msgs, err := s.ListMessages(ctx, store.ListFilter{
		ToAgent: agent,
		Kind:    store.KindStrandedDraft,
		Limit:   1000,
	})
	if err != nil {
		return nil, err
	}
	out := make([]strandedBookmark, 0, len(msgs))
	for _, m := range msgs {
		pane, trig, content, ok := parseStrandedBody(m.Body)
		bm := strandedBookmark{
			ID:          m.PublicID,
			Pane:        pane,
			TriggeredBy: trig,
			CreatedAt:   m.CreatedAt,
			Bytes:       len(content),
			Content:     content,
		}
		if !ok {
			bm.Pane = "(unparseable)"
		}
		out = append(out, bm)
	}
	return out, nil
}

// runStrandedCLI dispatches the stranded subcommands.
//
// Usage: tmux-tell-claude stranded <list|show|prune> [args]
func runStrandedCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "usage: %s stranded <list|show|prune> [args]\n", active.BinaryName)
		return exitUsage
	}
	switch args[0] {
	case "list":
		return runStrandedListCLI(args[1:], stdout, stderr)
	case "show":
		return runStrandedShowCLI(args[1:], stdout, stderr)
	case "prune":
		return runStrandedPruneCLI(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown stranded subcommand %q (want list|show|prune)\n", args[0])
		return exitUsage
	}
}

// --- list ---

func runStrandedListCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stranded list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	agent := fs.String("agent", "", "agent whose bookmarks to list (default: this session's identity)")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()
	ctx := context.Background()
	who, err := resolveStrandedAgent(ctx, s, *agent)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	return runStrandedListWithStore(ctx, s, who, *format, stdout, stderr)
}

func runStrandedListWithStore(ctx context.Context, s *store.Store, agent, format string, stdout, stderr io.Writer) int {
	switch format {
	case "", "text", "json":
	default:
		return writeJSONError(stdout, stderr, fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
	bms, err := listStrandedBookmarks(ctx, s, agent)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if format == "json" {
		// List view omits the full content (use `show` for that); emit a
		// content-free projection so a large paste doesn't bloat `list`.
		type listRow struct {
			ID          string `json:"id"`
			Pane        string `json:"pane"`
			TriggeredBy string `json:"triggered_by"`
			CreatedAt   string `json:"created_at"`
			Bytes       int    `json:"bytes"`
		}
		rows := make([]listRow, 0, len(bms))
		for _, b := range bms {
			rows = append(rows, listRow{b.ID, b.Pane, b.TriggeredBy, b.CreatedAt, b.Bytes})
		}
		_ = writeJSONResult(stdout, rows)
		return exitOK
	}
	if len(bms) == 0 {
		fmt.Fprintf(stdout, "no stranded-draft bookmarks for %s\n", agent)
		return exitOK
	}
	rows := make([][]string, 0, len(bms))
	for _, b := range bms {
		rows = append(rows, []string{
			b.ID, b.Pane, b.CreatedAt, fmt.Sprintf("%d", b.Bytes), b.TriggeredBy,
		})
	}
	renderTextTable(stdout, []string{"ID", "PANE", "CREATED", "BYTES", "TRIGGERED_BY"}, rows)
	return exitOK
}

// --- show ---

func runStrandedShowCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stranded show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	outFile := fs.String("o", "", "write the recovered content to this file instead of stdout")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: %s stranded show <bookmark-id> [-o file]\n", active.BinaryName)
		return exitUsage
	}
	id := fs.Arg(0)
	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()
	return runStrandedShowWithStore(context.Background(), s, id, *outFile, stdout, stderr)
}

func runStrandedShowWithStore(ctx context.Context, s *store.Store, id, outFile string, stdout, stderr io.Writer) int {
	m, err := s.GetMessage(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return writeJSONError(stdout, stderr, fmt.Sprintf("unknown id: %s", id), exitDataErr)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if m.Kind != store.KindStrandedDraft {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("%s is kind=%s, not a stranded-draft bookmark", id, m.Kind), exitDataErr)
	}
	_, _, content, ok := parseStrandedBody(m.Body)
	if !ok {
		// Defensive: a stranded_draft row whose body doesn't match the
		// snapshot format. Fall back to the raw body rather than claim
		// content we couldn't extract.
		fmt.Fprintf(stderr, "warning: bookmark %s body did not parse; showing raw body\n", id)
		content = m.Body
	}
	if outFile != "" {
		if err := os.WriteFile(outFile, []byte(content), 0o644); err != nil {
			return writeJSONError(stdout, stderr, fmt.Sprintf("write %s: %v", outFile, err), exitInternal)
		}
		fmt.Fprintf(stdout, "wrote %d bytes to %s\n", len(content), outFile)
		return exitOK
	}
	fmt.Fprint(stdout, content)
	if !strings.HasSuffix(content, "\n") {
		fmt.Fprintln(stdout)
	}
	return exitOK
}

// --- prune ---

func runStrandedPruneCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stranded prune", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	agent := fs.String("agent", "", "agent whose bookmarks to prune (default: this session's identity)")
	olderThan := fs.String("older-than", "", "remove bookmarks older than this (e.g. 7d, 24h, 90m) — required")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()
	ctx := context.Background()
	who, err := resolveStrandedAgent(ctx, s, *agent)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	return runStrandedPruneWithStore(ctx, s, who, *olderThan, time.Now(), stdout, stderr)
}

func runStrandedPruneWithStore(ctx context.Context, s *store.Store, agent, olderThan string, now time.Time, stdout, stderr io.Writer) int {
	// --older-than is required: pruning is destructive, so never default to
	// a silent window that would delete bookmarks the operator didn't name.
	if strings.TrimSpace(olderThan) == "" {
		return writeJSONError(stdout, stderr, "--older-than is required (e.g. --older-than 7d)", exitUsage)
	}
	w, err := parseWindow(olderThan, now)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	if w.All {
		return writeJSONError(stdout, stderr, "--older-than 'all' is not meaningful for prune; give a duration", exitUsage)
	}
	cutoff := w.Since.UTC().Format(strandedTimeFormat)
	n, err := s.DeleteStrandedDraftsBefore(ctx, agent, cutoff)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	fmt.Fprintf(stdout, "pruned %d stranded-draft bookmark(s) for %s older than %s\n", n, agent, olderThan)
	return exitOK
}

// resolveStrandedAgent resolves the agent whose bookmarks a list/prune
// operates on: the explicit --agent override, else this session's
// identity. Stranded drafts are self-addressed, so this is the to_agent.
func resolveStrandedAgent(ctx context.Context, s *store.Store, override string) (string, error) {
	name, _, err := identity.Resolve(ctx, s, override)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", errors.New("cannot resolve agent: pass --agent, set $TMUX_AGENT_NAME, or register this pane")
	}
	return name, nil
}
