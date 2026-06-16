package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	Env map[string]string `toml:"env"`
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
	defer s.Close()

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
	hooksWritten, envWritten, alreadyOK, warnings, writeErr := mergeCodexConfig(
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
func mergeCodexConfig(configPath, agentName string, dryRun bool) (hooksWritten, envWritten, alreadyOK bool, warnings []string, err error) {
	existing, readErr := os.ReadFile(configPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, false, false, nil, fmt.Errorf("read: %w", readErr)
	}
	fileExists := readErr == nil

	var probe codexConfigProbe
	if fileExists && len(existing) > 0 {
		if _, decErr := toml.Decode(string(existing), &probe); decErr != nil {
			return false, false, false, nil, fmt.Errorf("parse TOML: %w", decErr)
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
		if entry, ok := probe.McpServers["tmux-msg"]; ok {
			existingAgentName = entry.Env["TMUX_AGENT_NAME"]
		}
	}
	switch {
	case existingAgentName == "":
		toAppend = append(toAppend, fmt.Sprintf("[mcp_servers.tmux-msg.env]\nTMUX_AGENT_NAME = %q", agentName))
		envWritten = true
	case existingAgentName != agentName:
		warnings = append(warnings, fmt.Sprintf(
			"mcp_servers.tmux-msg.env.TMUX_AGENT_NAME is %q (want %q); skipped — update manually",
			existingAgentName, agentName))
	}

	if len(toAppend) == 0 {
		alreadyOK = len(warnings) == 0
		return false, false, alreadyOK, warnings, nil
	}

	if dryRun {
		return hooksWritten, envWritten, false, warnings, nil
	}

	// Build the content to append. Lead with a blank line if the file
	// already exists and has content, so new sections start cleanly.
	var sb strings.Builder
	if fileExists && len(existing) > 0 {
		if !strings.HasSuffix(string(existing), "\n") {
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
		return false, false, false, warnings, fmt.Errorf("create config dir: %w", mkErr)
	}

	// Atomic write: temp file → rename.
	tmp, tmpErr := os.CreateTemp(filepath.Dir(configPath), ".codex-install-*.toml")
	if tmpErr != nil {
		return false, false, false, warnings, fmt.Errorf("create temp: %w", tmpErr)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op after rename succeeds
	}()

	combined := append(append([]byte(nil), existing...), []byte(sb.String())...)
	if _, wErr := tmp.Write(combined); wErr != nil {
		return false, false, false, warnings, fmt.Errorf("write temp: %w", wErr)
	}
	if cErr := tmp.Close(); cErr != nil {
		return false, false, false, warnings, fmt.Errorf("close temp: %w", cErr)
	}
	if rErr := os.Rename(tmpName, configPath); rErr != nil {
		return false, false, false, warnings, fmt.Errorf("rename: %w", rErr)
	}

	return hooksWritten, envWritten, false, warnings, nil
}

func emitCodexInstallResult(stdout, stderr io.Writer, r codexInstallResult, format, configPath, agentName string, dryRun bool) int {
	switch format {
	case "json":
		_ = writeJSONResult(stdout, r)
	case "text", "":
		if r.AlreadyOK {
			fmt.Fprintf(stdout, "OK\tcodex config already wired for %q — no changes\n", agentName)
		} else {
			if r.Registered {
				fmt.Fprintf(stdout, "REGISTERED\t%s delivery_mode=hook-context\n", agentName)
			}
			if r.HooksWritten {
				fmt.Fprintf(stdout, "WRITTEN\thooks.UserPromptSubmit + hooks.SessionStart → %s\n", configPath)
			}
			if r.EnvWritten {
				fmt.Fprintf(stdout, "WRITTEN\tmcp_servers.tmux-msg.env.TMUX_AGENT_NAME=%q → %s\n", agentName, configPath)
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
