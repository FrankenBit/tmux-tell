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
	"regexp"
	"strconv"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/mcp"
)

// shellSafeIdentityRe matches a single shell-safe token: alphanumeric start,
// then alphanumerics / dot / underscore / hyphen. Deliberately STRICTER than
// control.ValidateForTask (#668), which permits spaces because its label is
// pasted into a pane as one unit. A remote identity instead crosses
// `ssh host cmd args…`, where the originating host's login shell re-splits and
// interprets the args — so a space would truncate the identity (silent
// mis-attribution) and a shell metacharacter would reach that shell.
var shellSafeIdentityRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateRemoteIdentity rejects an identity that isn't a single shell-safe
// token. cfg.Identity is the one caller-influenced argv element that crosses
// the originating host's login shell (the tool name is a registered constant
// and the JSON body rides stdin, which ssh does not re-parse), so validating it
// here is the single chokepoint that keeps the forward shell-safe.
func validateRemoteIdentity(name string) error {
	if name == "" {
		return errors.New("remote MCP mode: resolved agent identity is empty")
	}
	if !shellSafeIdentityRe.MatchString(name) {
		return fmt.Errorf(
			"remote MCP mode: agent identity %q is not a single shell-safe token "+
				"([A-Za-z0-9._-], starting alphanumeric) — it crosses to the originating host's "+
				"login shell via ssh, so whitespace would truncate it (silent mis-attribution) and "+
				"shell metacharacters would be interpreted; set $TMUX_AGENT_NAME to a single-token name",
			name)
	}
	return nil
}

// Remote MCP mode (#310). When $TMUX_TELL_REMOTE_HOST is set, the MCP server
// runs on a remote host reached over a reverse-SSH tunnel and forwards every
// tool call back to the originating host's tmux-tell-claude. The forwarding is
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
	Identity string // the name this session sends AS on the originating host
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
// set it natively to the chamber name, so it doubles as the agent identity. Held
// in a var so tests can stub it.
var remoteSessionName = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveRemoteIdentity determines the name this remote session sends as.
// Precedence: an explicit $TMUX_AGENT_NAME (or legacy $CLAUDE_AGENT_NAME)
// override — for CLIs that don't auto-name their tmux session — then the tmux
// session name. Fails loud when neither resolves: a remote session with no
// resolvable identity must not silently forward as nobody.
func resolveRemoteIdentity(ctx context.Context) (string, error) {
	name := strings.TrimSpace(os.Getenv("TMUX_AGENT_NAME"))
	if name == "" {
		name = strings.TrimSpace(os.Getenv("CLAUDE_AGENT_NAME"))
	}
	if name == "" {
		if n, err := remoteSessionName(ctx); err == nil {
			name = strings.TrimSpace(n)
		}
	}
	if name == "" {
		return "", errors.New(
			"remote MCP mode: cannot resolve agent identity — set $TMUX_AGENT_NAME to the name " +
				"this session sends AS on the originating host, or run inside a tmux whose session " +
				"name is that identity")
	}
	// The identity crosses the originating host's login shell via ssh — it must
	// be a single shell-safe token. Fail loud at startup rather than truncate or
	// inject at the first forwarded call.
	if err := validateRemoteIdentity(name); err != nil {
		return "", err
	}
	return name, nil
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

// forwardTool forwards one MCP tool call to the originating host over SSH. The
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
// originating host. It mirrors the canonical tool metadata (names/descriptions/
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
