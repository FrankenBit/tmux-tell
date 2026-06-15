package cli

import (
	"strings"
	"testing"
)

const (
	testCanonPath  = "/home/alex/.local/share/tmux-msg/messages.db"
	testCanonInode = 100
)

// TestClassifyDoctorProcs induces each divergence class through a synthetic
// binding-set and confirms the per-process verdict + the fleet-level divergence
// flag (#348). Pure core — the /proc enumeration that feeds it is the impure
// shell, so every branch is exercised without live processes.
func TestClassifyDoctorProcs(t *testing.T) {
	bindings := []dbBinding{
		// canonical: same inode as the reference
		{PID: 1, BinaryPath: "/usr/local/bin/tmux-tell-claude", DBPath: testCanonPath, DBInode: testCanonInode},
		// orphan: open handle, dirent removed (the deploy-mv inode)
		{PID: 2, BinaryPath: "/usr/local/bin/tmux-tell-claude", DBPath: "/var/lib/tmux-msg/messages.db", DBInode: 50, DBDeleted: true},
		// divergent inode: bound to a different live DB
		{PID: 3, BinaryPath: "/usr/local/bin/tmux-tell-claude", DBPath: "/tmp/other/messages.db", DBInode: 77},
		// stale binary: DB matches canonical, but running since-replaced code
		{PID: 4, BinaryPath: "/usr/local/bin/tmux-tell-claude (deleted)", DBPath: testCanonPath, DBInode: testCanonInode},
		// no open DB handle: reported, not counted divergent
		{PID: 5, BinaryPath: "/usr/local/bin/tmux-tell-claude", Note: "no open *.db handle (in-memory store, or DB not yet opened)"},
	}
	rep := classifyDoctorProcs(bindings, testCanonPath, testCanonInode)

	if !rep.Divergent {
		t.Fatal("fleet should be flagged divergent (PIDs 2/3/4 diverge)")
	}
	byPID := map[int]doctorProc{}
	for _, p := range rep.Procs {
		byPID[p.PID] = p
	}
	checks := []struct {
		pid        int
		divergent  bool
		canonical  bool
		verdictHas string
	}{
		{1, false, true, "canonical"},
		{2, true, false, "orphan"},
		{3, true, false, "different DB inode"},
		{4, true, false, "since-replaced binary"},
		{5, false, false, "no open"},
	}
	for _, c := range checks {
		p, ok := byPID[c.pid]
		if !ok {
			t.Errorf("PID %d missing from report", c.pid)
			continue
		}
		if p.Divergent != c.divergent {
			t.Errorf("PID %d Divergent=%v, want %v (verdict %q)", c.pid, p.Divergent, c.divergent, p.Verdict)
		}
		if p.Canonical != c.canonical {
			t.Errorf("PID %d Canonical=%v, want %v", c.pid, p.Canonical, c.canonical)
		}
		if !strings.Contains(p.Verdict, c.verdictHas) {
			t.Errorf("PID %d verdict %q missing %q", c.pid, p.Verdict, c.verdictHas)
		}
	}
}

// TestClassifyDoctorProcs_AllCanonicalNoDivergence confirms a healthy fleet is
// not flagged.
func TestClassifyDoctorProcs_AllCanonicalNoDivergence(t *testing.T) {
	bindings := []dbBinding{
		{PID: 1, BinaryPath: "/usr/local/bin/tmux-tell-claude", DBPath: testCanonPath, DBInode: testCanonInode},
		{PID: 2, BinaryPath: "/usr/local/bin/tmux-tell-claude", DBPath: testCanonPath, DBInode: testCanonInode},
	}
	rep := classifyDoctorProcs(bindings, testCanonPath, testCanonInode)
	if rep.Divergent {
		t.Errorf("healthy fleet flagged divergent: %+v", rep.Procs)
	}
}

// TestClassifyDoctorProcs_CanonicalAbsentCannotCompare confirms that with no
// canonical file (inode 0) a process is neither claimed canonical nor flagged
// divergent on the inode axis — substrate-honest "cannot compare".
func TestClassifyDoctorProcs_CanonicalAbsentCannotCompare(t *testing.T) {
	bindings := []dbBinding{
		{PID: 1, BinaryPath: "/usr/local/bin/tmux-tell-claude", DBPath: "/somewhere/messages.db", DBInode: 42},
	}
	rep := classifyDoctorProcs(bindings, testCanonPath, 0)
	if rep.Divergent {
		t.Error("should not flag divergence when canonical inode is unknown")
	}
	p := rep.Procs[0]
	if p.Canonical {
		t.Error("should not claim canonical when there's nothing to compare against")
	}
	if !strings.Contains(p.Verdict, "cannot compare") {
		t.Errorf("verdict %q should say cannot-compare", p.Verdict)
	}
	// but an orphan is still divergent even without a canonical reference
	orphan := classifyDoctorProcs([]dbBinding{
		{PID: 2, BinaryPath: "/usr/local/bin/tmux-tell-claude", DBPath: "/gone/messages.db", DBInode: 9, DBDeleted: true},
	}, testCanonPath, 0)
	if !orphan.Divergent {
		t.Error("orphan inode must flag divergent even with no canonical reference")
	}
}
