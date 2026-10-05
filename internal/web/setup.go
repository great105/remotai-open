package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/config"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/pty"
	"tgcontrol/internal/relay"
	"tgcontrol/internal/update"
	"tgcontrol/internal/version"
	"tgcontrol/internal/wincli"
)

// SetupDone is closed when the web setup wizard completes.
// main.go blocks on this channel in setup mode.
var SetupDone = make(chan struct{})
var setupDoneOnce sync.Once

// registerSetupRoutes adds unauthenticated setup wizard routes.
// These are registered BEFORE auth-protected routes.
func (s *Server) registerSetupRoutes() {
	// Setup wizard HTML page
	s.mux.HandleFunc("GET /setup", s.serveSetupPage)
	s.mux.HandleFunc("GET /setup/", s.serveSetupPage)

	// Setup API (no auth — read-only status/QR are open, всё мутирующее
	// дополнительно ограничено loopback'ом: управлять устройством можно
	// только с самого компьютера, не из LAN и не через relay-туннель).
	s.mux.HandleFunc("GET /api/setup/status", s.apiSetupStatus)
	s.mux.HandleFunc("GET /api/setup/detect", s.requireLoopback(s.apiSetupDetect))
	s.mux.HandleFunc("POST /api/setup/mode", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupMode)))
	s.mux.HandleFunc("POST /api/setup/central", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupCentral)))
	s.mux.HandleFunc("POST /api/setup/own-bot", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupOwnBot)))
	s.mux.HandleFunc("POST /api/setup/agents", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupAgents)))
	s.mux.HandleFunc("POST /api/setup/complete", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupComplete)))
	s.mux.HandleFunc("POST /api/setup/standalone", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupStandalone)))
	s.mux.HandleFunc("GET /api/setup/qr", s.apiSetupQR)

	// Cloud relay pairing (P2)
	s.mux.HandleFunc("POST /api/setup/cloud/pair-request", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupCloudPairRequest)))
	s.mux.HandleFunc("GET /api/setup/cloud/pair-status", s.requireLoopback(s.apiSetupCloudPairStatus))
	s.mux.HandleFunc("POST /api/setup/cloud/cancel", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupCloudCancel)))
	s.mux.HandleFunc("POST /api/setup/cloud/disconnect", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupCloudDisconnect)))
	s.mux.HandleFunc("POST /api/setup/cloud/enable", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupCloudEnable)))

	// Self-update from the desktop panel
	s.mux.HandleFunc("GET /api/setup/update-check", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupUpdateCheck)))
	s.mux.HandleFunc("POST /api/setup/update-apply", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupUpdateApply)))
	s.mux.HandleFunc("POST /api/setup/update-restart", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupUpdateRestart)))

	// Вход в локальный клиент (/miniapp) из панели и доступ с других устройств
	s.mux.HandleFunc("GET /api/setup/local-access", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupLocalAccess)))
	s.mux.HandleFunc("GET /api/setup/autostart", s.requireLoopback(s.apiSetupAutostart))
	s.mux.HandleFunc("POST /api/setup/autostart", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupAutostart)))
	s.mux.HandleFunc("GET /api/setup/lan-check", s.requireLoopback(s.apiSetupLANCheck))
	s.mux.HandleFunc("POST /api/setup/lan-check/allow", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupLANAllow)))
	s.mux.HandleFunc("POST /api/setup/port", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupPort)))
	s.mux.HandleFunc("POST /api/setup/open-window", s.requireLoopback(s.apiSetupOpenWindow))
	// «Уезжаю, пусть никто не подключается»: выход из программы из окна, а не
	// только из меню значка у часов (см. кнопку в карточке «Запуск и сеть»).
	s.mux.HandleFunc("POST /api/setup/quit", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupQuit)))

	// «Терминал на ПК»: окно терминала, сразу подключённое к новой сессии
	s.mux.HandleFunc("POST /api/setup/open-terminal", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupOpenTerminal)))

	// Внешние ссылки (remotai.ru, Telegram-бот, @BotFather) и установка
	// AI-помощника командой из реестра — оба открывают что-то ЗА пределами окна.
	s.mux.HandleFunc("POST /api/setup/open-external", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupOpenExternal)))
	s.mux.HandleFunc("POST /api/setup/install-agent", s.requireLoopback(s.rateLimit(s.setupLimiter, s.apiSetupInstallAgent)))

	// Диагностика стабильности WS-соединений (PTY + удалёнка) — loopback-only.
	s.mux.HandleFunc("GET /api/diag/connections", s.requireLoopback(s.apiDiagConnections))

	// Setup static assets
	setupDir := filepath.Join(filepath.Dir(exePath()), "setup_static")
	if info, err := os.Stat(setupDir); err == nil && info.IsDir() {
		s.mux.Handle("GET /setup/assets/", http.StripPrefix("/setup/assets/", http.FileServer(http.Dir(setupDir))))
	} else {
		// Use embedded setup assets
		sub, err := fs.Sub(embeddedSetup, "setup_static")
		if err == nil {
			s.mux.Handle("GET /setup/assets/", http.StripPrefix("/setup/assets/", http.FileServer(http.FS(sub))))
		}
	}
}

// setupGuard redirects to /setup if not configured, or to /miniapp if already done.
func (s *Server) setupGuard(w http.ResponseWriter, r *http.Request) bool {
	cfg := config.GetNoSetup()
	if !cfg.IsConfigured() && !strings.HasPrefix(r.URL.Path, "/setup") &&
		!strings.HasPrefix(r.URL.Path, "/api/setup") &&
		r.URL.Path != "/health" {
		http.Redirect(w, r, "/setup", http.StatusTemporaryRedirect)
		return true
	}
	return false
}

func exePath() string {
	exe, _ := os.Executable()
	return exe
}

// ── Setup API endpoints ─────────────────────────────────────────────

// isLoopbackRequest reports whether r originated on this machine. Requests
// tunneled through the relay have an empty RemoteAddr and count as remote.
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireLoopback ограничивает управляющие setup-эндпоинты локальной машиной
// (окно приложения ходит на localhost). Первичный setup-сервер и так слушает
// только loopback (StartLocal), так что легитимные сценарии не страдают.
func (s *Server) requireLoopback(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequest(r) {
			jsonError(w, "local access only", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// GET /api/setup/status — check if app is already configured
func (s *Server) apiSetupStatus(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetNoSetup()
	deviceID := config.GetOrCreateDeviceID()

	relayConfigured, relayConnected := false, false
	if s.relayStatus != nil {
		relayConfigured, relayConnected = s.relayStatus()
	}

	resp := map[string]any{
		"configured":       cfg.IsConfigured(),
		"mode":             cfg.Mode,
		"device_id":        deviceID,
		"hostname":         config.GetDeviceInfo().Hostname,
		"platform":         config.GetDeviceInfo().OS,
		"version":          version.Version,
		"auto_update":      !cfg.DisableAutoUpdate,
		"relay_configured": relayConfigured,
		"relay_connected":  relayConnected,
		"port":             cfg.Port(),
		"autostart":        getAutostartState(),
		// Живые клиенты приложения (телефон, браузер, окно exe). В режиме
		// локальной сети окно по ним показывает «приложение подключилось» —
		// раньше статус там навсегда застывал на «наведите камеру на QR».
		"clients_live": s.liveClientCount(),
		// Выход из программы есть только там, где ею владеет пользователь
		// (окно + трей). У службы/системного юнита кнопку не показываем.
		"can_quit": quitHandler != nil,
	}
	// Свой Telegram-бот: панель обязана показывать состояние ЕГО бота, а не
	// облачные строки (человек этот режим выбрал, чтобы облака не было).
	if cfg.IsOwnBot() {
		bot := map[string]any{"configured": s.botToken != ""}
		if s.botLiveness != nil {
			ok, last := s.botLiveness()
			bot["live"] = ok
			if !last.IsZero() {
				bot["last_contact"] = last.UnixMilli()
			}
		}
		resp["bot"] = bot
	}
	if s.startupBindError != nil {
		// original_free — освободился ли порт, который занимала чужая программа:
		// панель предлагает вернуться на него вместо «сменить порт ещё раз».
		resp["bind_error"] = map[string]any{
			"port":          s.startupBindError.Port,
			"owner":         s.startupBindError.Owner,
			"selected_port": s.startupBindError.SelectedPort,
			"message":       s.startupBindError.Message,
			"original_free": portAvailable(s.startupBindError.Port),
		}
	}
	// Владелец привязки: без него панель пишет «Всё работает», а человек с двумя
	// аккаунтами не понимает, почему компьютера нет в приложении.
	if acc := deviceAccountInfo(cfg); acc != nil {
		resp["account"] = acc
	}
	if !relayConnected && s.relayDetail != nil {
		if detail := s.relayDetail(); detail.Kind != "" {
			resp["relay_error"] = detail
		}
	}

	// Одноразовый маркер «только что обновились»: фоновое автообновление
	// оставляет его перед авто-рестартом, панель показывает «Обновлено до vX».
	if data, err := os.ReadFile(filepath.Join(paths.Base(), "update-applied")); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			resp["just_updated"] = v
		}
		_ = os.Remove(filepath.Join(paths.Base(), "update-applied"))
	}

	// Обновление скачано, но рестарт отложен из-за открытого окна — панель
	// должна об этом сказать и дать кнопку, иначе (как в проде до 2.26.0) о
	// готовом обновлении знает только лог.
	if v, since, ok := update.PendingInfo(); ok {
		resp["pending_update"] = map[string]any{
			"version":       v,
			"waiting_since": since.UnixMilli(),
		}
	}

	jsonResp(w, resp)
}

// liveClientCount — сколько клиентов приложения (телефон, браузер, окно exe)
// сейчас держат событийный WebSocket. Служебное соединение релея (uid = -1) не
// считаем: оно живёт всегда и «телефон подключился» из него не следует.
func (s *Server) liveClientCount() int {
	s.wsMu.RLock()
	defer s.wsMu.RUnlock()
	n := 0
	for uid, conns := range s.wsConns {
		if uid > 0 {
			n += len(conns)
		}
	}
	return n
}

// quitHandler — завершение программы тем же путём, что и пункт трея «Выйти».
// Ставит cmd/tgcontrol: веб-слой не владеет жизненным циклом приложения.
var quitHandler func()

// SetQuitHandler регистрирует завершение приложения для кнопки в панели.
func SetQuitHandler(fn func()) { quitHandler = fn }

// POST /api/setup/quit — «Завершить Remotai» из окна. До этого выход жил только
// в меню значка у часов, и намерение «пусть пока никто не подключится» ложилось
// на единственную заметную красную кнопку — «Удалить компьютер из аккаунта».
func (s *Server) apiSetupQuit(w http.ResponseWriter, r *http.Request) {
	if quitHandler == nil {
		jsonErrorCode(w, http.StatusServiceUnavailable, "quit_unavailable",
			"quit is not available in this mode", nil)
		return
	}
	jsonResp(w, map[string]bool{"ok": true})
	log.Printf("[SETUP] quit requested from panel")
	// Ответ уже ушёл — гасим приложение с небольшой задержкой, чтобы окно
	// успело показать плашку «Remotai завершается».
	go func() {
		time.Sleep(400 * time.Millisecond)
		quitHandler()
	}()
}

// portAvailable reports whether port can be bound right now (the panel offers
// «вернуться на прежний порт» only when the squatter has released it).
func portAvailable(port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// deviceAccountInfo reads the account this PC belongs to out of its own device
// JWT. The signature is not verified: the relay secret does not exist on the PC
// and the value is only displayed in the loopback-only panel. Not the pairing
// date — iat is refreshed on every relay connect — so only the account is shown.
func deviceAccountInfo(cfg *config.Config) map[string]any {
	token := cfg.RelayJWT
	if token == "" {
		token, _ = relay.LoadJWT()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		UserID   int64  `json:"user_id"`
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.UserID <= 0 {
		return nil
	}
	return map[string]any{"user_id": claims.UserID, "device_id": claims.DeviceID}
}

// POST /api/setup/cloud/enable — вернуть настроенный ПК из режима локальной сети
// в облачный. Вызывается панелью ПОСЛЕ подтверждения пейринга: сам режим меняем
// только тогда, когда устройство действительно попало в аккаунт, иначе
// отменённая попытка оставила бы LAN-пользователя без его QR-кода.
func (s *Server) apiSetupCloudEnable(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetNoSetup()
	jwt := cfg.RelayJWT
	if jwt == "" {
		jwt, _ = relay.LoadJWT()
	}
	if jwt == "" {
		jsonErrorCode(w, http.StatusConflict, "not_paired", "device is not paired to an account", nil)
		return
	}
	if err := config.Update(func(c *config.Config) {
		if c.Mode == config.ModeStandalone {
			c.Mode = config.ModeCentralBot
		}
	}); err != nil {
		jsonErrorCode(w, http.StatusInternalServerError, "config_save_failed", err.Error(), nil)
		return
	}
	// Релей-клиент работает во всех режимах, но перечитать конфиг с новым
	// режимом и свежим JWT ему стоит немедленно.
	s.relayKickMu.RLock()
	kick := s.relayKick
	s.relayKickMu.RUnlock()
	if kick != nil {
		kick()
	}
	mode := config.GetNoSetup().Mode
	log.Printf("[SETUP] cloud access enabled from panel (mode=%s)", mode)
	jsonResp(w, map[string]any{"ok": true, "mode": mode})
}

// POST /api/setup/open-window — activation target for a second Remotai process.
// The callback restores/navigates the existing WebView on its owning UI thread.
func (s *Server) apiSetupOpenWindow(w http.ResponseWriter, r *http.Request) {
	if s.openWindow == nil {
		jsonErrorCode(w, http.StatusServiceUnavailable, "window_unavailable", "native window is unavailable", nil)
		return
	}
	go s.openWindow()
	jsonResp(w, map[string]bool{"ok": true})
}

// GET/POST /api/setup/autostart — panel-safe wrappers around the same
// implementation used by the authenticated mobile System screen.
func (s *Server) apiSetupAutostart(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		jsonResp(w, getAutostartState())
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "invalid JSON", nil)
		return
	}
	var err error
	if body.Enabled {
		err = EnableAutostart()
	} else {
		err = DisableAutostart()
	}
	if err != nil {
		// Автозапуском управляет система (system-юнит systemd от install.sh) —
		// это не сбой панели, а «здесь так и должно быть»: тот же 409
		// managed_externally, что и у /api/system/autostart, чтобы окно exe и
		// телефон объясняли случай одинаково.
		if errors.Is(err, errAutostartManagedExternally) {
			jsonErrorCode(w, http.StatusConflict, "managed_externally", err.Error(),
				map[string]string{"method": getAutostartState().Method, "unit": systemUnitPath})
			return
		}
		jsonErrorCode(w, http.StatusInternalServerError, "autostart_failed", err.Error(), nil)
		return
	}
	jsonResp(w, getAutostartState())
}

func (s *Server) apiSetupLANCheck(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetNoSetup()
	allowed, detail := lanFirewallStatus(cfg.Port())
	s.httpSrvMu.Lock()
	bindAll := strings.HasPrefix(s.listenAddr, "0.0.0.0:")
	s.httpSrvMu.Unlock()
	jsonResp(w, map[string]any{
		"allowed":  allowed,
		"detail":   detail,
		"bind_all": bindAll,
		"port":     cfg.Port(),
	})
}

func (s *Server) apiSetupLANAllow(w http.ResponseWriter, r *http.Request) {
	if err := allowLANFirewall(config.GetNoSetup().Port()); err != nil {
		jsonErrorCode(w, http.StatusInternalServerError, "firewall_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "elevation_requested": runtime.GOOS == "windows"})
}

// POST /api/setup/port {port?:N}. With no port, chooses a free one. The value
// is persisted for the next restart; the response says whether the currently
// listening server must be restarted.
func (s *Server) apiSetupPort(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Port    int  `json:"port"`
		Restart bool `json:"restart"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	port := body.Port
	if port == 0 {
		port = findFreeWebPort(config.GetNoSetup().Port() + 1)
	}
	if port < 1 || port > 65535 {
		jsonErrorCode(w, http.StatusBadRequest, "bad_port", "port must be between 1 and 65535", nil)
		return
	}
	s.httpSrvMu.Lock()
	currentAddr := s.listenAddr
	s.httpSrvMu.Unlock()
	_, currentPortText, _ := net.SplitHostPort(currentAddr)
	currentPort, _ := strconv.Atoi(currentPortText)
	if port != currentPort {
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			jsonErrorCode(w, http.StatusConflict, "port_busy", fmt.Sprintf("port %d is busy", port), nil)
			return
		}
		_ = ln.Close()
	}
	if err := config.Update(func(c *config.Config) { c.WebPort = strconv.Itoa(port) }); err != nil {
		jsonErrorCode(w, http.StatusInternalServerError, "config_save_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "port": port, "restart_required": port != currentPort})
	if body.Restart && port != currentPort {
		go func() {
			time.Sleep(350 * time.Millisecond)
			if err := update.Restart(os.Args[1:]); err != nil {
				log.Printf("[SETUP] restart after port change failed: %v", err)
			}
		}()
	}
}

func findFreeWebPort(start int) int {
	if start < 1024 || start > 65535 {
		start = 8080
	}
	for port := start; port <= 65535 && port < start+200; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err == nil {
			_ = ln.Close()
			return port
		}
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err == nil {
		defer ln.Close()
		if _, p, splitErr := net.SplitHostPort(ln.Addr().String()); splitErr == nil {
			if n, convErr := strconv.Atoi(p); convErr == nil {
				return n
			}
		}
	}
	return 8080
}

// GET /api/setup/local-access — api-токен и адреса для входа в локальный
// клиент (/miniapp): из окна приложения, из браузера другого устройства в
// той же сети. Токен выдаётся только loopback-запросам (см. routes).
func (s *Server) apiSetupLocalAccess(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetNoSetup()
	uid := cfg.APITokenUID
	if uid == 0 {
		if id, err := strconv.ParseInt(cfg.TelegramUserID, 10, 64); err == nil && id > 0 {
			uid = id
		} else {
			uid = 1
		}
	}
	cfg.EnsureAPIToken(uid) // самосохраняется при генерации
	jsonResp(w, map[string]any{
		"token":    cfg.APIToken,
		"port":     cfg.Port(),
		"lan_urls": buildServerURLs(getLocalIPs(), cfg.Port()),
		"web_url":  "https://remotai.ru/app",
	})
}

// POST /api/setup/open-terminal — открыть на ПК окно терминала, сразу
// подключённое к НОВОЙ managed-сессии (remotai attach --new). Используют
// кнопка «Терминал на ПК» в панели и пункт трея; сессия тут же видна с
// телефона.
//
// ИСКЛЮЧЕНИЕ по окнам: здесь окно — и есть смысл действия, гасить его нельзя
// (см. procutil.Hidden). Все ОСТАЛЬНЫЕ запуски в этом файле и в соседних
// api_* — фоновые и идут без окна.
func (s *Server) apiSetupOpenTerminal(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetNoSetup()
	if !cfg.IsConfigured() {
		jsonError(w, "not configured yet", http.StatusBadRequest)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		jsonError(w, "executable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	cmd := fmt.Sprintf("& '%s' attach --new", strings.ReplaceAll(wincli.Executable(exe), "'", "''"))
	if err := pty.OpenOnHost(home, cmd); err != nil {
		log.Printf("[SETUP] open-terminal failed: %v", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[SETUP] host terminal opened (attach --new)")
	jsonResp(w, map[string]bool{"ok": true})
}

// externalLinkHosts — куда окну программы позволено уводить пользователя.
// Окно построено на WebView2 без обработчика новых окон: target="_blank" там
// не открывает ничего, поэтому страница отдаёт ссылки сюда. Белый список —
// чтобы loopback-эндпоинт не превратился в «запусти что угодно по протоколу».
var externalLinkHosts = map[string]bool{
	"remotai.ru":     true,
	"www.remotai.ru": true,
	"t.me":           true,
	"telegram.me":    true,
	// Redirect confirmation URLs returned by the YooKassa API.
	"yoomoney.ru": true,
	// Hermes device-code login and provider account pages. These always stay
	// in the OS browser, so authorizing a subscription never replaces Remotai.
	"auth.openai.com":         true,
	"portal.nousresearch.com": true,
	"github.com":              true,
	"openrouter.ai":           true,
}

// allowedExternalURL проверяет ссылку и возвращает её каноничный вид.
func allowedExternalURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("bad url")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if !externalLinkHosts[strings.ToLower(u.Hostname())] {
		return "", fmt.Errorf("host %q is not allowed", u.Hostname())
	}
	switch strings.ToLower(u.Hostname()) {
	case "auth.openai.com", "portal.nousresearch.com", "github.com", "openrouter.ai":
		if u.Scheme != "https" || u.User != nil || u.Port() != "" {
			return "", fmt.Errorf("provider login requires an HTTPS URL without credentials or a custom port")
		}
	}
	u.Scheme = "https"
	u.User = nil
	return u.String(), nil
}

// OpenExternalURL opens a URL in the user's default browser. Exported so the
// desktop host (cmd/tgcontrol) uses exactly the same launcher as the panel.
//
// ИСКЛЮЧЕНИЕ по окнам: procutil.Hidden здесь НЕ зовём — это «открыть человеку»
// (кнопки панели и пункты трея), окно браузера и есть результат. Лишней вспышки
// тут нет: rundll32/open/xdg-open — оконные программы, своей консоли не
// создают. А HideWindow ушёл бы дочернему процессу как nCmdShow=SW_HIDE, и
// браузер мог бы открыться невидимым — то есть кнопка «сломалась бы тихо».
func OpenExternalURL(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// POST /api/setup/open-external {url} — открыть ссылку в браузере пользователя.
// Без этого «Скачать на remotai.ru», ссылка на Telegram-бота и @BotFather в
// окне программы просто ничего не делают.
func (s *Server) apiSetupOpenExternal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "invalid JSON", nil)
		return
	}
	target, err := allowedExternalURL(body.URL)
	if err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "url_not_allowed", err.Error(), nil)
		return
	}
	if err := OpenExternalURL(target); err != nil {
		log.Printf("[SETUP] open-external failed (%s): %v", target, err)
		jsonErrorCode(w, http.StatusInternalServerError, "open_failed", err.Error(), nil)
		return
	}
	log.Printf("[SETUP] external link opened: %s", target)
	jsonResp(w, map[string]any{"ok": true, "url": target})
}

// POST /api/setup/install-agent {id} — открыть на этом компьютере окно
// терминала с командой установки AI-помощника из реестра. Шаг мастера
// «AI-агенты» иначе оставался списком без единого способа что-то поставить.
func (s *Server) apiSetupInstallAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "invalid JSON", nil)
		return
	}
	desc := agents.GetDescriptor(strings.TrimSpace(body.ID))
	if desc == nil {
		jsonErrorCode(w, http.StatusBadRequest, "unknown_agent", "unknown agent id", nil)
		return
	}
	command := desc.InstallCommand()
	if command == "" {
		jsonErrorCode(w, http.StatusBadRequest, "no_install_command", "agent has no install command", nil)
		return
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	if err := pty.OpenOnHost(home, command); err != nil {
		log.Printf("[SETUP] install-agent %s failed: %v", desc.ID, err)
		jsonErrorCode(w, http.StatusInternalServerError, "terminal_unavailable", err.Error(), nil)
		return
	}
	log.Printf("[SETUP] install terminal opened for %s: %s", desc.ID, command)
	jsonResp(w, map[string]any{"ok": true, "command": command})
}

// GET /api/setup/update-check — текущая версия + есть ли новая на remotai.ru.
// При сетевом сбое — check_failed:true (distinct статус), чтобы панель не
// показывала «У вас последняя версия» просто потому, что нет интернета.
func (s *Server) apiSetupUpdateCheck(w http.ResponseWriter, r *http.Request) {
	info, err := version.CheckForUpdate("")
	resp := map[string]any{
		"version": version.Version,
	}
	if err != nil {
		log.Printf("[UPDATE] check failed: %v", err)
		resp["check_failed"] = true
	} else if info != nil {
		resp["update"] = info
	}
	jsonResp(w, resp)
}

// POST /api/setup/update-apply — скачать новую версию, подменить бинарник и
// перезапуститься (тот же handoff-механизм, что у фонового автообновления).
func (s *Server) apiSetupUpdateApply(w http.ResponseWriter, r *http.Request) {
	info, err := version.CheckForUpdate("")
	if err != nil {
		jsonError(w, "update check failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if info == nil || !info.Available || info.DownloadURL == "" {
		jsonError(w, "no update available", http.StatusNotFound)
		return
	}
	log.Printf("[UPDATE] panel-triggered update to v%s", info.Version)
	if err := update.Apply(info.DownloadURL, info.SHA256); err != nil {
		log.Printf("[UPDATE] apply failed: %v", err)
		jsonError(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "version": info.Version})

	// Ответ уже ушёл — перезапускаемся в новый бинарник. Без --background:
	// пользователь обновился из окна, окно должно открыться снова.
	go func() {
		time.Sleep(700 * time.Millisecond)
		args := slices.Clone(os.Args[1:])
		if !slices.Contains(args, update.HandoffFlag) {
			args = append(args, update.HandoffFlag)
		}
		log.Printf("[UPDATE] v%s применена — перезапускаюсь", info.Version)
		if err := update.Restart(args); err != nil {
			log.Printf("[UPDATE] перезапуск не удался: %v", err)
		}
	}()
}

// POST /api/setup/update-restart — применить УЖЕ скачанное обновление, рестарт
// которого отложен из-за открытого окна. Ничего не качает: бинарник на диске
// подменён, нужен только перезапуск (кнопка «Перезапустить» в панели).
func (s *Server) apiSetupUpdateRestart(w http.ResponseWriter, r *http.Request) {
	version, _, ok := update.PendingInfo()
	if !ok {
		jsonError(w, "no pending update", http.StatusNotFound)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "version": version})
	// Ответ уже ушёл — рестарт запускает сам update (в своей горутине).
	update.RestartPending()
}

// setupFoundAgent — помощник, реально найденный на этом компьютере.
type setupFoundAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
	Path string `json:"path"`
}

// setupInstallableAgent — помощник, которого здесь нет, но есть чем поставить.
type setupInstallableAgent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	Command string `json:"command"`
}

// setupDetectResponse дополняет возможности системы двумя готовыми списками:
// шагу мастера нужны найденные помощники и команды установки для остальных, а
// не сырой реестр, из которого раньше получалась стена «не найден».
type setupDetectResponse struct {
	*config.SystemCapabilities
	AgentsFound       []setupFoundAgent       `json:"agents_found"`
	AgentsInstallable []setupInstallableAgent `json:"agents_installable"`
}

// setupAgentLists делит CLI-помощников на «найдены» и «можно установить».
// Встроенные (shell/orchestrator/researcher) пропускаются: подсистема сессий
// скрыта в интерфейсе, и на первом запуске это лишний шум.
func setupAgentLists(caps *config.SystemCapabilities) ([]setupFoundAgent, []setupInstallableAgent) {
	scanned := make(map[string]string, len(caps.Agents))
	for _, a := range caps.Agents {
		if a.BuiltIn {
			continue
		}
		scanned[a.ID] = a.Path
	}
	found := []setupFoundAgent{}
	installable := []setupInstallableAgent{}
	for _, d := range agents.Registry {
		if len(d.CLINames) == 0 {
			continue // built-in: ставить нечего и искать нечего
		}
		path, alreadyScanned := scanned[d.ID]
		if !alreadyScanned {
			// Агент есть в реестре, но не в наборе первичного сканирования —
			// проверяем его сами, иначе установленный помощник попал бы в
			// «можно установить».
			for _, cliName := range d.CLINames {
				if p := config.FindCLI(cliName); p != "" {
					path = p
					break
				}
			}
		}
		if path != "" {
			found = append(found, setupFoundAgent{ID: d.ID, Name: d.Name, Icon: d.Icon, Path: path})
			continue
		}
		if command := d.InstallCommand(); command != "" {
			installable = append(installable, setupInstallableAgent{
				ID: d.ID, Name: d.Name, Icon: d.Icon, Command: command,
			})
		}
	}
	return found, installable
}

// GET /api/setup/detect — auto-detect system capabilities
func (s *Server) apiSetupDetect(w http.ResponseWriter, r *http.Request) {
	caps := config.DetectCapabilities()
	found, installable := setupAgentLists(caps)
	jsonResp(w, setupDetectResponse{
		SystemCapabilities: caps,
		AgentsFound:        found,
		AgentsInstallable:  installable,
	})
}

// POST /api/setup/mode — set connection mode
func (s *Server) apiSetupMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}
	if req.Mode != config.ModeOwnBot && req.Mode != config.ModeCentralBot {
		jsonError(w, "Invalid mode: must be 'own_bot' or 'central_bot'", 400)
		return
	}

	cfg := config.GetNoSetup()
	cfg.Mode = req.Mode
	if err := cfg.Save(); err != nil {
		jsonError(w, "Failed to save config", 500)
		return
	}

	log.Printf("[SETUP] Mode set to: %s", req.Mode)
	jsonResp(w, map[string]bool{"ok": true})
}

// POST /api/setup/central — configure central bot mode
func (s *Server) apiSetupCentral(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TelegramUserID string `json:"telegram_user_id"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	// Validate user ID is numeric
	uid, err := strconv.ParseInt(req.TelegramUserID, 10, 64)
	if err != nil || uid <= 0 {
		jsonError(w, "Telegram User ID must be a positive number", 400)
		return
	}

	cfg := config.GetNoSetup()
	cfg.Mode = config.ModeCentralBot
	cfg.TelegramUserID = req.TelegramUserID
	cfg.ConnectionToken = generateSetupToken()
	cfg.RelayURL = config.RelayURL
	cfg.DeviceID = config.GetOrCreateDeviceID()

	if err := cfg.Save(); err != nil {
		jsonError(w, "Failed to save config", 500)
		return
	}

	log.Printf("[SETUP] Central bot configured: user_id=%s, device=%s", cfg.TelegramUserID, cfg.DeviceID)
	jsonResp(w, map[string]any{
		"ok":               true,
		"connection_token": cfg.ConnectionToken,
		"device_id":        cfg.DeviceID,
	})
}

// POST /api/setup/own-bot — configure own bot mode
func (s *Server) apiSetupOwnBot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BotToken     string `json:"bot_token"`
		AllowedUsers string `json:"allowed_users"` // comma-separated
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	if req.BotToken == "" {
		jsonError(w, "Bot token is required", 400)
		return
	}

	// Save to .env file (same dir as config.json — см. internal/paths)
	envPath := filepath.Join(paths.Base(), ".env")
	lines := []string{fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s", req.BotToken)}
	if req.AllowedUsers != "" {
		lines = append(lines, fmt.Sprintf("ALLOWED_USERS=%s", req.AllowedUsers))
	}
	if err := os.WriteFile(envPath, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		jsonError(w, "Failed to save .env", 500)
		return
	}

	cfg := config.GetNoSetup()
	cfg.Mode = config.ModeOwnBot
	cfg.DeviceID = config.GetOrCreateDeviceID()
	if err := cfg.Save(); err != nil {
		jsonError(w, "Failed to save config", 500)
		return
	}

	log.Printf("[SETUP] Own bot configured: .env saved")
	jsonResp(w, map[string]bool{"ok": true})
}

// POST /api/setup/agents — configure agent settings
func (s *Server) apiSetupAgents(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClaudePath           string `json:"claude_path"`
		ClaudeModel          string `json:"claude_model"`
		ClaudePermissionMode string `json:"claude_permission_mode"`
		CodexPath            string `json:"codex_path"`
		CodexModel           string `json:"codex_model"`
		CodexApprovalMode    string `json:"codex_approval_mode"`
		CodexReasoning       string `json:"codex_reasoning"`
		DefaultAgent         string `json:"default_agent"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	cfg := config.GetNoSetup()
	if req.ClaudePath != "" {
		cfg.ClaudePath = req.ClaudePath
	}
	if req.ClaudeModel != "" {
		cfg.ClaudeModel = req.ClaudeModel
	}
	if req.ClaudePermissionMode != "" {
		cfg.ClaudePermissionMode = req.ClaudePermissionMode
	}
	if req.CodexPath != "" {
		cfg.CodexPath = req.CodexPath
	}
	if req.CodexModel != "" {
		cfg.CodexModel = req.CodexModel
	}
	if req.CodexApprovalMode != "" {
		cfg.CodexApprovalMode = req.CodexApprovalMode
	}
	if req.CodexReasoning != "" {
		cfg.CodexReasoning = req.CodexReasoning
	}
	if req.DefaultAgent != "" {
		cfg.DefaultAgent = req.DefaultAgent
	}

	if err := cfg.Save(); err != nil {
		jsonError(w, "Failed to save config", 500)
		return
	}

	log.Printf("[SETUP] Agents configured: claude=%s, model=%s", cfg.ClaudePath, cfg.ClaudeModel)
	jsonResp(w, map[string]bool{"ok": true})
}

// POST /api/setup/standalone — finish setup in no-Telegram, app-only mode.
// Generates the api_token (if missing), marks setup complete and returns the
// pairing info (LAN addresses + token + code + QR) the user enters in the app.
func (s *Server) apiSetupStandalone(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetNoSetup()
	cfg.Mode = config.ModeStandalone
	cfg.DeviceID = config.GetOrCreateDeviceID()
	cfg.EnsureAPIToken(1)
	cfg.SetupComplete = true
	if err := cfg.Save(); err != nil {
		jsonError(w, "Failed to save config", 500)
		return
	}

	picked := s.pickServerURLInfo(cfg)
	pairingURL := buildPairingURL(picked.URL, cfg.APIToken, cfg.DeviceID)
	pairingCode := buildPairingCode(picked.URL, cfg.APIToken)

	log.Printf("[SETUP] Standalone (no-Telegram) setup complete! Device: %s", cfg.DeviceID)

	jsonResp(w, map[string]any{
		"ok":           true,
		"device_id":    cfg.DeviceID,
		"token":        cfg.APIToken,
		"pairing_code": pairingCode,
		"pairing_url":  pairingURL,
		"qr_url":       "/api/setup/qr?data=" + url.QueryEscape(pairingURL),
		"server_url":   picked.URL,
		"server_urls":  buildServerURLs(getLocalIPs(), cfg.Port()),
		"mode":         cfg.Mode,
	})

	setupDoneOnce.Do(func() { close(SetupDone) })
}

// POST /api/setup/complete — finalize setup and generate pairing info
func (s *Server) apiSetupComplete(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetNoSetup()
	cfg.SetupComplete = true
	cfg.DeviceID = config.GetOrCreateDeviceID()

	// Ensure API token exists
	var defaultUID int64 = 1
	if uid, err := strconv.ParseInt(cfg.TelegramUserID, 10, 64); err == nil && uid > 0 {
		defaultUID = uid
	}
	cfg.EnsureAPIToken(defaultUID)

	if err := cfg.Save(); err != nil {
		jsonError(w, "Failed to save config", 500)
		return
	}

	// Build pairing info — prefer tunnel URL over LAN-IP so the code works
	// outside the home Wi-Fi.
	picked := s.pickServerURLInfo(cfg)
	serverURL := picked.URL

	pairingURL := buildPairingURL(serverURL, cfg.APIToken, cfg.DeviceID)
	pairingCode := buildPairingCode(serverURL, cfg.APIToken)

	log.Printf("[SETUP] Setup complete! Device: %s (URL kind=%s)", cfg.DeviceID, picked.Kind)

	jsonResp(w, map[string]any{
		"ok":           true,
		"device_id":    cfg.DeviceID,
		"pairing_code": pairingCode,
		"pairing_url":  pairingURL,
		"qr_url":       "/api/setup/qr?data=" + url.QueryEscape(pairingURL),
		"server_url":   picked.URL,
		"server_kind":  picked.Kind,
		"server_note":  picked.Note,
		"server_urls":  buildServerURLs(getLocalIPs(), cfg.Port()),
		"mode":         cfg.Mode,
	})

	// Signal that setup is done — unblock main.go (safe for concurrent calls)
	setupDoneOnce.Do(func() { close(SetupDone) })
}

func buildServerURLs(ips []string, port int) []string {
	var urls []string
	for _, ip := range ips {
		urls = append(urls, fmt.Sprintf("http://%s:%d", ip, port))
	}
	return urls
}

func generateSetupToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ── Serve setup page ────────────────────────────────────────────────

func (s *Server) serveSetupPage(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetNoSetup()
	// When configured, a plain /setup hit (e.g. a browser) goes to the app.
	// The desktop window passes ?force=1 to get the setup page anyway — there
	// setup.js renders a "running" status screen + QR to add another phone.
	if cfg.IsConfigured() && r.URL.Query().Get("force") != "1" {
		target := "/miniapp/"
		if lang := r.URL.Query().Get("lang"); lang == "en" || lang == "ru" {
			target += "?lang=" + lang
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}

	// Try embedded setup HTML
	setupFile := "setup_static/index.html"
	if r.URL.Query().Get("lang") == "en" {
		setupFile = "setup_static/index.en.html"
	}
	data, err := embeddedSetup.ReadFile(setupFile)
	if err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
		return
	}

	// Try on-disk setup HTML
	exe, _ := os.Executable()
	indexPath := filepath.Join(filepath.Dir(exe), filepath.FromSlash(setupFile))
	if _, err := os.Stat(indexPath); err == nil {
		http.ServeFile(w, r, indexPath)
		return
	}

	// Inline fallback
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(setupFallbackHTML)
}

// Minimal inline HTML if setup_static is missing
var setupFallbackHTML = []byte(`<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>TGControl — Setup</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,system-ui,sans-serif;background:#0d1117;color:#c9d1d9;min-height:100vh;display:flex;align-items:center;justify-content:center}
.card{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:32px;max-width:480px;width:100%}
h1{font-size:24px;margin-bottom:8px;color:#f0f6fc}
p{color:#8b949e;margin-bottom:16px;line-height:1.5}
.error{color:#f85149;font-size:14px}
</style>
</head>
<body>
<div class="card">
<h1>TGControl Setup</h1>
<p>Setup wizard files not found. Please rebuild:</p>
<code style="color:#79c0ff;font-size:14px">go build -o tgcontrol.exe ./cmd/tgcontrol/</code>
<p class="error" style="margin-top:16px">Embedded setup_static/ missing from binary.</p>
</div>
</body>
</html>`)
