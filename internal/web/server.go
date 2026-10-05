package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/aiusage"
	"tgcontrol/internal/config"
	"tgcontrol/internal/license"
	"tgcontrol/internal/observability"
	"tgcontrol/internal/openrouter"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/pty"
	"tgcontrol/internal/relay"
	"tgcontrol/internal/sessions"
	"tgcontrol/internal/tokenusage"
	"tgcontrol/internal/tunnel"
	"tgcontrol/internal/vbrowser"
	"tgcontrol/internal/wsutil"
)

// wsClient wraps a hub WebSocket connection with its own write mutex so the
// broadcaster can write concurrently to different clients without ever holding
// the shared wsMu during a (potentially blocking) network write, and without two
// goroutines writing to the same conn at once (which gorilla forbids).
type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

// send writes one text frame with a bounded write deadline. A slow/dead client
// returns an error here (and is then evicted) instead of stalling every broadcast.
func (c *wsClient) send(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// initDataMaxAge is the maximum age of Telegram initData in seconds (12 hours).
const initDataMaxAge int64 = 43200

// Server is the Mini App web server (REST API + WebSocket + static).
type Server struct {
	hermesMu          sync.Mutex
	hermesManagers    map[int64]hermesRuntime
	hermesJobs        map[int64]bool
	hermesCtx         context.Context
	hermesCancel      context.CancelFunc
	hermesClosing     bool
	serverAccessCheck func(context.Context, bool) (relay.ServerAccess, error)
	store             *sessions.Store
	history           *sessions.History
	botToken          string
	allowed           map[int64]bool
	mux               *http.ServeMux
	upgrader          websocket.Upgrader
	distDir           string

	// Живой http.Server — нужен для Shutdown по требованию (см. Shutdown),
	// когда гасить сервер надо раньше, чем отменится ctx запуска.
	httpSrvMu  sync.Mutex
	httpSrv    *http.Server
	listenAddr string

	// Desktop setup/panel integration. openWindow is deliberately a callback:
	// web does not own the native UI lifecycle and remains usable headlessly.
	openWindow       func()
	startupBindError *StartupBindError

	// WebSocket connections: uid -> set of connections
	wsMu    sync.RWMutex
	wsConns map[int64]map[*wsClient]bool

	// botLiveness, if set, reports whether the Telegram bot has recently reached
	// Telegram (used by the health report). Set by main.go from the bot watchdog.
	botLiveness func() (ok bool, lastContact time.Time)

	// Running agent stop channels: (uid, session) -> cancel
	stopMu     sync.Mutex
	stopChans  map[string]chan struct{}
	agentMu    sync.Mutex
	agentLocks map[string]*sync.Mutex

	// Pending slash command selections: "uid:session" -> pending
	pendingMu         sync.Mutex
	pendingSelections map[string]*pendingSelection

	// Bookmarks: uid -> list, персистится в ~/.tgcontrol-bookmarks.json
	bookmarks *bookmarkStore
	// История папок, в которых открывали терминалы (вкладка «Недавние»).
	recent *recentStore

	// SSH-хаб агента: сохранённые хосты (ssh_hosts.json), история подключений
	// (ssh_history.json), активные форварды и пул SFTP-соединений. Форварды и
	// SFTP-пул живут только в памяти — рестарт агента их убивает.
	sshHosts        *pty.SSHHostStore
	sshHistory      *pty.SSHHistoryStore
	sshForwards     *pty.ForwardManager
	sshForwardSpecs *pty.SSHForwardSpecStore
	sftpPool        *pty.SFTPPool
	sshSecrets      *sshSecretStore
	sshKeys         *pty.SSHKeyStore

	// openrouter — ключ OpenRouter этого компьютера. Лежит рядом с паролями
	// серверов и по тем же правилам: шифруется secretbox, наружу не отдаётся.
	openrouter *openrouter.Store

	// PTY interactive terminals
	ptyManager *pty.Manager

	// Живые PTY-WS по ключу "uid|session" — серверная страховка от штормов
	// класса «зомби-циклы» (2.15.12): даже если клиент опять начнёт плодить
	// сокеты, при превышении капа старейший вытесняется (reason "evicted:cap")
	// и толпа не копится. Легитимный максимум — по сокету на поверхность
	// (телефон/web/tg/окно exe) с небольшим запасом.
	ptyLiveMu sync.Mutex
	ptyLive   map[string][]*ptyLiveSlot

	// Event ring buffer (per-uid) for replay on reconnect (?since=<id>)
	eventBuf *eventBuffer

	// relayEventSink, if set, receives a copy of every broadcast event (the
	// id-stamped JSON) so the relay client can forward it to remote cloud
	// clients. Set by main.go to relay.Client.ForwardEvent.
	relayEventSink func(data []byte)

	// relayKick, if set, forces the relay client to reconnect with fresh
	// credentials. Set by main.go to relay.Client.Kick; invoked after pairing
	// saves a new device JWT.
	relayKickMu sync.RWMutex
	relayKick   func()

	// Cloud-pairing continues in the server after the setup page is closed.
	// Only the latest code is watched; issuing a new one cancels the old poller.
	cloudPairMu         sync.Mutex
	cloudPairCompleteMu sync.Mutex
	cloudPairCode       string
	cloudPairCancel     context.CancelFunc

	// relayStatus, if set, reports the relay link state (configured, connected)
	// for the health dashboard. Set by main.go to relay.Client.Status.
	relayStatus func() (configured, connected bool)
	relayDetail func() RelayConnectionDetail

	// Auth manager (JWT + OIDC)
	authManager *AuthManager

	// License & billing
	licenseManager *license.Manager
	stripeClient   *license.StripeClient

	// Tunnel manager
	tunnelManager *tunnel.Manager

	// Rate limiters
	authLimiter  *rateLimiter
	setupLimiter *rateLimiter
	apiLimiter   *rateLimiter
	// Быстрые ответы агенту (POST /api/pty/{id}/input) — свой бакет, см.
	// комментарий в NewServer.
	ptyInputLimiter *rateLimiter

	// Переносы ПК ↔ SSH-сервер (#9): байты идут внутри ПК, телефон только
	// запускает и смотрит прогресс. Живут в памяти, как PTY-сессии.
	sshTransfers *sshTransferManager

	sendFileToTelegram func(context.Context, int64, string) error
}

// StartupBindError is exposed by /api/setup/status when the configured port
// belonged to another process and Remotai selected a safe free port.
type StartupBindError struct {
	Port         int    `json:"port"`
	Owner        string `json:"owner,omitempty"`
	SelectedPort int    `json:"selected_port"`
	Message      string `json:"message"`
}

// RelayConnectionDetail is the stable, UI-safe relay diagnosis. It contains no
// token or URL query data.
type RelayConnectionDetail struct {
	Kind       string `json:"kind,omitempty"`
	HTTPStatus int    `json:"http,omitempty"`
	At         int64  `json:"at,omitempty"`
	Message    string `json:"message,omitempty"`
}

type bookmark struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// pendingSelection tracks a slash command that showed a numbered list.
type pendingSelection struct {
	Cmd     string   // e.g. "/model"
	Options []string // e.g. ["sonnet", "opus", "haiku"]
	Time    time.Time
}

// NewServer creates a configured web server.
func NewServer(store *sessions.Store, history *sessions.History, botToken string, allowedUsers map[int64]bool) *Server {
	s := &Server{
		store:             store,
		history:           history,
		botToken:          botToken,
		allowed:           allowedUsers,
		mux:               http.NewServeMux(),
		wsConns:           make(map[int64]map[*wsClient]bool),
		stopChans:         make(map[string]chan struct{}),
		agentLocks:        make(map[string]*sync.Mutex),
		pendingSelections: make(map[string]*pendingSelection),
		bookmarks:         newBookmarkStore(),
		recent:            newRecentStore(),
		sshHosts:          pty.NewSSHHostStore(filepath.Join(paths.Base(), "ssh_hosts.json")),
		sshHistory:        pty.NewSSHHistoryStore(filepath.Join(paths.Base(), "ssh_history.json")),
		sshForwards:       pty.NewForwardManager(),
		sshForwardSpecs:   pty.NewSSHForwardSpecStore(filepath.Join(paths.Base(), "ssh_forwards.json")),
		sftpPool:          pty.NewSFTPPool(),
		sshSecrets:        newSSHSecretStore(filepath.Join(paths.Base(), "ssh_secrets.enc")),
		sshKeys:           pty.NewSSHKeyStore(filepath.Join(paths.Base(), "ssh_keys.json")),
		openrouter:        openrouter.NewStore(filepath.Join(paths.Base(), "openrouter.enc")),
		ptyManager:        pty.NewManager(),
		ptyLive:           make(map[string][]*ptyLiveSlot),
		eventBuf:          newEventBuffer(),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return isAllowedOriginForReq(r.Header.Get("Origin"), r)
			},
		},
		authLimiter:  newRateLimiter(10, time.Minute), // 10 req/min per IP for auth
		setupLimiter: newRateLimiter(60, time.Minute), // 60 req/min per IP — setup wizard (localhost, pre-auth) makes many calls
		apiLimiter:   newRateLimiter(60, time.Minute), // 60 req/min per IP for API
		// 120/мин: человек, отвечающий агенту (1/2/3, Enter, короткий текст),
		// легитимно кликает чаще, чем создаёт терминалы. Бакет ОТДЕЛЬНЫЙ от
		// apiLimiter, чтобы загрузки файлов и /api/ssh/connect не съедали лимит
		// ответов и не отдавали 429 ровно в тот момент, когда агент ждёт ответа
		// (бакеты ключуются по пользователю, см. rateLimitAuth).
		ptyInputLimiter: newRateLimiter(120, time.Minute),
	}
	s.sshTransfers = newSSHTransferManager(s)

	// Route PTY heuristic events through the per-user event broadcaster
	// (so they replay via ?since= and reach APK push notifications).
	s.ptyManager.SetBroadcaster(broadcastAdapter{s: s})

	// Распознавание вопросов агента по экрану — по настройке, по умолчанию
	// выключено (см. config.DetectAgentQuestions): текст на экране не отличает
	// рассказ о меню от самого меню.
	s.ptyManager.SetQuestionDetection(config.GetNoSetup().DetectAgentQuestions)

	// Разметка команд OSC 133 в новых терминалах (ST-10) — по настройке, по
	// умолчанию выключено (см. config.ShellIntegration).
	s.ptyManager.SetShellIntegration(config.GetNoSetup().ShellIntegration)

	// PrtScr на этом компьютере: снимок падает файлом туда, откуда его читает
	// агент. Выключено по умолчанию — клавишу нельзя отбирать молча.
	setScreenshotHotkey(config.GetNoSetup().ScreenshotHotkey)

	// Понятные уведомления: «Claude закончил» → «что именно сделал». Подключаем
	// всегда, саму настройку движок читает на каждом поводе — чтобы включение с
	// телефона работало без перезапуска агента (см. agent_summary.go).
	s.StartAgentSummaries()

	// Лимиты снимаются с КАЖДОГО аккаунта машины, а не только с того, что лежит
	// в домашнем каталоге (см. api_accounts.go). Связь односторонняя: aiusage не
	// знает, где список хранится, а хранилище — как опрашивают вендоров.
	aiusage.SetAccountsSource(usageAccounts)

	// Расход токенов по файлам сессий Claude и Codex — тем же аккаунтам, что и
	// лимиты. Первый проход фоном через полминуты после старта (см. tokenusage).
	tokenusage.SetSources(tokenUsageSources)
	if home, err := os.UserHomeDir(); err == nil {
		tokenusage.Start(context.Background(), paths.Base(), home)
	}

	// Сторож лимитов: когда у рабочего аккаунта кончается окно, а у соседнего
	// есть запас, владелец узнаёт об этом в Telegram — а не когда агент встал
	// посреди работы (см. account_alerts.go).
	StartAccountAlerts()

	// Fetch the H.264 codec plugin in the background so the FIRST remote-desktop
	// session already negotiates the video track instead of the JPEG fallback.
	ensureH264DLL()

	// События жизненного цикла виртуального браузера (предупреждение о простое,
	// автоостановка) едут на открытые экраны тем же каналом, что и события
	// страницы, — клиент уже умеет их принимать.
	vbrowser.SetEventCallback(func(kind string, fields map[string]any) {
		msg := map[string]any{"t": "browser", "kind": kind}
		for k, v := range fields {
			msg[k] = v
		}
		broadcastBrowserMsg(msg)
	})

	// Re-attach to any persistent PTY host processes that survived a previous
	// remotai run (terminals stay alive across a full restart).
	s.ptyManager.Reattach()

	// Find miniapp dist directory
	exe, _ := os.Executable()
	s.distDir = filepath.Join(filepath.Dir(exe), "miniapp", "dist")

	cfg := config.GetNoSetup()

	// Initialize auth manager (JWT + OIDC). API-token login is also a JWT
	// migration path, so standalone/central installs need it even without a bot.
	if botToken != "" || cfg.JWTSigningKey != "" || cfg.APIToken != "" {
		s.authManager = NewAuthManager(botToken)
	}

	// Initialize license manager & Stripe
	s.initLicense()

	// Initialize tunnel manager
	s.tunnelManager = tunnel.NewManager(cfg.Port())
	s.tunnelManager.SetConfig(tunnel.Config{
		Mode:            tunnel.Mode(cfg.TunnelMode),
		CloudflaredPath: cfg.CloudflaredPath,
		TunnelName:      cfg.TunnelName,
		TunnelURL:       cfg.TunnelURL,
		AutoStart:       cfg.TunnelAutoStart,
	})
	s.tunnelManager.OnURLChange(func(url string) {
		// Save URL to config (atomic, lock-guarded)
		_ = config.Update(func(cc *config.Config) { cc.TunnelURL = url })
		// Broadcast to WebSocket clients
		s.broadcastAll(map[string]any{"type": "tunnel_url", "url": url})
	})

	s.registerRoutes()
	s.resumeCloudPairing()
	return s
}

func (s *Server) registerRoutes() {
	s.registerHermesRoutes()
	s.mux.HandleFunc("GET /api/server-access", s.authWrap(s.apiServerAccess))
	// Health
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, map[string]bool{"ok": true})
	})
	// Full readiness ("готов к удалённой работе?") — no auth so dashboards can
	// poll before login. Output is safe (no secrets, only boolean flags).
	s.mux.HandleFunc("GET /api/health/full", s.apiHealthFull)

	// Setup wizard (no auth required) — must be registered before auth routes
	s.registerSetupRoutes()

	// Recipes (P3) — «починить одной кнопкой». Без auth, чтобы UI Troubleshoot
	// мог работать при battery-mode/частичной авторизации.
	s.registerRecipes()

	// Root: landing page (or setup redirect if not configured)
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		cfg := config.GetNoSetup()
		if !cfg.IsConfigured() {
			http.Redirect(w, r, "/setup", http.StatusTemporaryRedirect)
			return
		}
		s.serveLandingPage(w, r)
	})

	// Sessions API
	s.mux.HandleFunc("GET /api/sessions", s.authWrap(s.apiSessionsList))
	s.mux.HandleFunc("POST /api/sessions", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiSessionCreate)))
	s.mux.HandleFunc("GET /api/sessions/{name}", s.authWrap(s.apiSessionDetail))
	s.mux.HandleFunc("DELETE /api/sessions/{name}", s.authWrap(s.apiSessionDelete))
	s.mux.HandleFunc("POST /api/sessions/{name}/switch", s.authWrap(s.apiSessionSwitch))
	s.mux.HandleFunc("POST /api/sessions/{name}/rename", s.authWrap(s.apiSessionRename))
	s.mux.HandleFunc("PATCH /api/sessions/{name}/config", s.authWrap(s.apiSessionConfig))
	s.mux.HandleFunc("POST /api/sessions/{name}/send", s.authWrap(s.apiSessionSend))
	s.mux.HandleFunc("POST /api/sessions/{name}/stop", s.authWrap(s.apiSessionStop))

	// Agents API
	s.mux.HandleFunc("GET /api/agents", s.authWrap(s.apiAgentsList))
	s.mux.HandleFunc("POST /api/agents/rescan", s.authWrap(s.apiAgentsRescan))
	// Обновление CLI-агентов тем же менеджером (api_agent_update.go).
	s.mux.HandleFunc("GET /api/agents/updates", s.authWrap(s.apiAgentUpdates))
	s.mux.HandleFunc("GET /api/agents/{id}/versions", s.authWrap(s.apiAgentVersions))
	// Скиллы агентов (api_skills.go): список, установка из GitHub/ZIP,
	// копирование, удаление в резервную копию и возврат.
	s.mux.HandleFunc("GET /api/skills", s.authWrap(s.apiSkillsList))
	s.mux.HandleFunc("POST /api/skills/install", s.authWrap(s.apiSkillsInstall))
	s.mux.HandleFunc("POST /api/skills/copy", s.authWrap(s.apiSkillsCopy))
	s.mux.HandleFunc("DELETE /api/skills", s.authWrap(s.apiSkillsDelete))
	s.mux.HandleFunc("POST /api/skills/restore", s.authWrap(s.apiSkillsRestore))
	s.mux.HandleFunc("GET /api/skills/backups", s.authWrap(s.apiSkillsBackups))
	// MCP-серверы агентов без правки JSON (api_mcp.go, internal/mcpmgr).
	s.mux.HandleFunc("GET /api/mcp", s.authWrap(s.apiMCPList))
	s.mux.HandleFunc("POST /api/mcp", s.authWrap(s.apiMCPAdd))
	s.mux.HandleFunc("DELETE /api/mcp", s.authWrap(s.apiMCPDelete))
	s.mux.HandleFunc("POST /api/mcp/toggle", s.authWrap(s.apiMCPToggle))
	// Все беседы Claude и Codex на этом ПК (экран «Беседы», api_agent_sessions.go).
	s.mux.HandleFunc("GET /api/agent-sessions", s.authWrap(s.apiAgentSessions))
	// Честная проверка подключения агента (api_agent_check.go): что он реально
	// использует и отвечает ли модель. Ключи наружу не отдаются — только маска.
	s.mux.HandleFunc("GET /api/agents/{id}/connection", s.authWrap(s.apiAgentConnection))
	s.mux.HandleFunc("POST /api/agents/{id}/check", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiAgentCheck)))

	// Config API
	s.mux.HandleFunc("GET /api/config", s.authWrap(s.apiConfigGet))
	s.mux.HandleFunc("PATCH /api/config", s.authWrap(s.apiConfigUpdate))

	// Discover API
	s.mux.HandleFunc("GET /api/discover", s.authWrap(s.apiDiscover))
	s.mux.HandleFunc("POST /api/discover/import", s.authWrap(s.apiDiscoverImport))

	// Quick actions
	s.mux.HandleFunc("POST /api/sessions/{name}/clone", s.authWrap(s.apiSessionClone))
	s.mux.HandleFunc("POST /api/sessions/{name}/clear", s.authWrap(s.apiSessionClear))
	s.mux.HandleFunc("POST /api/sessions/{name}/agent", s.authWrap(s.apiSessionChangeAgent))

	// Pinned prompts
	s.mux.HandleFunc("GET /api/sessions/{name}/prompts", s.authWrap(s.apiPromptsGet))
	s.mux.HandleFunc("POST /api/sessions/{name}/prompts", s.authWrap(s.apiPromptsAdd))
	s.mux.HandleFunc("DELETE /api/sessions/{name}/prompts", s.authWrap(s.apiPromptsRemove))

	// Templates
	s.mux.HandleFunc("GET /api/templates", s.authWrap(s.apiTemplatesList))
	s.mux.HandleFunc("POST /api/templates", s.authWrap(s.apiTemplateCreate))
	s.mux.HandleFunc("DELETE /api/templates", s.authWrap(s.apiTemplateDelete))
	s.mux.HandleFunc("POST /api/templates/apply", s.authWrap(s.apiTemplateApply))

	// Multi-send
	s.mux.HandleFunc("POST /api/multi-send", s.authWrap(s.apiMultiSend))

	// Claude history sync
	s.mux.HandleFunc("GET /api/sessions/{name}/claude-history", s.authWrap(s.apiClaudeHistory))

	// Research (Pro+)
	s.mux.HandleFunc("GET /api/sessions/{name}/research", s.authWrap(s.requireTier(license.TierPro, s.apiResearchGet)))
	s.mux.HandleFunc("POST /api/sessions/{name}/research", s.authWrap(s.requireTier(license.TierPro, s.apiResearchStart)))
	s.mux.HandleFunc("POST /api/sessions/{name}/research/config", s.authWrap(s.requireTier(license.TierPro, s.apiResearchConfig)))
	s.mux.HandleFunc("POST /api/sessions/{name}/research/stop", s.authWrap(s.requireTier(license.TierPro, s.apiResearchStop)))

	// Orchestrator models (Pro+)
	s.mux.HandleFunc("GET /api/orchestrator/models", s.authWrap(s.requireTier(license.TierPro, s.apiOrchModels)))
	s.registerOrchRuns()

	// Files API
	s.mux.HandleFunc("GET /api/files", s.authWrap(s.apiFilesList))
	s.mux.HandleFunc("GET /api/files/quick-paths", s.authWrap(s.apiQuickPaths))
	s.mux.HandleFunc("GET /api/files/download", s.authWrap(s.apiFileDownload))
	s.mux.HandleFunc("POST /api/files/upload", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.requireFeature("file_write", s.apiFileUpload))))
	s.mux.HandleFunc("GET /api/files/upload/status", s.authWrap(s.apiFileUploadStatus))
	s.mux.HandleFunc("POST /api/files/upload/abort", s.authWrap(s.requireFeature("file_write", s.apiFileUploadAbort)))
	s.mux.HandleFunc("DELETE /api/files", s.authWrap(s.requireFeature("file_write", s.apiFileDelete)))
	s.mux.HandleFunc("DELETE /api/files/dir", s.authWrap(s.requireFeature("file_write", s.apiFileDirDelete)))
	s.mux.HandleFunc("POST /api/files/mkdir", s.authWrap(s.requireFeature("file_write", s.apiFileMkdir)))
	s.mux.HandleFunc("POST /api/files/rename", s.authWrap(s.requireFeature("file_write", s.apiFileRename)))
	s.mux.HandleFunc("POST /api/files/copy", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.requireFeature("file_write", s.apiFileCopy))))
	s.mux.HandleFunc("GET /api/files/preview", s.authWrap(s.apiFilePreview))
	s.mux.HandleFunc("GET /api/files/search", s.authWrap(s.apiFilesSearch))
	// POST-вариант нужен глубокому поиску: HttpClient намеренно ретраит GET,
	// а повтор полного обхода диска после таймаута только умножает нагрузку.
	s.mux.HandleFunc("POST /api/files/search", s.authWrap(s.apiFilesSearch))
	// dir-stat — чтение (счёт содержимого перед удалением), без гейта file_write.
	s.mux.HandleFunc("GET /api/files/dir-stat", s.authWrap(s.apiFileDirStat))
	s.mux.HandleFunc("GET /api/files/disk-info", s.authWrap(s.apiDiskInfo))
	s.mux.HandleFunc("POST /api/files/send-to-telegram", s.authWrap(s.apiFileSendToTelegram))

	// Presets / quick-launch tiles
	s.mux.HandleFunc("GET /api/presets", s.authWrap(s.apiPresetsList))
	s.mux.HandleFunc("POST /api/presets", s.authWrap(s.apiPresetsUpsert))
	s.mux.HandleFunc("DELETE /api/presets", s.authWrap(s.apiPresetsDelete))
	s.mux.HandleFunc("POST /api/presets/launch", s.authWrap(s.apiPresetLaunch))
	// Свои команды — ОДИН список на компьютер (см. api_commands.go): раньше их
	// было два и оба в localStorage пульта, поэтому команда с телефона в окне
	// exe отсутствовала.
	s.mux.HandleFunc("GET /api/commands", s.authWrap(s.apiCommandsList))
	s.mux.HandleFunc("POST /api/commands", s.authWrap(s.apiCommandsUpsert))
	s.mux.HandleFunc("DELETE /api/commands", s.authWrap(s.apiCommandsDelete))
	s.mux.HandleFunc("POST /api/commands/import", s.authWrap(s.apiCommandsImport))

	// Аккаунты нейросетей: у человека бывает несколько подписок на одного
	// провайдера, и аккаунт у CLI-агента — это каталог (см. api_accounts.go).
	s.mux.HandleFunc("GET /api/accounts", s.authWrap(s.apiAccountsList))
	s.mux.HandleFunc("POST /api/accounts", s.authWrap(s.apiAccountsUpsert))
	s.mux.HandleFunc("POST /api/accounts/activate", s.authWrap(s.apiAccountsActivate))
	// Настройки для AI-агента: он должен ПОНИМАТЬ, что можно настроить, а не
	// угадывать (см. api_settings_catalog.go). Опасное меняет только владелец.
	s.mux.HandleFunc("GET /api/settings/catalog", s.authWrap(s.apiSettingsCatalog))
	s.mux.HandleFunc("POST /api/settings/apply", s.authWrap(s.apiSettingsApply))
	s.mux.HandleFunc("POST /api/settings/request", s.authWrap(s.apiSettingsRequest))
	s.mux.HandleFunc("GET /api/settings/requests", s.authWrap(s.apiSettingsRequests))
	s.mux.HandleFunc("POST /api/settings/requests/answer", s.authWrap(s.apiSettingsAnswer))
	// Закрепление аккаунта за папкой: рабочий код открывают рабочей подпиской.
	s.mux.HandleFunc("POST /api/accounts/pin", s.authWrap(s.apiAccountsPin))
	// Каким аккаунтом запущен агент КОНКРЕТНОГО терминала (у соседнего может
	// быть другой — окружение выдаётся процессу при запуске).
	s.mux.HandleFunc("POST /api/pty/{id}/account", s.authWrap(s.apiPtySetAccount))
	s.mux.HandleFunc("DELETE /api/accounts", s.authWrap(s.apiAccountsDelete))

	// OpenRouter: один ключ вместо подписки на каждого вендора. Ключ живёт на
	// ПК (см. api_openrouter.go) и наружу не отдаётся — только расход и метка.
	s.mux.HandleFunc("GET /api/openrouter", s.authWrap(s.apiOpenRouterStatus))
	s.mux.HandleFunc("POST /api/openrouter/key", s.authWrap(s.apiOpenRouterSetKey))
	s.mux.HandleFunc("DELETE /api/openrouter/key", s.authWrap(s.apiOpenRouterForgetKey))
	s.mux.HandleFunc("GET /api/openrouter/models", s.authWrap(s.apiOpenRouterModels))
	s.mux.HandleFunc("POST /api/openrouter/model", s.authWrap(s.apiOpenRouterSetModel))

	// Bookmarks API
	s.mux.HandleFunc("GET /api/bookmarks", s.authWrap(s.apiBookmarksList))
	s.mux.HandleFunc("POST /api/bookmarks", s.authWrap(s.apiBookmarkAdd))
	s.mux.HandleFunc("DELETE /api/bookmarks", s.authWrap(s.apiBookmarkRemove))

	// Recent folders API
	s.mux.HandleFunc("GET /api/recent-folders", s.authWrap(s.apiRecentFolders))

	// Stats API
	s.mux.HandleFunc("GET /api/stats", s.authWrap(s.apiCostStats))
	s.mux.HandleFunc("GET /api/ai-usage", s.authWrap(s.apiAIUsage))
	s.mux.HandleFunc("GET /api/token-usage", s.authWrap(s.apiTokenUsage))

	// Version & Update API
	s.mux.HandleFunc("GET /api/system/version", s.authWrap(s.apiVersion))
	s.mux.HandleFunc("POST /api/system/update", s.authWrap(s.apiUpdate))
	s.mux.HandleFunc("GET /api/system/service", s.authWrap(s.apiServiceStatus))

	// System API
	s.mux.HandleFunc("GET /api/system/stats", s.authWrap(s.apiSystemStats))
	s.mux.HandleFunc("GET /api/system/processes", s.authWrap(s.apiProcessesList))
	s.mux.HandleFunc("POST /api/system/kill", s.authWrap(s.apiProcessKill))
	s.mux.HandleFunc("POST /api/system/power", s.authWrap(s.apiPower))
	s.mux.HandleFunc("GET /api/system/screenshot", s.authWrap(s.apiScreenshot))
	// Снимок ФАЙЛОМ на сам компьютер — для агента, который читает только свою
	// файловую систему (см. apiScreenshotSave). POST: он создаёт файл.
	s.mux.HandleFunc("POST /api/system/screenshot/save", s.authWrap(s.apiScreenshotSave))
	s.mux.HandleFunc("GET /api/system/autostart", s.authWrap(s.apiAutostartStatus))
	s.mux.HandleFunc("POST /api/system/autostart", s.authWrap(s.apiAutostartToggle))

	// Здоровье компьютера: почему пропадал, что со связью, хвост своего лога.
	// Эти же три ручки видит соседнее устройство аккаунта в режиме диагностики
	// (см. internal/relay/peer_access.go).
	s.mux.HandleFunc("GET /api/system/boot-report", s.authWrap(s.apiBootReport))
	s.mux.HandleFunc("GET /api/system/network", s.authWrap(s.apiNetworkStatus))
	s.mux.HandleFunc("POST /api/system/vpn", s.authWrap(s.apiVPNControl))
	s.mux.HandleFunc("GET /api/system/logs", s.authWrap(s.apiAgentLogs))

	// Terminal API
	s.mux.HandleFunc("POST /api/terminal", s.authWrap(s.apiTerminalExec))

	// PTY (interactive terminals)
	s.mux.HandleFunc("GET /api/pty", s.authWrap(s.apiPtyList))
	s.mux.HandleFunc("POST /api/pty", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiPtyCreate)))
	s.mux.HandleFunc("POST /api/pty/upload", s.authWrap(s.apiPtyUpload))
	s.mux.HandleFunc("DELETE /api/pty/dead", s.authWrap(s.apiPtyCloseDead))
	// Продолжить терминал, не переживший перезагрузку компьютера (тот же id).
	s.mux.HandleFunc("POST /api/pty/{id}/restore", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiPtyRestore)))
	// Вернуть связь с живым процессом терминала (оборвался канал, а работа идёт).
	s.mux.HandleFunc("POST /api/pty/{id}/reattach", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiPtyReattach)))
	s.mux.HandleFunc("DELETE /api/pty/{id}", s.authWrap(s.apiPtyClose))
	s.mux.HandleFunc("GET /api/pty/{id}/cwd", s.authWrap(s.apiPtyCWD))
	s.mux.HandleFunc("GET /api/pty/{id}/state", s.authWrap(s.apiPtyState))
	// Усыпить агента и разбудить его ту же беседу (internal/pty/agent_sleep.go).
	s.mux.HandleFunc("POST /api/pty/{id}/sleep", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiPtySleep)))
	s.mux.HandleFunc("POST /api/pty/{id}/wake", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiPtyWake)))
	s.mux.HandleFunc("PATCH /api/pty/meta", s.authWrap(s.apiPtySetMeta))
	s.mux.HandleFunc("PATCH /api/pty/{id}", s.authWrap(s.apiPtyRename))
	s.mux.HandleFunc("POST /api/pty/{id}/handoff", s.authWrap(s.apiPtyHandoff))
	// Ответить агенту, не открывая экран терминала: кнопки на карточке «Требует
	// внимания», действия уведомления, inline-кнопки Telegram-бота (релей шлёт
	// это обычным client-request), «Остановить» (Ctrl+C).
	s.mux.HandleFunc("POST /api/pty/{id}/input", s.authWrap(s.rateLimitAuth(s.ptyInputLimiter, s.apiPtyInput)))
	s.mux.HandleFunc("POST /api/pty/{id}/resize", s.authWrap(s.rateLimitAuth(s.ptyInputLimiter, s.apiPtyResize)))
	s.mux.HandleFunc("GET /api/pty/{id}/export", s.authWrap(s.apiPtyExport))

	// SSH over agent (бастион): подключение к внешнему серверу как PTY-терминал
	s.mux.HandleFunc("POST /api/ssh/connect", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiSSHConnect)))
	// Сохранённые хосты (config + saved) и история подключений
	s.mux.HandleFunc("GET /api/ssh/hosts", s.authWrap(s.apiSSHHostsList))
	s.mux.HandleFunc("POST /api/ssh/hosts", s.authWrap(s.apiSSHHostCreate))
	s.mux.HandleFunc("PATCH /api/ssh/hosts/{id}", s.authWrap(s.apiSSHHostUpdate))
	s.mux.HandleFunc("DELETE /api/ssh/hosts/{id}", s.authWrap(s.apiSSHHostDelete))
	s.mux.HandleFunc("POST /api/ssh/hosts/{id}/unlock", s.authWrap(s.apiSSHHostUnlock))
	s.mux.HandleFunc("DELETE /api/ssh/hosts/{id}/unlock", s.authWrap(s.apiSSHHostForgetSecret))
	s.mux.HandleFunc("DELETE /api/ssh/known-hosts", s.authWrap(s.apiSSHKnownHostDelete))
	// SSH-ключи внутри приложения: принести, сгенерировать, поставить на сервер.
	s.mux.HandleFunc("GET /api/ssh/keys", s.authWrap(s.apiSSHKeysList))
	s.mux.HandleFunc("POST /api/ssh/keys", s.authWrap(s.apiSSHKeyImport))
	s.mux.HandleFunc("POST /api/ssh/keys/generate", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiSSHKeyGenerate)))
	s.mux.HandleFunc("PATCH /api/ssh/keys/{id}", s.authWrap(s.apiSSHKeyRename))
	s.mux.HandleFunc("DELETE /api/ssh/keys/{id}", s.authWrap(s.apiSSHKeyDelete))
	s.mux.HandleFunc("POST /api/ssh/keys/{id}/install", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiSSHKeyInstall)))
	s.mux.HandleFunc("GET /api/ssh/history", s.authWrap(s.apiSSHHistory))
	// Порт-форвардинг (-L/-R/-D); живёт только в памяти агента
	s.mux.HandleFunc("GET /api/ssh/forwards", s.authWrap(s.apiSSHForwardsList))
	s.mux.HandleFunc("POST /api/ssh/forwards", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiSSHForwardCreate)))
	s.mux.HandleFunc("DELETE /api/ssh/forwards/{id}", s.authWrap(s.apiSSHForwardDelete))
	s.mux.HandleFunc("DELETE /api/ssh/forward-specs/{id}", s.authWrap(s.apiSSHForwardSpecDelete))
	// SFTP к SSH-серверам (пул соединений в памяти агента)
	s.mux.HandleFunc("GET /api/ssh/sftp/list", s.authWrap(s.apiSFTPList))
	s.mux.HandleFunc("GET /api/ssh/sftp/preview", s.authWrap(s.apiSFTPPreview))
	s.mux.HandleFunc("GET /api/ssh/sftp/download", s.authWrap(s.apiSFTPDownload))
	// POST-вариант скачивания — предпочтительный: пароль уезжает в теле, а не в
	// query, где при чанковании он попал бы в access-логи на КАЖДЫЙ кусок.
	s.mux.HandleFunc("POST /api/ssh/sftp/download", s.authWrap(s.apiSFTPDownloadPost))
	s.mux.HandleFunc("POST /api/ssh/sftp/upload", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiSFTPUpload)))
	s.mux.HandleFunc("POST /api/ssh/sftp/mkdir", s.authWrap(s.apiSFTPMkdir))
	s.mux.HandleFunc("POST /api/ssh/sftp/delete", s.authWrap(s.apiSFTPDelete))
	s.mux.HandleFunc("POST /api/ssh/sftp/rename", s.authWrap(s.apiSFTPRename))
	// Переносы ПК ↔ сервер: байты не гуляют через телефон (#9). Отвечают 202 +
	// id, копирование идёт горутиной на ПК и переживает закрытие приложения.
	s.mux.HandleFunc("POST /api/ssh/sftp/push", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.requireFeature("file_write", s.apiSFTPPush))))
	s.mux.HandleFunc("POST /api/ssh/sftp/pull", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.requireFeature("file_write", s.apiSFTPPull))))
	s.mux.HandleFunc("GET /api/ssh/sftp/transfers", s.authWrap(s.apiSFTPTransfers))
	s.mux.HandleFunc("POST /api/ssh/sftp/transfers/cancel", s.authWrap(s.apiSFTPTransferCancel))

	// Virtual browser (Xvfb + браузер на headless Linux-агенте → RD-браузинг)
	s.mux.HandleFunc("GET /api/vbrowser/status", s.authWrap(s.apiVBrowserStatus))
	s.mux.HandleFunc("POST /api/vbrowser/start", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiVBrowserStart)))
	s.mux.HandleFunc("POST /api/vbrowser/stop", s.authWrap(s.apiVBrowserStop))
	s.mux.HandleFunc("POST /api/vbrowser/install-input", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiVBrowserInstallInput)))
	s.mux.HandleFunc("POST /api/vbrowser/navigate", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiVBrowserNavigate)))
	s.mux.HandleFunc("POST /api/vbrowser/keepalive", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiVBrowserKeepalive)))
	s.mux.HandleFunc("GET /api/vbrowser/downloads", s.authWrap(s.apiVBrowserDownloads))
	// Оболочка браузера: адрес, вкладки, профиль устройства, масштаб.
	s.mux.HandleFunc("GET /api/vbrowser/page", s.authWrap(s.apiBrowserPage))
	s.mux.HandleFunc("GET /api/vbrowser/tabs", s.authWrap(s.apiBrowserTabs))
	s.mux.HandleFunc("POST /api/vbrowser/tabs", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiBrowserTabNew)))
	s.mux.HandleFunc("POST /api/vbrowser/tabs/{id}/activate", s.authWrap(s.apiBrowserTabActivate))
	s.mux.HandleFunc("DELETE /api/vbrowser/tabs/{id}", s.authWrap(s.apiBrowserTabClose))
	s.mux.HandleFunc("POST /api/vbrowser/emulate", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiBrowserEmulate)))
	s.mux.HandleFunc("POST /api/vbrowser/scale", s.authWrap(s.apiBrowserScale))
	s.mux.HandleFunc("GET /api/vbrowser/devices", s.authWrap(s.apiBrowserDevices))
	s.mux.HandleFunc("GET /api/vbrowser/places", s.authWrap(s.apiBrowserPlaces))
	s.mux.HandleFunc("POST /api/vbrowser/places", s.authWrap(s.apiBrowserPlaceAdd))
	s.mux.HandleFunc("DELETE /api/vbrowser/places", s.authWrap(s.apiBrowserPlaceDelete))
	s.mux.HandleFunc("GET /api/vbrowser/history", s.authWrap(s.apiBrowserHistory))
	s.mux.HandleFunc("DELETE /api/vbrowser/history", s.authWrap(s.apiBrowserHistoryDelete))
	s.mux.HandleFunc("GET /api/vbrowser/logins", s.authWrap(s.apiBrowserLogins))
	s.mux.HandleFunc("POST /api/vbrowser/logins", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiBrowserLoginSave)))
	s.mux.HandleFunc("DELETE /api/vbrowser/logins/{id}", s.authWrap(s.apiBrowserLoginDelete))
	s.mux.HandleFunc("PUT /api/vbrowser/profile", s.authWrap(s.apiBrowserProfileSave))
	s.mux.HandleFunc("GET /api/vbrowser/form", s.authWrap(s.apiBrowserForm))
	s.mux.HandleFunc("POST /api/vbrowser/fill", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiBrowserFill)))
	s.mux.HandleFunc("POST /api/vbrowser/hit", s.authWrap(s.apiBrowserHit))
	s.mux.HandleFunc("GET /api/vbrowser/selection", s.authWrap(s.apiBrowserSelection))
	s.mux.HandleFunc("POST /api/vbrowser/find", s.authWrap(s.apiBrowserFind))
	s.mux.HandleFunc("POST /api/vbrowser/viewport", s.authWrap(s.apiBrowserViewport))
	s.mux.HandleFunc("POST /api/vbrowser/file-chooser", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiBrowserFileChooser)))
	s.mux.HandleFunc("POST /api/vbrowser/dialog", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiBrowserDialog)))

	s.mux.HandleFunc("GET /api/projects", s.authWrap(s.apiProjects))

	// Auth (APK token & pairing — legacy)
	s.mux.HandleFunc("GET /api/auth/token", s.authWrap(s.apiAuthToken))
	s.mux.HandleFunc("GET /api/auth/pair", s.authWrap(s.apiAuthPair))

	// JWT Auth endpoints
	s.registerAuthRoutes()

	// License & billing
	s.registerLicenseRoutes()

	// Tunnel management
	s.registerTunnelRoutes()

	// WebSocket
	s.mux.HandleFunc("GET /ws", s.wsHandler)
	s.mux.HandleFunc("GET /ws/screen", s.wsScreenHandler)
	s.mux.HandleFunc("GET /ws/pty/{id}", s.wsPtyHandler)

	// WebRTC Remote Desktop signaling (non-trickle offer/answer). Works over both
	// the LAN path and the relay request-tunnel — no relay changes needed.
	s.mux.HandleFunc("GET /api/remote/preview", s.authWrap(s.apiRemotePreview))
	s.mux.HandleFunc("POST /api/webrtc/offer", s.authWrap(s.apiWebRTCOffer))

	// Static SPA — prefer on-disk files, fallback to embedded
	cacheWrap := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Set("CDN-Cache-Control", "max-age=31536000")
			h.ServeHTTP(w, r)
		})
	}
	if info, err := os.Stat(s.distDir); err == nil && info.IsDir() {
		// On-disk miniapp
		assetsFS := http.FileServer(http.Dir(filepath.Join(s.distDir, "assets")))
		s.mux.Handle("GET /miniapp/assets/", http.StripPrefix("/miniapp/assets/", cacheWrap(assetsFS)))
		s.mux.Handle("GET /assets/", http.StripPrefix("/assets/", cacheWrap(assetsFS)))
		log.Println("Serving miniapp from disk:", s.distDir)
	} else {
		// Embedded miniapp
		sub, err := fs.Sub(embeddedDist, "miniapp_dist/assets")
		if err == nil {
			assetsFS := http.FileServer(http.FS(sub))
			s.mux.Handle("GET /miniapp/assets/", http.StripPrefix("/miniapp/assets/", cacheWrap(assetsFS)))
			s.mux.Handle("GET /assets/", http.StripPrefix("/assets/", cacheWrap(assetsFS)))
			log.Println("Serving miniapp from embedded binary")
		}
	}
	s.mux.HandleFunc("GET /miniapp/favicon.svg", s.serveMiniAppFile("favicon.svg", "image/svg+xml"))
	s.mux.HandleFunc("GET /miniapp/manifest.webmanifest", s.serveMiniAppFile("manifest.webmanifest", "application/manifest+json"))
	s.mux.HandleFunc("GET /miniapp", s.serveIndex)
	s.mux.HandleFunc("GET /miniapp/", s.serveIndex)

	// Privacy policy (required for App Store / Google Play)
	s.mux.HandleFunc("GET /privacy", s.servePrivacyPolicy)
	s.mux.HandleFunc("GET /privacy/", s.servePrivacyPolicy)
}

// StartLocal starts the HTTP server listening only on localhost (for setup wizard).
func (s *Server) StartLocal(ctx context.Context, port int) error {
	return s.startOnAddr(ctx, fmt.Sprintf("127.0.0.1:%d", port))
}

// Start starts the HTTP server on all interfaces.
func (s *Server) Start(ctx context.Context, port int) error {
	agents.DetectOnce()
	return s.startOnAddr(ctx, fmt.Sprintf("0.0.0.0:%d", port))
}

// Shutdown гасит HTTP-сервер независимо от ctx, с которым он был запущен.
// Shutdown сначала закрывает listener — порт освобождается сразу, — и лишь
// потом ждёт активные соединения (до истечения переданного ctx). Используется
// мастером первого запуска: setup-listener должен умереть ДО того, как основной
// сервер забиндит тот же порт, иначе на Windows оба сокета сосуществуют и
// localhost-трафик окна уходит в «мёртвый» setup-сервер (nil store/relayStatus).
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownHermes(ctx)
	s.httpSrvMu.Lock()
	srv := s.httpSrv
	s.httpSrvMu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (s *Server) startOnAddr(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: observability.HTTPMiddleware(s.corsMiddleware(s.mux)),
	}
	s.httpSrvMu.Lock()
	s.httpSrv = srv
	s.listenAddr = addr
	s.httpSrvMu.Unlock()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.shutdownHermes(shutdownCtx)
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("Mini App server started on http://%s", addr)
	return srv.ListenAndServe()
}

// SetOpenWindow wires POST /api/setup/open-window to the native window owner.
func (s *Server) SetOpenWindow(fn func()) { s.openWindow = fn }

// SetStartupBindError publishes the startup port recovery in the local panel.
func (s *Server) SetStartupBindError(info *StartupBindError) { s.startupBindError = info }

// SetRelayDetail registers a richer relay status callback for the setup panel.
func (s *Server) SetRelayDetail(fn func() RelayConnectionDetail) { s.relayDetail = fn }

// SetFileSender wires the web API to the Telegram bot for file delivery.
func (s *Server) SetFileSender(sender func(context.Context, int64, string) error) {
	s.sendFileToTelegram = sender
}

// PtyManager exposes the PTY manager so other packages (e.g. bot) can
// list/select/write to terminals.
func (s *Server) PtyManager() *pty.Manager {
	return s.ptyManager
}

// ServeHTTP makes *Server itself an http.Handler — convenient for relay
// dispatch and for tests using httptest.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// ── Middleware ────────────────────────────────────────────────────────

// allowedOrigins returns origins that are permitted for CORS & WebSocket.
func allowedOrigins() []string {
	return []string{
		"http://localhost",
		"https://localhost",
		"http://127.0.0.1",
		"https://127.0.0.1",
		"https://web.telegram.org",
		"https://webk.telegram.org",
		"https://webz.telegram.org",
		"https://weba.telegram.org",
	}
}

// isAllowedOrigin checks if the origin is in the allowed list or is same-origin
// (e.g. requests from the Mini App served through a tunnel like Cloudflare).
func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return true // same-origin requests have no Origin header
	}
	lower := strings.ToLower(origin)
	for _, allowed := range allowedOrigins() {
		if strings.HasPrefix(lower, allowed) {
			return true
		}
	}
	return false
}

// isAllowedOriginForReq additionally checks if the origin matches the request's
// Host header (same-origin via reverse proxy / tunnel).
func isAllowedOriginForReq(origin string, r *http.Request) bool {
	if isAllowedOrigin(origin) {
		return true
	}
	// Allow same-origin: origin host matches the Host header (tunnel/proxy)
	if origin != "" && r != nil {
		host := r.Host // e.g. "bot.123mysite.xyz"
		// Extract host from origin URL: "https://bot.123mysite.xyz" -> "bot.123mysite.xyz"
		trimmed := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
		trimmed = strings.Split(trimmed, "/")[0] // remove path if any
		if strings.EqualFold(trimmed, host) {
			return true
		}
	}
	return false
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip noisy WebSocket upgrade & static asset requests.
		p := r.URL.Path
		if p != "/ws" && p != "/ws/screen" && !strings.HasPrefix(p, "/ws/pty/") &&
			!strings.HasPrefix(p, "/assets/") && p != "/miniapp" && p != "/miniapp/" {
			log.Printf("[HTTP] %s %s", r.Method, p)
		}
		origin := r.Header.Get("Origin")
		if isAllowedOriginForReq(origin, r) && origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Telegram-Init-Data, X-API-Token")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		// Без Expose-Headers кросс-origin клиент (APK на https://localhost →
		// http://192.168.x.x:8080) не прочитает признаки файла, по которым
		// сверяется целостность чанкованного скачивания. Хендлер download
		// выставляет тот же заголовок сам — для облака, где запрос идёт мимо
		// этого middleware (relay/client.go бьёт прямо в mux).
		w.Header().Set("Access-Control-Expose-Headers", fileMetaExposeHeaders)
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Rate Limiter ─────────────────────────────────────────────────────

type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitorEntry
	limit    int           // max requests per window
	window   time.Duration // time window
}

type visitorEntry struct {
	count   int
	resetAt time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		visitors: make(map[string]*visitorEntry),
		limit:    limit,
		window:   window,
	}
	// Cleanup stale entries every 5 minutes
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			rl.mu.Lock()
			now := time.Now()
			for k, v := range rl.visitors {
				if now.After(v.resetAt) {
					delete(rl.visitors, k)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

// allow — один бакет на ключ. Ключ задаёт вызывающий: до авторизации это IP
// (rateLimit), после — пользователь (rateLimitAuth).
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	v, ok := rl.visitors[key]
	if !ok || now.After(v.resetAt) {
		rl.visitors[key] = &visitorEntry{count: 1, resetAt: now.Add(rl.window)}
		return true
	}
	v.count++
	return v.count <= rl.limit
}

// rateLimit wraps a handler with IP-based rate limiting.
func (s *Server) rateLimit(rl *rateLimiter, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip == "" {
			ip = r.RemoteAddr
		}
		if !rl.allow(ip) {
			jsonError(w, "Too many requests", 429)
			return
		}
		handler(w, r)
	}
}

// rateLimitAuth wraps an auth handler with rate limiting per (пользователь, IP).
//
// Ключ был только IP, а у запроса, проксированного релеем, RemoteAddr пуст
// (internal/relay/client.go: handleCmd прогоняет Cmd через httptest-recorder) —
// поэтому ВСЕ облачные вызовы этого ПК делили один бакет с ключом "". Теперь в
// ключ входит uid; IP из ключа не убран намеренно, иначе LAN-устройства,
// ходящие под общим api_token ПК, наоборот СЛИЛИСЬ бы в один бакет.
//
// Честная граница: развести облачных вызывающих между собой агент не может в
// принципе — релей подставляет api_token ЭТОГО ПК (ValidateAPIToken отдаёт один
// uid на всех) и личности вызывающего в запрос не кладёт. Лимит «на аккаунт»
// для облака — задача релея; здесь гарантируется лишь то, что ответы агенту
// (ptyInputLimiter) не делят бакет с загрузками файлов и /api/ssh/connect.
func (s *Server) rateLimitAuth(rl *rateLimiter, handler func(w http.ResponseWriter, r *http.Request, uid int64)) func(w http.ResponseWriter, r *http.Request, uid int64) {
	return func(w http.ResponseWriter, r *http.Request, uid int64) {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip == "" {
			ip = r.RemoteAddr
		}
		if !rl.allow("uid:" + strconv.FormatInt(uid, 10) + "|ip:" + ip) {
			// С машинным кодом: без него бот показывал бы «Не получилось
			// отправить ответ» вместо «слишком часто» (ветвление по code, а не
			// по тексту — общее правило, см. jsonErrorCode).
			jsonErrorCode(w, 429, "rate_limited", "Too many requests", nil)
			return
		}
		handler(w, r, uid)
	}
}

type contextKey string

const uidKey contextKey = "uid"

func (s *Server) authWrap(handler func(w http.ResponseWriter, r *http.Request, uid int64)) http.HandlerFunc {
	handler = s.serverAccessWrap(handler)
	return func(w http.ResponseWriter, r *http.Request) {
		// Priority 1: Authorization: Bearer <jwt>
		if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if s.authManager != nil {
				uid, err := s.authManager.ValidateAccessToken(token)
				if err == nil && uid > 0 {
					if len(s.allowed) > 0 && !s.allowed[uid] {
						jsonError(w, "Forbidden", 403)
						return
					}
					handler(w, r, uid)
					return
				}
			}
			jsonError(w, "Invalid token", 401)
			return
		}

		// Priority 2: X-API-Token (legacy, for standalone APK)
		if apiToken := r.Header.Get("X-API-Token"); apiToken != "" {
			uid := ValidateAPIToken(apiToken)
			if uid == 0 {
				log.Printf("[AUTH] %s %s → 401 (invalid API token)", r.Method, r.URL.Path)
				jsonError(w, "Invalid API token", 401)
				return
			}
			if len(s.allowed) > 0 && !s.allowed[uid] {
				log.Printf("[AUTH] %s %s → 403 (uid=%d not allowed)", r.Method, r.URL.Path, uid)
				jsonError(w, "Forbidden", 403)
				return
			}
			handler(w, r, uid)
			return
		}

		// Priority 3: X-Telegram-Init-Data or ?initData= (Telegram Mini App)
		initData := r.Header.Get("X-Telegram-Init-Data")
		if initData == "" {
			initData = r.URL.Query().Get("initData")
		}
		if initData == "" {
			jsonError(w, "Unauthorized", 401)
			return
		}
		// Standalone APK auth: ?initData=token:<API_TOKEN> — same convention
		// the WebSocket layer accepts. Useful for plain GET links that can't
		// set headers (e.g. <a download> opened in an external browser).
		if strings.HasPrefix(initData, "token:") {
			uid := ValidateAPIToken(strings.TrimPrefix(initData, "token:"))
			if uid == 0 {
				log.Printf("[AUTH] %s %s → 401 (invalid token: in initData)", r.Method, r.URL.Path)
				jsonError(w, "Invalid API token", 401)
				return
			}
			if len(s.allowed) > 0 && !s.allowed[uid] {
				jsonError(w, "Forbidden", 403)
				return
			}
			handler(w, r, uid)
			return
		}
		data := ValidateInitData(initData, s.botToken, initDataMaxAge)
		if data == nil {
			log.Printf("[AUTH] %s %s → 401 (invalid initData)", r.Method, r.URL.Path)
			jsonError(w, "Unauthorized", 401)
			return
		}
		uid := ExtractUID(data)
		if uid == 0 {
			jsonError(w, "Unauthorized", 401)
			return
		}
		if len(s.allowed) > 0 && !s.allowed[uid] {
			jsonError(w, "Forbidden", 403)
			return
		}
		handler(w, r, uid)
	}
}

// ── WebSocket ────────────────────────────────────────────────────────

// authenticateWS validates initData query param for WebSocket connections.
// Supports: Telegram initData, "relay:TOKEN", "token:API_TOKEN".
// Returns uid (>0 for users, -1 for relay) or 0 if invalid.
func (s *Server) authenticateWS(initData string) int64 {
	// Relay token
	if strings.HasPrefix(initData, "relay:") {
		token := strings.TrimPrefix(initData, "relay:")
		cfg := config.GetNoSetup()
		if cfg.ConnectionToken == "" || token != cfg.ConnectionToken {
			return 0
		}
		return -1
	}
	// Legacy APK token
	if strings.HasPrefix(initData, "token:") {
		token := strings.TrimPrefix(initData, "token:")
		return ValidateAPIToken(token)
	}
	// JWT Bearer token
	if strings.HasPrefix(initData, "Bearer ") {
		token := strings.TrimPrefix(initData, "Bearer ")
		if s.authManager != nil {
			uid, err := s.authManager.ValidateAccessToken(token)
			if err == nil && uid > 0 {
				return uid
			}
		}
		return 0
	}
	// Telegram initData
	data := ValidateInitData(initData, s.botToken, initDataMaxAge)
	if data == nil {
		return 0
	}
	return ExtractUID(data)
}

func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	initData := r.URL.Query().Get("initData")
	uid := s.authenticateWS(initData)
	if uid == 0 {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if len(s.allowed) > 0 && uid != -1 && !s.allowed[uid] {
		http.Error(w, "Forbidden", 403)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	client := &wsClient{conn: conn}

	// Replay missed events if client reconnects with ?since=<lastEventId>.
	// Done before adding to wsConns; client.send applies a write deadline so a
	// stalled peer can't hang the connect path.
	if since, ok := parseUint64(r.URL.Query().Get("since")); ok && since > 0 && uid > 0 {
		missed := s.eventBuf.since(uid, since)
		for _, data := range missed {
			if err := client.send(data); err != nil {
				return
			}
		}
	}

	s.wsMu.Lock()
	if s.wsConns[uid] == nil {
		s.wsConns[uid] = make(map[*wsClient]bool)
	}
	s.wsConns[uid][client] = true
	s.wsMu.Unlock()

	defer func() {
		s.wsMu.Lock()
		delete(s.wsConns[uid], client)
		s.wsMu.Unlock()
	}()

	// Keepalive: server pings + read deadline so dead peers are evicted promptly
	// instead of lingering as half-open sockets the broadcaster keeps writing to.
	ka := wsutil.Start(conn)
	defer ka.Stop()

	// Read loop — drains pongs/client frames and keeps the deadline fresh.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
		ka.Touch()
	}
}

func parseUint64(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, true
}

// SetRelayEventSink registers a sink that receives every broadcast event so the
// relay client can forward it to remote cloud clients. Pass nil to disable.
func (s *Server) SetRelayEventSink(fn func(data []byte)) { s.relayEventSink = fn }

// SetRelayKick registers a callback that forces the relay client to reconnect
// with freshly-loaded credentials. Called by the pairing flow right after a
// new device JWT is saved, so cloud access works without an app restart.
func (s *Server) SetRelayKick(fn func()) {
	s.relayKickMu.Lock()
	s.relayKick = fn
	s.relayKickMu.Unlock()
}

// SetRelayStatus registers a callback reporting the relay link state
// (configured, connected) for the health dashboard.
func (s *Server) SetRelayStatus(fn func() (configured, connected bool)) { s.relayStatus = fn }

// Broadcast sends an event to all WebSocket connections for a user.
// Each event is assigned a unique monotonic id and stored in a per-user ring
// buffer so reconnecting clients can request missed events via ?since=<id>.
func (s *Server) Broadcast(uid int64, event any) {
	id, data, err := s.eventBuf.addIDToEvent(event)
	if err != nil {
		return
	}
	if uid > 0 {
		s.eventBuf.store(uid, id, data)
	}
	if s.relayEventSink != nil {
		s.relayEventSink(data)
	}

	// Snapshot the client set under the lock, then write OUTSIDE the lock so one
	// slow/dead socket can never block other broadcasts or WS connect/disconnect.
	s.wsMu.RLock()
	clients := make([]*wsClient, 0, len(s.wsConns[uid]))
	for c := range s.wsConns[uid] {
		clients = append(clients, c)
	}
	s.wsMu.RUnlock()

	s.writeToClients(uid, clients, data)
}

// writeToClients writes data to each client (each with its own write deadline +
// write mutex) without holding wsMu, then evicts any that errored.
func (s *Server) writeToClients(uid int64, clients []*wsClient, data []byte) {
	var dead []*wsClient
	for _, c := range clients {
		if err := c.send(data); err != nil {
			dead = append(dead, c)
		}
	}
	if len(dead) == 0 {
		return
	}
	s.wsMu.Lock()
	for _, c := range dead {
		if s.wsConns[uid] != nil {
			delete(s.wsConns[uid], c)
		}
		c.conn.Close()
	}
	s.wsMu.Unlock()
}

// StartTunnel starts the tunnel if autostart is configured.
func (s *Server) StartTunnel(ctx context.Context) {
	if s.tunnelManager == nil {
		return
	}
	cfg := config.GetNoSetup()
	if !cfg.TunnelAutoStart || cfg.TunnelMode == "" || cfg.TunnelMode == "disabled" {
		return
	}
	log.Printf("[TUNNEL] Auto-starting tunnel (mode=%s, supervised)", cfg.TunnelMode)
	// Supervise blocks for the life of ctx, restarting cloudflared if it dies so
	// the Mini App doesn't go permanently unreachable. Caller runs this in a
	// goroutine.
	s.tunnelManager.Supervise(ctx)
}

// StopTunnel stops the tunnel on shutdown.
func (s *Server) StopTunnel() {
	if s.tunnelManager != nil {
		s.tunnelManager.Stop()
	}
}

// broadcastAll sends an event to all connected WebSocket clients.
// Events are id-stamped and stored per-uid for reconnect replay.
func (s *Server) broadcastAll(event any) {
	id, data, err := s.eventBuf.addIDToEvent(event)
	if err != nil {
		return
	}
	if s.relayEventSink != nil {
		s.relayEventSink(data)
	}

	// Snapshot (uid, clients) under the lock, then write outside it.
	type bucket struct {
		uid     int64
		clients []*wsClient
	}
	s.wsMu.RLock()
	buckets := make([]bucket, 0, len(s.wsConns))
	for uid, conns := range s.wsConns {
		cs := make([]*wsClient, 0, len(conns))
		for c := range conns {
			cs = append(cs, c)
		}
		buckets = append(buckets, bucket{uid: uid, clients: cs})
	}
	s.wsMu.RUnlock()

	for _, b := range buckets {
		if b.uid > 0 {
			s.eventBuf.store(b.uid, id, data)
		}
		s.writeToClients(b.uid, b.clients, data)
	}
}

// SetBotLiveness registers a callback the health report uses to tell whether the
// Telegram bot has recently reached Telegram. Set by main.go from the watchdog.
func (s *Server) SetBotLiveness(fn func() (ok bool, lastContact time.Time)) {
	s.botLiveness = fn
}

// ── Static files ─────────────────────────────────────────────────────

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	// Try on-disk first
	indexPath := filepath.Join(s.distDir, "index.html")
	if _, err := os.Stat(indexPath); err == nil {
		http.ServeFile(w, r, indexPath)
		return
	}
	// Try embedded
	data, err := embeddedDist.ReadFile("miniapp_dist/index.html")
	if err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<h1>Mini App not found</h1><p>Run: cd miniapp && npm run build</p>`))
}

func (s *Server) serveMiniAppFile(name string, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
			http.NotFound(w, r)
			return
		}
		filePath := filepath.Join(s.distDir, name)
		if _, err := os.Stat(filePath); err == nil {
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			http.ServeFile(w, r, filePath)
			return
		}
		data, err := embeddedDist.ReadFile("miniapp_dist/" + name)
		if err == nil {
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Write(data)
			return
		}
		http.NotFound(w, r)
	}
}

// ── JSON helpers ─────────────────────────────────────────────────────

func jsonResp(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	// Limit request body to 1 MB to prevent OOM attacks.
	limited := io.LimitReader(r.Body, 1<<20)
	return json.NewDecoder(limited).Decode(v)
}

func stopKey(uid int64, session string) string {
	return fmt.Sprintf("%d:%s", uid, session)
}
