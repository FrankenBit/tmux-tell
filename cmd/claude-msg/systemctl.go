package main

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

// startMailman runs `systemctl --user enable --now claude-mailman@NAME.service`.
// Returns nil on success; the output is included in the error on failure so
// the operator sees the systemd reason.
func startMailman(ctx context.Context, agent string) error {
	out, err := systemctlRun(ctx, "enable", "--now", mailmanUnit(agent))
	if err != nil {
		return fmt.Errorf("systemctl enable: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stopMailman runs `systemctl --user disable --now claude-mailman@NAME.service`.
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

func mailmanUnit(agent string) string {
	return "claude-mailman@" + agent + ".service"
}
