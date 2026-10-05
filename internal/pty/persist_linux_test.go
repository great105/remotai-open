//go:build linux

package pty

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// safeReader accumulates decoded PTY output from a host client for assertions.
type safeReader struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *safeReader) drain(pc ptyConn) {
	b := make([]byte, 4096)
	for {
		n, err := pc.Read(b)
		if n > 0 {
			r.mu.Lock()
			r.buf.Write(b[:n])
			r.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (r *safeReader) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Contains(r.buf.String(), s)
}

func waitFor(t *testing.T, r *safeReader, marker string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if r.has(marker) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q", marker)
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestPersistAcrossRestart exercises the core "mini-tmux" guarantee end-to-end on
// Linux against the real built binary: a shell spawned in a host process survives
// the client disconnecting (simulating a remotai restart), the reconnecting
// client gets the prior scrollback, input still works, DEC modes are carried in
// Hello, and an explicit kill tears the host down. Skipped unless
// build/remotai-linux-amd64 exists.
func TestPersistAcrossRestart(t *testing.T) {
	exe, err := filepath.Abs(filepath.FromSlash("../../build/remotai-linux-amd64"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Skip("build/remotai-linux-amd64 not present — cross-build it first")
	}

	// Пин сокет-дир на /tmp: детерминирует путь для host+test и обходит
	// нестабильность WSL2 /run/user/0 (на реальном Linux XDG_RUNTIME_DIR надёжен,
	// а system-unit без сессии и так фолбэчит на os.TempDir — см. addr_linux.go).
	t.Setenv("XDG_RUNTIME_DIR", "/tmp")
	id := randomID()
	tmp := t.TempDir()

	host := exec.Command(exe, "--pty-host", "--id", id, "--cwd", tmp, "--shell", "bash", "--cols", "80", "--rows", "24")
	host.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := host.Start(); err != nil {
		t.Fatalf("spawn host: %v", err)
	}
	hostPID := host.Process.Pid
	go host.Wait()
	defer func() {
		if p, err := os.FindProcess(hostPID); err == nil {
			_ = p.Kill()
		}
	}()

	// First client: connect, run a marker command.
	pc1, err := dialHost(id, 5*time.Second, 80, 24)
	if err != nil {
		t.Fatalf("dial host #1: %v", err)
	}
	r1 := &safeReader{}
	go r1.drain(pc1)
	if _, err := pc1.Write([]byte("echo MARKER_ONE\n")); err != nil {
		t.Fatalf("write #1: %v", err)
	}
	waitFor(t, r1, "MARKER_ONE", 8*time.Second)

	// DEC-режимы переживают рестарт: включаем alt-screen сырым ESC — хост обязан
	// отдать 1049 в HelloMsg.Modes при следующем attach.
	if _, err := pc1.Write([]byte("printf '\\033[?1049h'\n")); err != nil {
		t.Fatalf("write modes: %v", err)
	}
	waitFor(t, r1, "\x1b[?1049", 8*time.Second)

	// Simulate remotai restart: detach (host stays alive).
	if err := pc1.Close(); err != nil {
		t.Logf("close pc1: %v", err)
	}

	// Reconnect: scrollback must carry the prior marker.
	pc2, err := dialHost(id, 5*time.Second, 0, 0)
	if err != nil {
		t.Fatalf("dial host #2 (reattach): %v", err)
	}
	r2 := &safeReader{}
	go r2.drain(pc2)
	waitFor(t, r2, "MARKER_ONE", 5*time.Second) // came from the snapshot

	// Режимы, включённые до «рестарта», приехали в Hello от хоста.
	if !slices.Contains(pc2.hostModes(), 1049) {
		t.Fatalf("hostModes=%v, want содержит 1049 (alt-screen)", pc2.hostModes())
	}

	// Input still works after reattach.
	if _, err := pc2.Write([]byte("echo MARKER_TWO\n")); err != nil {
		t.Fatalf("write #2: %v", err)
	}
	waitFor(t, r2, "MARKER_TWO", 8*time.Second)

	// Explicit kill tears the host down.
	if err := pc2.(*sockClient).kill(); err != nil {
		t.Logf("kill: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(hostPID) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("host pid %d still alive after kill", hostPID)
}
