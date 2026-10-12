package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite_ReplacesAndLeavesNoLitter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := Write(path, []byte("the first and longer contents"), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := Write(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Errorf("contents = %q; a rewrite must replace, not append or leave a tail", data)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

func TestWrite_HonoursPermissions(t *testing.T) {
	// CreateTemp hands back 0600, so a mode another service can read has to be
	// set deliberately.
	path := filepath.Join(t.TempDir(), "metrics.prom")
	if err := Write(path, []byte("x 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644", perm)
	}
}

func TestWrite_ReportsAMissingDirectory(t *testing.T) {
	err := Write(filepath.Join(t.TempDir(), "no-such-dir", "f"), []byte("x"), 0o600)
	if err == nil {
		t.Fatal("writing into a missing directory should be reported, not ignored")
	}
}

func TestWrite_ReportsAFailedRename(t *testing.T) {
	// A caller that points -prom or -history at a directory gets an error, not a
	// silent no-op and a metrics file that never appears.
	dir := t.TempDir()
	occupied := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(filepath.Join(occupied, "child"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Write(occupied, []byte("x"), 0o600); err == nil {
		t.Fatal("renaming over a non-empty directory should be reported")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("failed write left %s behind", e.Name())
		}
	}
}
