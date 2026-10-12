// Package atomicfile replaces a file's contents in one step.
//
// Both files this tool writes are read by something else on its own schedule —
// the next run reads the state file, node_exporter reads the metrics file — and
// a partial write destroys both: a truncated state file loses every trend the
// fleet has accumulated, and a truncated metrics file is a parse error that
// discards the whole scrape rather than the one bad line.
package atomicfile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Write creates path with the given contents and permissions, replacing any
// existing file in a single rename.
//
// The temporary file goes in the destination directory because rename is only
// atomic within one filesystem, and is removed on every failure path, so an
// interrupted run leaves the previous contents in place rather than a
// half-written file plus litter.
func Write(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rack-sentinel-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}

	// CreateTemp makes the file 0600, which is right for state and wrong for
	// anything another service has to read.
	if err := os.Chmod(name, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
