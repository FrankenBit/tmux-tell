package store_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/testpin"
)

// The pins in this file are the *cross-process* counterparts to
// TestPin_AtomicCapEnforcement_CeilingUnderConcurrency in pin_test.go.
// Same architectural commitment (`AtomicCapEnforcement`), different
// concurrency axis: that one exercises BeginTx atomicity inside one
// *Store via N goroutines sharing one *sql.DB; this one exercises
// SQLite's file-level RESERVED lock + _txlock=immediate +
// busy_timeout across distinct OS-level processes — the load-bearing
// real-world case (mailman daemons + claude-msg CLI invocations + MCP
// server children all hit the same messages.db from separate
// processes).
//
// The probe binary that each child runs lives under
// internal/store/cmd/concurrency-probe and exits with one of three
// documented codes (0 accepted / 2 cap-rejected / 1 unexpected).
// buildProbeBinary compiles it once per test run into the test's
// temp dir; each child invocation is a cheap fork+exec of the built
// binary.

// buildProbeBinary compiles the concurrency-probe binary into the
// test's temp dir and returns its path. Failure of `go build` fails
// the test — no way to exercise the cross-process axis without it.
func buildProbeBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "concurrency-probe")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/concurrency-probe")
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build concurrency-probe: %v\n%s", err, outBytes)
	}
	return out
}

// runProbe invokes the probe binary against the given DB / sender /
// recipient / cap / mode and returns the exit code. exec.ExitError is
// translated to its code so the caller can branch on the documented
// 0/1/2 contract.
func runProbe(t *testing.T, probe, db, from, to string, capN int, mode string) int {
	t.Helper()
	cmd := exec.Command(probe,
		"--db", db,
		"--from", from,
		"--to", to,
		"--cap", itoa(capN),
		"--mode", mode,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		// The probe wrote to stderr on its way out; surface that on
		// any code other than the cap-rejected one so debugging an
		// unexpected failure doesn't require re-running.
		if ee.ExitCode() != 2 {
			t.Logf("probe (code=%d): %s", ee.ExitCode(), out)
		}
		return ee.ExitCode()
	}
	t.Fatalf("probe exec: %v\n%s", err, out)
	return -1
}

// itoa is a local helper so the file doesn't need strconv just for
// a flag-formatting concern.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var (
		buf [20]byte
		i   = len(buf)
		neg bool
	)
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// PIN: caps are ceilings, never floors — atomic under CROSS-PROCESS
// concurrency via SQLite's file-level RESERVED lock + _txlock=immediate
// + busy_timeout. N separate processes, each opening its own *Store
// against the same messages.db, attempt inserts against the same
// recipient with MaxRecipientQueue=cap. Exactly `cap` must succeed;
// the rest must report ErrRecipientQueueFull; the table state must
// match the accept-count.
//
// Without _txlock=immediate, two processes could each read depth=X,
// each decide X+1 ≤ cap, and each insert — overshooting by up to
// (concurrent_writers - 1). The in-process pin in pin_test.go can't
// catch this regression (single *sql.DB, no cross-connection lock
// race); this pin can. Sibling-not-replacement: keep both.
//
// Surveyor's #29 review (b) named this as the load-bearing real-
// world case (mailman + CLI + MCP children all separate processes
// against /var/lib/tmux-msg/messages.db). Tracked as #33.
func TestPin_AtomicCapEnforcement_CrossProcessCeilingForSingleInsert(t *testing.T) {
	testpin.Triage(t, "AtomicCapEnforcement",
		"caps are ceilings under cross-process concurrency — SQLite file-level RESERVED lock + _txlock=immediate atomicity holds across distinct OS-level processes")

	if testing.Short() {
		t.Skip("cross-process pin requires `go build` of concurrency-probe; skipped in -short mode")
	}

	const (
		cap         = 5
		concurrentN = 20
		// Sender-backlog cap defaults are bypassed in the probe
		// (MaxSenderBacklog left zero). The recipient cap is the
		// thing under test.
	)

	probe := buildProbeBinary(t)
	dbPath := filepath.Join(t.TempDir(), "xprocess.db")

	// Seed alice + bob via a parent-process *Store. The child probes
	// then re-Open the same file from N separate processes.
	{
		s, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("seed open: %v", err)
		}
		ctx := context.Background()
		if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
			t.Fatalf("seed sender: %v", err)
		}
		if err := s.UpsertAgent(ctx, "bob", "%2"); err != nil {
			t.Fatalf("seed recipient: %v", err)
		}
		_ = s.Close()
	}

	var (
		wg            sync.WaitGroup
		acceptedCount atomic.Int64
		rejectedCount atomic.Int64
		otherErrors   atomic.Int64
	)
	wg.Add(concurrentN)
	for i := 0; i < concurrentN; i++ {
		go func() {
			defer wg.Done()
			switch runProbe(t, probe, dbPath, "alice", "bob", cap, "single") {
			case 0:
				acceptedCount.Add(1)
			case 2:
				rejectedCount.Add(1)
			default:
				otherErrors.Add(1)
			}
		}()
	}
	wg.Wait()

	if otherErrors.Load() != 0 {
		t.Fatalf("got %d unexpected probe exit codes; check go test -v output for stderr",
			otherErrors.Load())
	}
	if acceptedCount.Load() != cap {
		t.Errorf("cross-process accepted = %d, want exactly cap=%d (overshoot indicates _txlock=immediate or BEGIN IMMEDIATE not holding across processes)",
			acceptedCount.Load(), cap)
	}
	if rejectedCount.Load() != concurrentN-cap {
		t.Errorf("cross-process rejected = %d, want %d",
			rejectedCount.Load(), concurrentN-cap)
	}

	// Verify the table state matches the accept-count. If the cap
	// held but rows leaked some other way, this catches it.
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("post-test open: %v", err)
	}
	defer s.Close()
	depth, err := s.RecipientQueueDepth(context.Background(), "bob")
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if depth != cap {
		t.Errorf("post-test queue depth = %d, want %d (cap-as-ceiling held but rows leaked another way?)",
			depth, cap)
	}
}

// PIN: caps are ceilings under cross-process concurrency for the
// two-row InsertMessagePair path too. Each pair attempt needs 2 free
// slots — with cap=5 and pair attempts running concurrently from
// separate processes, exactly 2 pairs must succeed (4 rows land, 1
// cap slot left unfilled since the third pair would need 2 and only
// 1 is available). Same architectural commitment as the single-insert
// variant; this pin catches a regression that ONLY broke the pair
// path while leaving the single path intact.
//
// Without the BEGIN IMMEDIATE wrapping holding across processes,
// concurrent pair attempts could each decide they fit, all insert,
// and overshoot beyond cap. Cross-process axis sister to the in-
// process TestInsertMessagePair_AtomicityUnderConcurrency in
// messages_concurrent_test.go.
func TestPin_AtomicCapEnforcement_CrossProcessCeilingForInsertPair(t *testing.T) {
	testpin.Triage(t, "AtomicCapEnforcement",
		"caps are ceilings under cross-process concurrency for InsertMessagePair — both rows must land or neither, atomicity across separate processes")

	if testing.Short() {
		t.Skip("cross-process pin requires `go build` of concurrency-probe; skipped in -short mode")
	}

	const (
		cap         = 5
		concurrentN = 8
		// Each pair tries to claim 2 slots; with cap=5 only 2 pairs
		// can fit (4 rows), the third would need slot 6.
		wantPairsAccepted = 2
		wantRowsAfter     = wantPairsAccepted * 2
	)

	probe := buildProbeBinary(t)
	dbPath := filepath.Join(t.TempDir(), "xprocess-pair.db")

	// Seed alice + bob in the parent process.
	{
		s, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("seed open: %v", err)
		}
		ctx := context.Background()
		if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
			t.Fatalf("seed sender: %v", err)
		}
		if err := s.UpsertAgent(ctx, "bob", "%2"); err != nil {
			t.Fatalf("seed recipient: %v", err)
		}
		_ = s.Close()
	}

	var (
		wg            sync.WaitGroup
		acceptedCount atomic.Int64
		rejectedCount atomic.Int64
		otherErrors   atomic.Int64
	)
	wg.Add(concurrentN)
	for i := 0; i < concurrentN; i++ {
		go func() {
			defer wg.Done()
			switch runProbe(t, probe, dbPath, "alice", "bob", cap, "pair") {
			case 0:
				acceptedCount.Add(1)
			case 2:
				rejectedCount.Add(1)
			default:
				otherErrors.Add(1)
			}
		}()
	}
	wg.Wait()

	if otherErrors.Load() != 0 {
		t.Fatalf("got %d unexpected probe exit codes; check go test -v output for stderr",
			otherErrors.Load())
	}
	if acceptedCount.Load() != wantPairsAccepted {
		t.Errorf("cross-process pair accepted = %d, want exactly %d (cap=%d, each pair claims 2 slots)",
			acceptedCount.Load(), wantPairsAccepted, cap)
	}
	if rejectedCount.Load() != concurrentN-wantPairsAccepted {
		t.Errorf("cross-process pair rejected = %d, want %d",
			rejectedCount.Load(), concurrentN-wantPairsAccepted)
	}

	// Pair atomicity: each accepted pair lands 2 rows; the final
	// table depth must be exactly wantRowsAfter. A regression where
	// one row of a pair landed but the other didn't (atomicity break)
	// surfaces as a depth that's odd or off by one.
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("post-test open: %v", err)
	}
	defer s.Close()
	depth, err := s.RecipientQueueDepth(context.Background(), "bob")
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if depth != wantRowsAfter {
		t.Errorf("post-test queue depth = %d, want %d (pair atomicity break would show as odd/off-by-one depth)",
			depth, wantRowsAfter)
	}
}
