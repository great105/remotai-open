//go:build windows

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

// safeReader accumulates decoded PTY output from a pipeClient for assertions.
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

// size — сколько байт уже прочитано. Нужен проверке длинной истории: снапшот
// больше предела кадра приезжает кусками, и ждать надо именно объём.
func (r *safeReader) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Len()
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
	const synchronize = 0x00100000
	const waitTimeout = 0x00000102 // WAIT_TIMEOUT — handle not signaled → still running
	h, err := syscall.OpenProcess(synchronize, false, uint32(pid))
	if err != nil {
		return false // process gone
	}
	defer syscall.CloseHandle(h)
	ev, _ := syscall.WaitForSingleObject(h, 0)
	return ev == waitTimeout
}

// TestPersistAcrossRestart exercises the core "mini-tmux" guarantee end-to-end
// against the real built binary: a shell spawned in a host process survives the
// client disconnecting (simulating a remotai restart), the reconnecting client
// gets the prior scrollback, input still works, and an explicit kill tears the
// host down. Skipped unless build/remotai.exe exists.
func TestPersistAcrossRestart(t *testing.T) {
	exe, err := filepath.Abs(filepath.FromSlash("../../build/remotai.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Skip("build/remotai.exe not present — run scripts/build-local.ps1 first")
	}

	id := randomID()
	tmp := t.TempDir()
	const flags = 0x00000008 | 0x00000200 | 0x08000000 // DETACHED|NEW_GROUP|NO_WINDOW

	host := exec.Command(exe, "--pty-host", "--id", id, "--cwd", tmp, "--shell", "cmd", "--cols", "80", "--rows", "24")
	host.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
	if err := host.Start(); err != nil {
		t.Fatalf("spawn host: %v", err)
	}
	hostPID := host.Process.Pid
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
	if _, err := pc1.Write([]byte("echo MARKER_ONE\r")); err != nil {
		t.Fatalf("write #1: %v", err)
	}
	waitFor(t, r1, "MARKER_ONE", 8*time.Second)

	// DEC-режимы переживают рестарт: «приложение» включает alt-screen — хост
	// обязан отдать 1049 в HelloMsg.Modes при следующем attach. Ждём СЫРОЙ ESC
	// в выводе (эхо набранной команды содержит лишь текст "[char]27", не байт).
	if _, err := pc1.Write([]byte("powershell -command \"Write-Host ([char]27+'[?1049h')\"\r")); err != nil {
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
	if _, err := pc2.Write([]byte("echo MARKER_TWO\r")); err != nil {
		t.Fatalf("write #2: %v", err)
	}
	waitFor(t, r2, "MARKER_TWO", 8*time.Second)

	// ДЛИННАЯ история переживает переподключение.
	//
	// Живой случай владельца (2026-07-30): буфер терминала 4 МБ против предела
	// кадра 1 МБ — хост не мог отдать накопленный вывод и рвал соединение, а
	// агент переподключался по кругу, пока терминал не объявляли завершённым.
	// Печатаем больше мегабайта и переподключаемся ещё раз: и снапшот должен
	// доехать, и ввод после него — работать.
	if _, err := pc2.Write([]byte("powershell -command \"1..40000 | %{ 'X' * 40 }\"\r")); err != nil {
		t.Fatalf("write big output: %v", err)
	}
	bigDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(bigDeadline) {
		if r2.size() > maxFrame+64<<10 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got := r2.size(); got <= maxFrame {
		t.Skipf("шелл напечатал только %d байт — проверять снапшот больше кадра нечем", got)
	}
	if err := pc2.Close(); err != nil {
		t.Logf("close pc2: %v", err)
	}

	pc3, err := dialHost(id, 5*time.Second, 0, 0)
	if err != nil {
		t.Fatalf("dial host #3 (после длинного вывода): %v", err)
	}
	r3 := &safeReader{}
	go r3.drain(pc3)
	// Снапшот приходит кусками — ждём, пока приедет больше предела кадра.
	snapDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(snapDeadline) {
		if r3.size() > maxFrame {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got := r3.size(); got <= maxFrame {
		t.Fatalf("снапшот после длинной истории оборвался: получено %d байт (предел кадра %d)", got, maxFrame)
	} else {
		t.Logf("снапшот приехал кусками: %d байт при пределе кадра %d", got, maxFrame)
	}
	if _, err := pc3.Write([]byte("echo MARKER_THREE\r")); err != nil {
		t.Fatalf("write #3: %v", err)
	}
	waitFor(t, r3, "MARKER_THREE", 10*time.Second)
	pc2 = pc3 // kill ниже добивает хост через живое соединение

	// Explicit kill tears the host down.
	if err := pc2.(*pipeClient).kill(); err != nil {
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
