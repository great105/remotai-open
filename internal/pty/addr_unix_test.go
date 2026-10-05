//go:build linux || darwin

package pty

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPipeNameLongRuntimeDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("nested-", 20))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	path := pipeName("0123456789abcdef")
	if len(path) > 103 {
		t.Fatalf("socket path exceeds Darwin limit: %d bytes", len(path))
	}
	if path != pipeName("0123456789abcdef") || path == pipeName("fedcba9876543210") {
		t.Fatal("address must be stable and session-specific")
	}
	t.Setenv("XDG_RUNTIME_DIR", dir+"-other")
	if path == pipeName("0123456789abcdef") {
		t.Fatal("different runtime directories must not share a socket")
	}
	server, err := createUnixServer(path)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("short socket must be inside an owner-only directory: %v", err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket must remain owner-only: %v, %v", info, err)
	}
}

func TestShortSocketRejectsUnsafeDirectory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), strings.Repeat("long", 40)))
	path := pipeName("unsafe")
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dir)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := preparePrivateSocketDir(path); err == nil {
		t.Fatal("a shared directory must not host a private shell socket")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if err := preparePrivateSocketDir(path); err == nil {
		t.Fatal("a symlink must not redirect the private socket directory")
	}
}

func TestPipeNamePreservesExistingShortAddress(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/example")
	if got := pipeName("short"); got != "/tmp/example/remotai-pty-short.sock" {
		t.Fatal(got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", "/tmp")
	if got := pipeName("short"); got != "/tmp/remotai-pty-short.sock" {
		t.Fatal(got)
	}
}
