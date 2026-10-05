// Package atomicfile provides crash-safe file writes via temp+fsync+rename.
//
// The app runs as a Scheduled Task that is force-stopped on updates, so a plain
// os.WriteFile can leave config.json / session stores truncated if the process
// dies mid-write. WriteFile here writes to a sibling ".tmp" file, fsyncs it, and
// atomically renames it over the target — a torn write can never be observed.
package atomicfile

import (
	"os"
	"path/filepath"
)

// WriteFile atomically writes data to path with the given permissions.
// It writes to path+".tmp" in the same directory (same filesystem, so Rename is
// atomic), fsyncs, then renames over path. On any error the temp file is removed.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}

	// Best-effort directory fsync so the rename itself is durable (POSIX). No-op
	// / harmless error on Windows where directories can't be opened this way.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Quarantine renames a corrupt file out of the way (path -> path+".corrupt") so
// a bad load does not get silently overwritten with empty defaults and can be
// recovered later. Returns the quarantine path (best-effort; errors ignored by
// callers that just want to avoid clobbering good data).
func Quarantine(path string) (string, error) {
	bad := path + ".corrupt"
	_ = os.Remove(bad) // keep only the latest corrupt copy
	return bad, os.Rename(path, bad)
}
