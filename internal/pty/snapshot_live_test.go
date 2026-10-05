package pty

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Живая проверка причины «терминалы отваливаются»: терминал с историей БОЛЬШЕ
// одного кадра протокола обязан переподключаться.
//
// Что именно ломалось (2026-07-30): буфер вывода 4 МБ против предела кадра
// 1 МБ — хост не мог отдать снапшот и рвал соединение на каждом подключении.
// Тест поднимает настоящий pty-host, наливает в него больше мегабайта вывода,
// отключается и подключается заново: связь должна встать и отдать историю.
//
// Прогон только на Windows/Linux с настоящим шеллом и по запросу: поднимает
// процесс и пишет мегабайты.
//
//	PTY_LIVE=1 go test ./internal/pty/ -run TestBigScrollbackSurvivesReconnect -v
func TestBigScrollbackSurvivesReconnect(t *testing.T) {
	if os.Getenv("PTY_LIVE") == "" {
		t.Skip("нет PTY_LIVE — прогон поднимает настоящий pty-host")
	}
	id := fmt.Sprintf("test-big-%d", time.Now().UnixNano())
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "/bin/sh"
	}
	cwd, _ := os.Getwd()

	conn, rec, err := newPersistentPTY(id, 120, 30, cwd, shell)
	if err != nil {
		t.Skipf("pty-host не поднялся в этой среде: %v", err)
	}
	t.Cleanup(func() {
		if c, ok := conn.(interface{ kill() error }); ok {
			_ = c.kill()
		}
		_ = conn.Close()
	})
	t.Logf("хост поднят: pid=%d shell=%s", rec.HostPID, shell)

	// Наливаем в терминал больше мегабайта вывода: команда печатает длинные
	// строки, а мы вычитываем их, чтобы буфер хоста наполнился.
	line := strings.Repeat("x", 200)
	var cmd string
	if strings.Contains(strings.ToLower(shell), "cmd") {
		cmd = fmt.Sprintf("for /l %%i in (1,1,8000) do @echo %s\r\n", line)
	} else {
		cmd = fmt.Sprintf("i=0; while [ $i -lt 8000 ]; do echo %s; i=$((i+1)); done\n", line)
	}
	if _, err := conn.Write([]byte(cmd)); err != nil {
		t.Fatalf("ввод команды: %v", err)
	}
	buf := make([]byte, 64<<10)
	got := 0
	deadline := time.Now().Add(45 * time.Second)
	for got < maxFrame+256<<10 && time.Now().Before(deadline) {
		n, err := conn.Read(buf)
		got += n
		if err != nil {
			t.Fatalf("чтение вывода оборвалось на %d байт: %v", got, err)
		}
	}
	t.Logf("налито %d байт вывода (предел кадра %d)", got, maxFrame)
	if got < maxFrame {
		t.Skipf("шелл не успел напечатать больше кадра (%d) — проверять нечего", got)
	}

	// Отключаемся, как это делает Remotai при перезапуске, и подключаемся снова:
	// именно здесь хост отдаёт весь накопленный буфер.
	_ = conn.Close()
	time.Sleep(500 * time.Millisecond)

	again, err := dialHost(id, 3*time.Second, 120, 30)
	if err != nil {
		t.Fatalf("повторное подключение: %v", err)
	}
	defer again.Close()
	snap := 0
	snapDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(snapDeadline) {
		n, err := again.Read(buf)
		snap += n
		if err != nil {
			t.Fatalf("связь оборвалась после %d байт снапшота — снапшот снова не проходит: %v", snap, err)
		}
		if snap > maxFrame {
			break // история больше кадра доехала — то, чего раньше не было
		}
	}
	if snap <= maxFrame {
		t.Fatalf("снапшот доехал не целиком: %d байт (ожидалось больше %d)", snap, maxFrame)
	}
	t.Logf("после переподключения получено %d байт истории — связь живая", snap)
}
