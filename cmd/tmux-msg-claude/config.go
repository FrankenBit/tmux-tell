package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/config"
)

// runConfigCLI is the umbrella for the `config` subcommand family.
// Today there's one verb (`show`); future verbs (`edit`, `validate`,
// `migrate`) layer in here.
//
// Usage: tmux-msg-claude config <verb> [args]
func runConfigCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: tmux-msg-claude config <verb> [args]")
		fmt.Fprintln(stderr, "verbs:")
		fmt.Fprintln(stderr, "  show   Print the resolved config for an agent (#54)")
		return exitUsage
	}
	switch args[0] {
	case "show":
		return runConfigShowCLI(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "tmux-msg-claude config: unknown verb %q\n", args[0])
		return exitUsage
	}
}

// runConfigShowCLI implements `tmux-msg-claude config show --agent NAME`
// — prints the fully-resolved config for the given agent so the
// operator can debug precedence without tracing through TOML manually.
//
// Output reflects what `serve --agent NAME` would actually use. The
// config-path field shows where the file was loaded from (or
// reports the missing-file fallback).
func runConfigShowCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agent := fs.String("agent", "", "agent name to resolve config for (required)")
	format := fs.String("format", "text", "text|json")
	configPath := fs.String("config", "",
		"override config path (else $CLAUDE_MSG_CONFIG, else /etc/tmux-msg/config.toml)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if *agent == "" {
		return writeJSONError(stdout, stderr, "--agent required", exitUsage)
	}

	path := *configPath
	if path == "" {
		path = os.Getenv("CLAUDE_MSG_CONFIG")
		if path == "" {
			path = config.DefaultPath
		}
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("load config: %v", err), exitInternal)
	}
	view := config.Resolve(cfg, path, *agent)

	switch *format {
	case "json":
		_ = writeJSONResult(stdout, view)
		return exitOK
	case "text", "":
		// Path + a flag indicating whether the file actually loaded.
		fmt.Fprintf(stdout, "CONFIG\t%s", view.ConfigPath)
		if _, statErr := os.Stat(view.ConfigPath); statErr != nil {
			fmt.Fprint(stdout, "\t(missing — using defaults)")
		}
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "AGENT\t%s\n", view.Agent)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "FIELD\tVALUE")
		fmt.Fprintf(stdout, "notify-on-failed\t%t\n", view.NotifyOnFailed)
		fmt.Fprintf(stdout, "notify-on-delivered-in-input-box\t%t\n", view.NotifyOnDeliveredInInputBox)
		fmt.Fprintf(stdout, "drift-soft-fail\t%t\n", view.DriftSoftFail)
		fmt.Fprintf(stdout, "gate-disabled\t%t\n", view.GateDisabled)
		fmt.Fprintf(stdout, "poll-interval-min\t%s\n", view.PollIntervalMin)
		fmt.Fprintf(stdout, "poll-interval-max\t%s\n", view.PollIntervalMax)
		fmt.Fprintf(stdout, "input-stale-threshold\t%s\n", view.InputStaleThreshold)
		fmt.Fprintf(stdout, "notify-emoji-disabled\t%t\n", view.NotifyEmojiDisabled)
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
}
