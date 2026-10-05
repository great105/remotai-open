//go:build windows

package netwatch

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Остановка VPN проверяется на ПОДСТАВНОМ процессе, а не на живом клиенте.
//
// ПОЧЕМУ ТАК, А НЕ «ПРОВЕРИМ НА НАСТОЯЩЕМ HIDDIFY»: у этой машины маршрут до
// облака идёт через tun0, поэтому проверка на живом клиенте обрывает связь
// владельца с его же компьютером — ровно то, чего сторож и должен избегать
// (правило проекта: опасные действия на стенде не проверяем).
//
// Подстава честная: берём системный ping.exe, кладём под именем из списка
// знакомых клиентов и убеждаемся, что механизм видит его и гасит. Именно этот
// путь кода выполнится потом на настоящем Hiddify.
func TestОстановкаГаситПроцессКлиента(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "nekoray.exe") // имя из knownVPN
	src := filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("нет ping.exe для подставы: %v", err)
	}
	if err := os.WriteFile(fake, data, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(fake, "-t", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("подставной клиент не запустился: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	v := &VPN{Name: "NekoRay", PID: cmd.Process.Pid, Exe: fake, Running: true}
	if err := StopVPN(v); err != nil {
		t.Fatalf("StopVPN: %v", err)
	}
	// StopVPN обязан дождаться фактической смерти: сразу после Kill
	// tun-интерфейс ещё в системе, и проверка связи померила бы прошлое.
	if alive, _ := pidAlive(v.PID); alive {
		t.Fatal("процесс пережил остановку — сторож решил бы, что починил, ничего не сделав")
	}
	_ = cmd.Wait()
}

// Гасим только СВОЙ клиент: процессы других программ рядом не трогаем.
// Ошибка здесь стоила бы человеку убитой чужой программы.
func TestОстановкаНеТрогаетЧужиеПроцессы(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("нет ping.exe: %v", err)
	}
	mine := filepath.Join(dir, "nekoray.exe")
	other := filepath.Join(dir, "nekobox.exe") // тоже из списка, но ДРУГОЙ клиент
	if err := os.WriteFile(mine, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, data, 0o755); err != nil {
		t.Fatal(err)
	}

	a := exec.Command(mine, "-t", "127.0.0.1")
	b := exec.Command(other, "-t", "127.0.0.1")
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = a.Process.Kill()
		_ = b.Process.Kill()
		_, _ = a.Process.Wait()
		_, _ = b.Process.Wait()
	}()

	if err := StopVPN(&VPN{Name: "NekoRay", PID: a.Process.Pid, Running: true}); err != nil {
		t.Fatalf("StopVPN: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if alive, _ := pidAlive(b.Process.Pid); !alive {
		t.Fatal("остановка задела чужой процесс: гасить можно только тот клиент, что назван")
	}
}

// Запуск по пути к файлу: ветка без задачи планировщика (переносимые сборки,
// клиенты без автозапуска с правами).
func TestЗапускПоПутиКФайлу(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("нет ping.exe: %v", err)
	}
	fake := filepath.Join(dir, "nekoray.exe")
	if err := os.WriteFile(fake, data, 0o755); err != nil {
		t.Fatal(err)
	}
	how, err := StartVPN(&VPN{Name: "NekoRay", Exe: fake})
	if err != nil {
		t.Fatalf("StartVPN: %v", err)
	}
	if how != fake {
		t.Fatalf("вернулся не тот способ запуска: %q", how)
	}
}
