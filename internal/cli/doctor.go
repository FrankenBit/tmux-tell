package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// doctorProc is one live tmux-msg process in the doctor report (#348): its
// /proc-read DB binding plus the verdict of comparing that binding against the
// canonical DB the binary-on-disk resolves to.
type doctorProc struct {
	dbBinding
	// Canonical is true when the process's open DB inode matches the canonical
	// DB's inode (it's writing where fresh mailmen read).
	Canonical bool `json:"canonical"`
	// Divergent is true when the process is writing somewhere a fresh mailman
	// can't see — an orphaned (unlinked) inode, a different inode than
	// canonical, or running a since-replaced ("(deleted)") binary.
	Divergent bool `json:"divergent"`
	// Verdict is a one-line human explanation of the classification.
	Verdict string `json:"verdict"`
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

// classifyDoctorProcs is the pure core (#348): given the live processes' DB
// bindings and the canonical DB identity, decide per-process whether each is
// canonical or divergent, and whether the fleet as a whole has a divergence.
// Pure + table-tested — the /proc enumeration that feeds it is the impure shell
// (gatherDoctorProcs).
//
// A process diverges when it's writing somewhere a freshly-started mailman
// can't see:
//   - db_deleted: the open DB handle's dirent is gone (the deploy-mv orphan) —
//     the operationally-critical case; the writes vanish from every other surface
//   - a different open DB inode than canonical (bound to another live file)
//   - a "(deleted)" binary path: the process is running since-replaced code, the
//     pre-deploy-process smell even if its DB still happens to match
//
// A process with no open DB handle (Note set) is reported but not counted
// divergent — an MCP server that hasn't opened the DB, or an in-memory store.
func classifyDoctorProcs(bindings []dbBinding, canonicalPath string, canonicalInode uint64) doctorReport {
	rep := doctorReport{CanonicalPath: canonicalPath, CanonicalInode: canonicalInode}
	for _, b := range bindings {
		p := doctorProc{dbBinding: b}
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
			p.Divergent = true
			p.Verdict = "running a since-replaced binary (process predates the on-disk binary)"
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
// binary still matches) and reads each one's DB binding (#348). This is the
// impure shell around classifyDoctorProcs.
func gatherDoctorProcs() []dbBinding {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	wantBins := map[string]bool{active.BinaryName: true}
	if active.DeprecatedAlias != "" {
		wantBins[active.DeprecatedAlias] = true // pre-rename processes still count
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
	for _, pid := range pids {
		bindings = append(bindings, collectBinding(pid, false))
	}
	return bindings
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
// the recovery action when anything diverged.
func renderDoctorReport(stdout io.Writer, rep doctorReport) {
	fmt.Fprintf(stdout, "CANONICAL\t%s", rep.CanonicalPath)
	if rep.CanonicalInode != 0 {
		fmt.Fprintf(stdout, " (inode %d)", rep.CanonicalInode)
	} else {
		fmt.Fprint(stdout, " (no file at canonical path)")
	}
	fmt.Fprintln(stdout)
	for _, p := range rep.Procs {
		mark := "✓"
		if p.Divergent {
			mark = "✗"
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
	} else {
		fmt.Fprintln(stdout, "OK: all live processes bound to the canonical DB.")
	}
}

// runDoctorCLI implements `tmux-msg-claude doctor` (#348): walk live tmux-msg
// processes, compare each one's open DB binding against the canonical DB, emit
// a substrate-honest report, and exit non-zero on any divergence so the command
// is usable as a runbook gate. Touches no DB — pure /proc introspection.
//
// Usage: tmux-msg-claude doctor [--format text|json]
func runDoctorCLI(args []string, stdout, stderr io.Writer) int {
	format := "text"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--format":
			if i+1 < len(args) {
				format = args[i+1]
				i++
			}
		case "--format=text":
			format = "text"
		case "--format=json":
			format = "json"
		default:
			fmt.Fprintf(stderr, "usage: %s doctor [--format text|json]\n", active.BinaryName)
			return exitUsage
		}
	}
	switch format {
	case "", "text", "json":
	default:
		return writeJSONError(stdout, stderr, fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}

	path, inode := canonicalDBIdentity()
	rep := classifyDoctorProcs(gatherDoctorProcs(), path, inode)

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
