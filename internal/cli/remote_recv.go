package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// runRemoteRecvCLI is the originating-host receiver for remote MCP mode (#310).
// A remote tmux-tell-claude MCP server (running with $TMUX_TELL_REMOTE_HOST set)
// forwards each tool call here over SSH: the tool name and the originating
// session's bus identity arrive as flags, the tool's JSON arguments arrive on
// stdin, and the handler's JSON result is written to stdout. It re-runs the
// SAME MCP handler the local server would, against the local (originating-host)
// store, so the structured result is preserved by construction.
//
// Hidden subcommand (double-underscore): substrate-internal plumbing, not an
// operator surface — it never appears in usageText.
func runRemoteRecvCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(remoteRecvSubcommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	tool := fs.String("tool", "", "MCP tool name to dispatch (e.g. tmux-tell.send)")
	from := fs.String("from", "", "bus identity the remote session sends as")
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *tool == "" || *from == "" {
		fmt.Fprintln(stderr, remoteRecvSubcommand+": --tool and --from are required")
		return exitUsage
	}

	rawArgs, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "%s: read args: %v\n", remoteRecvSubcommand, err)
		return exitInternal
	}
	if len(rawArgs) == 0 {
		rawArgs = []byte("{}")
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		fmt.Fprintf(stderr, "%s: open store: %v\n", remoteRecvSubcommand, err)
		return exitInternal
	}
	defer func() { _ = s.Close() }()

	result, err := dispatchRemoteRecv(context.Background(), s, *tool, *from, rawArgs)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", remoteRecvSubcommand, err)
		return exitInternal
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		fmt.Fprintf(stderr, "%s: encode result: %v\n", remoteRecvSubcommand, err)
		return exitInternal
	}
	return exitOK
}

// dispatchRemoteRecv runs one forwarded tool call against the local store with
// the remote session's bus identity injected as authoritative — this SSH
// session has no $TMUX_PANE on the originating host, so the identity must come
// from the caller, not from pane resolution. Extracted as the testable seam.
func dispatchRemoteRecv(ctx context.Context, s *store.Store, tool, from string, rawArgs []byte) (any, error) {
	if len(rawArgs) == 0 {
		rawArgs = []byte("{}")
	}
	ctx = withInjectedIdentity(ctx, from)
	return newMCPServer(s).Dispatch(ctx, tool, json.RawMessage(rawArgs))
}
