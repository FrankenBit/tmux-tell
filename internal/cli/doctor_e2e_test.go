package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		if _, _, _, found := openDBHandle(mcp.Process.Pid); found {
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

	// doctor (fresh process, same XDG) must flag the divergence.
	doctor := exec.Command(bin, "doctor", "--format", "json")
	doctor.Env = env
	// Output() (not CombinedOutput) so the JSON parse sees stdout only — Run emits
	// the #440 Phase-3 deprecation WARNs (legacy DB/config path) to stderr, which
	// would otherwise corrupt the machine-readable stream. Parse stdout, WARNs on
	// stderr is the correct consumer contract.
	out, derr := doctor.Output()
	exit := 0
	if ee, ok := derr.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if derr != nil {
		t.Fatalf("run doctor: %v\n%s", derr, out)
	}
	if exit == 0 {
		t.Errorf("doctor exit=0, want non-zero (an orphaned mcp is present)\n%s", out)
	}

	var rep doctorReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("parse doctor json: %v\n%s", err, out)
	}
	found := false
	for _, p := range rep.Procs {
		if p.PID != mcp.Process.Pid {
			continue
		}
		found = true
		if !p.Divergent {
			t.Errorf("orphaned mcp pid %d not flagged divergent: %+v", p.PID, p)
		}
		if !p.DBDeleted {
			t.Errorf("orphaned mcp pid %d not marked db_deleted: %+v", p.PID, p)
		}
	}
	if !found {
		t.Errorf("doctor did not list our mcp pid %d\n%s", mcp.Process.Pid, out)
	}
}
