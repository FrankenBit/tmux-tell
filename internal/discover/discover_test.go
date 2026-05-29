package discover

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

func testWalker(cmdlines map[int]string, children map[int][]int) *Walker {
	return &Walker{
		CmdlineReader: func(pid int) (string, error) {
			if c, ok := cmdlines[pid]; ok {
				return c, nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(p int) []int { return children[p] },
		MaxDepth:       3,
	}
}

func TestExtractResumeName_Simple(t *testing.T) {
	cases := map[string]struct {
		argv []string
		want string
	}{
		"single word": {
			[]string{"claude", "--resume", "bosun"},
			"bosun",
		},
		"multi-word collected": {
			[]string{"claude", "--resume", "Master", "Bosun", "of", "Nimbus", "--remote-control"},
			"Master Bosun of Nimbus",
		},
		"equals form": {
			[]string{"claude", "--resume=Pilot", "--remote-control"},
			"Pilot",
		},
		"no resume": {
			[]string{"claude", "--remote-control"},
			"",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := extractResumeName(tc.argv); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseCmdline_NULSeparated(t *testing.T) {
	raw := "claude\x00--resume\x00bosun\x00--remote-control\x00"
	want := []string{"claude", "--resume", "bosun", "--remote-control"}
	got := parseCmdline(raw)
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestStripTitleIndicators(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"  ":                     "",
		"⠐ Bosun":                "Bosun",
		"✳ Master Bosun":         "Master Bosun",
		"   ⠂   Alcatraz Infra Admin": "Alcatraz Infra Admin",
		"bosun":                  "bosun",
		"●  Foo":                 "Foo",
		"!!! something":          "",  // hits ASCII punct, gives up
	}
	for in, want := range cases {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			if got := stripTitleIndicators(in); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestIsGenericWindowName(t *testing.T) {
	for _, generic := range []string{"", "bash", "Bash", "zsh", "claude", "tmux"} {
		if !isGenericWindowName(generic) {
			t.Errorf("%q should be generic", generic)
		}
	}
	for _, agent := range []string{"bosun", "pilot", "Master Bosun of Nimbus"} {
		if isGenericWindowName(agent) {
			t.Errorf("%q should not be generic", agent)
		}
	}
}

func TestResolve_RootIsClaudeWithResume(t *testing.T) {
	w := testWalker(
		map[int]string{100: "claude\x00--resume\x00bosun\x00"},
		nil,
	)
	r, ok := w.Resolve(tmuxio.PaneInfo{ID: "%1", PID: 100})
	if !ok || r.AgentName != "bosun" || r.Source != SourceCmdline {
		t.Errorf("got %+v ok=%v", r, ok)
	}
}

func TestResolve_DescendantWithResume(t *testing.T) {
	// pane_pid is bash; bash → claude with --resume
	w := testWalker(
		map[int]string{
			100: "/bin/bash\x00-i\x00",
			200: "claude\x00--resume\x00surveyor\x00",
		},
		map[int][]int{100: {200}},
	)
	r, ok := w.Resolve(tmuxio.PaneInfo{ID: "%4", PID: 100})
	if !ok || r.AgentName != "surveyor" || r.Source != SourceCmdline {
		t.Errorf("got %+v ok=%v", r, ok)
	}
}

func TestResolve_DepthRespected(t *testing.T) {
	// 5 levels deep, MaxDepth=3 → claude not found
	w := testWalker(
		map[int]string{
			1: "bash\x00",
			2: "bash\x00",
			3: "bash\x00",
			4: "bash\x00",
			5: "claude\x00--resume\x00deep\x00",
		},
		map[int][]int{1: {2}, 2: {3}, 3: {4}, 4: {5}},
	)
	if _, ok := w.Resolve(tmuxio.PaneInfo{ID: "%1", PID: 1}); ok {
		t.Error("MaxDepth=3 should not reach pid 5")
	}
	w.MaxDepth = 5
	if _, ok := w.Resolve(tmuxio.PaneInfo{ID: "%1", PID: 1}); !ok {
		t.Error("MaxDepth=5 should reach pid 5")
	}
}

func TestResolve_TitleFallback(t *testing.T) {
	// No --resume anywhere, but title is "✳ Pilot".
	w := testWalker(
		map[int]string{100: "claude\x00--remote-control\x00"},
		nil,
	)
	r, ok := w.Resolve(tmuxio.PaneInfo{ID: "%5", PID: 100, Title: "✳ Pilot"})
	if !ok || r.AgentName != "Pilot" || r.Source != SourceTitle {
		t.Errorf("got %+v ok=%v", r, ok)
	}
}

func TestResolve_WindowNameFallback(t *testing.T) {
	// No cmdline match, blank title, but window_name is meaningful.
	w := testWalker(
		map[int]string{100: "claude\x00"},
		nil,
	)
	r, ok := w.Resolve(tmuxio.PaneInfo{ID: "%5", PID: 100, WindowName: "Captain"})
	if !ok || r.AgentName != "Captain" || r.Source != SourceWindowName {
		t.Errorf("got %+v ok=%v", r, ok)
	}
}

func TestResolve_WindowNameGenericIgnored(t *testing.T) {
	w := testWalker(map[int]string{100: "claude\x00"}, nil)
	if _, ok := w.Resolve(tmuxio.PaneInfo{ID: "%5", PID: 100, WindowName: "bash"}); ok {
		t.Error("generic window name should not produce a match")
	}
}

func TestResolve_NothingMatches(t *testing.T) {
	w := testWalker(map[int]string{100: "bash\x00"}, nil)
	if _, ok := w.Resolve(tmuxio.PaneInfo{ID: "%5", PID: 100}); ok {
		t.Error("no name source → no match")
	}
}

func TestLookupByName(t *testing.T) {
	// Stub WalkAll by stubbing tmuxio's pane lister + readers.
	prev := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		// Two panes; one runs bosun, other pilot.
		return []byte("%1\t100\t✳ Master Bosun of Nimbus\tclaude\n" +
			"%5\t200\t✳ Pilot\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prev) })

	w := &Walker{
		CmdlineReader: func(pid int) (string, error) {
			switch pid {
			case 100:
				return "claude\x00--resume\x00Master\x00Bosun\x00of\x00Nimbus\x00", nil
			case 200:
				return "claude\x00--remote-control\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       3,
	}

	got, err := w.LookupByName(context.Background(), "Pilot")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "%5" {
		t.Errorf("got %q, want %%5", got)
	}
	got, _ = w.LookupByName(context.Background(), "ghost")
	if got != "" {
		t.Errorf("ghost should not resolve, got %q", got)
	}
}
