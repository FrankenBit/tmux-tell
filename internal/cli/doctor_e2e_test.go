package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestDoctor_E2E_FlagsOrphanedMCP is the #348 acceptance e2e: start a real
// tmux-msg MCP server holding the canonical DB open, unlink the DB out from
// under it (the deploy-`mv`/`rm` orphan), then run `doctor` and confirm it
// flags that process as divergent — exit non-zero + db_deleted in the report.
//
// This closes the loop the unit tests can't: the pure classifier proves the
// verdict logic and the /proc-fd core proves orphan-inode resolution, but only
// a live process proves the wiring end-to-end. Isolated from the live chamber
// fleet (which doctor also enumerates) by asserting on OUR pid specifically.
func TestDoctor_E2E_FlagsOrphanedMCP(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("doctor walks /proc — Linux-only")
	}
	if testing.Short() {
		t.Skip("builds a binary + starts a subprocess")
	}

	bin := filepath.Join(t.TempDir(), "tmux-tell-claude") // basename must match active.BinaryName
	build := exec.Command("go", "build", "-o", bin, "git.frankenbit.de/frankenbit/tmux-tell/cmd/tmux-tell-claude")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build tmux-tell-claude: %v\n%s", err, out)
	}

	xdg := t.TempDir()
	canon := filepath.Join(xdg, "tmux-tell", "messages.db")
	// Force the XDG default (clear any inherited DB-path env overrides, new + legacy).
	env := append(os.Environ(), "XDG_DATA_HOME="+xdg, "TMUX_TELL_DB=", "CLAUDE_MSG_DB=")

	// Start `mcp`, which opens the canonical DB at startup and then blocks
	// reading stdin — keeping the file handle (and thus the inode) open.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	mcp := exec.Command(bin, "mcp")
	mcp.Env = env
	mcp.Stdin = stdinR
	if err := mcp.Start(); err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	_ = stdinR.Close() // parent keeps only the write end (held open to keep mcp alive)
	defer func() {
		_ = stdinW.Close()
		_ = mcp.Process.Kill()
		_, _ = mcp.Process.Wait()
	}()

	// Wait for the mcp process to actually open the DB handle.
	opened := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if _, _, _, found, _ := openDBHandle(mcp.Process.Pid); found {
			opened = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !opened {
		t.Fatal("mcp never opened a DB handle within 5s")
	}

	// The orphan: unlink the canonical DB while the mcp still holds it open.
	if err := os.Remove(canon); err != nil {
		t.Fatalf("unlink canonical DB: %v", err)
	}

	// Poll doctor's report for our mcp pid — #605.
	//
	// doctor's /proc walk (gatherDoctorProcs) takes a directory snapshot with
	// os.ReadDir("/proc") and then per-pid os.Readlink("/proc/<pid>/exe"). If a
	// live pid's exe symlink can't be resolved on that per-pid step (the process
	// exited between the snapshot and the readlink, or a transient EACCES from a
	// loaded runner), the pid drops silently from the walk. On the release-cut
	// runner this manifested as "doctor did not list our mcp pid" even though
	// openDBHandle above confirmed the DB fd IS open at test time — pure jitter.
	//
	// Retry the doctor invocation briefly if our pid isn't in the report but the
	// mcp process is still alive. If mcp has actually exited (rare, but possible
	// on a heavily loaded runner if the parent's stdinW handling races), fail
	// with an explicit diagnostic rather than the earlier confusing "not listed".
	//
	// Output() (not CombinedOutput) so the JSON parse sees stdout only — Run emits
	// the #440 Phase-3 deprecation WARNs (legacy DB/config path) to stderr, which
	// would otherwise corrupt the machine-readable stream. Parse stdout, WARNs on
	// stderr is the correct consumer contract.
	var out []byte
	var exit int
	var rep doctorReport
	found := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		doctor := exec.Command(bin, "doctor", "--format", "json")
		doctor.Env = env
		o, derr := doctor.Output()
		exit = 0
		if ee, ok := derr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else if derr != nil {
			t.Fatalf("run doctor: %v\n%s", derr, o)
		}
		out = o
		rep = doctorReport{}
		if err := json.Unmarshal(o, &rep); err != nil {
			t.Fatalf("parse doctor json: %v\n%s", err, o)
		}
		for _, p := range rep.Procs {
			if p.PID == mcp.Process.Pid {
				found = true
				break
			}
		}
		if found {
			break
		}
		// mcp missing from the report — retry only if it's still alive. Signal 0
		// is the standard null-signal alive probe on Linux (no side effect,
		// returns error if the pid is gone).
		if err := mcp.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("mcp pid %d exited before doctor could observe it (%v); this is not the /proc-scan slew #605 targets\n%s", mcp.Process.Pid, err, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatalf("doctor never listed our mcp pid %d within 3s (mcp still alive; /proc-scan slew persisted longer than the retry deadline)\n%s", mcp.Process.Pid, out)
	}

	if exit == 0 {
		t.Errorf("doctor exit=0, want non-zero (an orphaned mcp is present)\n%s", out)
	}
	for _, p := range rep.Procs {
		if p.PID != mcp.Process.Pid {
			continue
		}
		if !p.Divergent {
			t.Errorf("orphaned mcp pid %d not flagged divergent: %+v", p.PID, p)
		}
		if !p.DBDeleted {
			t.Errorf("orphaned mcp pid %d not marked db_deleted: %+v", p.PID, p)
		}
	}
}
