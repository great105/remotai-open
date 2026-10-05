package web

// Оболочка браузера: адресная строка, вкладки, масштаб, профиль устройства.
//
// Кадр, который видит человек, — это САМА СТРАНИЦА, без вкладок и адресной
// строки самого браузера. Значит всё, что в обычном браузере нарисовано вокруг
// страницы, обязано появиться в нашем интерфейсе — иначе получается не браузер,
// а картинка, по которой можно тыкать.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tgcontrol/internal/cdp"
	"tgcontrol/internal/vbrowser"
)

// browserCtx — общий срок на разговор с браузером: он на этой же машине, и
// если не ответил за пару секунд, значит занят чем-то тяжёлым.
func browserCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 8*time.Second)
}

// GET /api/vbrowser/page — что показать в адресной строке.
func (s *Server) apiBrowserPage(w http.ResponseWriter, r *http.Request, _ int64) {
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	ctx, cancel := browserCtx(r)
	defer cancel()
	jsonResp(w, client.Info(ctx))
}

// GET /api/vbrowser/tabs — открытые вкладки.
func (s *Server) apiBrowserTabs(w http.ResponseWriter, r *http.Request, _ int64) {
	endpoint := vbrowser.DebugEndpoint()
	if endpoint == "" {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не запущен.", nil)
		return
	}
	// Какую вкладку человек смотрит. Спрашивать об этом живое подключение
	// нельзя: сразу после переключения оно ещё не поднялось, и список вкладок
	// приходил бы вообще без выбранной — в переключателе не подсвечено ничего.
	active := activeTargetID()
	ctx, cancel := browserCtx(r)
	defer cancel()
	tabs, err := cdp.Tabs(ctx, endpoint, active)
	if err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "tabs_failed", err.Error(), nil)
		return
	}
	// Значок сайта и картинка страницы: по ним вкладка узнаётся взглядом, а не
	// чтением заголовка. Значок берётся из кэша домена, превью — из кадра,
	// снятого при уходе с вкладки; ни то, ни другое не ходит в сеть здесь,
	// поэтому список остаётся быстрым.
	list := make([]map[string]any, 0, len(tabs))
	for _, tab := range tabs {
		item := map[string]any{
			"id": tab.ID, "title": tab.Title, "url": tab.URL, "active": tab.Active,
		}
		if icon := browserIconForHost(tab.URL); icon != "" {
			item["icon"] = icon
		}
		if preview := tabPreview(tab.ID); preview != "" {
			item["preview"] = preview
		}
		list = append(list, item)
	}
	jsonResp(w, map[string]any{"tabs": list})
}

// POST /api/vbrowser/tabs {url} — открыть вкладку и сразу перейти на неё.
func (s *Server) apiBrowserTabNew(w http.ResponseWriter, r *http.Request, _ int64) {
	endpoint := vbrowser.DebugEndpoint()
	if endpoint == "" {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не запущен.", nil)
		return
	}
	vbrowser.NoteActivity() // работа со вкладками сдвигает дедлайн простоя
	var req struct {
		URL string `json:"url"`
	}
	_ = readJSON(r, &req)
	target := normalizeBrowserURL(req.URL)
	if strings.TrimSpace(req.URL) == "" {
		target = "" // пустая вкладка
	}
	ctx, cancel := browserCtx(r)
	defer cancel()
	id, err := cdp.NewTab(ctx, endpoint, target)
	if err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "tab_failed", err.Error(), nil)
		return
	}
	switchBrowserTab(ctx, endpoint, id)
	jsonResp(w, map[string]any{"ok": true, "id": id})
}

// POST /api/vbrowser/tabs/{id}/activate — переключиться на вкладку.
func (s *Server) apiBrowserTabActivate(w http.ResponseWriter, r *http.Request, _ int64) {
	endpoint := vbrowser.DebugEndpoint()
	if endpoint == "" {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не запущен.", nil)
		return
	}
	vbrowser.NoteActivity()
	ctx, cancel := browserCtx(r)
	defer cancel()
	switchBrowserTab(ctx, endpoint, r.PathValue("id"))
	jsonResp(w, map[string]any{"ok": true})
}

// DELETE /api/vbrowser/tabs/{id} — закрыть вкладку.
func (s *Server) apiBrowserTabClose(w http.ResponseWriter, r *http.Request, _ int64) {
	endpoint := vbrowser.DebugEndpoint()
	if endpoint == "" {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не запущен.", nil)
		return
	}
	vbrowser.NoteActivity()
	id := r.PathValue("id")
	ctx, cancel := browserCtx(r)
	defer cancel()
	if err := cdp.CloseTab(ctx, endpoint, id); err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "tab_failed", err.Error(), nil)
		return
	}
	forgetTabPreview(id) // вкладки нет — её картинке в списке места нет
	// Закрыли ту, что смотрели, — переходим на любую оставшуюся, иначе экран
	// остался бы с кадром несуществующей страницы.
	if c := currentCDPClient(); c != nil && c.TargetID() == id {
		if tabs, err := cdp.Tabs(ctx, endpoint, ""); err == nil && len(tabs) > 0 {
			switchBrowserTab(ctx, endpoint, tabs[0].ID)
		} else {
			dropBrowserLink()
		}
	}
	jsonResp(w, map[string]any{"ok": true})
}

// POST /api/vbrowser/emulate {device,width,height,scale} — кем притворяется
// браузер. Профиль применяется к текущей вкладке и запоминается для новых.
func (s *Server) apiBrowserEmulate(w http.ResponseWriter, r *http.Request, _ int64) {
	var req struct {
		Device string  `json:"device"`
		Width  int     `json:"width"`
		Height int     `json:"height"`
		Scale  float64 `json:"scale"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	vbrowser.NoteActivity() // смена профиля устройства — тоже активность
	dev := cdp.DeviceProfile(req.Device, req.Width, req.Height, req.Scale)
	rememberDevice(dev)
	ctx, cancel := browserCtx(r)
	defer cancel()
	if err := client.Emulate(ctx, dev); err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "emulate_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "device": dev.Name})
}

// POST /api/vbrowser/scale {scale} — масштаб страницы (щипок-зум).
func (s *Server) apiBrowserScale(w http.ResponseWriter, r *http.Request, _ int64) {
	var req struct {
		Scale float64 `json:"scale"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	// Управляющее подключение, а не поток кадров: щипок должен срабатывать и
	// сразу после переключения вкладки, когда канал кадров ещё не поднялся.
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	ctx, cancel := browserCtx(r)
	defer cancel()
	if err := client.SetPageScale(ctx, req.Scale); err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "scale_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true})
}

// POST /api/vbrowser/file-chooser
//
//	{
//	  "chooser_id": 7,
//	  "files": [{"path":"/root/Remotai/files/pty-upload-...","name":"report.pdf"}]
//	}
//
// Телефон сначала загружает выбранные файлы уже проверенным чанкованным
// /api/pty/upload. Здесь путь НЕ принимается на веру: разрешены только прямые
// регулярные pty-upload-* из каталога Remotai/files. Затем файлы копируются в
// приватный каталог пользователя браузера и назначаются настоящему input.
func (s *Server) apiBrowserFileChooser(w http.ResponseWriter, r *http.Request, _ int64) {
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	var req struct {
		ChooserID uint64 `json:"chooser_id"`
		Cancel    bool   `json:"cancel"`
		Files     []struct {
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := readJSON(r, &req); err != nil || req.ChooserID == 0 {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "invalid file chooser request", nil)
		return
	}
	if req.Cancel {
		if err := client.CancelFileChooser(req.ChooserID); err != nil {
			jsonErrorCode(w, http.StatusConflict, "chooser_stale", err.Error(), nil)
			return
		}
		jsonResp(w, map[string]any{"ok": true, "cancelled": true})
		return
	}
	if len(req.Files) == 0 || len(req.Files) > 20 {
		jsonErrorCode(w, http.StatusBadRequest, "bad_files", "choose 1 to 20 files", nil)
		return
	}

	sourcePaths := make([]string, 0, len(req.Files))
	preparedInput := make([]vbrowser.UploadFile, 0, len(req.Files))
	for _, item := range req.Files {
		src, err := browserUploadSource(item.Path)
		if err != nil {
			jsonErrorCode(w, http.StatusBadRequest, "bad_file", err.Error(), nil)
			return
		}
		sourcePaths = append(sourcePaths, src)
		preparedInput = append(preparedInput, vbrowser.UploadFile{
			Path: src,
			Name: safeUploadFilename(item.Name),
		})
	}
	prepared, err := vbrowser.PrepareUploads(preparedInput)
	if err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "prepare_failed", err.Error(), nil)
		return
	}
	ctx, cancel := browserCtx(r)
	defer cancel()
	if err := client.SetFileInputFiles(ctx, req.ChooserID, prepared); err != nil {
		vbrowser.DiscardUploads(prepared)
		jsonErrorCode(w, http.StatusConflict, "chooser_failed", err.Error(), nil)
		return
	}
	// Исходные pty-upload-* были лишь транспортом телефон → агент. Копия для
	// Chrome живёт до Stop браузера, поэтому двойники в ~/Remotai/files больше
	// не нужны и не засоряют файловый менеджер.
	for _, src := range sourcePaths {
		_ = os.Remove(src)
	}
	vbrowser.NoteActivity()
	jsonResp(w, map[string]any{"ok": true, "files": len(prepared)})
}

func browserUploadSource(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("empty upload path")
	}
	root, err := filepath.Abs(uploadDestDir())
	if err != nil {
		return "", fmt.Errorf("upload root: %w", err)
	}
	return validateBrowserUploadPath(root, path)
}

func validateBrowserUploadPath(root, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("upload path: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("upload root: %w", err)
	}
	// Только прямой потомок: вложенный каталог/.. и случайный файл из TEMP не
	// превращают browser chooser в чтение произвольного файла машины.
	if !sameBrowserPath(filepath.Dir(abs), root) {
		return "", fmt.Errorf("file is outside the upload directory")
	}
	base := filepath.Base(abs)
	if !strings.HasPrefix(base, "pty-upload-") {
		return "", fmt.Errorf("file was not uploaded for browser selection")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("uploaded file is unavailable")
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("uploaded path is not a regular file")
	}
	return abs, nil
}

func sameBrowserPath(a, b string) bool {
	if filepath.Separator == '\\' {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// POST /api/vbrowser/dialog
// {"dialog_id": 8, "accept": true, "prompt_text": "value"}
func (s *Server) apiBrowserDialog(w http.ResponseWriter, r *http.Request, _ int64) {
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	var req struct {
		DialogID   uint64 `json:"dialog_id"`
		Accept     bool   `json:"accept"`
		PromptText string `json:"prompt_text"`
	}
	if err := readJSON(r, &req); err != nil || req.DialogID == 0 {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "invalid dialog request", nil)
		return
	}
	ctx, cancel := browserCtx(r)
	defer cancel()
	if err := client.HandleJavaScriptDialog(ctx, req.DialogID, req.Accept, req.PromptText); err != nil {
		jsonErrorCode(w, http.StatusConflict, "dialog_stale", err.Error(), nil)
		return
	}
	vbrowser.NoteActivity()
	jsonResp(w, map[string]any{"ok": true})
}
