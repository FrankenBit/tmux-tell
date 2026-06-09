package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// systemctlRun is the indirection for shelling out to `systemctl --user`.
// Tests swap it via setSystemctlRunner.
var systemctlRun = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	return cmd.CombinedOutput()
}

// setSystemctlRunner installs a test double and returns the previous
// runner so tests can restore it.
func setSystemctlRunner(r func(ctx context.Context, args ...string) ([]byte, error)) func(ctx context.Context, args ...string) ([]byte, error) {
	prev := systemctlRun
	systemctlRun = r
	return prev
}

// startMailman runs `systemctl --user enable --now tmux-msg-claude-mailman@NAME.service`.
// Returns nil on success; the output is included in the error on failure so
// the operator sees the systemd reason.
func startMailman(ctx context.Context, agent string) error {
	out, err := systemctlRun(ctx, "enable", "--now", mailmanUnit(agent))
	if err != nil {
		return fmt.Errorf("systemctl enable: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stopMailman runs `systemctl --user disable --now tmux-msg-claude-mailman@NAME.service`.
// Treats "not-loaded" output as success so the call is idempotent.
func stopMailman(ctx context.Context, agent string) error {
	out, err := systemctlRun(ctx, "disable", "--now", mailmanUnit(agent))
	if err != nil {
		// "Unit … not loaded" / "does not exist" / "no such file" all map
		// to idempotent success — the operator asked us to stop something
		// that already wasn't running.
		trimmed := strings.TrimSpace(string(out))
		low := strings.ToLower(trimmed)
		for _, harmless := range []string{
			"not loaded", "does not exist", "no such file",
		} {
			if strings.Contains(low, harmless) {
				return nil
			}
		}
		return fmt.Errorf("systemctl disable: %w: %s", err, trimmed)
	}
	return nil
}

// mailmanActive reports whether the recipient's mailman unit is active, via
// `systemctl --user is-active`. is-active prints "active" + exits 0 only when
// the unit is running; any other state ("inactive"/"failed"/unknown) or a
// non-zero exit reads as not-running. Used by the send-time recipient-status
// probe (#152) — best-effort, so a systemctl error is treated as "not active"
// rather than surfaced.
func mailmanActive(ctx context.Context, agent string) bool {
	out, _ := systemctlRun(ctx, "is-active", mailmanUnit(agent))
	return strings.TrimSpace(string(out)) == "active"
}

// mailmanUnit is the per-adapter systemd template instance for an agent (#177).
// The template renamed from claude-mailman@ to tmux-msg-claude-mailman@ when the
// binary became tmux-msg-claude; install.sh drops a claude-mailman@ → this
// symlink for the deprecation cycle, so a pre-rename `systemctl … claude-mailman@X`
// still resolves, but new invocations target the canonical name. The prefix is
// the adapter's binary name (#248): tmux-msg-codex installs a parallel
// tmux-msg-codex-mailman@ template, so each adapter targets its own daemon.
func mailmanUnit(agent string) string {
	return active.BinaryName + "-mailman@" + agent + ".service"
}
