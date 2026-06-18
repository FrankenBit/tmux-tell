package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"github.com/BurntSushi/toml"
)

// codexHookCommand is the command string written to both hook blocks.
const codexHookCommand = "tmux-tell-codex hook-context"

// codexMCPServerProbe captures the fields of a single mcp_servers entry
// that codex-install cares about (other fields pass through untouched).
type codexMCPServerProbe struct {
	Command string            `toml:"command"`
	Env     map[string]string `toml:"env"`
}

// codexConfigProbe is the partial shape of ~/.codex/config.toml used for
// the existence check. The TOML decoder ignores all unknown fields
// (non-strict), so other operator-managed entries are preserved.
type codexConfigProbe struct {
	Hooks struct {
		UserPromptSubmit *struct {
			Command string `toml:"command"`
		} `toml:"UserPromptSubmit"`
		SessionStart *struct {
			Command string `toml:"command"`
		} `toml:"SessionStart"`
	} `toml:"hooks"`
	McpServers map[string]codexMCPServerProbe `toml:"mcp_servers"`
}

// codexInstallResult is the structured return shape for `codex-install`.
type codexInstallResult struct {
	OK           bool     `json:"ok"`
	Agent        string   `json:"agent"`
	Registered   bool     `json:"registered,omitempty"`
	HooksWritten bool     `json:"hooks_written,omitempty"`
	EnvWritten   bool     `json:"env_written,omitempty"`
	AlreadyOK    bool     `json:"already_ok,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
	Notices      []string `json:"notices,omitempty"`
}

// legacyMcpRenames lists codex `[mcp_servers.<old>]` section renames that
// codex-install migrates in-place at install time (#486, the codex-config half
// of the #478 substrate rename). A table so the next substrate rename appends a
// row rather than re-shaping the call site — the same list-shaped seam as the
// binary alias chain (#440 Phase 3). Today it carries only tmux-msg → tmux-tell.
var legacyMcpRenames = []struct{ oldName, newName string }{
	{"tmux-msg", "tmux-tell"},
}

// migrateLegacyMcpSection rewrites a pre-rename `[mcp_servers.<old>]` section to
// `<new>` in place, operating on the raw file text (BurntSushi/toml does NOT
// round-trip — re-encoding a decoded config would strip operator comments and
// reorder keys, so we surgically rewrite only the lines that name the old
// server/binary and leave every other byte — `env`, `args`, `approval_mode`,
// comments, key order — untouched). Returns:
//
//   - "renamed": the `<old>` section(s) were rewritten to `<new>` (the common
//     pre-rename case — no `<new>` section existed yet). See
//     renameLegacyMcpSections for exactly which bytes change.
//   - "removed": a `<new>` section already exists (the post-#478 dup case), so
//     the orphaned `<old>` section(s) are dropped entirely — renaming would
//     produce a duplicate `[mcp_servers.<new>]` and a stale section keeps
//     mis-advertising tools under the old wire prefix.
//   - "": no `<old>` section present — no-op (so a re-run is idempotent).
//
// Only the bare-key header form the writer emits (`[mcp_servers.tmux-msg]`,
// `[mcp_servers.tmux-msg.env]`, `[mcp_servers.tmux-msg.tools."tmux-msg.<tool>"]`)
// is matched on `mcp_servers.<old>`; `tmux-msg`/`tmux-tell` are valid TOML bare
// keys so the writer never quotes the server segment. A hand-quoted operator
// variant of the server segment is out of scope (left as-is).
func migrateLegacyMcpSection(content *string, oldName, newName string) string {
	oldHdr := regexp.MustCompile(
		`(?m)^[ \t]*\[mcp_servers\.` + regexp.QuoteMeta(oldName) + `(\.[^\]]*)?\][ \t]*$`)
	if !oldHdr.MatchString(*content) {
		return ""
	}
	newHdr := regexp.MustCompile(
		`(?m)^[ \t]*\[mcp_servers\.` + regexp.QuoteMeta(newName) + `(\.[^\]]*)?\][ \t]*$`)
	if newHdr.MatchString(*content) {
		*content = removeTomlSections(*content, oldHdr)
		return "removed"
	}
	*content = renameLegacyMcpSections(*content, oldHdr, oldName, newName)
	return "renamed"
}

// renameLegacyMcpSections advances a pre-rename `[mcp_servers.<old>…]` section to
// `<new>` (#486). For each matched header line it rewrites EVERY `<old>`
// occurrence — both the section-path segment (`mcp_servers.<old>`) AND any inner
// per-tool key (`."<old>.<tool>"`) — because codex keys a tool's `approval_mode`
// sub-table by the fully-qualified tool name; leaving the inner key at `<old>`
// while the live tool is now `<new>.<tool>` would silently de-link the operator's
// per-tool approval setting. Within each migrated section's body it also rewrites
// a `command` value naming the pre-rename `<old>-<adapter>` binary to
// `<new>-<adapter>`. Lines outside a migrated section, and non-command body lines,
// pass through byte-identical.
func renameLegacyMcpSections(content string, oldHdr *regexp.Regexp, oldName, newName string) string {
	anyHeader := regexp.MustCompile(`^[ \t]*\[`)
	cmdLine := regexp.MustCompile(`^[ \t]*command[ \t]*=`)
	lines := strings.Split(content, "\n")
	inSection := false
	for i, line := range lines {
		switch {
		case oldHdr.MatchString(line):
			lines[i] = strings.ReplaceAll(line, oldName, newName)
			inSection = true
		case anyHeader.MatchString(line):
			inSection = false // a non-legacy section begins
		case inSection && cmdLine.MatchString(line) && strings.Contains(line, oldName+"-"):
			lines[i] = strings.ReplaceAll(line, oldName+"-", newName+"-")
		}
	}
	return strings.Join(lines, "\n")
}

// removeTomlSections drops every section whose header matches hdr, along with
// each section's body lines (everything up to the next `[`-header or EOF). Used
// for the dup-case migration where the legacy section is orphaned.
func removeTomlSections(content string, hdr *regexp.Regexp) string {
	anyHeader := regexp.MustCompile(`^[ \t]*\[`)
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		switch {
		case hdr.MatchString(line):
			skipping = true // start (or continue into another) removed section
			continue
		case skipping && anyHeader.MatchString(line):
			skipping = false // a different section begins — keep it
		case skipping:
			continue // body line of the removed section
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// runCodexInstallCLI implements `tmux-tell-codex codex-install` — the
// codex-adapter bootstrap called by install.sh after binary + systemd
// template land (#384).
//
// Unlike the claude bootstrap (mailmen + MCP refresh), the codex bootstrap
// writes codex config entries — hook blocks + MCP env block — since Codex
// agents deliver via hook-context, not pane paste. Three steps:
//
//  1. Discover: populate agent pane IDs from current tmux state.
//  2. Register: set delivery_mode=hook-context for the named agent.
//  3. Config: merge hook blocks + TMUX_AGENT_NAME env into ~/.codex/config.toml.
//     Idempotent: parse+check+skip-or-append, not blind append.
//
// Prints post-install instructions naming Codex's hook-trust prompt.
func runCodexInstallCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("codex-install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentName := fs.String("agent", "", "agent name to register as hook-context (required)")
	codexConfigPath := fs.String("codex-config", "",
		"path to codex config file (default: $HOME/.codex/config.toml)")
	dryRun := fs.Bool("dry-run", false, "print what would change without writing")
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	format := fs.String("format", "text", "text|json")
	skipDiscover := fs.Bool("skip-discover", false,
		"skip the tmux discover step (for tests that pre-seed the agents table)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	if *agentName == "" {
		return writeJSONError(stdout, stderr, "--agent required", exitUsage)
	}

	// Resolve codex config path.
	resolvedConfig := *codexConfigPath
	if resolvedConfig == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return writeJSONError(stdout, stderr,
				"HOME not set; pass --codex-config to specify the config path",
				exitUsage)
		}
		resolvedConfig = filepath.Join(home, ".codex", "config.toml")
	}

	ctx := context.Background()
	result := codexInstallResult{Agent: *agentName}

	resolvedDB := resolveDBPath(*dbPath)
	s, err := store.Open(resolvedDB)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store at %s: %v", resolvedDB, err),
			exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	// Step 1: discover — populate/update pane IDs from current tmux state.
	if !*skipDiscover {
		if rc := runDiscoverWithStore(ctx, s, discover.New(), false, false, "json", io.Discard, stderr); rc != exitOK {
			return writeJSONError(stdout, stderr, "discover failed; see stderr", rc)
		}
	}

	// Step 2: set delivery_mode=hook-context.
	if !*dryRun {
		a, getErr := s.GetAgent(ctx, *agentName)
		if getErr != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("agent %q not found after discover — ensure the codex pane is in "+
					"the current tmux session with TMUX_AGENT_NAME=%s set, or run "+
					"`%s register --name %s --delivery-mode=hook-context` first",
					*agentName, *agentName, active.BinaryName, *agentName),
				exitDataErr)
		}
		if a.DeliveryMode != store.DeliveryModeHookContext {
			if setErr := s.SetDeliveryMode(ctx, *agentName, store.DeliveryModeHookContext); setErr != nil {
				return writeJSONError(stdout, stderr,
					fmt.Sprintf("set hook-context delivery mode for %q: %v", *agentName, setErr),
					exitInternal)
			}
		}
		result.Registered = true
	}

	// Step 3: merge hook blocks + env into codex config.
	hooksWritten, envWritten, alreadyOK, warnings, notices, writeErr := mergeCodexConfig(
		resolvedConfig, *agentName, *dryRun,
	)
	if writeErr != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("write codex config %s: %v", resolvedConfig, writeErr),
			exitInternal)
	}
	result.HooksWritten = hooksWritten
	result.EnvWritten = envWritten
	result.AlreadyOK = alreadyOK
	result.Warnings = warnings
	result.Notices = notices
	result.OK = true

	return emitCodexInstallResult(stdout, stderr, result, *format, resolvedConfig, *agentName, *dryRun)
}

// mergeCodexConfig reads the existing codex config, probes which of the
// three needed blocks are already correctly present, and atomically appends
// the missing ones.
//
// present-but-wrong values surface as warnings and are not modified (the
// operator may have intentionally configured them differently). The caller
// decides whether to abort or proceed.
func mergeCodexConfig(configPath, agentName string, dryRun bool) (hooksWritten, envWritten, alreadyOK bool, warnings, notices []string, err error) {
	existing, readErr := os.ReadFile(configPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, false, false, nil, nil, fmt.Errorf("read: %w", readErr)
	}
	fileExists := readErr == nil
	content := string(existing)

	// Migrate any pre-rename [mcp_servers.<old>] section IN PLACE before the
	// probe (#486), so the existence check + atomic write below operate on the
	// canonical section name. Text-surgical (see migrateLegacyMcpSection) so
	// operator customizations survive byte-identical. `migrated` gates the write
	// so a migrate-only change (no other appends) is not silently dropped.
	migrated := false
	for _, r := range legacyMcpRenames {
		switch migrateLegacyMcpSection(&content, r.oldName, r.newName) {
		case "renamed":
			migrated = true
			notices = append(notices, fmt.Sprintf(
				"migrating_legacy_codex_mcp_section old=mcp_servers.%s new=mcp_servers.%s path=%s",
				r.oldName, r.newName, configPath))
		case "removed":
			migrated = true
			notices = append(notices, fmt.Sprintf(
				"removing_orphaned_codex_mcp_section old_section=[mcp_servers.%s] "+
					"reason=\"post-rename coexistence with canonical [mcp_servers.%s]\"",
				r.oldName, r.newName))
		}
	}

	var probe codexConfigProbe
	if fileExists && len(content) > 0 {
		if _, decErr := toml.Decode(content, &probe); decErr != nil {
			return false, false, false, nil, nil, fmt.Errorf("parse TOML: %w", decErr)
		}
	}

	// Residual stale-binary WARN (#486). A migrating `[mcp_servers.tmux-msg…]`
	// section has its `command` rewritten in place by renameLegacyMcpSections
	// above, so this only fires for the no-migration case: a config already at the
	// canonical `[mcp_servers.tmux-tell]` section name whose `command` STILL names
	// the pre-rename `tmux-msg-*` binary. That residue is the operator's to fix
	// (we don't rewrite a command outside a section we're actively migrating).
	if probe.McpServers != nil {
		if entry, ok := probe.McpServers["tmux-tell"]; ok &&
			entry.Command != "" && strings.Contains(entry.Command, "tmux-msg-") {
			warnings = append(warnings, fmt.Sprintf(
				"mcp_servers.tmux-tell.command is %q (still names a pre-rename tmux-msg-* binary); "+
					"skipped — update manually to the tmux-tell-* binary", entry.Command))
		}
	}

	var toAppend []string

	switch {
	case probe.Hooks.UserPromptSubmit == nil:
		toAppend = append(toAppend, fmt.Sprintf("[hooks.UserPromptSubmit]\ncommand = %q", codexHookCommand))
		hooksWritten = true
	case probe.Hooks.UserPromptSubmit.Command != codexHookCommand:
		warnings = append(warnings, fmt.Sprintf(
			"hooks.UserPromptSubmit.command is %q (not %q); skipped — update manually",
			probe.Hooks.UserPromptSubmit.Command, codexHookCommand))
	}

	switch {
	case probe.Hooks.SessionStart == nil:
		toAppend = append(toAppend, fmt.Sprintf("[hooks.SessionStart]\ncommand = %q", codexHookCommand))
		hooksWritten = true
	case probe.Hooks.SessionStart.Command != codexHookCommand:
		warnings = append(warnings, fmt.Sprintf(
			"hooks.SessionStart.command is %q (not %q); skipped — update manually",
			probe.Hooks.SessionStart.Command, codexHookCommand))
	}

	existingAgentName := ""
	if probe.McpServers != nil {
		if entry, ok := probe.McpServers["tmux-tell"]; ok {
			existingAgentName = entry.Env["TMUX_AGENT_NAME"]
		}
	}
	switch {
	case existingAgentName == "":
		toAppend = append(toAppend, fmt.Sprintf("[mcp_servers.tmux-tell.env]\nTMUX_AGENT_NAME = %q", agentName))
		envWritten = true
	case existingAgentName != agentName:
		warnings = append(warnings, fmt.Sprintf(
			"mcp_servers.tmux-tell.env.TMUX_AGENT_NAME is %q (want %q); skipped — update manually",
			existingAgentName, agentName))
	}

	// Write when there's something to append OR a migration rewrote the content
	// (a migrate-only change has no appends but MUST still be persisted — the
	// `|| migrated` arm is the load-bearing half of the write gate).
	if len(toAppend) == 0 && !migrated {
		alreadyOK = len(warnings) == 0
		return false, false, alreadyOK, warnings, notices, nil
	}

	if dryRun {
		return hooksWritten, envWritten, false, warnings, notices, nil
	}

	// Build the content to append onto the (possibly migrated) base. Lead with a
	// blank line if the base has content and we're appending, so new sections
	// start cleanly; a migrate-only write appends nothing and keeps the base
	// byte-identical apart from the rewritten header(s).
	var sb strings.Builder
	if fileExists && len(content) > 0 && len(toAppend) > 0 {
		if !strings.HasSuffix(content, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	for i, block := range toAppend {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(block)
		sb.WriteString("\n")
	}

	if mkErr := os.MkdirAll(filepath.Dir(configPath), 0o700); mkErr != nil {
		return false, false, false, warnings, notices, fmt.Errorf("create config dir: %w", mkErr)
	}

	// Atomic write: temp file → rename.
	tmp, tmpErr := os.CreateTemp(filepath.Dir(configPath), ".codex-install-*.toml")
	if tmpErr != nil {
		return false, false, false, warnings, notices, fmt.Errorf("create temp: %w", tmpErr)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op after rename succeeds
	}()

	combined := append([]byte(content), []byte(sb.String())...)
	if _, wErr := tmp.Write(combined); wErr != nil {
		return false, false, false, warnings, notices, fmt.Errorf("write temp: %w", wErr)
	}
	if cErr := tmp.Close(); cErr != nil {
		return false, false, false, warnings, notices, fmt.Errorf("close temp: %w", cErr)
	}
	if rErr := os.Rename(tmpName, configPath); rErr != nil {
		return false, false, false, warnings, notices, fmt.Errorf("rename: %w", rErr)
	}

	return hooksWritten, envWritten, false, warnings, notices, nil
}

func emitCodexInstallResult(stdout, stderr io.Writer, r codexInstallResult, format, configPath, agentName string, dryRun bool) int {
	switch format {
	case "json":
		_ = writeJSONResult(stdout, r)
	case "text", "":
		if r.AlreadyOK {
			fmt.Fprintf(stdout, "OK\tcodex config already wired for %q — no changes\n", agentName)
		} else {
			for _, n := range r.Notices {
				fmt.Fprintf(stdout, "NOTICE\t%s\n", n)
			}
			if r.Registered {
				fmt.Fprintf(stdout, "REGISTERED\t%s delivery_mode=hook-context\n", agentName)
			}
			if r.HooksWritten {
				fmt.Fprintf(stdout, "WRITTEN\thooks.UserPromptSubmit + hooks.SessionStart → %s\n", configPath)
			}
			if r.EnvWritten {
				fmt.Fprintf(stdout, "WRITTEN\tmcp_servers.tmux-tell.env.TMUX_AGENT_NAME=%q → %s\n", agentName, configPath)
			}
			for _, w := range r.Warnings {
				fmt.Fprintf(stdout, "WARN\t%s\n", w)
			}
		}
		if dryRun {
			fmt.Fprintln(stderr, "(--dry-run: no changes written)")
		} else if !r.AlreadyOK {
			fmt.Fprintf(stdout, "\nPost-install: launch or restart your Codex session.\n")
			fmt.Fprintf(stdout, "When Codex prompts for hook approval, enable:\n")
			fmt.Fprintf(stdout, "  UserPromptSubmit: %s\n", codexHookCommand)
			fmt.Fprintf(stdout, "  SessionStart:     %s\n", codexHookCommand)
			fmt.Fprintf(stdout, "Messages addressed to %q will appear as additionalContext on each turn.\n", agentName)
		}
		if r.OK {
			fmt.Fprintln(stdout, "OK\tcodex-install complete")
		}
	}
	return exitOK
}
