package web

import (
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kbinani/screenshot"
)

// Снимок для агента обязан оказаться ФАЙЛОМ на этом компьютере, а не картинкой
// в ответе: читать его будет Claude Code или Codex, а они видят только свою
// файловую систему. Проверяем запуском — настоящим захватом экрана.
//
// На машине без дисплея (CI-раннеры, сервер) захватывать нечего, и правильный
// ответ там — честный 503, а не пустой файл. Оба случая проверяются здесь же:
// пропускать тест на CI молча значит не проверять его вовсе.
func TestScreenshotSaveWritesFileForAgent(t *testing.T) {
	home := t.TempDir()
	// bundle.FilesDir() читает домашнюю папку из окружения — подменяем её, чтобы
	// тест не писал в настоящую ~/Remotai/files владельца.
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("HOME", home)        // Unix

	var s *Server // обработчик не трогает поля сервера
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/system/screenshot/save", nil)
	s.apiScreenshotSave(rec, req, 1)

	if screenshot.NumActiveDisplays() == 0 {
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("без дисплея ждём 503, получили %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "no_display") {
			t.Fatalf("без дисплея ответ обязан назвать причину no_display: %s", rec.Body.String())
		}
		return
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("ждём 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Path   string `json:"path"`
		Name   string `json:"name"`
		Dir    string `json:"dir"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("ответ не разобрался: %v (%s)", err, rec.Body.String())
	}

	// Путь уходит в командную строку агента: латиница и никаких пробелов —
	// иначе его придётся брать в кавычки, а это делает человек, а не мы.
	if !strings.HasPrefix(got.Name, "screen-") || !strings.HasSuffix(got.Name, ".png") {
		t.Fatalf("имя снимка должно быть screen-*.png, получили %q", got.Name)
	}
	if strings.ContainsAny(got.Name, " \t") {
		t.Fatalf("в имени снимка не должно быть пробелов: %q", got.Name)
	}
	for _, r := range got.Name {
		if r > 127 {
			t.Fatalf("имя снимка обязано быть латиницей: %q", got.Name)
		}
	}

	// Файл лежит в общей папке файлов агентов, а не где-то ещё.
	wantDir := filepath.Join(home, "Remotai", "files")
	if got.Dir != wantDir {
		t.Fatalf("снимок должен лежать в %q, а лежит в %q", wantDir, got.Dir)
	}
	if got.Path != filepath.Join(wantDir, got.Name) {
		t.Fatalf("путь не совпадает с папкой и именем: %q", got.Path)
	}

	// Главное: по этому пути ДЕЙСТВИТЕЛЬНО лежит читаемый PNG, а не обрывок.
	file, err := os.Open(got.Path)
	if err != nil {
		t.Fatalf("агент не сможет открыть снимок: %v", err)
	}
	defer file.Close()
	img, err := png.Decode(file)
	if err != nil {
		t.Fatalf("файл не читается как PNG: %v", err)
	}
	if img.Bounds().Dx() != got.Width || img.Bounds().Dy() != got.Height {
		t.Fatalf("размер в ответе (%dx%d) не совпал с файлом (%dx%d)",
			got.Width, got.Height, img.Bounds().Dx(), img.Bounds().Dy())
	}
	if got.Width <= 0 || got.Height <= 0 {
		t.Fatalf("пустой кадр: %dx%d", got.Width, got.Height)
	}

	// Временный файл не остаётся: агент листает эту папку и не должен видеть
	// недописанные куски.
	if _, err := os.Stat(got.Path + ".part"); !os.IsNotExist(err) {
		t.Fatalf("временный файл .part остался рядом со снимком")
	}
}
