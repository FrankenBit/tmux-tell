package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenReadOnly_RejectsMissingFile is the sandbox-visible entry gate: a
// caller pointing at a path that doesn't exist gets a clean sentinel-shaped
// error, not SQLite's "unable to open database file" that would suggest a
// mode-bit remedy.
func TestOpenReadOnly_RejectsMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.db")
	_, err := OpenReadOnly(missing)
	if err == nil {
		t.Fatal("expected error for missing DB, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should name the missing-file remedy: %v", err)
	}
}

// TestOpenReadOnly_RejectsMemory documents the ":memory:" refusal — the RO
// mode on an empty in-memory DB would yield a schema-less handle, which is
// never what a caller means (they'd get "no such table" on every query).
func TestOpenReadOnly_RejectsMemory(t *testing.T) {
	_, err := OpenReadOnly(":memory:")
	if err == nil {
		t.Fatal("expected error for :memory:, got nil")
	}
	if !strings.Contains(err.Error(), ":memory:") {
		t.Errorf("error should name :memory: as the refused shape: %v", err)
	}
}

// TestOpenReadOnly_ReadsPopulatedDB is the happy path: a store populated
// through the writer opens cleanly for read and returns the same data.
func TestOpenReadOnly_ReadsPopulatedDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	ctx := context.Background()
	if err := seed.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	got, err := ro.GetAgent(ctx, "alice")
	if err != nil {
		t.Fatalf("GetAgent under RO: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Name = %q, want alice", got.Name)
	}
	if got.PaneID != "%1" {
		t.Errorf("PaneID = %q, want %%1", got.PaneID)
	}
}

// TestOpenReadOnly_RefusesWrites is the load-bearing invariant: any mutating
// operation through the RO handle must surface SQLite's readonly rejection,
// so a caller that mis-classifies a verb as read-only fails-loud instead of
// silently no-op'ing writes.
func TestOpenReadOnly_RefusesWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	err = ro.UpsertAgent(context.Background(), "bob", "%2")
	if err == nil {
		t.Fatal("expected write refusal, got nil error")
	}
	// The exact wording is SQLite-driver-defined; assert on the class rather
	// than the phrasing so a driver upgrade doesn't churn this test.
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "readonly") && !strings.Contains(msg, "read-only") && !strings.Contains(msg, "read only") {
		t.Errorf("write error should signal readonly rejection: %v", err)
	}
}

// TestOpenReadOnly_SucceedsOnReadOnlyFile is the actual #722 sandbox
// reproduction: the DB file mode is 0444, and Open would fail on the healing
// DML that runs unconditionally. OpenReadOnly must succeed and read cleanly.
func TestOpenReadOnly_SucceedsOnReadOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	ctx := context.Background()
	if err := seed.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	// Chmod the DB + sidecars to read-only to force the sandbox failure mode.
	// Also chmod the parent dir so SQLite can't create fresh sidecars.
	dir := filepath.Dir(path)
	for _, sfx := range []string{"", "-wal", "-shm"} {
		p := path + sfx
		if _, statErr := os.Stat(p); statErr == nil {
			if err := os.Chmod(p, 0o444); err != nil {
				t.Fatalf("chmod %s: %v", p, err)
			}
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
		for _, sfx := range []string{"", "-wal", "-shm"} {
			_ = os.Chmod(path+sfx, 0o644)
		}
	})

	// Sanity — Open should now fail. This is the #722 signature.
	if _, err := Open(path); err == nil {
		t.Fatal("Open on 0444 DB should fail (sandbox reproduction); did not")
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly on 0444 DB failed: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	got, err := ro.GetAgent(ctx, "alice")
	if err != nil {
		t.Fatalf("GetAgent under RO on 0444 DB: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Name = %q, want alice", got.Name)
	}
}
