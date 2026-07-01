package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// inodeOf returns the inode of a path via syscall.Stat (test helper).
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Ino
}

// TestOpenDBHandle_FindsOpenDB proves the /proc/fd binding read finds this
// process's open *.db handle and reports its real inode + non-deleted state
// (#348). Sequential in-package tests + defer-close discipline mean no other
// *.db handle is open while this runs, so the first match is ours.
func TestOpenDBHandle_FindsOpenDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	f, err := os.Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // best-effort close
	wantInode := inodeOf(t, dbPath)

	path, inode, deleted, found, _ := openDBHandle(os.Getpid())
	if !found {
		t.Fatal("openDBHandle did not find the open *.db handle")
	}
	if path != dbPath {
		t.Errorf("path = %q, want %q", path, dbPath)
	}
	if inode != wantInode {
		t.Errorf("inode = %d, want %d", inode, wantInode)
	}
	if deleted {
		t.Error("deleted = true for a live (non-unlinked) file")
	}
}

// TestOpenDBHandle_OrphanInode is the load-bearing case: a process holds a *.db
// open, then the dirent is removed out from under it (the deploy-`mv` orphan).
// The handle must still resolve — deleted=true, and the SAME inode as before the
// unlink — because that's the invisible-to-sqlite3 inode the incident was about.
func TestOpenDBHandle_OrphanInode(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	f, err := os.Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // best-effort close
	wantInode := inodeOf(t, dbPath)

	if err := os.Remove(dbPath); err != nil { // unlink while the fd stays open
		t.Fatal(err)
	}

	path, inode, deleted, found, _ := openDBHandle(os.Getpid())
	if !found {
		t.Fatal("openDBHandle lost the handle after unlink — orphan inode must stay visible")
	}
	if !deleted {
		t.Error("deleted = false, want true for an unlinked-but-open handle")
	}
	if inode != wantInode {
		t.Errorf("orphan inode = %d, want %d (must resolve the open handle, not the gone dirent)", inode, wantInode)
	}
	if path != dbPath {
		t.Errorf("path = %q, want %q (the as-opened path, minus the kernel ' (deleted)' marker)", path, dbPath)
	}
}

// TestCollectBinding_Self confirms the self-binding assembles the pid, a binary
// path, and (when processStart is set) a started_at timestamp.
func TestCollectBinding_Self(t *testing.T) {
	prev := processStart
	processStart = time.Now()
	defer func() { processStart = prev }()

	b := collectBinding(os.Getpid(), true)
	if b.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", b.PID, os.Getpid())
	}
	if b.BinaryPath == "" {
		t.Error("BinaryPath empty — /proc/self/exe should resolve the test binary")
	}
	if b.StartedAt == "" {
		t.Error("StartedAt empty despite processStart set")
	}
}
