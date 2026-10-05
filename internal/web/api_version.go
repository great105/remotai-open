package web

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/kbinani/screenshot"

	"tgcontrol/internal/config"
	"tgcontrol/internal/connstat"
	"tgcontrol/internal/desktopui"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/selfheal"
	"tgcontrol/internal/service"
	"tgcontrol/internal/update"
	"tgcontrol/internal/vbrowser"
	"tgcontrol/internal/version"
)

// GET /api/system/version — return version info and check for updates
func (s *Server) apiVersion(w http.ResponseWriter, r *http.Request, uid int64) {
	info := version.Info()

	// Check for updates (best-effort, short timeout to avoid blocking)
	type updateResult struct {
		info *version.UpdateInfo
	}
	ch := make(chan updateResult, 1)
	go func() {
		info, _ := version.CheckForUpdate("")
		ch <- updateResult{info}
	}()

	var updateInfo *version.UpdateInfo
	select {
	case res := <-ch:
		updateInfo = res.info
	case <-time.After(2 * time.Second):
		// Timeout — skip update check
	}

	resp := map[string]any{
		"version":         info["version"],
		"current":         info["version"],
		"commit":          info["commit"],
		"build_date":      info["build_date"],
		"auto_update":     !config.GetNoSetup().DisableAutoUpdate,
		"pending_restart": false,
	}
	if updateInfo != nil {
		resp["update"] = updateInfo
		resp["latest"] = updateInfo.Version
		resp["available"] = updateInfo.Available
	}
	if pendingVersion, since, ok := update.PendingInfo(); ok {
		resp["pending_restart"] = true
		resp["latest"] = pendingVersion
		resp["deferred_since"] = since.UnixMilli()
	}

	jsonResp(w, resp)
}

// POST /api/system/update — trigger self-update
func (s *Server) apiUpdate(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		DownloadURL string `json:"download_url"`
		SHA256      string `json:"sha256"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	// Обновление уже скачано и ждёт тихой минуты — качать нечего, нужен только
	// перезапуск. Раньше эта ветка отсутствовала, и «Обновить» с телефона шло
	// в сеть за файлом, который уже лежит на диске: лишняя минута ожидания на
	// мобильном интернете и лишний шанс упасть на скачивании.
	if pendingVersion, _, ok := update.PendingInfo(); ok && req.DownloadURL == "" {
		log.Printf("[UPDATE] v%s уже скачана — перезапускаюсь по запросу клиента", pendingVersion)
		jsonResp(w, map[string]any{
			"ok":      true,
			"version": pendingVersion,
			"message": "Update applied. Restarting…",
		})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// RestartPending сам зовёт restartNow из autoupdate.go: тот предупредит
		// живые сеансы и поставит маркер «обновлён до vX».
		update.RestartPending()
		return
	}

	knownVersion := ""
	if req.DownloadURL == "" {
		// Check for update to get URL
		info, err := version.CheckForUpdate("")
		if err != nil || !info.Available {
			jsonError(w, "No update available", 404)
			return
		}
		req.DownloadURL = info.DownloadURL
		req.SHA256 = info.SHA256
		knownVersion = info.Version
	}

	if req.DownloadURL == "" {
		jsonError(w, "No download URL", 400)
		return
	}

	log.Printf("[UPDATE] Starting update from: %s", req.DownloadURL)

	if err := update.Apply(req.DownloadURL, req.SHA256); err != nil {
		log.Printf("[UPDATE] Failed: %v", err)
		jsonError(w, "Update failed: "+err.Error(), 500)
		return
	}

	// Раньше путь обрывался здесь: «Update applied — restart required», и если
	// перезапускать было некому (окно панели висит сутками), процесс НАВСЕГДА
	// оставался на старой версии при подменённом бинарнике — живой случай
	// 2026-07-29: ПК застрял на 2.46.3 при трёх «применённых» обновлениях, и
	// человек не понимал, почему фиксы не работают. Теперь перезапускаемся
	// сами, как автообновление: предупреждение живым сеансам → маркер для
	// панели → handoff-рестарт. Ответ клиенту уходит ДО рестарта.
	log.Println("[UPDATE] Update applied successfully — restarting")
	jsonResp(w, map[string]any{
		"ok":      true,
		"message": "Update applied. Restarting…",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go s.restartAfterAppliedUpdate(knownVersion)
}

// restartAfterAppliedUpdate завершает ручное обновление перезапуском процесса
// (зеркало restartNow из cmd/tgcontrol/autoupdate.go). В service-режиме не
// перезапускаемся: за этим следит SCM, поведение прежнее.
func (s *Server) restartAfterAppliedUpdate(newVersion string) {
	// Служба, которую сервис-менеджер сам не поднимет и попросить его нельзя, —
	// единственный случай, когда мы честно ждём чужого перезапуска.
	managed := service.RunAsService()
	if managed && !service.ManagerCanRestart() {
		log.Printf("[UPDATE] вступит в силу после перезапуска сервиса")
		return
	}
	// Предупреждаем живые сеансы — иначе обрыв выглядит безликим
	// «Переподключение…» посреди работы (тот же приём, что у автообновления).
	warned := make(map[int64]bool)
	for _, c := range connstat.Default.Snapshot(1).Active {
		if warned[c.UID] {
			continue
		}
		warned[c.UID] = true
		s.Broadcast(c.UID, map[string]any{"type": "agent_updating", "version": newVersion})
	}
	if len(warned) > 0 {
		time.Sleep(500 * time.Millisecond) // дать событию уйти в сокеты/релей
	}
	// Маркер для панели («Обновлено до vX» при следующем открытии) — как
	// markUpdateApplied у автообновления; версия известна не всегда (клиент мог
	// прислать голый URL), тогда маркер не пишем.
	if newVersion != "" {
		p := filepath.Join(paths.Base(), "update-applied")
		if err := os.WriteFile(p, []byte(newVersion), 0o600); err != nil {
			log.Printf("[UPDATE] маркер обновления не записан: %v", err)
		}
	}
	// Прощаемся в heartbeat, как это делает автообновление (restartNow в
	// cmd/tgcontrol/autoupdate.go). Без отметки следующий запуск называл ручное
	// обновление с телефона падением: «Remotai пропадал на 54 с — приложение
	// закрыли или оно упало», три раза за вечер 01.09.2026 в боевом логе, а при
	// перерыве дольше трёх минут ушло бы и сообщение владельцу.
	selfheal.MarkStop("update")
	// launchd: просим менеджер перезапустить службу. Сами перезапуститься не
	// можем — процесс родился бы вне домена службы и умер при первом kickstart
	// (см. service.RunAsService). Живой мак 10.08.2026 больше часа работал на
	// 2.55.16 с уже подменённым на 2.55.18 бинарём и скачивал одно и то же
	// обновление по кругу: применили — и никто не перезапустил.
	if managed {
		log.Printf("[UPDATE] прошу сервис-менеджер перезапустить агента на v%s", newVersion)
		if err := service.RestartByManager(); err != nil {
			log.Printf("[UPDATE] перезапуск менеджером не удался: %v", err)
		}
		return
	}
	if service.UnderSystemd() {
		log.Printf("[UPDATE] выхожу — systemd поднимет сервис на новой версии")
		os.Exit(0)
	}
	args := slices.Clone(os.Args[1:])
	if !slices.Contains(args, update.HandoffFlag) {
		args = append(args, update.HandoffFlag)
	}
	args = slices.DeleteFunc(args, func(a string) bool { return a == "--background" })
	// Окно открыто — возвращаемся с окном; нет — тихо в трей.
	if !desktopui.WindowOpen() {
		args = append(args, "--background")
	}
	if err := update.Restart(args); err != nil {
		log.Printf("[UPDATE] перезапуск не удался: %v", err)
	}
}

// GET /api/system/service — return Windows service status
func (s *Server) apiServiceStatus(w http.ResponseWriter, r *http.Request, uid int64) {
	deviceInfo := config.GetDeviceInfo()
	runningAsService := service.RunAsService()
	vbs := vbrowser.GetStatus()
	// Виртуальный браузер (Xvfb) сам создаёт дисплей — для него и «нет
	// дисплея», и запрет service-mode не действуют: захват идёт с нашего
	// виртуального экрана, а не чужой интерактивной сессии.
	display := hasDisplay() || vbs.Running
	rdSupported := (!runningAsService && display) || vbs.Running

	jsonResp(w, map[string]any{
		"service_installed":  service.IsInstalled(),
		"service_running":    service.IsRunning(),
		"running_as_service": runningAsService,
		// Remote Desktop нужен реальный дисплей: на headless-сервере (нет X11/
		// Wayland/активных мониторов) захват экрана невозможен — клиент по этому
		// флагу прячет вкладку удалёнки целиком (vbrowser.available её вернёт).
		"remote_desktop_supported": rdSupported,
		"has_display":              display,
		"remote_desktop_warning":   serviceModeRemoteWarning(runningAsService && !vbs.Running, display),
		"vbrowser":                 vbs,
		"device_id":                deviceInfo.DeviceID,
		"hostname":                 deviceInfo.Hostname,
		"os":                       deviceInfo.OS,
		"arch":                     deviceInfo.Arch,
	})
}

// hasDisplay reports whether a capturable display exists. On Linux/other a
// headless server has no X11/Wayland session — check the env first (cheap and
// avoids poking an absent X server) before asking the screenshot backend.
func hasDisplay() bool {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
	}
	return screenshot.NumActiveDisplays() > 0
}

func serviceModeRemoteWarning(runningAsService, display bool) string {
	if runningAsService {
		return "Remote Desktop capture requires Remotai to run in the interactive user session."
	}
	if !display {
		return "No display detected — Remote Desktop is unavailable on a headless server. Log into an X11 session to enable it."
	}
	return ""
}
