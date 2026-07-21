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
	rep := classifyDoctorProcs(bindings, nil, testCanonPath, testCanonInode, classifyOpts{})

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
	rep := classifyDoctorProcs(bindings, nil, testCanonPath, testCanonInode, classifyOpts{})
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
	rep := classifyDoctorProcs(bindings, nil, testCanonPath, 0, classifyOpts{})
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
	}, nil, testCanonPath, 0, classifyOpts{})
	if !orphan.Divergent {
		t.Error("orphan inode must flag divergent even with no canonical reference")
	}
}

// TestClassifyDoctorProcs_AllowActiveAgents exercises the #791 softening
// matrix: stale-binary agent-side MCPs are reclassified as pending-drain only
// when (a) the flag is set, (b) role=="mcp", (c) the agent resolves in the
// observed_state snapshot, and (d) the observed_state is mid-turn (working /
// at-rest-in-compaction / awaiting-operator per isMidTurnObservedState).
// Every other case stays divergent. SYNC divergence classes
// (db-inode-mismatch, db-deleted) are unaffected by the flag.
func TestClassifyDoctorProcs_AllowActiveAgents(t *testing.T) {
	stalePath := "/usr/local/bin/tmux-tell-claude (deleted)"
	softenCase := []dbBinding{
		{PID: 10, BinaryPath: stalePath, DBPath: testCanonPath, DBInode: testCanonInode},
	}
	softenInfos := []doctorProcInfo{{Role: "mcp", AgentName: "bosun"}}

	// mid-turn subtests: each of the three softenable observed_state values
	// must produce Softened=true, Divergent=false.
	for _, midTurn := range []string{"working", "at-rest-in-compaction", "awaiting-operator"} {
		midTurn := midTurn
		t.Run("mid-turn/"+midTurn, func(t *testing.T) {
			rep := classifyDoctorProcs(softenCase, softenInfos, testCanonPath, testCanonInode, classifyOpts{
				AllowActiveAgents: true,
				ObservedByChamber: map[string]string{"bosun": midTurn},
			})
			if rep.Divergent {
				t.Errorf("mid-turn (%s) chamber stale-MCP should be softened, not divergent (got %+v)", midTurn, rep.Procs[0])
			}
			if !rep.Procs[0].Softened {
				t.Errorf("mid-turn (%s) softened proc should carry Softened=true", midTurn)
			}
			if rep.Procs[0].ObservedState != midTurn {
				t.Errorf("proc should carry the resolved observed_state, got %q want %q", rep.Procs[0].ObservedState, midTurn)
			}
			if !strings.Contains(rep.Procs[0].Verdict, "mid-turn") ||
				!strings.Contains(rep.Procs[0].Verdict, "--allow-active-agents") ||
				!strings.Contains(rep.Procs[0].Verdict, midTurn) {
				t.Errorf("softened verdict should name mid-turn + the flag + the observed_state, got %q", rep.Procs[0].Verdict)
			}
		})
	}

	// non-mid-turn subtests: every non-softenable observed_state must stay
	// divergent (idle-at-prompt is the Surveyor 3ea9 case; uncertain states
	// are safer-to-fail).
	for _, notMidTurn := range []string{"idle", "copy-mode", "rate-limited", "usage-limited", "unknown"} {
		notMidTurn := notMidTurn
		t.Run("not-mid-turn/"+notMidTurn, func(t *testing.T) {
			rep := classifyDoctorProcs(softenCase, softenInfos, testCanonPath, testCanonInode, classifyOpts{
				AllowActiveAgents: true,
				ObservedByChamber: map[string]string{"bosun": notMidTurn},
			})
			if !rep.Divergent {
				t.Errorf("non-mid-turn (%s) chamber stale-MCP must stay divergent even under --allow-active-agents", notMidTurn)
			}
			if rep.Procs[0].Softened {
				t.Errorf("non-mid-turn (%s) case must NOT be softened", notMidTurn)
			}
			if !strings.Contains(rep.Procs[0].Verdict, "not mid-turn") {
				t.Errorf("non-mid-turn verdict should name the case, got %q", rep.Procs[0].Verdict)
			}
		})
	}

	// chamber resolvable from cgroup but ABSENT from snapshot (unregistered
	// OR stale-past-TTL) → orphan case, still divergent.
	orphanRep := classifyDoctorProcs(softenCase, softenInfos, testCanonPath, testCanonInode, classifyOpts{
		AllowActiveAgents: true,
		ObservedByChamber: map[string]string{},
	})
	if !orphanRep.Divergent {
		t.Error("orphan chamber (resolved from cgroup, missing from snapshot) must stay divergent")
	}
	if !strings.Contains(orphanRep.Procs[0].Verdict, "no fresh observed_state") {
		t.Errorf("orphan verdict should name the no-fresh-observation case, got %q", orphanRep.Procs[0].Verdict)
	}

	// mailmen never soften — even for a mid-turn chamber
	mailmanBinding := []dbBinding{
		{PID: 11, BinaryPath: stalePath, DBPath: testCanonPath, DBInode: testCanonInode},
	}
	mailmanInfos := []doctorProcInfo{{Role: "mailman", AgentName: "bosun"}}
	mailmanRep := classifyDoctorProcs(mailmanBinding, mailmanInfos, testCanonPath, testCanonInode, classifyOpts{
		AllowActiveAgents: true,
		ObservedByChamber: map[string]string{"bosun": "working"},
	})
	if !mailmanRep.Divergent {
		t.Error("mailman stale-binary must stay divergent regardless of chamber activity")
	}
	if mailmanRep.Procs[0].Softened {
		t.Error("mailman case must NOT be softened")
	}

	// flag ABSENT → pre-#791 behavior preserved
	noFlagRep := classifyDoctorProcs(softenCase, softenInfos, testCanonPath, testCanonInode, classifyOpts{
		AllowActiveAgents: false,
		ObservedByChamber: map[string]string{"bosun": "working"},
	})
	if !noFlagRep.Divergent {
		t.Error("without --allow-active-agents, stale-binary must be divergent unchanged")
	}
	if noFlagRep.Procs[0].Softened {
		t.Error("without flag, softening must not apply")
	}

	// SYNC divergence (db-inode-mismatch) unaffected by the flag
	syncBinding := []dbBinding{
		{PID: 12, BinaryPath: "/usr/local/bin/tmux-tell-claude", DBPath: "/tmp/other/messages.db", DBInode: 77},
	}
	syncInfos := []doctorProcInfo{{Role: "mcp", AgentName: "bosun"}}
	syncRep := classifyDoctorProcs(syncBinding, syncInfos, testCanonPath, testCanonInode, classifyOpts{
		AllowActiveAgents: true,
		ObservedByChamber: map[string]string{"bosun": "working"},
	})
	if !syncRep.Divergent {
		t.Error("SYNC divergence (db-inode mismatch) must stay divergent even for mid-turn chamber")
	}
	if syncRep.Procs[0].Softened {
		t.Error("SYNC divergence must NOT be softened")
	}

	// SYNC divergence (db-deleted / orphan-inode) unaffected by the flag too
	dbDelBinding := []dbBinding{
		{PID: 13, BinaryPath: "/usr/local/bin/tmux-tell-claude", DBPath: "/var/lib/tmux-msg/messages.db", DBInode: 50, DBDeleted: true},
	}
	dbDelRep := classifyDoctorProcs(dbDelBinding, syncInfos, testCanonPath, testCanonInode, classifyOpts{
		AllowActiveAgents: true,
		ObservedByChamber: map[string]string{"bosun": "working"},
	})
	if !dbDelRep.Divergent {
		t.Error("SYNC divergence (db-deleted) must stay divergent even for mid-turn chamber")
	}
}
