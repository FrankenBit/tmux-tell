package cli

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// doctorProcInfo augments a dbBinding with doctor-only /proc-derived context —
// which role the process plays (mailman vs chamber-side MCP) and, for the MCP
// case, which chamber owns it. Populated by gatherDoctorProcs; consumed by
// classifyDoctorProcs when the --allow-active-chambers flag is set (#791).
//
// Kept SEPARATE from dbBinding so whoami_db's wire schema is unchanged — this
// is a doctor-internal augmentation, not part of the tmux-tell.whoami_db tool's
// contract.
type doctorProcInfo struct {
	// Role is "mailman" | "mcp" | "unknown".
	//   - "mailman":  `serve --agent X` process — the host-side systemd-user
	//     delivery daemon for chamber X. NEVER softened: mailmen must always be
	//     on the canonical binary (deploy replaces atomically). A stale-binary
	//     mailman is a REAL divergence regardless of chamber activity.
	//   - "mcp":      `mcp` verb — chamber-side MCP subprocess (child of a
	//     claude/codex chamber). Async-refresh applies: chamber picks up the
	//     new binary on next MCP restart (natural boundary or /mcp cycle).
	//     Softening APPLIES when the owning chamber is mid-turn.
	//   - "unknown":  neither pattern matched. Not softened.
	Role string

	// ChamberName is the chamber that owns this process, when resolvable.
	//   - For "mailman": parsed from the `--agent <name>` argv value.
	//   - For "mcp":     extracted from the cgroup path
	//     (`chamber-<name>.slice` — chamber-launch discipline in
	//     /srv/CLAUDE.md § Chamber launches).
	//   - For "unknown": empty.
	//
	// Empty for the "mcp" role when cgroup does not carry a chamber-slice tag
	// (e.g. a stray MCP process launched outside the chamber wrapper). In that
	// case the softening cannot proceed — treated as orphan, still divergent.
	ChamberName string
}

// chamberSliceRegex extracts the chamber name from a cgroup path segment
// `chamber-<name>.slice`. The chamber-launch discipline (chamber-{claude,codex}.sh
// via /srv/CLAUDE.md § Chamber launches) places every chamber process in a
// systemd-user slice named `chamber-<lower-case-chamber-name>.slice`; child MCP
// subprocesses inherit that cgroup at fork.
//
// The pattern matches `chamber-` + one or more chars that are neither `.` nor `/`
// + `.slice`. Non-greedy on the name so nested-slice or path-junk after the
// segment does not swallow more than one component. The literal `chamber.slice`
// (chamber-root slice, no hyphen) does NOT match — the hyphen is required.
var chamberSliceRegex = regexp.MustCompile(`chamber-([^./]+)\.slice`)

// procCmdlineForPID / procCgroupForPID are the /proc readers the resolver uses,
// kept as package vars so tests can inject synthetic process fixtures without a
// real /proc.
var (
	procCmdlineForPID = realProcCmdlineForPID
	procCgroupForPID  = realProcCgroupForPID
)

// realProcCmdlineForPID reads /proc/<pid>/cmdline and returns the argv as a
// slice (\0-terminated fields split; trailing empty fields trimmed). Returns
// an empty slice on any error — the caller treats that as "unknown role".
func realProcCmdlineForPID(pid int) []string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil
	}
	// /proc/<pid>/cmdline uses \0 as the argv separator and typically ends in
	// a trailing \0. Split + trim empties.
	parts := strings.Split(string(raw), "\x00")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// realProcCgroupForPID reads /proc/<pid>/cgroup verbatim. cgroup v2 renders a
// single line `0::<path>`; cgroup v1 can carry multiple hierarchies. The chamber
// resolver runs the regex over the whole payload so both layouts work — the
// `chamber-<name>.slice` marker appears on any hierarchy that hosts the chamber
// slice, and matching only the marker (not the leading `0::` framing) keeps this
// version-neutral.
func realProcCgroupForPID(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return ""
	}
	return string(raw)
}

// resolveDoctorProcInfo classifies a live process into (role, chamberName) by
// reading its cmdline + cgroup (#791). Pure over its inputs — the /proc reads
// are injected via package-var stubs above so this is table-testable.
//
// Rules — the tuple below is the whole decision:
//
//	verb (argv[1] normalised)  argv `--agent X`  cgroup `chamber-Y.slice`  →  (role, chamber)
//	-------------------------  ----------------  ----------------------  -------------------
//	"serve"                    X (any)           (ignored)                 mailman, X
//	"mcp"                      —                 Y                         mcp, Y
//	"mcp"                      —                 (missing)                 mcp, "" (orphan)
//	other / unresolvable       —                 —                         unknown, ""
//
// The "mailman with --agent argv" rule takes precedence over cgroup for
// substrate-honesty: `serve --agent X` is the mailman for chamber X regardless
// of which slice systemd placed it in, and the cmdline is the daemon's own
// self-identification. For the `mcp` verb the argv carries no chamber name (the
// chamber-side subprocess resolves identity via TMUX_AGENT_NAME / TMUX_PANE at
// runtime), so cgroup is the substrate-authoritative source.
func resolveDoctorProcInfo(pid int) doctorProcInfo {
	cmdline := procCmdlineForPID(pid)
	if len(cmdline) < 2 {
		return doctorProcInfo{Role: "unknown"}
	}
	verb := cmdline[1]
	switch verb {
	case "serve":
		// `serve --agent <name>` — find the --agent argv pair.
		for i := 2; i < len(cmdline); i++ {
			if cmdline[i] == "--agent" && i+1 < len(cmdline) {
				return doctorProcInfo{Role: "mailman", ChamberName: cmdline[i+1]}
			}
			// Support `--agent=<name>` shape too, defensively.
			if strings.HasPrefix(cmdline[i], "--agent=") {
				return doctorProcInfo{Role: "mailman", ChamberName: strings.TrimPrefix(cmdline[i], "--agent=")}
			}
		}
		// `serve` without --agent shouldn't happen in the systemd-unit path but
		// keep the classification honest — it's a serve process, just missing
		// its chamber tag.
		return doctorProcInfo{Role: "mailman"}
	case "mcp":
		cgroup := procCgroupForPID(pid)
		if m := chamberSliceRegex.FindStringSubmatch(cgroup); len(m) == 2 {
			return doctorProcInfo{Role: "mcp", ChamberName: m[1]}
		}
		// MCP subprocess outside any chamber slice — orphan. Softening cannot
		// proceed (no chamber-attention to consult); the caller still classifies
		// stale binary as divergent.
		return doctorProcInfo{Role: "mcp"}
	default:
		return doctorProcInfo{Role: "unknown"}
	}
}
