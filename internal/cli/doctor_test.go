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
			if rep.Procs[0].IdleStale {
				t.Errorf("mid-turn (%s) SOFTENED proc must NOT carry IdleStale — softened != idle-stale (#797)", midTurn)
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
	// are safer-to-fail). Under #797 these carry IdleStale=true so the exit
	// code splits from real-divergence classes.
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
			if !rep.Procs[0].IdleStale {
				t.Errorf("non-mid-turn (%s) case must carry IdleStale=true (#797)", notMidTurn)
			}
			if !strings.Contains(rep.Procs[0].Verdict, "not mid-turn") {
				t.Errorf("non-mid-turn verdict should name the case, got %q", rep.Procs[0].Verdict)
			}
		})
	}

	// chamber resolvable from cgroup but ABSENT from snapshot (unregistered
	// OR stale-past-TTL) → still divergent, and carries IdleStale=true so
	// exit-71 applies when this is the only divergence class (#797).
	orphanRep := classifyDoctorProcs(softenCase, softenInfos, testCanonPath, testCanonInode, classifyOpts{
		AllowActiveAgents: true,
		ObservedByChamber: map[string]string{},
	})
	if !orphanRep.Divergent {
		t.Error("orphan chamber (resolved from cgroup, missing from snapshot) must stay divergent")
	}
	if !orphanRep.Procs[0].IdleStale {
		t.Error("orphan-chamber case (resolved from cgroup, absent from snapshot) must carry IdleStale=true (#797)")
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
	if mailmanRep.Procs[0].IdleStale {
		t.Error("mailman stale-binary must NOT be IdleStale — refresh-all-mcps cannot close mailman drift (#797)")
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
	if syncRep.Procs[0].IdleStale {
		t.Error("SYNC divergence (db-inode mismatch) must NOT be IdleStale — refresh-all-mcps cannot close SYNC drift (#797)")
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
	if dbDelRep.Procs[0].IdleStale {
		t.Error("SYNC divergence (db-deleted) must NOT be IdleStale — refresh-all-mcps cannot close db-deleted (#797)")
	}
}

// TestDoctorExitCode pins the three-state exit-code split (#797): the doctor
// case-switches deploy.yml (and any other caller) reads. The mapping preserves
// hard-fail on real-divergence classes while distinguishing idle-stale-only —
// the class refresh-all-mcps actually closes — so downstream can downgrade to
// a warning without collapsing the signal.
//
// Emitter ships with its consumer per Engineer's
// `feedback_check_what_consumes_the_emitted_BEFORE_shipping_the_emitter`
// discipline: exercised here + in `.forgejo/workflows/deploy.yml` case-switch
// simultaneously.
func TestDoctorExitCode(t *testing.T) {
	// Cases construct doctorProc values directly (post-classification), so
	// the impure /proc walk + info-resolution steps are out of scope. The
	// classifier's own IdleStale-setting is covered by TestClassifyDoctorProcs_AllowActiveAgents.
	cases := []struct {
		name     string
		procs    []doctorProc
		wantExit int
		reason   string
	}{
		{
			name:     "clean",
			procs:    []doctorProc{{dbBinding: dbBinding{PID: 1}, Canonical: true}},
			wantExit: exitOK,
			reason:   "no divergence at all",
		},
		{
			name: "sync-divergence-alone",
			procs: []doctorProc{
				{dbBinding: dbBinding{PID: 1}, Divergent: true, IdleStale: false, Verdict: "db-inode mismatch"},
			},
			wantExit: exitUnavailable,
			reason:   "SYNC divergence must stay hard-fail (69)",
		},
		{
			name: "idle-stale-alone",
			procs: []doctorProc{
				{dbBinding: dbBinding{PID: 1}, Divergent: true, IdleStale: true, Verdict: "idle-stale"},
			},
			wantExit: exitDoctorIdleStaleOnly,
			reason:   "sole idle-stale divergence downgrades to 71",
		},
		{
			name: "mixed-idle-and-sync-still-fails-hard",
			procs: []doctorProc{
				{dbBinding: dbBinding{PID: 1}, Divergent: true, IdleStale: true, Verdict: "idle-stale"},
				{dbBinding: dbBinding{PID: 2}, Divergent: true, IdleStale: false, Verdict: "db-deleted"},
			},
			wantExit: exitUnavailable,
			reason:   "ANY non-idle-stale divergence forces 69 (safety-net direction)",
		},
		{
			name: "softened-with-no-hard-divergence",
			procs: []doctorProc{
				{dbBinding: dbBinding{PID: 1}, Canonical: false, Divergent: false, Softened: true, Verdict: "softened"},
			},
			wantExit: exitOK,
			reason:   "softened-only (pending-drain) is not a divergence",
		},
		{
			name: "multiple-idle-stale-only",
			procs: []doctorProc{
				{dbBinding: dbBinding{PID: 1}, Divergent: true, IdleStale: true, Verdict: "idle-stale bosun"},
				{dbBinding: dbBinding{PID: 2}, Divergent: true, IdleStale: true, Verdict: "idle-stale herald"},
			},
			wantExit: exitDoctorIdleStaleOnly,
			reason:   "N idle-stale, no real-divergence → 71",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Derive the fleet-level Divergent flag the way the classifier does
			// (any per-proc Divergent → true), so the test exercises the same
			// invariant runDoctorCLI relies on.
			divergent := false
			for _, p := range c.procs {
				if p.Divergent {
					divergent = true
					break
				}
			}
			rep := doctorReport{Procs: c.procs, Divergent: divergent}
			if got := doctorExitCode(rep); got != c.wantExit {
				t.Errorf("doctorExitCode = %d, want %d (%s)", got, c.wantExit, c.reason)
			}
		})
	}

	// Belt-and-braces: the sysexits collision — the point of picking 71
	// instead of 70 — must hold. Anyone bumping exitInternal, exitOK,
	// exitUnavailable, or exitDoctorIdleStaleOnly to a value that overlaps
	// another exit constant here fails-loud immediately.
	if exitDoctorIdleStaleOnly == exitInternal {
		t.Fatal("exitDoctorIdleStaleOnly collides with exitInternal — pick a distinct code (#797)")
	}
	if exitDoctorIdleStaleOnly == exitUnavailable {
		t.Fatal("exitDoctorIdleStaleOnly collides with exitUnavailable — pick a distinct code (#797)")
	}
	if exitDoctorIdleStaleOnly == exitOK {
		t.Fatal("exitDoctorIdleStaleOnly collides with exitOK — pick a distinct code (#797)")
	}
}
