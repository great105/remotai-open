package pty

import (
	"os"
	"testing"
	"time"
)

// Диагностический прогон по ЖИВОМУ pty-host: подключается к его каналу и
// печатает весь диалог (Hello, кадры, ошибку обрыва). Нужен, когда «терминал
// отвалился», а процесс хоста жив: только так видно, на каком шаге рвётся
// связь. По умолчанию пропускается.
//
//	PTY_PROBE_ID=<id сессии> go test ./internal/pty/ -run TestProbeLiveHost -v
func TestProbeLiveHost(t *testing.T) {
	id := os.Getenv("PTY_PROBE_ID")
	if id == "" {
		t.Skip("нет PTY_PROBE_ID — прогон только по живому хосту")
	}
	pc, err := dialHost(id, 3*time.Second, 0, 0)
	if err != nil {
		t.Fatalf("dialHost: %v", err)
	}
	defer pc.Close()
	cwd, _ := pc.currentCWD()
	t.Logf("подключились: proto=%d shellPID=%d cwd=%q modes=%v",
		pc.protoVersion(), pc.shellPID(), cwd, pc.hostModes())

	buf := make([]byte, 64<<10)
	total, frames := 0, 0
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		n, err := pc.Read(buf)
		if n > 0 {
			frames++
			total += n
			if frames <= 3 {
				t.Logf("кадр %d: %d байт, начало=%q", frames, n, string(buf[:min(60, n)]))
			}
		}
		if err != nil {
			t.Logf("ОБРЫВ после %d кадров / %d байт: %v (тип %T)", frames, total, err, err)
			return
		}
	}
	t.Logf("связь жива: %d кадров / %d байт за 8 секунд", frames, total)
}
