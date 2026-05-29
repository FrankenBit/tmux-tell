package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

func TestExtractResumeName_SimpleSingleWord(t *testing.T) {
	argv := []string{"claude", "--resume", "bosun", "--dangerously-skip-permissions"}
	if got := extractResumeName(argv); got != "bosun" {
		t.Errorf("got %q, want bosun", got)
	}
}

func TestExtractResumeName_MultiWordSpaceSeparated(t *testing.T) {
	argv := []string{"claude", "--resume", "Master", "Bosun", "of", "Nimbus", "--dangerously-skip-permissions"}
	if got := extractResumeName(argv); got != "Master Bosun of Nimbus" {
		t.Errorf("got %q, want 'Master Bosun of Nimbus'", got)
	}
}

func TestExtractResumeName_EqualsForm(t *testing.T) {
	argv := []string{"claude", "--remote-control", "--resume=Alcatraz", "--dangerously-skip-permissions"}
	if got := extractResumeName(argv); got != "Alcatraz" {
		t.Errorf("got %q, want Alcatraz", got)
	}
}

func TestExtractResumeName_NoFlag(t *testing.T) {
	if got := extractResumeName([]string{"claude", "--print", "hello"}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestParseCmdline(t *testing.T) {
	raw := "claude\x00--resume\x00bosun\x00--dangerously-skip-permissions\x00"
	got := parseCmdline(raw)
	want := []string{"claude", "--resume", "bosun", "--dangerously-skip-permissions"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// withFakeTmuxAndProc swaps out the pane lister + /proc reader.
func withFakeTmuxAndProc(t *testing.T, panes []tmuxio.PaneInfo, cmdlines map[int]string) {
	t.Helper()
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		var buf bytes.Buffer
		for _, p := range panes {
			fmt.Fprintf(&buf, "%s %d\n", p.ID, p.PID)
		}
		return buf.Bytes(), nil
	})
	prevRdr := cmdlineReader
	cmdlineReader = func(pid int) (string, error) {
		if c, ok := cmdlines[pid]; ok {
			return c, nil
		}
		return "", fmt.Errorf("no fake for pid %d", pid)
	}
	t.Cleanup(func() {
		tmuxio.SetListPanesWithPIDRunner(prevList)
		cmdlineReader = prevRdr
	})
}

func TestDiscover_UpdatesExistingAgent(t *testing.T) {
	s := newCmdTestStore(t, "bosun")
	withFakeTmuxAndProc(t,
		[]tmuxio.PaneInfo{{ID: "%5", PID: 100}},
		map[int]string{100: "claude\x00--resume\x00bosun\x00--dangerously-skip-permissions\x00"},
	)

	var stdout, stderr bytes.Buffer
	exit := runDiscoverWithStore(context.Background(), s, false, "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var results []discoverResult
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &results)
	if len(results) != 1 || results[0].Status != "updated" || results[0].NewPaneID != "%5" {
		t.Errorf("results = %+v", results)
	}
	a, _ := s.GetAgent(context.Background(), "bosun")
	if a.PaneID != "%5" {
		t.Errorf("pane_id = %q, want %%5", a.PaneID)
	}
}

func TestDiscover_DryRunDoesNotWrite(t *testing.T) {
	s := newCmdTestStore(t, "bosun")
	withFakeTmuxAndProc(t,
		[]tmuxio.PaneInfo{{ID: "%5", PID: 100}},
		map[int]string{100: "claude\x00--resume\x00bosun\x00"},
	)

	var stdout bytes.Buffer
	exit := runDiscoverWithStore(context.Background(), s, true, "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	a, _ := s.GetAgent(context.Background(), "bosun")
	if a.PaneID != "%99" { // original from newCmdTestStore
		t.Errorf("dry-run wrote: pane_id = %q", a.PaneID)
	}
}

func TestDiscover_NewAgentRegistration(t *testing.T) {
	s := newCmdTestStore(t) // empty registry
	withFakeTmuxAndProc(t,
		[]tmuxio.PaneInfo{{ID: "%5", PID: 100}},
		map[int]string{100: "claude\x00--resume\x00bosun\x00"},
	)

	var stdout bytes.Buffer
	exit := runDiscoverWithStore(context.Background(), s, false, "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	a, err := s.GetAgent(context.Background(), "bosun")
	if err != nil {
		t.Fatalf("bosun not registered: %v", err)
	}
	if a.PaneID != "%5" {
		t.Errorf("pane_id = %q, want %%5", a.PaneID)
	}
}

func TestDiscover_MissingAgentKeepsPaneIDWithWarning(t *testing.T) {
	s := newCmdTestStore(t, "bosun")
	// No panes visible.
	withFakeTmuxAndProc(t, nil, nil)

	var stdout, stderr bytes.Buffer
	exit := runDiscoverWithStore(context.Background(), s, false, "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	a, _ := s.GetAgent(context.Background(), "bosun")
	if a.PaneID != "%99" {
		t.Errorf("pane_id changed to %q, want preserved %%99", a.PaneID)
	}
	if !strings.Contains(stderr.String(), "no current pane matches") {
		t.Errorf("missing warning in stderr: %q", stderr.String())
	}
}

func TestDiscover_TextOutput(t *testing.T) {
	s := newCmdTestStore(t, "bosun")
	withFakeTmuxAndProc(t,
		[]tmuxio.PaneInfo{{ID: "%5", PID: 100}},
		map[int]string{100: "claude\x00--resume\x00bosun\x00"},
	)
	var stdout bytes.Buffer
	_ = runDiscoverWithStore(context.Background(), s, false, "text", &stdout, &bytes.Buffer{})
	out := stdout.String()
	if !strings.Contains(out, "NAME\tSTATUS\tOLD_PANE\tNEW_PANE") {
		t.Errorf("missing header in %q", out)
	}
}
