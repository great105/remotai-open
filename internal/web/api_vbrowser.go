package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tgcontrol/internal/vbrowser"
)

// GET /api/vbrowser/status — virtual browser feature state (Linux agents).
func (s *Server) apiVBrowserStatus(w http.ResponseWriter, _ *http.Request, _ int64) {
	jsonResp(w, vbrowser.GetStatus())
}

// POST /api/vbrowser/start {browser?, width?, height?} — bring up Xvfb +
// browser. Idempotent: an already-running session returns its status.
func (s *Server) apiVBrowserStart(w http.ResponseWriter, r *http.Request, _ int64) {
	var req struct {
		Browser string `json:"browser"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
		// Язык интерфейса приложения: браузер на сервере иначе говорит
		// по-английски и просит у сайтов английские страницы, хотя человек
		// сидит в русском приложении.
		Lang string `json:"lang"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // empty body is fine
	}
	st, err := vbrowser.Start(req.Browser, req.Width, req.Height, req.Lang)
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	// Счётчик зрителей живёт в web-слое и переживает перезапуск сессии
	// (стрим мог остаться открытым) — подсовываем его новой сессии целиком.
	vbrowser.SetViewers(int(streamViewers.Load()))
	jsonResp(w, st)
}

// POST /api/vbrowser/stop — tear the virtual display session down.
func (s *Server) apiVBrowserStop(w http.ResponseWriter, _ *http.Request, _ int64) {
	vbrowser.Stop()
	jsonResp(w, map[string]any{"ok": true, "running": false})
}

// POST /api/vbrowser/keepalive — «оставить включённым»: сдвигает дедлайн
// автоостановки по простою. Сам браузер не трогает — только отметку
// активности, поэтому вызов дешёвый и ответ мгновенный.
func (s *Server) apiVBrowserKeepalive(w http.ResponseWriter, _ *http.Request, _ int64) {
	vbrowser.NoteActivity()
	jsonResp(w, map[string]any{"ok": true})
}

// GET /api/vbrowser/downloads — файлы, скачанные виртуальным браузером
// (новейшие сверху). Само скачивание на телефон — обычным /api/files/download
// по полю path: оно умеет поток и диапазоны, дублировать его здесь незачем.
func (s *Server) apiVBrowserDownloads(w http.ResponseWriter, _ *http.Request, _ int64) {
	type item struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Size  int64  `json:"size"`
		Mtime int64  `json:"mtime"`
	}
	list := []item{}
	entries, err := os.ReadDir(downloadsDir())
	if err == nil {
		for _, e := range entries {
			// .crdownload — недокачанные: показывать их как готовые файлы нельзя.
			if e.IsDir() || strings.HasSuffix(e.Name(), ".crdownload") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			list = append(list, item{
				Name:  e.Name(),
				Path:  filepath.Join(downloadsDir(), e.Name()),
				Size:  info.Size(),
				Mtime: info.ModTime().Unix(),
			})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Mtime > list[j].Mtime })
	jsonResp(w, map[string]any{"downloads": list})
}

// POST /api/vbrowser/install-input — доустановить xdotool, чтобы экран начал
// принимать нажатия. Экран без него показывается, но остаётся картинкой, а
// раньше единственным выходом был совет «установите его на компьютере»: с
// телефона это значит бросить всё и лезть в SSH. Ставим фиксированный пакет
// системным менеджером — произвольных имён снаружи не принимаем.
// Ставим ФОНОМ и сразу отвечаем: облачный запрос живёт 30 секунд, а apt на
// свежей машине занимает минуты. Ход установки экран узнаёт из
// /api/vbrowser/status (input_installing → input_ready).
func (s *Server) apiVBrowserInstallInput(w http.ResponseWriter, _ *http.Request, _ int64) {
	if err := vbrowser.StartInstallInput(); err != nil {
		log.Printf("[VBROWSER] install input refused: %v", err)
		jsonErrorCode(w, http.StatusConflict, "install_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "status": vbrowser.GetStatus()})
}

// POST /api/vbrowser/navigate {url} | {action:"back"|"forward"|"reload"} —
// управление вкладкой напрямую.
//
// Зачем ручка, если есть клавиатура: кадр, который видит человек, — это САМА
// СТРАНИЦА, без адресной строки и вкладок браузера. Раньше открыть новый адрес
// с телефона можно было только вслепую (Ctrl+L и печать в невидимое поле), а
// «назад» — попаданием пальцем в кнопку размером в несколько пикселей.
func (s *Server) apiVBrowserNavigate(w http.ResponseWriter, r *http.Request, _ int64) {
	var req struct {
		URL    string `json:"url"`
		Action string `json:"action"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser",
			"Браузер не на связи — запустите его на этой машине.", nil)
		return
	}
	vbrowser.NoteActivity() // навигация — активность, сдвигает дедлайн простоя
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var err error
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "back":
		err = client.Go(ctx, -1)
	case "forward":
		err = client.Go(ctx, 1)
	case "reload":
		err = client.Reload(ctx)
	default:
		url := normalizeBrowserURL(req.URL)
		if url == "" {
			jsonErrorCode(w, 400, "bad_request", "адрес не указан", nil)
			return
		}
		err = client.Navigate(ctx, url)
	}
	if err != nil {
		log.Printf("[CDP] navigate: %v", err)
		jsonErrorCode(w, http.StatusBadGateway, "navigate_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "url": client.URL(ctx)})
}

// normalizeBrowserURL превращает то, что человек набрал с телефона, в адрес.
// Без схемы браузер не откроет ничего, а строку с пробелами разумнее считать
// поисковым запросом, чем адресом с опечаткой.
func normalizeBrowserURL(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "about:") || strings.HasPrefix(lower, "file://") {
		return text
	}
	if strings.ContainsAny(text, " 	") || !strings.Contains(text, ".") {
		return "https://www.google.com/search?q=" + url.QueryEscape(text)
	}
	return "https://" + text
}
