//go:build windows

package pty

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// ЖИВОЙ прогон: вернувшийся клиент получает только хвост, а не всю историю.
//
// Поднимается НАСТОЯЩИЙ pty-host, в нём печатается заметный объём, клиент
// отцепляется и возвращается двумя способами — по-старому (без позиции) и
// по-новому (с позицией). Первый обязан получить всю историю, второй — почти
// ничего. Это же и есть проверка совместимости: старый способ продолжает
// работать байт в байт, потому что именно им ходит агент, переживший обновление.
func TestHostReattachWithKnownSendsOnlyTail(t *testing.T) {
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
	// Не t.TempDir: это рабочая папка шелла внутри хоста, а Kill хоста его
	// детей (cmd → powershell) не гасит. Под нагрузкой полного прогона они
	// ещё живы при уборке, и t.TempDir валил проверку, которая уже прошла
	// (13.09.2026, выпуск 2.70.0). Папку убираем по возможности.
	tmp, err := os.MkdirTemp("", "host-known-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	const flags = 0x00000008 | 0x00000200 | 0x08000000 // DETACHED|NEW_GROUP|NO_WINDOW
	host := exec.Command(exe, "--pty-host", "--id", id, "--cwd", tmp, "--shell", "cmd", "--cols", "100", "--rows", "30")
	host.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
	if err := host.Start(); err != nil {
		t.Fatalf("запуск хоста: %v", err)
	}
	hostPID := host.Process.Pid
	defer func() {
		// Всё дерево: хост, conpty, cmd и powershell.
		_ = exec.Command("taskkill", "/PID", strconv.Itoa(hostPID), "/T", "/F").Run()
		if p, err := os.FindProcess(hostPID); err == nil {
			_ = p.Kill()
		}
	}()

	pc1, err := dialHost(id, 5*time.Second, 100, 30)
	if err != nil {
		t.Fatalf("первое подключение: %v", err)
	}
	r1 := &safeReader{}
	go r1.drain(pc1)
	if _, err := pc1.Write([]byte("powershell -command \"1..2000 | %{ 'ИСТОРИЯ ' + $_ }\"\r")); err != nil {
		t.Fatalf("печать истории: %v", err)
	}
	// Ждём, пока накопится заметный объём.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && r1.size() < 40<<10 {
		time.Sleep(200 * time.Millisecond)
	}
	produced := r1.size()
	if produced < 20<<10 {
		t.Skipf("шелл напечатал только %d байт — сравнивать нечего", produced)
	}
	_ = pc1.Close()
	time.Sleep(300 * time.Millisecond)

	// СТАРЫЙ способ (как ходит агент, переживший обновление): позиции нет —
	// приезжает вся история. Это поведение обязано сохраниться.
	pcOld, err := dialHost(id, 5*time.Second, 0, 0)
	if err != nil {
		t.Fatalf("возврат без позиции: %v", err)
	}
	rOld := &safeReader{}
	go rOld.drain(pcOld)
	time.Sleep(3 * time.Second)
	gotOld := rOld.size()
	_ = pcOld.Close()
	time.Sleep(300 * time.Millisecond)

	// НОВЫЙ способ: говорим позицию — приезжает только то, чего у нас нет.
	pcNew, err := dialHostFrom(id, 5*time.Second, 0, 0, uint64(produced))
	if err != nil {
		t.Fatalf("возврат с позицией: %v", err)
	}
	rNew := &safeReader{}
	go rNew.drain(pcNew)
	time.Sleep(3 * time.Second)
	gotNew := rNew.size()
	_ = pcNew.Close()

	t.Logf("напечатано ~%d байт; возврат без позиции принёс %d, с позицией — %d", produced, gotOld, gotNew)
	if gotOld < produced/2 {
		t.Fatalf("возврат БЕЗ позиции принёс %d байт при истории ~%d — старое поведение сломано",
			gotOld, produced)
	}
	if gotNew >= gotOld/2 {
		t.Fatalf("возврат С позицией принёс %d байт против %d — хвост не выделен, история приедет дубликатом",
			gotNew, gotOld)
	}
}
