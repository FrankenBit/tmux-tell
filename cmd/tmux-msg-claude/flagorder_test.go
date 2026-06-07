package main

import (
	"bytes"
	"flag"
	"reflect"
	"testing"
)

// newTestFlagSet creates a FlagSet for testing reorderFlagsFirst.
// Mirrors the typical control-subcommand flag shape:
//   --to STRING (value)
//   --command STRING (value)
//   --quiet-disabled BOOL
func newTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	fs.String("to", "", "recipient")
	fs.String("command", "", "command")
	fs.Bool("quiet-disabled", false, "bypass quiet gate")
	return fs
}

// TestReorderFlagsFirst_FlagsAlreadyFirst is the no-op case — operator
// followed the discipline and put flags first.
func TestReorderFlagsFirst_FlagsAlreadyFirst(t *testing.T) {
	fs := newTestFlagSet()
	got := reorderFlagsFirst(fs, []string{"--to", "alice", "--command", "compact"})
	want := []string{"--to", "alice", "--command", "compact"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestReorderFlagsFirst_PositionalFirstBuggyForm is the #44 bug case
// — operator put the agent name first (natural English order).
// Without the helper, `--command compact` would be silently dropped.
func TestReorderFlagsFirst_PositionalFirstBuggyForm(t *testing.T) {
	fs := newTestFlagSet()
	got := reorderFlagsFirst(fs, []string{"alice", "--command", "compact"})
	want := []string{"--command", "compact", "alice"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestReorderFlagsFirst_InterleavedFlagsAndPositionals — flag, positional,
// flag, positional. All flags should land at the front in original order;
// positionals at the back in original order.
func TestReorderFlagsFirst_InterleavedFlagsAndPositionals(t *testing.T) {
	fs := newTestFlagSet()
	got := reorderFlagsFirst(fs, []string{"--to", "alice", "compact", "--quiet-disabled", "extra"})
	want := []string{"--to", "alice", "--quiet-disabled", "compact", "extra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestReorderFlagsFirst_BundledFlagEquals — `--flag=value` is a single
// token; no value-swallow needed.
func TestReorderFlagsFirst_BundledFlagEquals(t *testing.T) {
	fs := newTestFlagSet()
	got := reorderFlagsFirst(fs, []string{"alice", "--command=compact"})
	want := []string{"--command=compact", "alice"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestReorderFlagsFirst_BoolFlagDoesNotSwallow — bool flags don't take
// a value, so the following token must stay positional.
func TestReorderFlagsFirst_BoolFlagDoesNotSwallow(t *testing.T) {
	fs := newTestFlagSet()
	got := reorderFlagsFirst(fs, []string{"--quiet-disabled", "alice", "extra"})
	want := []string{"--quiet-disabled", "alice", "extra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestReorderFlagsFirst_DashDashTerminator — `--` ends flag parsing;
// everything after stays positional in original order.
func TestReorderFlagsFirst_DashDashTerminator(t *testing.T) {
	fs := newTestFlagSet()
	got := reorderFlagsFirst(fs, []string{"--to", "alice", "--", "--command", "compact"})
	want := []string{"--to", "alice", "--command", "compact"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestReorderFlagsFirst_UnknownFlagAssumedNoValue — a flag the FlagSet
// doesn't know about: we don't swallow a value. Better to surface as
// "unknown flag" than to eat operator's positional.
func TestReorderFlagsFirst_UnknownFlagAssumedNoValue(t *testing.T) {
	fs := newTestFlagSet()
	got := reorderFlagsFirst(fs, []string{"--unknown", "alice"})
	// Unknown flag with no value swallowed; `alice` stays as positional.
	want := []string{"--unknown", "alice"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestReorderFlagsFirst_BareDashIsPositional — a single `-` is the
// stdin convention; treat it as positional.
func TestReorderFlagsFirst_BareDashIsPositional(t *testing.T) {
	fs := newTestFlagSet()
	got := reorderFlagsFirst(fs, []string{"--to", "alice", "-"})
	want := []string{"--to", "alice", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestReorderFlagsFirst_EmptyArgs — defensive.
func TestReorderFlagsFirst_EmptyArgs(t *testing.T) {
	fs := newTestFlagSet()
	got := reorderFlagsFirst(fs, nil)
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// TestReorderFlagsFirst_IntegrationWithControlBuggyForm verifies the
// #44 scenario end-to-end: parse the reordered args and confirm both
// --to and --command land where they should.
func TestReorderFlagsFirst_IntegrationWithControlBuggyForm(t *testing.T) {
	fs := newTestFlagSet()
	to := fs.Lookup("to").Value
	command := fs.Lookup("command").Value
	// The exact buggy form from #44.
	reordered := reorderFlagsFirst(fs, []string{"alice", "--command", "compact"})
	if err := fs.Parse(reordered); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if command.String() != "compact" {
		t.Errorf("after reorder, --command = %q, want compact", command.String())
	}
	if to.String() != "" {
		t.Errorf("--to should be empty (no --to in input); got %q", to.String())
	}
	// The positional `alice` should land in fs.Args().
	if len(fs.Args()) != 1 || fs.Args()[0] != "alice" {
		t.Errorf("positionals = %v, want [alice]", fs.Args())
	}
}

// TestReorderFlagsFirst_IntegrationWithControlWorkingForm — sanity:
// the form that works today still works after reorder.
func TestReorderFlagsFirst_IntegrationWithControlWorkingForm(t *testing.T) {
	fs := newTestFlagSet()
	to := fs.Lookup("to").Value
	command := fs.Lookup("command").Value
	reordered := reorderFlagsFirst(fs, []string{"--to", "alice", "--command", "compact"})
	if err := fs.Parse(reordered); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if command.String() != "compact" {
		t.Errorf("--command = %q, want compact", command.String())
	}
	if to.String() != "alice" {
		t.Errorf("--to = %q, want alice", to.String())
	}
}

// TestRunControlCLI_AutoBindsPositionalToTo verifies the operator's
// natural typing pattern `control alice --command help` resolves
// `alice` as `--to`. Closes the actual operator friction from #44 —
// the reorder helper alone would parse --command but still error on
// "--to required"; the auto-bind catches the trailing single positional.
func TestRunControlCLI_AutoBindsPositionalToTo(t *testing.T) {
	_ = newCmdTestStore(t, "alice", "bob")
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")

	var stdout, stderr bytes.Buffer
	exit := runControlCLI([]string{"bob", "--command", "help"}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true {
		t.Errorf("ok != true: %v", got)
	}
	if got["command"] != "/help" {
		t.Errorf("command = %v, want /help", got["command"])
	}
}

// TestRunControlCLI_FlagOnlyFormStillWorks — non-regression: the
// existing flag-only invocation must continue working.
func TestRunControlCLI_FlagOnlyFormStillWorks(t *testing.T) {
	_ = newCmdTestStore(t, "alice", "bob")
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	t.Setenv("CLAUDE_MSG_DB", ":memory:")

	var stdout, stderr bytes.Buffer
	exit := runControlCLI([]string{"--to", "bob", "--command", "help"}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true {
		t.Errorf("ok != true: %v", got)
	}
}
