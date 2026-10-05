package web

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPtyKeyBytesMatchTerminalView — таблица key→байты продублирована ЯВНО, а не
// взята из ptyKeyBytes: это страховка от «поправили Enter в одном месте».
// Эталон — sendRaw(…) в apk/src/pages/PtyTermView.tsx: ответ на один и тот же
// вопрос агента обязан быть одинаковым с экрана терминала, с карточки главной,
// из уведомления и из inline-кнопки бота.
func TestPtyKeyBytesMatchTerminalView(t *testing.T) {
	want := map[string]string{
		"enter":  "\r",
		"y":      "y\r", // y/n — С Enter
		"n":      "n\r",
		"1":      "1", // цифры меню — БЕЗ Enter
		"2":      "2",
		"3":      "3",
		"esc":    "\x1b",
		"tab":    "\t",
		"up":     "\x1b[A",
		"down":   "\x1b[B",
		"left":   "\x1b[D",
		"right":  "\x1b[C",
		"ctrl-c": "\x03",
		"ctrl-d": "\x04",
		"ctrl-z": "\x1a",
	}
	for k, v := range want {
		got, ok := ptyKeyBytes[k]
		if !ok {
			t.Errorf("клавиша %q пропала из ptyKeyBytes", k)
			continue
		}
		if got != v {
			t.Errorf("ptyKeyBytes[%q] = %q, ожидалось %q (см. sendRaw в PtyTermView.tsx)", k, got, v)
		}
	}
	for k := range ptyKeyBytes {
		if _, ok := want[k]; !ok {
			t.Errorf("новая клавиша %q не сверена с PtyTermView.tsx — добавьте её в этот тест", k)
		}
	}
}

// TestPtyKeyListStable — контракт allowlist'а: ключи в нижнем регистре, без
// пробелов (хендлер делает ToLower+TrimSpace и сравнивает точно), значения
// непустые (пустая запись = «ответ ушёл», но агент ничего не получил).
func TestPtyKeyListStable(t *testing.T) {
	for k, v := range ptyKeyBytes {
		if k != strings.ToLower(strings.TrimSpace(k)) {
			t.Errorf("ключ %q должен быть в нижнем регистре и без пробелов", k)
		}
		if v == "" {
			t.Errorf("у клавиши %q пустые байты", k)
		}
	}
	list := ptyKeyList()
	parts := strings.Split(list, ",")
	if len(parts) != len(ptyKeyBytes) {
		t.Fatalf("ptyKeyList() вернул %d элементов, в таблице %d: %q", len(parts), len(ptyKeyBytes), list)
	}
	for i := 1; i < len(parts); i++ {
		if parts[i-1] >= parts[i] {
			t.Fatalf("ptyKeyList() не отсортирован: %q", list)
		}
	}
	if !strings.Contains(list, "ctrl-c") || !strings.Contains(list, "enter") {
		t.Fatalf("ptyKeyList() потерял базовые клавиши: %q", list)
	}
}

// chunkRecorder — заменитель PTY: запоминает каждый чанк отдельно, чтобы
// проверить границы записи без живого ConPTY.
type chunkRecorder struct {
	chunks [][]byte
	failAt int // номер записи, на которой вернуть ошибку (0 = не падать)
	n      int
}

func (c *chunkRecorder) Write(p []byte) (int, error) {
	c.n++
	if c.failAt > 0 && c.n == c.failAt {
		return 0, errors.New("pipe closed")
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	c.chunks = append(c.chunks, cp)
	return len(p), nil
}

func (c *chunkRecorder) all() string {
	var b strings.Builder
	for _, ch := range c.chunks {
		b.Write(ch)
	}
	return b.String()
}

// TestWritePtyInputChunksUTF8 — кириллица (2 байта на руну) режется на чанки по
// 512 байт: ни один чанк не должен обрываться посреди руны, склейка обязана
// совпасть со входом.
func TestWritePtyInputChunksUTF8(t *testing.T) {
	in := strings.Repeat("привет", 125) // 750 рун, 1500 байт → 3+ чанка
	rec := &chunkRecorder{}
	if err := writePtyInput(rec, []byte(in)); err != nil {
		t.Fatalf("writePtyInput: %v", err)
	}
	if len(rec.chunks) < 2 {
		t.Fatalf("ожидали разбиение на чанки, получили %d", len(rec.chunks))
	}
	for i, ch := range rec.chunks {
		if !utf8.Valid(ch) {
			t.Fatalf("чанк %d рвёт UTF-8: %q", i, ch)
		}
	}
	if got := rec.all(); got != in {
		t.Fatalf("склейка чанков не равна входу (%d байт против %d)", len(got), len(in))
	}
}

// Короткий ответ (та самая «y\r») уходит одной записью — без пауз и лишних
// системных вызовов.
func TestWritePtyInputShortSingleWrite(t *testing.T) {
	rec := &chunkRecorder{}
	if err := writePtyInput(rec, []byte(ptyKeyBytes["y"])); err != nil {
		t.Fatalf("writePtyInput: %v", err)
	}
	if len(rec.chunks) != 1 || rec.all() != "y\r" {
		t.Fatalf("ожидали одну запись \"y\\r\", получили %q", rec.chunks)
	}
}

// Ошибка записи возвращается наверх, а не глотается: хендлер по ней отличает
// «PTY умер» (410) от «ввод доставлен».
func TestWritePtyInputPropagatesError(t *testing.T) {
	rec := &chunkRecorder{failAt: 1}
	if err := writePtyInput(rec, []byte("что-нибудь")); err == nil {
		t.Fatal("ожидали ошибку записи")
	}
}
