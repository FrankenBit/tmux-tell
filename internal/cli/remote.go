package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/mcp"
)

// Remote MCP mode (#310). When $TMUX_TELL_REMOTE_HOST is set, the MCP server
// runs on a remote host reached over a reverse-SSH tunnel and forwards every
// tool call back to the originating bus's tmux-tell-claude. The forwarding is
// universal — one mechanism for all tools — rather than a per-tool dispatcher:
// the remote server mirrors each tool's name+schema but replaces its handler
// with forwardTool, which shells `tmux-tell-claude __remote-mcp-recv` over SSH.
// The receiver re-runs the ACTUAL handler on the originating host, so the
// structured MCP result is preserved by construction (a per-tool map onto the
// text-emitting CLI subcommands would lose it). See docs/reference.md.

const (
	// defaultRemotePort is the reverse-tunnel port assumed when
	// $TMUX_TELL_REMOTE_HOST omits one. 7777 is the doc-recommended default:
	// memorable, unprivileged, unlikely to clash.
	defaultRemotePort = 7777

	// remoteRecvSubcommand is the hidden receiver dispatched on the originating
	// host. Double-underscore marks it substrate-internal — it is not an operator
	// surface and never appears in usageText.
	remoteRecvSubcommand = "__remote-mcp-recv"
)

// remoteConfig is the resolved remote-mode parameters, fixed for the MCP
// process lifetime.
type remoteConfig struct {
	Host     string // [user@]host reached over SSH
	Port     int    // reverse-tunnel port
	Identity string // bus name this session sends AS on the originating bus
}

// parseRemoteHost parses $TMUX_TELL_REMOTE_HOST in the form [user@]host[:port].
// An ssh:// scheme prefix is tolerated (the issue's original doc form). The
// port defaults to defaultRemotePort when omitted.
func parseRemoteHost(env string) (host string, port int, err error) {
	env = strings.TrimSpace(env)
	if env == "" {
		return "", 0, errors.New("TMUX_TELL_REMOTE_HOST is empty")
	}
	env = strings.TrimPrefix(env, "ssh://")
	port = defaultRemotePort
	// A trailing :<digits> is the port; a bare host (or user@host) keeps the
	// default. LastIndex keeps user@host intact when there's no port.
	if i := strings.LastIndex(env, ":"); i >= 0 {
		if p, perr := strconv.Atoi(env[i+1:]); perr == nil {
			port = p
			env = env[:i]
		}
	}
	if env == "" {
		return "", 0, errors.New("TMUX_TELL_REMOTE_HOST missing host")
	}
	return env, port, nil
}

// remoteSessionName queries the local tmux session name (`#S`). Claude and codex
// set it natively to the chamber name, so it doubles as the bus identity. Held
// in a var so tests can stub it.
var remoteSessionName = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveRemoteIdentity determines the bus name this remote session sends as.
// Precedence: an explicit $TMUX_AGENT_NAME (or legacy $CLAUDE_AGENT_NAME)
// override — for CLIs that don't auto-name their tmux session — then the tmux
// session name. Fails loud when neither resolves: a remote session with no
// resolvable identity must not silently forward as nobody.
func resolveRemoteIdentity(ctx context.Context) (string, error) {
	if v := strings.TrimSpace(os.Getenv("TMUX_AGENT_NAME")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_AGENT_NAME")); v != "" {
		return v, nil
	}
	if name, err := remoteSessionName(ctx); err == nil && name != "" {
		return name, nil
	}
	return "", errors.New(
		"remote MCP mode: cannot resolve bus identity — set $TMUX_AGENT_NAME to the name " +
			"this session sends AS on the originating bus, or run inside a tmux whose session " +
			"name is that identity")
}

// sshRun executes `ssh -p <port> <host> <remoteArgs...>` with stdin piped in,
// returning stdout, stderr, and the run error. Held in a var so tests can stub
// the SSH boundary (mirrors tmuxio's tmuxRun indirection).
var sshRun = func(ctx context.Context, host string, port int, remoteArgs []string, stdin io.Reader) (stdout, stderr []byte, err error) {
	args := append([]string{"-p", strconv.Itoa(port), host}, remoteArgs...)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = stdin
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// forwardTool forwards one MCP tool call to the originating bus over SSH. The
// tool name and resolved identity ride as flags; the tool's JSON arguments ride
// on stdin; the receiver's JSON result comes back on stdout and is passed
// through opaquely (re-marshalled at the MCP layer for the client).
func forwardTool(ctx context.Context, cfg remoteConfig, toolName string, args json.RawMessage) (any, error) {
	remoteArgs := []string{active.BinaryName, remoteRecvSubcommand, "--tool", toolName, "--from", cfg.Identity}
	body := args
	if len(bytes.TrimSpace(body)) == 0 {
		body = json.RawMessage("{}")
	}
	stdout, stderr, err := sshRun(ctx, cfg.Host, cfg.Port, remoteArgs, bytes.NewReader(body))
	if err != nil {
		// Fail loud: an unreachable tunnel or a non-zero receiver surfaces as a
		// tool error, never a silent empty success (#310 error-path decision).
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("remote forward of %s to %s:%d failed: %s", toolName, cfg.Host, cfg.Port, msg)
	}
	out := bytes.TrimSpace(stdout)
	if len(out) == 0 {
		return nil, fmt.Errorf("remote forward of %s to %s:%d returned no result", toolName, cfg.Host, cfg.Port)
	}
	return json.RawMessage(out), nil
}

// newRemoteMCPServer builds an MCP server whose every tool forwards to the
// originating bus. It mirrors the canonical tool metadata (names/descriptions/
// schemas) so remote clients see exactly the local tool surface, then swaps in
// forwardTool for each handler.
func newRemoteMCPServer(cfg remoteConfig) *mcp.Server {
	// Harvest the canonical tool metadata by constructing a local server purely
	// to read its tool list. Its handlers are never invoked here — every tool is
	// re-registered below with a forwarding handler — so the nil store is safe
	// (the discarded server's handler closures capture it but are never called).
	meta := newMCPServer(nil).ToolList()
	srv := mcp.NewServer("tmux-tell", "0.1.0")
	for _, t := range meta {
		name := t.Name // capture per iteration for the closure
		srv.RegisterTool(t.Name, t.Description, t.InputSchema,
			func(ctx context.Context, args json.RawMessage) (any, error) {
				return forwardTool(ctx, cfg, name, args)
			})
	}
	return srv
}

// runRemoteMCP is the remote-mode entry point reached from runMCPCLI when
// $TMUX_TELL_REMOTE_HOST is set. It resolves the tunnel target + identity (both
// fail loud) and serves the forwarding MCP server on stdio.
func runRemoteMCP(remoteHost string, stdin io.Reader, stdout, stderr io.Writer) int {
	host, port, err := parseRemoteHost(remoteHost)
	if err != nil {
		fmt.Fprintf(stderr, "mcp: remote mode: %v\n", err)
		return exitUsage
	}
	identityName, err := resolveRemoteIdentity(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "mcp: remote mode: %v\n", err)
		return exitUsage
	}
	cfg := remoteConfig{Host: host, Port: port, Identity: identityName}
	// Startup line mirrors #290's DB-path log so operators see at-a-glance that
	// this MCP is in remote mode and where it routes.
	fmt.Fprintf(stderr,
		"mcp: remote mode host=%s port=%d identity=%s (forwarding all tool calls over reverse-SSH; $TMUX_TELL_REMOTE_HOST set)\n",
		cfg.Host, cfg.Port, cfg.Identity)

	srv := newRemoteMCPServer(cfg)
	if err := srv.Serve(context.Background(), stdin, stdout); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(stderr, "mcp serve: %v\n", err)
		return exitInternal
	}
	return exitOK
}
