package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// buildCodexBinary builds the tmux-tell-codex binary to the test's temp dir and
// returns its path. Mirrors internal/store's cross-process probe pattern: the
// only way to exercise the *native CLI invocation* axis (AC#4) is to build and
// exec the real binary.
func buildCodexBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "tmux-tell-codex")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build tmux-tell-codex: %v\n%s", err, b)
	}
	return out
}

// TestCodexAdapter_EndToEnd exercises the tmux-tell-codex binary (the #248 second
// adapter) through its native CLI: seed two agents, send alice→bob, list bob's
// inbox, then present the queued message via the hook-context helper and confirm
// the additionalContext carries it with Codex's hook schema.
//
// This is the load-bearing validation of ADR-0009's boundary: the codex binary
// is a thin wrapper over the same internal/cli the claude binary uses, so a
// genuine second adapter needs ZERO substrate changes. Codex's hook output schema
// (hookSpecificOutput.hookEventName + additionalContext) matches Claude's, so the
// #249 hook-context helper presents codex-bound messages unchanged.
func TestCodexAdapter_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds + execs the codex binary; skipped in -short mode")
	}
	bin := buildCodexBinary(t)
	db := filepath.Join(t.TempDir(), "codex-e2e.db")

	// Seed two agents directly in the store — avoids register's systemd side
	// effects (mailman enablement). bob delivers via hook-context per #248 (B).
	ctx := context.Background()
	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := s.UpsertAgent(ctx, "bob", "%2"); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	if err := s.SetDeliveryMode(ctx, "bob", store.DeliveryModeHookContext); err != nil {
		t.Fatalf("set bob hook-context: %v", err)
	}
	_ = s.Close()

	run := func(stdin string, args ...string) (stdout, stderr string, code int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "CLAUDE_MSG_DB="+db)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		runErr := cmd.Run()
		if ee, ok := runErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if runErr != nil {
			t.Fatalf("exec %v: %v", args, runErr)
		}
		return so.String(), se.String(), code
	}

	// send alice → bob (native CLI invocation)
	if so, se, code := run("", "send", "--from", "alice", "--to", "bob", "--body", "hello from codex"); code != 0 {
		t.Fatalf("send exit %d: stdout=%s stderr=%s", code, so, se)
	}

	// inbox bob → message is queued (hook-context agent has no mailman, so it
	// stays queued until the hook-helper claims it)
	if so, se, code := run("", "inbox", "--format", "json", "bob"); code != 0 || !strings.Contains(so, "hello from codex") {
		t.Fatalf("inbox exit %d: missing queued message: stdout=%s stderr=%s", code, so, se)
	}

	// hook-context bob → presents the message as additionalContext, marking it
	// delivered (ADR-0009 3b). The stdin payload carries Codex's event name.
	so, se, code := run(`{"hook_event_name":"UserPromptSubmit"}`, "hook-context", "--from", "bob")
	if code != 0 {
		t.Fatalf("hook-context exit %d: stdout=%s stderr=%s", code, so, se)
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(so), &out); err != nil {
		t.Fatalf("hook-context output not valid JSON: %v\n%s", err, so)
	}
	if !strings.Contains(out.HookSpecificOutput.AdditionalContext, "hello from codex") {
		t.Fatalf("additionalContext missing the message body: %q", out.HookSpecificOutput.AdditionalContext)
	}
	if got := out.HookSpecificOutput.HookEventName; got != "UserPromptSubmit" {
		t.Fatalf("hookEventName = %q, want UserPromptSubmit (echoed from stdin)", got)
	}

	// after presentation, the message is delivered — bob's queued inbox is empty.
	if so, se, code := run("", "inbox", "--format", "json", "bob"); code != 0 || strings.Contains(so, "hello from codex") {
		t.Fatalf("post-hook inbox still shows the message: exit %d stdout=%s stderr=%s", code, so, se)
	}
}
