//go:build linux || darwin

package pty

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// pipeName is the unix-socket path for a session's persistent pty-host. Prefers
// $XDG_RUNTIME_DIR (per-user tmpfs, 0700, cleared on logout) so sockets don't
// leak into a world-listable /tmp; falls back to os.TempDir() when unset (e.g. a
// system service without a runtime dir — then the socket is 0600, see pipe_linux).
func pipeName(id string) string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, "remotai-pty-"+id+".sock")
	// Darwin's sockaddr_un allows 103 path bytes (Linux allows 107). Nested
	// TMPDIRs on macOS can exceed this even with a short session ID. Keep the
	// existing address when it fits, and hash the entire address otherwise so
	// separate runtime directories and users cannot collide. The fallback lives
	// in a private directory: portable Unix code cannot rely on socket modes
	// alone to exclude other users.
	if len(path) > 103 {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", os.Getuid(), path)))
		return fmt.Sprintf("/tmp/remotai-pty-%d-%x/host.sock", os.Getuid(), digest[:16])
	}
	return path
}

// Only the shortened address owns a directory here. Never chmod or remove the
// caller's XDG_RUNTIME_DIR, TMPDIR, or any existing shared directory.
func privateSocketDir(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(path) == "host.sock" && filepath.Dir(dir) == "/tmp" && strings.HasPrefix(filepath.Base(dir), fmt.Sprintf("remotai-pty-%d-", os.Getuid())) {
		return dir
	}
	return ""
}

func preparePrivateSocketDir(path string) error {
	dir := privateSocketDir(path)
	if dir == "" {
		return nil
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("PTY socket directory is not private: %s", dir)
	}
	return nil
}
