//go:build linux

package input

// Постоянный xdotool вместо процесса на каждое нажатие.
//
// Было: `exec.Command("xdotool", …)` на КАЖДОЕ событие — форк, exec, загрузка
// libX11, новое подключение к X-серверу, разрыв. На слабом VPS это 10–20 мс
// накладных расходов на событие, и они складываются ровно там, где заметнее
// всего: ведение мыши и прокрутка идут потоком по 30–60 событий в секунду, из
// которых каждое ждало своей очереди. Курсор двигался рывками, перетаскивание
// разваливалось, прокрутка отставала на секунды.
//
// Стало: один живой процесс `xdotool -` читает команды из stdin — событие
// стоит записи строки в pipe. Процесс поднимается лениво, переживает
// перезапуск X (пересоздаётся) и следит за сменой DISPLAY: виртуальный браузер
// подменяет дисплей на лету, и команды обязаны уходить на новый экран.
//
// Через pipe идёт всё, кроме ввода текста: xdotool разбирает строку stdin
// пробелами, поэтому `type` с пробелами и переводами строк остаётся отдельным
// процессом — он и так редкий (вставка из буфера), а надёжность там важнее.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// xdotoolPipe — живой `xdotool -`, принимающий команды построчно.
type xdotoolPipe struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	display string        // DISPLAY, с которым процесс запущен
	stderr  *tailBuffer   // хвост жалоб xdotool — попадает в текст ошибки
	exited  chan struct{} // закрывается, когда процесс умер
}

var pipe = &xdotoolPipe{}

// tailBuffer хранит последние ~2 КБ вывода: полный лог не нужен, а причина
// смерти («Can't open display») всегда в конце.
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > 2048 {
		b.data = b.data[len(b.data)-2048:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}

// alive — процесс запущен и ещё не умер.
func (p *xdotoolPipe) alive() bool {
	if p.cmd == nil {
		return false
	}
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// ensureLocked поднимает процесс, если его нет, он умер или сменился DISPLAY.
func (p *xdotoolPipe) ensureLocked() error {
	display := os.Getenv("DISPLAY")
	if p.alive() && p.display == display {
		return nil
	}
	p.stopLocked()

	bin, err := exec.LookPath("xdotool")
	if err != nil {
		return fmt.Errorf("%w: xdotool не установлен", ErrInputUnavailable)
	}
	cmd := exec.Command(bin, "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("%w: xdotool stdin: %v", ErrInputUnavailable, err)
	}
	tail := &tailBuffer{}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: xdotool: %v", ErrInputUnavailable, err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	p.cmd, p.stdin, p.display, p.stderr, p.exited = cmd, stdin, display, tail, exited
	return nil
}

// stopLocked закрывает текущий процесс (без ожидания — Wait живёт в горутине).
func (p *xdotoolPipe) stopLocked() {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	p.cmd, p.stdin = nil, nil
}

// run отправляет команды одной пачкой. Умерший процесс поднимается заново и
// пачка повторяется один раз: X мог перезапуститься между событиями, и терять
// из-за этого нажатие человека нельзя.
func (p *xdotoolPipe) run(lines ...string) error {
	payload := strings.Join(lines, "\n") + "\n"
	p.mu.Lock()
	defer p.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if err := p.ensureLocked(); err != nil {
			return err
		}
		if _, err := io.WriteString(p.stdin, payload); err == nil {
			return nil
		}
		// Труба закрыта — процесса больше нет. Второй заход поднимет новый.
		detail := ""
		if p.stderr != nil {
			detail = p.stderr.String()
		}
		p.stopLocked()
		if attempt == 1 {
			if detail == "" {
				detail = "процесс ввода не отвечает"
			}
			return fmt.Errorf("%w: xdotool: %s", ErrInputUnavailable, lastLine(detail))
		}
	}
	return nil
}

// lastLine — последняя содержательная строка (в ней и лежит причина).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return s
}
