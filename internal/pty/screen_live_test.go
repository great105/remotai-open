package pty

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ЖИВАЯ ПРОВЕРКА ЗЕРКАЛА: настоящий pty-host, настоящая оболочка, настоящий
// поток — и вопрос ровно тот, из-за которого всё затевалось: можно ли по кадру
// показать человеку экран, если приложение рисует ДИФФОМ и полного кадра в
// потоке нет.
//
// Приложение здесь имитируем честно: печатаем экран целиком, а дальше меняем
// ТОЛЬКО отдельные ячейки курсорными адресациями — ровно так ведут себя
// Claude Code и Codex. Хвост потока такую картинку не восстанавливает по
// определению, а зеркало обязано.
//
//	PTY_LIVE=1 go test ./internal/pty/ -run TestScreenMirrorOnLiveSession -v
func TestScreenMirrorOnLiveSession(t *testing.T) {
	if os.Getenv("PTY_LIVE") == "" {
		t.Skip("нет PTY_LIVE — прогон поднимает настоящий pty-host")
	}
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "/bin/sh"
	}
	if strings.Contains(strings.ToLower(shell), "cmd") {
		// cmd.exe не даёт удобного способа печатать ESC-последовательности.
		if ps, err := os.Stat(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`); err == nil && !ps.IsDir() {
			shell = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
		} else {
			t.Skip("нужен powershell для печати ESC-последовательностей")
		}
	}
	id := fmt.Sprintf("test-screen-%d", time.Now().UnixNano())
	cwd, _ := os.Getwd()

	conn, rec, err := newPersistentPTY(id, 60, 20, cwd, shell)
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

	mirror := newScreenMirror(60, 20)
	defer mirror.Close()

	// Читаем поток и кормим им зеркало — так же, как это делает readLoop.
	stop := make(chan struct{})
	go func() {
		buf := make([]byte, 8192)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := conn.Read(buf)
			if n > 0 {
				mirror.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	defer close(stop)

	esc := "$([char]27)"
	// 1. Приложение рисует экран целиком — один раз, как при старте.
	write := func(cmd string) {
		if _, err := conn.Write([]byte(cmd + "\r\n")); err != nil {
			t.Fatalf("ввод: %v", err)
		}
		time.Sleep(900 * time.Millisecond)
	}
	write(fmt.Sprintf(`Write-Host -NoNewline "%s[2J%s[H"; 1..8 | %% { Write-Host "STROKA $_ ---------------" }`, esc, esc))
	// 2. Дальше — ТОЛЬКО точечные правки, как у настоящего агента.
	write(fmt.Sprintf(`Write-Host -NoNewline "%s[3;8HZAMENA"`, esc))
	write(fmt.Sprintf(`Write-Host -NoNewline "%s[6;8HVTORAYA"`, esc))

	frame := mirror.Frame()
	if frame == "" {
		t.Fatal("зеркало не отдало кадр")
	}
	for _, want := range []string{"STROKA 1", "STROKA 8", "ZAMENA", "VTORAYA"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("в кадре нет %q — зеркало не собрало экран:\n%q", want, frame)
		}
	}
	t.Logf("кадр живой сессии: %d байт, есть и исходный экран, и точечные правки", len(frame))

	// И главное: кадр должен быть НАМНОГО дешевле того, что мы слали раньше.
	// Дешевизна тут не украшение — это и есть разница между «телефон показал
	// экран за секунду» и «телефон качал полмегабайта».
	if len(frame) > 32<<10 {
		t.Fatalf("кадр раздулся до %d байт — что-то не так со сборкой", len(frame))
	}
}
