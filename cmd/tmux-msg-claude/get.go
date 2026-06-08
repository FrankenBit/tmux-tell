package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/config"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// errGetNotFound covers BOTH the "no message with that ID exists" and
// "the requester is not authorized to fetch it" cases. The two are
// deliberately indistinguishable so a requester with no business
// fetching a message can't probe for existence (per #111 access model).
var errGetNotFound = errors.New("no such message")

// errGetAmbiguous reports a short-prefix that matched multiple messages
// the requester IS authorized to see. The error body names the matching
// IDs so the operator can disambiguate by re-issuing with a longer
// prefix; access-filtered IDs only, never the full set, to preserve
// the no-existence-leak invariant.
var errGetAmbiguous = errors.New("ambiguous prefix")

// getResult is the structured response shape for both the CLI subcommand
// and the MCP tool. Includes the full message body — that's the whole
// point of the get-by-id surface (recovery for swallowed deliveries).
type getResult struct {
	OK          bool   `json:"ok"`
	ID          string `json:"id"`
	From        string `json:"from"`
	To          string `json:"to"`
	Body        string `json:"body"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
	DeliveredAt string `json:"delivered_at,omitempty"`
	ReplyTo     string `json:"reply_to,omitempty"`
}

// doGet is the shared lookup pipeline used by both the CLI subcommand
// and the MCP tool. Single source of truth for: identity resolution,
// short-prefix lookup with access-filtered disambiguation, and the
// not-found-vs-not-authorized response shape.
//
// Parameters:
//   - requester: the resolved agent name making the call (sender/recipient
//     check uses this).
//   - id: full public_id OR short prefix; the store's FindMessagesByPrefix
//     handles both via `LIKE prefix%`.
//   - cfg: the loaded config, used for the privileged-agents allowlist
//     check. May be nil — treated as "no privileged agents."
func doGet(ctx context.Context, s *store.Store, cfg *config.File,
	requester, id string,
) (*getResult, error) {
	if requester == "" {
		return nil, errors.New("requester required")
	}
	if id == "" {
		return nil, errors.New("id required")
	}

	candidates, err := s.FindMessagesByPrefix(ctx, id)
	if err != nil {
		return nil, err
	}

	// Access-filter the candidates BEFORE disambiguation so a requester
	// who has access to only one of N prefix-matching messages sees the
	// unambiguous case. The filtered-out messages are indistinguishable
	// from "no such message" — no existence leak.
	var accessible []store.Message
	for _, m := range candidates {
		if requester == m.FromAgent || requester == m.ToAgent || cfg.IsPrivileged(requester) {
			accessible = append(accessible, m)
		}
	}

	switch len(accessible) {
	case 0:
		return nil, fmt.Errorf("%w: %s", errGetNotFound, id)
	case 1:
		m := accessible[0]
		out := &getResult{
			OK:        true,
			ID:        m.PublicID,
			From:      m.FromAgent,
			To:        m.ToAgent,
			Body:      m.Body,
			Kind:      string(m.Kind),
			State:     displayState(m),
			CreatedAt: m.CreatedAt,
		}
		if m.DeliveredAt.Valid {
			out.DeliveredAt = m.DeliveredAt.String
		}
		if m.ReplyTo.Valid {
			out.ReplyTo = m.ReplyTo.String
		}
		return out, nil
	default:
		ids := make([]string, len(accessible))
		for i, m := range accessible {
			ids[i] = m.PublicID
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("%w: %d messages match %q: %s",
			errGetAmbiguous, len(accessible), id, strings.Join(ids, ", "))
	}
}

// runGetCLI parses the get-subcommand flags and dispatches.
//
// Usage: tmux-msg-claude get <id> [--from <name>] [--format text|json]
func runGetCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "requesting agent name (env: TMUX_AGENT_NAME)")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: tmux-msg-claude get <id> [flags]")
		return exitUsage
	}
	id := fs.Arg(0)

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	requester, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		fmt.Fprintf(stderr, "WARN config: %v — using defaults\n", cfgErr)
	}

	res, err := doGet(ctx, s, cfg, requester, id)
	if err != nil {
		switch {
		case errors.Is(err, errGetNotFound):
			return writeJSONError(stdout, stderr, err.Error(), exitUnavailable)
		case errors.Is(err, errGetAmbiguous):
			return writeJSONError(stdout, stderr, err.Error(), exitUsage)
		default:
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
	}

	switch *format {
	case "json":
		_ = writeJSONResult(stdout, res)
	case "text", "":
		fmt.Fprintf(stdout, "ID\t%s\n", res.ID)
		fmt.Fprintf(stdout, "FROM\t%s\n", res.From)
		fmt.Fprintf(stdout, "TO\t%s\n", res.To)
		fmt.Fprintf(stdout, "STATE\t%s\n", res.State)
		fmt.Fprintf(stdout, "KIND\t%s\n", res.Kind)
		fmt.Fprintf(stdout, "CREATED\t%s\n", res.CreatedAt)
		if res.DeliveredAt != "" {
			fmt.Fprintf(stdout, "DELIVERED\t%s\n", res.DeliveredAt)
		}
		if res.ReplyTo != "" {
			fmt.Fprintf(stdout, "REPLY_TO\t%s\n", res.ReplyTo)
		}
		fmt.Fprintln(stdout, "---")
		fmt.Fprintln(stdout, res.Body)
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
	return exitOK
}
