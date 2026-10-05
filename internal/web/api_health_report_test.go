package web

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Лог агента на боевой машине доходил до 19 МБ — читать его целиком нельзя ни
// в ответ, ни в память. Тест держит поведение «читаем хвост».
func TestХвостЛогаНеЧитаетФайлЦеликом(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remotai.log")
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "строка %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	lines, size, err := tailLines(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 10 {
		t.Fatalf("ожидалось 10 строк, пришло %d", len(lines))
	}
	if lines[9] != "строка 5000" {
		t.Fatalf("последняя строка не последняя: %q", lines[9])
	}
	if lines[0] != "строка 4991" {
		t.Fatalf("хвост считан со сдвигом: %q", lines[0])
	}
	if size <= 0 {
		t.Fatal("размер файла обязан приезжать: по нему видно, что лог распух")
	}
}

// Файл короче запроса — отдаём всё, что есть, и без обрезанной первой строки.
func TestХвостКороткогоЛога(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotai.log")
	if err := os.WriteFile(path, []byte("одна\nдве\nтри\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, _, err := tailLines(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0] != "одна" {
		t.Fatalf("короткий файл должен приходить целиком: %#v", lines)
	}
}
