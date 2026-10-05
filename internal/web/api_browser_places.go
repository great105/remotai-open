package web

// Стартовая страница браузера: закладки и «часто открываю».

import (
	"net/http"
	"strconv"
	"strings"
)

// GET /api/vbrowser/places — чем наполнить стартовый экран пустой вкладки.
func (s *Server) apiBrowserPlaces(w http.ResponseWriter, _ *http.Request, _ int64) {
	jsonResp(w, map[string]any{
		"bookmarks": places.Bookmarks(),
		"top":       places.Top(12),
	})
}

// GET /api/vbrowser/history?q=&limit= — история посещений, свежее сверху.
//
// Отдельно от «часто открываю»: то — про сайты, а история отвечает на вопрос
// «где я видел ту страницу вчера», и без поиска по ней пятьсот строк листать
// бессмысленно.
func (s *Server) apiBrowserHistory(w http.ResponseWriter, r *http.Request, _ int64) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	query := r.URL.Query().Get("q")
	jsonResp(w, map[string]any{
		"history": places.History(query, limit),
		"query":   query,
	})
}

// DELETE /api/vbrowser/history?url=… — забыть страницу; без url — всю историю.
func (s *Server) apiBrowserHistoryDelete(w http.ResponseWriter, r *http.Request, _ int64) {
	// «Очистить всю историю» — действие, которое нельзя отменить, поэтому его
	// нужно просить явно: пустой url без флага all мог прийти из-за опечатки в
	// адресе запроса и стереть всё молча.
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	all := r.URL.Query().Get("all") == "1"
	if target == "" && !all {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request",
			"укажите url страницы или all=1 для очистки всей истории", nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "removed": places.ForgetHistory(target)})
}

// POST /api/vbrowser/places {url, title} — добавить закладку. Пустой url
// означает «текущую страницу»: именно так это делается в браузере — звёздочкой
// на открытом сайте, а не вводом адреса руками.
func (s *Server) apiBrowserPlaceAdd(w http.ResponseWriter, r *http.Request, _ int64) {
	var req struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "bad request", nil)
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		client := controlCDPClient()
		if client == nil {
			jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
			return
		}
		ctx, cancel := browserCtx(r)
		defer cancel()
		info := client.Info(ctx)
		req.URL, req.Title = info.URL, info.Title
	}
	mark, err := places.AddBookmark(req.URL, req.Title)
	if err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_url", "Это не адрес страницы.", nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "bookmark": mark})
}

// DELETE /api/vbrowser/places?url=… — убрать закладку, а с параметром host —
// плитку из «часто открываю».
func (s *Server) apiBrowserPlaceDelete(w http.ResponseWriter, r *http.Request, _ int64) {
	if host := strings.TrimSpace(r.URL.Query().Get("host")); host != "" {
		jsonResp(w, map[string]any{"ok": places.ForgetSite(host)})
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	if target == "" {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "не указан адрес", nil)
		return
	}
	jsonResp(w, map[string]any{"ok": places.RemoveBookmark(target)})
}
