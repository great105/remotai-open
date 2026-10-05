package web

// Действия, которые в телефонном браузере делаются пальцем, а через кадр
// недоступны: долгое нажатие по ссылке, выделение текста, поиск по странице,
// подъём поля над клавиатурой.
//
// Координаты приходят ДОЛЯМИ кадра (как весь остальной ввод) — клиент не знает
// и не должен знать размер страницы на сервере. Перевод в CSS-пиксели делаем
// здесь, по размеру последнего кадра: ровно так же считается тап.

import (
	"net/http"
	"strings"

	"tgcontrol/internal/cdp"
	"tgcontrol/internal/vbrowser"
)

// browserPoint переводит доли кадра (0..1) в координаты страницы.
//
// Размер берём у последнего кадра, а когда кадров ещё нет (никто не смотрит
// стрим, а действие уже просят) — у профиля устройства: он задаёт ровно те же
// CSS-пиксели, в которых живёт страница.
func browserPoint(client *cdp.Client, rx, ry float64) (float64, float64, bool) {
	width, height := 0, 0
	if frame, ok := client.LastFrame(); ok {
		width, height = frame.Width, frame.Height
	}
	if width <= 0 || height <= 0 {
		dev := client.CurrentDevice()
		width, height = dev.Width, dev.Height
	}
	if width <= 0 || height <= 0 {
		return 0, 0, false
	}
	clamp := func(v float64) float64 {
		if v < 0 {
			return 0
		}
		if v > 1 {
			return 1
		}
		return v
	}
	return clamp(rx) * float64(width), clamp(ry) * float64(height), true
}

// POST /api/vbrowser/hit {x,y} — что под пальцем: ссылка, картинка, поле ввода.
// Из этого собирается контекстное меню долгого нажатия.
func (s *Server) apiBrowserHit(w http.ResponseWriter, r *http.Request, _ int64) {
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	var req struct {
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		Select bool    `json:"select"` // заодно выделить слово под пальцем
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "bad request", nil)
		return
	}
	x, y, ok := browserPoint(client, req.X, req.Y)
	if !ok {
		jsonErrorCode(w, http.StatusConflict, "no_frame", "Кадра страницы ещё нет.", nil)
		return
	}
	ctx, cancel := browserCtx(r)
	defer cancel()
	hit, err := client.HitTest(ctx, x, y)
	if err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "hit_failed", err.Error(), nil)
		return
	}
	vbrowser.NoteActivity()
	resp := map[string]any{
		"link": hit.Link, "link_text": hit.LinkText, "image": hit.Image,
		"video": hit.Video, "text": hit.Text, "editable": hit.Editable, "tag": hit.Tag,
	}
	// Выделяем слово только там, где брать нечего кроме текста: на ссылке и
	// картинке подсветка слова мешала бы прочитать, что именно откроется.
	if req.Select && hit.Link == "" && hit.Image == "" && !hit.Editable {
		if word, err := client.SelectWordAt(ctx, x, y); err == nil && strings.TrimSpace(word) != "" {
			resp["selection"] = word
		}
	}
	jsonResp(w, resp)
}

// GET /api/vbrowser/selection — что выделено на странице (для «Копировать»).
func (s *Server) apiBrowserSelection(w http.ResponseWriter, r *http.Request, _ int64) {
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	ctx, cancel := browserCtx(r)
	defer cancel()
	text, err := client.SelectionText(ctx)
	if err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "selection_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"text": text})
}

// POST /api/vbrowser/find {query, forward, fresh} — поиск по странице.
func (s *Server) apiBrowserFind(w http.ResponseWriter, r *http.Request, _ int64) {
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	var req struct {
		Query   string `json:"query"`
		Forward *bool  `json:"forward"`
		Fresh   bool   `json:"fresh"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "bad request", nil)
		return
	}
	forward := true
	if req.Forward != nil {
		forward = *req.Forward
	}
	ctx, cancel := browserCtx(r)
	defer cancel()
	res, err := client.Find(ctx, strings.TrimSpace(req.Query), forward, req.Fresh)
	if err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "find_failed", err.Error(), nil)
		return
	}
	vbrowser.NoteActivity()
	jsonResp(w, map[string]any{"matches": res.Matches, "query": res.Query})
}

// POST /api/vbrowser/viewport {visible} — на телефоне выехала клавиатура и
// закрыла часть экрана (1 — клавиатуры нет).
//
// Поле ввода надо поднять над клавиатурой. Первая попытка (2.48.0) сжимала для
// этого САМО ОКНО страницы — как делает настоящий телефон. На удалённом экране
// это оказалось хуже болезни: кадр меняет пропорции, а он у нас задаёт форму
// области экрана, и под ней появляется полоса, по которой нажатия уходят в
// пустоту. Живая жалоба владельца: «всё работало, а потом перестала нажиматься
// на экран» — он тапал по кнопке внизу страницы, которая после сжатия оказалась
// уже вне кадра.
//
// Поэтому размер окна страницы НЕ меняется никогда: вместо этого прокручиваем к
// полю, в котором стоит курсор. Для человека результат тот же — поле видно, —
// а геометрия кадра остаётся постоянной.
func (s *Server) apiBrowserViewport(w http.ResponseWriter, r *http.Request, _ int64) {
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	var req struct {
		Visible float64 `json:"visible"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "bad request", nil)
		return
	}
	ctx, cancel := browserCtx(r)
	defer cancel()
	if req.Visible > 0 && req.Visible < 1 {
		if err := client.ScrollFocusIntoView(ctx); err != nil {
			jsonErrorCode(w, http.StatusBadGateway, "viewport_failed", err.Error(), nil)
			return
		}
	}
	vbrowser.NoteActivity()
	jsonResp(w, map[string]any{"ok": true})
}

// GET /api/vbrowser/devices — каталог видов сайта для меню.
//
// Список живёт на агенте, а не в приложении: профили привязаны к тому, что
// умеет конкретная версия браузера, и приложение обязано показывать ровно их.
func (s *Server) apiBrowserDevices(w http.ResponseWriter, _ *http.Request, _ int64) {
	devices := cdp.Catalog()
	list := make([]map[string]any, 0, len(devices))
	for _, d := range devices {
		list = append(list, map[string]any{
			"id": d.Name, "title": d.Title, "mobile": d.Mobile,
			"width": d.Width, "height": d.Height,
		})
	}
	current := ""
	if client := currentCDPClient(); client != nil {
		current = client.CurrentDevice().Name
	}
	jsonResp(w, map[string]any{"devices": list, "current": current})
}
