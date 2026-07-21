package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// observedStateFreshness is the TTL applied to the agents.observed_state
// snapshot the doctor consumes under --allow-active-agents (#791). The
// mailman writes observed_state on its throttled probe cadence (a few
// captures per minute at most), so 2 minutes is generous headroom on the
// write cadence and short enough that a crashed mailman's stale value ages
// out before an operator would trust a softening decision. Matches the
// order-of-magnitude of the #448 provider-cap TTL for the same reason.
const observedStateFreshness = 2 * time.Minute

// doctorProc is one live tmux-msg process in the doctor report (#348): its
// /proc-read DB binding plus the verdict of comparing that binding against the
// canonical DB the binary-on-disk resolves to. Since #791 also carries the
// doctor-only role/chamber tag + resolved mailman-observed AgentState that
// softens the stale-binary class under --allow-active-agents.
type doctorProc struct {
	dbBinding
	// Canonical is true when the process's open DB inode matches the canonical
	// DB's inode (it's writing where fresh mailmen read).
	Canonical bool `json:"canonical"`
	// Divergent is true when the process is writing somewhere a fresh mailman
	// can't see — an orphaned (unlinked) inode, a different inode than
	// canonical, or running a since-replaced ("(deleted)") binary that has NOT
	// been softened by --allow-active-agents on a mid-turn chamber (#791).
	Divergent bool `json:"divergent"`
	// Verdict is a one-line human explanation of the classification.
	Verdict string `json:"verdict"`
	// Role classifies the process shape — "mailman" | "mcp" | "unknown" (#791).
	// Empty for pre-#791 tests / callers that don't wire the augmentation.
	Role string `json:"role,omitempty"`
	// AgentName names the agent that owns this process, when resolvable
	// (#791). For mailmen: the `--agent X` argv value. For agent-side MCPs:
	// the `chamber-<name>.slice` cgroup segment (the `<name>` slot — the
	// substring after the systemd chamber-launcher-wrapper's slice prefix).
	// Empty when unresolvable.
	AgentName string `json:"agent_name,omitempty"`
	// ObservedState carries the chamber's #448 mailman-observed AgentState when
	// both the process was resolved to a chamber AND that chamber has a fresh
	// observed_state in the agents store (#791). Empty when either resolution
	// step could not complete or the stored observation is stale (past the TTL
	// applied in runDoctorCLI). Uses the same wire vocabulary as tmuxio.State
	// ("working" / "idle" / "at-rest-in-compaction" / "awaiting-operator" /
	// "unknown" / "copy-mode" / "rate-limited" / "usage-limited").
	ObservedState string `json:"observed_state,omitempty"`
	// Softened is true when a stale-binary process was reclassified as
	// non-divergent because --allow-active-agents was set AND the owning
	// chamber is mid-turn (#791). Visible on the JSON wire so a reader can
	// distinguish "canonical" from "would-be-divergent, softened by policy" —
	// per /srv/CLAUDE.md § Mechanism-design ("each PASS names its silence").
	Softened bool `json:"softened,omitempty"`
}

// doctorReport is the full diagnostic: every live tmux-msg process + the
// canonical reference it was compared against (#348).
type doctorReport struct {
	CanonicalPath  string       `json:"canonical_path"`
	CanonicalInode uint64       `json:"canonical_inode,omitempty"`
	Procs          []doctorProc `json:"procs"`
	// Divergent is true when ANY process diverged — drives the exit code.
	Divergent bool `json:"divergent"`
}

// classifyOpts carries the #791 augmentation for the pure classifier.
// AllowActiveAgents softens the stale-binary class when the owning chamber
// is mid-turn (observed_state in the mid-turn set); ObservedByChamber is the
// fresh snapshot of the agents store's observed_state column (#448) the CLI
// hands in — stale entries filtered out by TTL upstream. Both empty → pre-#791
// behavior (stale-binary always divergent).
type classifyOpts struct {
	AllowActiveAgents bool
	ObservedByChamber   map[string]string
}

// isMidTurnObservedState reports whether the chamber's mailman-observed
// AgentState value counts as "mid-turn" for the #791 softening decision.
// The mid-turn set is chosen so the softened case implies "the chamber will
// hit a natural boundary at which a queued refresh-all-mcps command can
// actually fire":
//
//   - "working"              → actively processing; queued /mcp command runs
//     on next turn boundary
//   - "at-rest-in-compaction" → /compact restarts the whole session, which
//     restarts every MCP subprocess
//   - "awaiting-operator"    → open turn awaiting operator answer; once the
//     operator responds, the queued command fires next
//
// Every OTHER value is treated as non-mid-turn:
//
//   - "idle": chamber is stable at prompt. If refresh had fired, this is
//     precisely where the drain would have completed — so stale-here IS a
//     real divergence surface, not a transient one. (Surveyor 3ea9.)
//   - "copy-mode" / "rate-limited" / "usage-limited" / "unknown":
//     uncertain state; safer to keep the hard-fail than to soften
//     speculatively.
func isMidTurnObservedState(s string) bool {
	switch s {
	case "working", "at-rest-in-compaction", "awaiting-operator":
		return true
	default:
		return false
	}
}

// classifyDoctorProcs is the pure core (#348, extended #791): given the live
// processes' DB bindings, the per-process /proc-derived role/chamber
// augmentation, and the canonical DB identity, decide per-process whether each
// is canonical, divergent, or (under --allow-active-agents) softened as
// pending-drain. Pure + table-tested — the /proc enumeration + store fetch that
// feed it are the impure shell (gatherDoctorProcs + runDoctorCLI).
//
// A process diverges when it's writing somewhere a freshly-started mailman
// can't see:
//   - db_deleted: the open DB handle's dirent is gone (the deploy-mv orphan) —
//     the operationally-critical case; the writes vanish from every other surface
//   - a different open DB inode than canonical (bound to another live file)
//   - a "(deleted)" binary path: the process is running since-replaced code, the
//     pre-deploy-process smell even if its DB still happens to match
//
// The DB-inode / db-deleted branches are the SYNC divergence class — real
// substrate-state trouble that the operator must resolve. They stay
// hard-divergent regardless of --allow-active-agents.
//
// The stale-binary branch is the ASYNC class — a chamber's MCP subprocess that
// still holds an fd on the pre-deploy inode because the chamber has not yet
// hit an MCP-restart boundary. When AllowActiveAgents is set AND the process
// is a chamber-side MCP (Role=="mcp") AND its owning chamber has a fresh
// observed_state in the mid-turn set (see isMidTurnObservedState), the process
// is reclassified as pending-drain (Softened=true, Divergent=false). Every
// other stale-binary case — mailmen, orphan MCPs with no chamber resolution,
// idle-at-prompt chambers, chambers with stale/absent observations, unknown
// roles — stays divergent. See tmux-tell#791 (un-conflate SYNC vs ASYNC per
// Surveyor 3ea9 reframe) for the scoping rationale.
//
// A process with no open DB handle (Note set) is reported but not counted
// divergent — an MCP server that hasn't opened the DB, or an in-memory store.
func classifyDoctorProcs(bindings []dbBinding, infos []doctorProcInfo, canonicalPath string, canonicalInode uint64, opts classifyOpts) doctorReport {
	rep := doctorReport{CanonicalPath: canonicalPath, CanonicalInode: canonicalInode}
	for i, b := range bindings {
		p := doctorProc{dbBinding: b}
		var info doctorProcInfo
		if i < len(infos) {
			info = infos[i]
		}
		p.Role = info.Role
		p.AgentName = info.AgentName
		if info.AgentName != "" {
			p.ObservedState = opts.ObservedByChamber[info.AgentName]
		}
		staleBinary := strings.HasSuffix(b.BinaryPath, " (deleted)")
		switch {
		case b.Note != "" && b.DBPath == "":
			p.Verdict = b.Note
		case b.DBDeleted:
			p.Divergent = true
			p.Verdict = "orphan DB inode — file removed; writes invisible to mailmen on the canonical path"
		case canonicalInode != 0 && b.DBInode != canonicalInode:
			p.Divergent = true
			p.Verdict = fmt.Sprintf("bound to a different DB inode (%d) than canonical (%d)", b.DBInode, canonicalInode)
		case staleBinary:
			// #791: soften only when explicitly opted in AND the process is a
			// chamber-side MCP owned by a mid-turn chamber. Every other case
			// — mailmen, orphan MCPs, idle chambers, unknown role, stale
			// observation — stays divergent. Softening emits Softened=true so
			// the wire distinguishes canonical from softened.
			if opts.AllowActiveAgents && info.Role == "mcp" && info.AgentName != "" {
				obs := p.ObservedState
				switch {
				case obs == "":
					// Chamber resolved from cgroup but the store has no fresh
					// observation for it — either the chamber is unregistered
					// (orphan) or its mailman hasn't observed within the TTL.
					// Cannot substantiate mid-turn, so stay divergent.
					p.Divergent = true
					p.Verdict = fmt.Sprintf("stale binary — agent %q has no fresh observed_state (orphan or crashed mailman); MCP cannot be substantiated as mid-turn", info.AgentName)
				case !isMidTurnObservedState(obs):
					// Chamber is at-prompt-idle (or in a state where softening
					// cannot be substantiated — copy-mode, rate-limited, etc.).
					// The MCP will not self-heal on any activity boundary;
					// this is a real divergence surface even under the flag.
					p.Divergent = true
					p.Verdict = fmt.Sprintf("stale binary — agent %q observed_state=%s (not mid-turn); MCP will not drain without external action", info.AgentName, obs)
				default:
					// Mid-turn: working / at-rest-in-compaction /
					// awaiting-operator. Pending-drain, softened.
					p.Softened = true
					p.Verdict = fmt.Sprintf("stale binary softened — agent %q is mid-turn (observed_state=%s); MCP will refresh on next natural boundary [--allow-active-agents]", info.AgentName, obs)
				}
			} else {
				p.Divergent = true
				p.Verdict = "running a since-replaced binary (process predates the on-disk binary)"
			}
		case canonicalInode == 0:
			// No canonical file to compare against — can't substantiate
			// divergence, so don't claim canonical either (substrate-honest).
			p.Verdict = "canonical DB path absent — cannot compare inode"
		default:
			p.Canonical = true
			p.Verdict = "canonical"
		}
		if p.Divergent {
			rep.Divergent = true
		}
		rep.Procs = append(rep.Procs, p)
	}
	return rep
}

// gatherDoctorProcs walks /proc for every live process whose executable is the
// active adapter binary (matching the basename, so a since-replaced "(deleted)"
// binary still matches) and reads each one's DB binding (#348). Since #791 it
// also returns a parallel slice of doctorProcInfo — the doctor-only role +
// chamber-name augmentation derived from /proc/<pid>/cmdline + cgroup — that
// classifyDoctorProcs consumes when --allow-active-agents is set. This is the
// impure shell around classifyDoctorProcs.
func gatherDoctorProcs() ([]dbBinding, []doctorProcInfo) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil
	}
	wantBins := map[string]bool{active.BinaryName: true}
	for _, alias := range active.DeprecatedAliases {
		wantBins[alias] = true // pre-rename processes (any legacy alias) still count
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // non-numeric /proc entry
		}
		exe := procExe(pid)
		if exe == "" {
			continue
		}
		base := filepath.Base(strings.TrimSuffix(exe, " (deleted)"))
		if wantBins[base] {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	bindings := make([]dbBinding, 0, len(pids))
	infos := make([]doctorProcInfo, 0, len(pids))
	for _, pid := range pids {
		bindings = append(bindings, collectBinding(pid, false))
		infos = append(infos, resolveDoctorProcInfo(pid))
	}
	return bindings, infos
}

// canonicalDBIdentity resolves the canonical DB path the binary-on-disk would
// use and stats it for an inode (0 if the canonical file is absent — itself
// notable, surfaced in the report).
func canonicalDBIdentity() (string, uint64) {
	path := resolveDBPath("")
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err == nil {
		return path, st.Ino
	}
	return path, 0
}

// renderDoctorReport writes the human-readable diagnostic. The per-process
// lines mirror the issue's worked example; a trailing DIVERGENCE block names
// the recovery action when anything diverged. Since #791 a "~" mark
// distinguishes softened (pending-drain) rows from canonical "✓" rows so the
// PASS output names its silence: "which processes were let through by policy?"
// is immediately readable.
func renderDoctorReport(stdout io.Writer, rep doctorReport) {
	fmt.Fprintf(stdout, "CANONICAL\t%s", rep.CanonicalPath)
	if rep.CanonicalInode != 0 {
		fmt.Fprintf(stdout, " (inode %d)", rep.CanonicalInode)
	} else {
		fmt.Fprint(stdout, " (no file at canonical path)")
	}
	fmt.Fprintln(stdout)
	softenedCount := 0
	for _, p := range rep.Procs {
		mark := "✓"
		switch {
		case p.Divergent:
			mark = "✗"
		case p.Softened:
			mark = "~"
			softenedCount++
		}
		db := p.DBPath
		if db == "" {
			db = "(none)"
		} else {
			if p.DBInode != 0 {
				db += fmt.Sprintf(" (inode %d)", p.DBInode)
			}
			if p.DBDeleted {
				db += " [file removed]"
			}
		}
		fmt.Fprintf(stdout, "%s PID %d  binary=%s  db=%s  — %s\n", mark, p.PID, p.BinaryPath, db, p.Verdict)
	}
	if rep.Divergent {
		fmt.Fprintf(stdout, "DIVERGENCE: one or more processes hold a DB binding invisible to mailmen on the canonical path. Run `%s refresh-all-mcps` to restart MCP servers (and check for stale mailmen).\n", active.BinaryName)
	} else if softenedCount > 0 {
		// PASS-with-disclosure per /srv/CLAUDE.md § Mechanism-design: name the
		// scope this pass covered — chamber-mid-turn stale-binary MCPs were
		// admitted as pending-drain rather than divergent (#791).
		fmt.Fprintf(stdout, "OK: no unrecoverable divergence — %d agent-side MCP(s) softened as pending-drain (mid-turn agents under --allow-active-agents).\n", softenedCount)
	} else {
		fmt.Fprintln(stdout, "OK: all live processes bound to the canonical DB.")
	}
}

// runDoctorCLI implements `tmux-tell-claude doctor` (#348): walk live tmux-msg
// processes, compare each one's open DB binding against the canonical DB, emit
// a substrate-honest report, and exit non-zero on any divergence so the command
// is usable as a runbook gate. Touches the DB only when --allow-active-agents
// is set (to resolve chamber observed_state); otherwise pure /proc
// introspection.
//
// Usage: tmux-tell-claude doctor [--format text|json] [--allow-active-agents]
//
// --allow-active-agents (#791): softens the stale-binary class for
// chamber-side MCP subprocesses whose owning chamber has a fresh mid-turn
// observed_state (working / at-rest-in-compaction / awaiting-operator per
// #448). The SYNC divergence classes — db-deleted, db-inode-mismatch —
// remain hard-fail. Mailmen, orphan MCPs (no chamber resolvable),
// idle-at-prompt chambers, and chambers with stale/absent observations also
// remain hard-fail. Intended for the deploy-workflow post-deploy smoke where
// a chamber caught mid-turn does not represent a substrate defect. See
// tmux-tell#791.
func runDoctorCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	formatFlag := fs.String("format", "text", "output format: text|json")
	allowActive := fs.Bool("allow-active-agents", false,
		"soften stale-binary agent-side MCP processes when the owning agent's "+
			"fresh observed AgentState is mid-turn; real divergence classes "+
			"stay hard-fail (#791)")
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB) — only opened when --allow-active-agents is set")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage: %s doctor [--format text|json] [--allow-active-agents] [--db PATH]\n", active.BinaryName)
		return exitUsage
	}
	format := *formatFlag
	switch format {
	case "text", "json":
	default:
		// Reject an explicit empty --format= too (consistent with rejecting any
		// other bad value; base rejected it via the hand-rolled default branch).
		// Bare `doctor` still defaults to text via the flag default, not "".
		return writeJSONError(stdout, stderr, fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}

	opts := classifyOpts{AllowActiveAgents: *allowActive}
	if *allowActive {
		// Fail-loud on store-open error: the operator asked us to soften based
		// on chamber observed_state (#448) — and we can't read it. Refusing to
		// soften is the substrate-honest response.
		s, err := store.Open(resolveDBPath(*dbPath))
		if err != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("open store (--allow-active-agents requires reading agent observed_state): %v", err),
				exitInternal)
		}
		snapshot, err := s.ObservedStateSnapshot(context.Background(), observedStateFreshness, time.Now())
		_ = s.Close() //nolint:errcheck // best-effort close on the read-only path
		if err != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("observed_state snapshot: %v", err),
				exitInternal)
		}
		opts.ObservedByChamber = snapshot
	}

	path, inode := canonicalDBIdentity()
	bindings, infos := gatherDoctorProcs()
	rep := classifyDoctorProcs(bindings, infos, path, inode, opts)

	if format == "json" {
		_ = writeJSONResult(stdout, rep)
	} else {
		renderDoctorReport(stdout, rep)
	}
	if rep.Divergent {
		return exitUnavailable
	}
	return exitOK
}
