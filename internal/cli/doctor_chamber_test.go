package cli

import (
	"testing"
)

// TestResolveDoctorProcInfo covers the #791 role/chamber resolver over a
// stubbed /proc: each row of the truth table under resolveDoctorProcInfo, plus
// the empty-cmdline defensive case + a defensive `--agent=<name>` form. The
// package-var stubs are restored after each subtest via t.Cleanup so parallel
// tests that touch these globals don't interleave.
func TestResolveDoctorProcInfo(t *testing.T) {
	tests := []struct {
		name      string
		pid       int
		cmdline   []string
		cgroup    string
		wantRole  string
		wantAgent string
	}{
		{
			name:      "mailman with --agent X (space-separated)",
			pid:       100,
			cmdline:   []string{"/usr/local/bin/tmux-tell-claude", "serve", "--agent", "bosun"},
			cgroup:    "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-tmux\\x2dtell\\x2dclaude\\x2dmailman.slice/tmux-tell-claude-mailman@bosun.service\n",
			wantRole:  "mailman",
			wantAgent: "bosun",
		},
		{
			name:      "mailman with --agent=X (equals form)",
			pid:       101,
			cmdline:   []string{"/usr/local/bin/tmux-tell-claude", "serve", "--agent=quartermaster"},
			cgroup:    "irrelevant — cmdline wins for mailmen",
			wantRole:  "mailman",
			wantAgent: "quartermaster",
		},
		{
			name:      "mailman serve missing --agent (defensive)",
			pid:       102,
			cmdline:   []string{"/usr/local/bin/tmux-tell-claude", "serve"},
			cgroup:    "",
			wantRole:  "mailman",
			wantAgent: "",
		},
		{
			name:      "chamber-MCP resolves from cgroup",
			pid:       200,
			cmdline:   []string{"tmux-tell-codex", "mcp"},
			cgroup:    "0::/user.slice/user-1000.slice/user@1000.service/chamber.slice/chamber-carpenter.slice/run-p66821-i33600199.scope\n",
			wantRole:  "mcp",
			wantAgent: "carpenter",
		},
		{
			name:      "chamber-MCP with cgroup name containing hyphen",
			pid:       201,
			cmdline:   []string{"tmux-tell-claude", "mcp"},
			cgroup:    "0::/user.slice/user-1000.slice/user@1000.service/chamber.slice/chamber-multi-word-name.slice/run-p1-i2.scope\n",
			wantRole:  "mcp",
			wantAgent: "multi-word-name",
		},
		{
			name:      "chamber-MCP without a chamber slice (orphan)",
			pid:       202,
			cmdline:   []string{"tmux-tell-claude", "mcp"},
			cgroup:    "0::/user.slice/user-1000.slice/user@1000.service/other.slice/run-p1-i2.scope\n",
			wantRole:  "mcp",
			wantAgent: "",
		},
		{
			name:      "unknown verb",
			pid:       300,
			cmdline:   []string{"/usr/local/bin/tmux-tell-claude", "status"},
			cgroup:    "",
			wantRole:  "unknown",
			wantAgent: "",
		},
		{
			name:      "empty cmdline (unreadable /proc/<pid>/cmdline)",
			pid:       400,
			cmdline:   nil,
			cgroup:    "0::/user.slice/user-1000.slice/user@1000.service/chamber.slice/chamber-bosun.slice/run.scope\n",
			wantRole:  "unknown",
			wantAgent: "",
		},
		{
			name:      "cmdline with only argv[0]",
			pid:       401,
			cmdline:   []string{"tmux-tell-claude"},
			cgroup:    "",
			wantRole:  "unknown",
			wantAgent: "",
		},
		// Guard against a naive-regex mismatch: chamber.slice (no hyphen, the
		// parent umbrella slice) must NOT match — the resolver's regex requires
		// the hyphen. Confirms the pattern binds to the per-chamber leaf, not
		// the umbrella.
		{
			name:      "umbrella chamber.slice alone does not extract a name",
			pid:       500,
			cmdline:   []string{"tmux-tell-claude", "mcp"},
			cgroup:    "0::/user.slice/user-1000.slice/user@1000.service/chamber.slice/other-non-chamber.slice/run.scope\n",
			wantRole:  "mcp",
			wantAgent: "",
		},
	}

	origCmd := procCmdlineForPID
	origCg := procCgroupForPID
	t.Cleanup(func() {
		procCmdlineForPID = origCmd
		procCgroupForPID = origCg
	})

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			procCmdlineForPID = func(pid int) []string {
				if pid != tc.pid {
					t.Fatalf("cmdline lookup for wrong pid: got %d want %d", pid, tc.pid)
				}
				return tc.cmdline
			}
			procCgroupForPID = func(pid int) string {
				if pid != tc.pid {
					t.Fatalf("cgroup lookup for wrong pid: got %d want %d", pid, tc.pid)
				}
				return tc.cgroup
			}
			got := resolveDoctorProcInfo(tc.pid)
			if got.Role != tc.wantRole {
				t.Errorf("Role = %q, want %q", got.Role, tc.wantRole)
			}
			if got.AgentName != tc.wantAgent {
				t.Errorf("AgentName = %q, want %q", got.AgentName, tc.wantAgent)
			}
		})
	}
}
