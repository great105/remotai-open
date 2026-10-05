package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tgcontrol/internal/update"
)

// Скачанное обновление применяется БЕЗ сети: кнопка «Обновить сейчас» с
// телефона должна просто перезапустить агент. Раньше этой ветки не было, и
// запрос уходил качать файл, который уже лежит на диске (лишняя минута на
// мобильном интернете и лишний шанс упасть на скачивании).
func TestUpdateAppliesPendingWithoutDownload(t *testing.T) {
	restarted := make(chan struct{})
	var once sync.Once
	update.SetPending("2.48.12", func() { once.Do(func() { close(restarted) }) })
	// Провалившийся тест не должен оставить готовое обновление следующим:
	// RestartPending забирает запись (в успешном пути её уже нет).
	t.Cleanup(func() { update.RestartPending() })

	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/system/update", strings.NewReader("{}"))
	s.apiUpdate(rec, req, 0)

	if rec.Code != 200 {
		t.Fatalf("код ответа = %d; хотели 200 (тело: %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не разобрался: %v (%s)", err, rec.Body.String())
	}
	if !body.OK || body.Version != "2.48.12" {
		t.Fatalf("ответ = %+v; хотели ok и версию 2.48.12", body)
	}
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("перезапуск не запущен: скачанное обновление осталось лежать")
	}
}
