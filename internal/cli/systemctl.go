package cli

import (
	"context"
	"fmt"
	"os"
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

// startMailmanWouldMismatchSystemd reports whether starting the systemd-managed
// mailman would silently misroute against the caller's intent (#293).
//
// The systemd-managed mailman does NOT inherit the env of whoever ran register.
// Since #308 dropped the unit-file `Environment=CLAUDE_MSG_DB=...` directive, the
// mailman resolves the DB via the same `defaultDBLocation()` the binary computes
// on its own (XDG user-home). So a caller who set `$CLAUDE_MSG_DB=/sandbox.db`
// and ran `register --start-mailman=true` would write the agent row to the
// sandbox DB while the mailman polls the user-home default; the two never meet.
// Detection is structural: if the resolved DB path the caller is using differs
// from `defaultDBLocation()`, the next systemd-managed mailman start will
// silently mismatch.
//
// Returns the mismatch flag plus the caller's resolved DB path so the error
// message can name both sides of the divergence.
//
// Calibration note (Surveyor PR #302 review): detection compares the caller's
// resolved path against `defaultDBLocation()` (the XDG user-home default the
// binary computes on its own). Post-#308 this is exact for default installs —
// the mailman resolves the *same* `defaultDBLocation()` (no unit-file override
// to diverge from), so a caller without `--db`/`$CLAUDE_MSG_DB` matches and a
// caller with an override is correctly flagged. A bespoke install that re-adds
// an `Environment=CLAUDE_MSG_DB=...` override to a non-default path can still
// over/under-fire — the substrate-honest fix compares against the
// runtime-observed unit-file value, composing with #290. Default-install
// operators see correct behavior today.
func startMailmanWouldMismatchSystemd(resolvedDBPath string) (mismatched bool, callerDB string) {
	return resolvedDBPath != defaultDBLocation(), resolvedDBPath
}

// startMailmanMismatchError formats a caller-actionable error for the #293
// silent-mismatch case. Names both DB paths + recommends the
// foreground-`serve` recovery so the caller's env propagates to the mailman.
func startMailmanMismatchError(agentName, callerDB string) string {
	return fmt.Sprintf(
		"refusing to start a systemd-managed mailman with non-default "+
			"CLAUDE_MSG_DB (%s): systemd-managed mailmen do NOT inherit your "+
			"shell environment, and with no unit-file override (#308 dropped it) "+
			"they resolve the user-home default DB at %s — so the mailman would "+
			"silently poll that default instead of the sandbox DB this agent was "+
			"registered against. "+
			"To run the agent against a non-default DB, use "+
			"`--start-mailman=false` here and start the mailman as a "+
			"foreground subprocess that inherits your environment: "+
			"`%s serve --agent %s` (or `nohup %s serve --agent %s &`).",
		callerDB, defaultDBLocation(), active.BinaryName, agentName,
		active.BinaryName, agentName)
}

// startMailmanMissingEnv returns the names of env vars required by
// `systemctl --user` that are absent from the current process environment (#356).
// An empty result means the env is complete. systemctl --user connects via
// the D-Bus session bus, which requires both DBUS_SESSION_BUS_ADDRESS and
// XDG_RUNTIME_DIR; MCP child processes (e.g. codex) may not inherit these.
func startMailmanMissingEnv() []string {
	var missing []string
	for _, v := range []string{"DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR"} {
		if os.Getenv(v) == "" {
			missing = append(missing, v)
		}
	}
	return missing
}

// startMailmanEnvError formats a caller-actionable error for the env-incomplete
// case (#356). Names the missing vars and names two recovery paths: set the
// vars in the MCP wrapper env block, or use foreground serve.
func startMailmanEnvError(agentName string, missing []string) string {
	return fmt.Sprintf(
		"refusing to start systemd-managed mailman for %s: "+
			"%s not in process environment — `systemctl --user` requires "+
			"D-Bus session-bus access. Add the missing var(s) to the codex "+
			"MCP wrapper env block (docs/reference.md §Codex), or use "+
			"--start-mailman=false and start the mailman as a foreground "+
			"subprocess: `%s serve --agent %s` (or `nohup %s serve --agent %s &`).",
		agentName, strings.Join(missing, ", "),
		active.BinaryName, agentName, active.BinaryName, agentName)
}

// restartMailman runs `systemctl --user restart tmux-msg-<adapter>-mailman@NAME.service`.
//
// Used by `bootstrap` (step 4) after `enable`: an already-active mailman left
// from a prior install does NOT pick up a freshly-installed binary (the
// process keeps a handle on the now-replaced inode); `enable --now` is a
// no-op on already-active units, so without an explicit restart the mailman
// goes on running the deleted-inode binary indefinitely. doctor catches this
// as DIVERGENCE; the first-deploy-lane learning surface fired on alcatraz
// 2026-06-14 and named the gap. Restart kills the old PID + spawns a new one
// with the canonical binary, closing the substrate gap at its source.
//
// Returns nil on success; the output is included in the error on failure so
// the operator sees the systemd reason. systemctl restart starts the unit if
// it isn't running, so the call composes with enable to give a substrate-
// honest "ensure mailman is alive with the canonical binary" semantic.
func restartMailman(ctx context.Context, agent string) error {
	out, err := systemctlRun(ctx, "restart", mailmanUnit(agent))
	if err != nil {
		return fmt.Errorf("systemctl restart: %w: %s", err, strings.TrimSpace(string(out)))
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
