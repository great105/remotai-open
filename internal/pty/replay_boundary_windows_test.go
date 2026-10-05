//go:build windows

package pty

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// dump — накопленный вывод строкой (safeReader живёт в persist_windows_test.go).
func (r *safeReader) dump() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// ЖИВОЙ ПРОГОН ГРАНИЦЫ ПЕРЕИГРОВКИ на настоящем pty-host.
//
// Сценарий владельца «перезапуск агента при открытом терминале»: хост живёт
// своей жизнью, агент к нему переподключается и получает СНАЧАЛА весь буфер
// (переигровку), потом живой вывод. До 2.57.15 сессия этой границы не видела, и
// переигровка оседала в истории зеркала дублями (скриншот 14.08.2026).
//
// Здесь проверяется на живом протоколе:
//   - граница объявляется РОВНО ОДИН раз за подключение;
//   - всё, что напечатано ДО переподключения, приходит ДО границы;
//   - всё, что напечатано ПОСЛЕ, приходит ПОСЛЕ неё.
func TestReplayBoundaryOnLiveHostReattach(t *testing.T) {
	if testing.Short() {
		t.Skip("живой pty-host: пропускаем в -short")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "build", "remotai.exe")
	if _, err := os.Stat(exe); err != nil {
		t.Skip("build/remotai.exe отсутствует — сначала scripts/build-local.ps1")
	}

	id := randomID()
	tmp := t.TempDir()
	const flags = 0x00000008 | 0x00000200 | 0x08000000 // DETACHED|NEW_GROUP|NO_WINDOW
	host := exec.Command(exe, "--pty-host", "--id", id, "--cwd", tmp, "--shell", "cmd", "--cols", "100", "--rows", "30")
	host.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
	if err := host.Start(); err != nil {
		t.Fatalf("запуск хоста: %v", err)
	}
	hostPID := host.Process.Pid
	defer func() {
		if p, err := os.FindProcess(hostPID); err == nil {
			_ = p.Kill()
		}
	}()

	// ── Первый агент: печатает то, что станет «прошлым» ──────────────────────
	pc1, err := dialHost(id, 5*time.Second, 100, 30)
	if err != nil {
		t.Fatalf("первое подключение: %v", err)
	}
	r1 := &safeReader{}
	go r1.drain(pc1)
	if _, err := pc1.Write([]byte("echo ДОРЕСТАРТА\r")); err != nil {
		t.Fatalf("печать до рестарта: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(r1.dump(), "ДОРЕСТАРТА") {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(r1.dump(), "ДОРЕСТАРТА") {
		t.Fatal("хост не напечатал строку до рестарта")
	}
	_ = pc1.Close()

	// ── Второй агент: это и есть перезапуск ──────────────────────────────────
	pc2, err := dialHost(id, 5*time.Second, 100, 30)
	if err != nil {
		t.Fatalf("переподключение: %v", err)
	}
	defer pc2.Close()

	type boundaryAware interface{ setReplayEnd(func()) }
	aware, ok := pc2.(boundaryAware)
	if !ok {
		t.Fatal("клиент хоста не умеет сообщать границу переигровки")
	}

	var mu sync.Mutex
	var fired int
	var beforeLen int
	rec := &safeReader{}
	aware.setReplayEnd(func() {
		mu.Lock()
		fired++
		beforeLen = len(rec.dump())
		mu.Unlock()
	})
	go rec.drain(pc2)

	// Даём переигровке доехать, затем печатаем ЖИВОЙ вывод.
	time.Sleep(700 * time.Millisecond)
	if _, err := pc2.Write([]byte("echo ПОСЛЕРЕСТАРТА\r")); err != nil {
		t.Fatalf("печать после рестарта: %v", err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(rec.dump(), "ПОСЛЕРЕСТАРТА") {
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	gotFired, gotBefore := fired, beforeLen
	mu.Unlock()
	all := rec.dump()

	if gotFired != 1 {
		t.Fatalf("граница объявлена %d раз вместо одного", gotFired)
	}
	replayed := all[:min(gotBefore, len(all))]
	live := all[min(gotBefore, len(all)):]
	if !strings.Contains(replayed, "ДОРЕСТАРТА") {
		t.Fatalf("прошлое не попало в переигровку (до границы %d Б):\n%q", gotBefore, replayed)
	}
	if strings.Contains(replayed, "ПОСЛЕРЕСТАРТА") {
		t.Fatal("живой вывод оказался ДО границы — граница объявлена слишком поздно")
	}
	if !strings.Contains(live, "ПОСЛЕРЕСТАРТА") {
		t.Fatalf("живой вывод не пришёл после границы:\n%q", live)
	}
}
