package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParseRemoteHost(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"alex@caymans", "alex@caymans", 7777, false},
		{"alex@localhost:9876", "alex@localhost", 9876, false},
		{"caymans:7777", "caymans", 7777, false},
		{"caymans", "caymans", 7777, false},
		{"ssh://alex@localhost:7777", "alex@localhost", 7777, false},
		{"ssh://alex@localhost", "alex@localhost", 7777, false},
		{"  alex@host  ", "alex@host", 7777, false},
		{"", "", 0, true},
	}
	for _, c := range cases {
		host, port, err := parseRemoteHost(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseRemoteHost(%q): want error, got host=%q port=%d", c.in, host, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRemoteHost(%q): unexpected error %v", c.in, err)
			continue
		}
		if host != c.wantHost || port != c.wantPort {
			t.Errorf("parseRemoteHost(%q) = (%q,%d), want (%q,%d)", c.in, host, port, c.wantHost, c.wantPort)
		}
	}
}

func TestResolveRemoteIdentity_EnvOverrideWins(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "Admin")
	// Session-name query must NOT be consulted when the explicit override is set.
	prev := remoteSessionName
	remoteSessionName = func(context.Context) (string, error) {
		t.Fatal("session-name query consulted despite $TMUX_AGENT_NAME override")
		return "", nil
	}
	t.Cleanup(func() { remoteSessionName = prev })

	got, err := resolveRemoteIdentity(context.Background())
	if err != nil || got != "Admin" {
		t.Fatalf("resolveRemoteIdentity = (%q,%v), want (Admin,nil)", got, err)
	}
}

func TestResolveRemoteIdentity_FallsBackToSessionName(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("CLAUDE_AGENT_NAME", "")
	prev := remoteSessionName
	remoteSessionName = func(context.Context) (string, error) { return "Caymans", nil }
	t.Cleanup(func() { remoteSessionName = prev })

	got, err := resolveRemoteIdentity(context.Background())
	if err != nil || got != "Caymans" {
		t.Fatalf("resolveRemoteIdentity = (%q,%v), want (Caymans,nil)", got, err)
	}
}

func TestValidateRemoteIdentity(t *testing.T) {
	valid := []string{"Admin", "bosun", "caymans-admin", "agent_1", "a.b.c", "X"}
	for _, v := range valid {
		if err := validateRemoteIdentity(v); err != nil {
			t.Errorf("validateRemoteIdentity(%q) = %v, want nil", v, err)
		}
	}
	// The injection / truncation vectors that cross the ssh remote shell.
	invalid := []string{
		"",                    // empty
		"Pilot tmux-tell#286", // whitespace → silent truncation at the space (#286 made this live)
		"a b",                 // any space
		"a\tb",                // tab
		"a;rm -rf /",          // command separator
		"a$(whoami)",          // command substitution
		"a`id`",               // backtick substitution
		"a|b",                 // pipe
		"a&b",                 // background
		"a>b",                 // redirect
		"$HOME",               // var expansion (also: starts non-alnum)
		"-rf",                 // leading hyphen → reads as a flag on the receiver
		"#abc",                // leading # → shell comment
		"/leading",            // leading slash
	}
	for _, v := range invalid {
		if err := validateRemoteIdentity(v); err == nil {
			t.Errorf("validateRemoteIdentity(%q) = nil, want rejection", v)
		}
	}
}

func TestResolveRemoteIdentity_RejectsUnsafe(t *testing.T) {
	// A multi-word session name (real since #286) must fail loud at resolution,
	// not silently truncate when forwarded.
	t.Setenv("TMUX_AGENT_NAME", "Pilot tmux-tell#286")
	if _, err := resolveRemoteIdentity(context.Background()); err == nil {
		t.Fatal("resolveRemoteIdentity: want rejection of whitespace identity, got nil")
	}
}

func TestResolveRemoteIdentity_FailsLoudWhenUnresolvable(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("CLAUDE_AGENT_NAME", "")
	prev := remoteSessionName
	remoteSessionName = func(context.Context) (string, error) { return "", errors.New("no tmux server") }
	t.Cleanup(func() { remoteSessionName = prev })

	if _, err := resolveRemoteIdentity(context.Background()); err == nil {
		t.Fatal("resolveRemoteIdentity: want fail-loud error, got nil")
	}
}

// stubSSH installs a fake ssh boundary that records the last invocation and
// returns canned output. Restored on cleanup.
func stubSSH(t *testing.T, out, errOut []byte, runErr error) *struct {
	host       string
	port       int
	remoteArgs []string
	stdin      []byte
	calls      int
} {
	t.Helper()
	rec := &struct {
		host       string
		port       int
		remoteArgs []string
		stdin      []byte
		calls      int
	}{}
	prev := sshRun
	sshRun = func(_ context.Context, host string, port int, remoteArgs []string, stdin io.Reader) ([]byte, []byte, error) {
		b, _ := io.ReadAll(stdin)
		rec.host, rec.port, rec.remoteArgs, rec.stdin = host, port, remoteArgs, b
		rec.calls++
		return out, errOut, runErr
	}
	t.Cleanup(func() { sshRun = prev })
	return rec
}

func TestForwardTool_RoutesThroughSSH(t *testing.T) {
	rec := stubSSH(t, []byte(`{"ok":true,"id":"abcd"}`+"\n"), nil, nil)
	cfg := remoteConfig{Host: "alex@localhost", Port: 7777, Identity: "Admin"}

	got, err := forwardTool(context.Background(), cfg, "tmux-tell.send", json.RawMessage(`{"to":"bosun","body":"hi"}`))
	if err != nil {
		t.Fatalf("forwardTool err: %v", err)
	}
	// Routed through SSH with the expected receiver argv.
	wantArgs := []string{active.BinaryName, remoteRecvSubcommand, "--tool", "tmux-tell.send", "--from", "Admin"}
	if strings.Join(rec.remoteArgs, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("remoteArgs = %v, want %v", rec.remoteArgs, wantArgs)
	}
	if rec.host != "alex@localhost" || rec.port != 7777 {
		t.Errorf("ssh target = %s:%d, want alex@localhost:7777", rec.host, rec.port)
	}
	// Tool args ride on stdin.
	if string(rec.stdin) != `{"to":"bosun","body":"hi"}` {
		t.Errorf("stdin = %q, want the JSON args", rec.stdin)
	}
	// Result is passed through opaquely (preserves the structured shape).
	raw, ok := got.(json.RawMessage)
	if !ok {
		t.Fatalf("result type = %T, want json.RawMessage", got)
	}
	if !bytes.Contains(raw, []byte(`"id":"abcd"`)) {
		t.Errorf("result = %s, want the receiver's JSON passed through", raw)
	}
}

func TestForwardTool_SSHFailureFailsLoud(t *testing.T) {
	stubSSH(t, nil, []byte("ssh: connect to host localhost port 7777: Connection refused"), errors.New("exit status 255"))
	cfg := remoteConfig{Host: "alex@localhost", Port: 7777, Identity: "Admin"}

	_, err := forwardTool(context.Background(), cfg, "tmux-tell.send", json.RawMessage(`{"to":"bosun","body":"hi"}`))
	if err == nil {
		t.Fatal("forwardTool: want fail-loud error on SSH failure, got nil")
	}
	if !strings.Contains(err.Error(), "Connection refused") {
		t.Errorf("error = %v, want it to surface the SSH stderr", err)
	}
}

func TestForwardTool_EmptyResultFailsLoud(t *testing.T) {
	stubSSH(t, []byte("   \n"), nil, nil)
	cfg := remoteConfig{Host: "h", Port: 7777, Identity: "Admin"}
	if _, err := forwardTool(context.Background(), cfg, "tmux-tell.ping", nil); err == nil {
		t.Fatal("forwardTool: want error on empty result, got nil")
	}
}

// TestNewRemoteMCPServer_AllToolsForward is the AC anchor: in remote mode EVERY
// MCP tool routes through the SSH forwarder (none touches a local store). It
// also smoke-tests the newMCPServer(nil) metadata harvest.
func TestNewRemoteMCPServer_AllToolsForward(t *testing.T) {
	rec := stubSSH(t, []byte(`{"ok":true}`), nil, nil)
	cfg := remoteConfig{Host: "h", Port: 7777, Identity: "Admin"}
	srv := newRemoteMCPServer(cfg)

	tools := srv.ToolList()
	if len(tools) < 20 {
		t.Fatalf("remote server registered %d tools, want the full surface (~22)", len(tools))
	}
	for _, tool := range tools {
		before := rec.calls
		if _, err := srv.Dispatch(context.Background(), tool.Name, json.RawMessage(`{}`)); err != nil {
			t.Errorf("dispatch %s: %v", tool.Name, err)
		}
		if rec.calls != before+1 {
			t.Errorf("tool %s did not route through the SSH forwarder", tool.Name)
		}
		// Each call must forward under its own tool name.
		if got := rec.remoteArgs[3]; got != tool.Name {
			t.Errorf("forwarded --tool = %s, want %s", got, tool.Name)
		}
	}
}

// TestRunRemoteMCP_EndToEnd drives a JSON-RPC tools/call through the remote MCP
// serve loop with a stubbed SSH boundary — the faithful "remote mode forwards"
// path including identity resolution.
func TestRunRemoteMCP_EndToEnd(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "Admin")
	rec := stubSSH(t, []byte(`{"ok":true,"id":"feed"}`), nil, nil)

	req := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "tmux-tell.send",
			"arguments": json.RawMessage(`{"to":"bosun","body":"hi"}`),
		},
	}
	reqLine, _ := json.Marshal(req)
	in := bytes.NewReader(append(reqLine, '\n'))
	var stdout, stderr bytes.Buffer

	exit := runRemoteMCP("alex@localhost:7777", in, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", exit, exitOK, stderr.String())
	}
	if rec.calls != 1 {
		t.Fatalf("ssh forwarder calls = %d, want 1; stderr=%s", rec.calls, stderr.String())
	}
	if rec.remoteArgs[3] != "tmux-tell.send" || rec.remoteArgs[5] != "Admin" {
		t.Errorf("forwarded argv = %v, want --tool tmux-tell.send --from Admin", rec.remoteArgs)
	}
	if !strings.Contains(stderr.String(), "remote mode host=alex@localhost port=7777 identity=Admin") {
		t.Errorf("startup log missing remote-mode line; stderr=%s", stderr.String())
	}
	// The receiver's JSON result reached the client.
	if !strings.Contains(stdout.String(), "feed") {
		t.Errorf("stdout missing forwarded result; stdout=%s", stdout.String())
	}
}

// TestRunMCPCLI_RemoteModeDetection is the AC anchor for the opt-in gesture:
// $TMUX_TELL_REMOTE_HOST set → runMCPCLI enters remote mode (forwards over SSH,
// never opens a local store). The negative (env unset → local) is covered by
// every other MCP test, which run without the env var.
func TestRunMCPCLI_RemoteModeDetection(t *testing.T) {
	t.Setenv("TMUX_TELL_REMOTE_HOST", "alex@localhost:7777")
	t.Setenv("TMUX_AGENT_NAME", "Admin")
	rec := stubSSH(t, []byte(`{"ok":true}`), nil, nil)

	req := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "tmux-tell.ping", "arguments": json.RawMessage(`{}`)},
	}
	reqLine, _ := json.Marshal(req)
	var stdout, stderr bytes.Buffer
	exit := runMCPCLI(nil, bytes.NewReader(append(reqLine, '\n')), &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", exit, exitOK, stderr.String())
	}
	if rec.calls != 1 {
		t.Fatalf("remote-mode not entered: ssh calls = %d, want 1; stderr=%s", rec.calls, stderr.String())
	}
}

func TestRunRemoteMCP_NoIdentityFailsLoud(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	t.Setenv("CLAUDE_AGENT_NAME", "")
	prev := remoteSessionName
	remoteSessionName = func(context.Context) (string, error) { return "", errors.New("no tmux") }
	t.Cleanup(func() { remoteSessionName = prev })

	var stdout, stderr bytes.Buffer
	exit := runRemoteMCP("alex@localhost:7777", strings.NewReader(""), &stdout, &stderr)
	if exit == exitOK {
		t.Fatalf("exit = %d, want non-OK when identity unresolvable", exit)
	}
	if !strings.Contains(stderr.String(), "cannot resolve bus identity") {
		t.Errorf("stderr = %s, want identity fail-loud message", stderr.String())
	}
}
