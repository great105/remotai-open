// Package config handles app configuration and first-launch setup wizard.
package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"tgcontrol/internal/localize"

	"tgcontrol/internal/atomicfile"
	"tgcontrol/internal/paths"
)

// Mode constants
const (
	ModeOwnBot     = "own_bot"
	ModeCentralBot = "central_bot"
	// ModeStandalone — no Telegram at all. The app is controlled directly by
	// the mobile client over LAN / the user's own tunnel, authenticated with
	// the static api_token. No bot, no relay.
	ModeStandalone = "standalone"
)

// Relay server URLs (for Mode 1 — TGControl Cloud)
const (
	// RelayURL — backward-compat: ws-style endpoint. Новые клиенты используют RelayBaseURL (HTTP).
	RelayURL        = "wss://remotai.ru/v1/agent/connect"
	DefaultRelayURL = "https://remotai.ru"
	RegisterURL     = "https://remotai.ru/v1/pair/request"
)

// Config holds all application settings.
type Config struct {
	Mode                 string `json:"mode"`
	TelegramUserID       string `json:"telegram_user_id"`
	ConnectionToken      string `json:"connection_token"`
	RelayURL             string `json:"relay_url"`
	ClaudePath           string `json:"claude_path"`
	ClaudeModel          string `json:"claude_model"`
	ClaudePermissionMode string `json:"claude_permission_mode"`
	CodexPath            string `json:"codex_path"`
	CodexModel           string `json:"codex_model"`
	CodexApprovalMode    string `json:"codex_approval_mode"`
	CodexReasoning       string `json:"codex_reasoning"`
	DefaultAgent         string `json:"default_agent"`
	DefaultCwd           string `json:"default_cwd"`
	NotificationsEnabled string `json:"notifications_enabled"`
	WebPort              string `json:"web_port"`

	// Orchestrator settings
	OrchestratorModel string `json:"orchestrator_model,omitempty"` // short name or full model ID

	// ACP-inspired dispatch settings
	MaxConcurrentSessions int      `json:"max_concurrent_sessions,omitempty"` // 0 = unlimited
	AllowedAgents         []string `json:"allowed_agents,omitempty"`          // empty = all
	DefaultTTLMinutes     int      `json:"default_ttl_minutes,omitempty"`     // 0 = no TTL

	// Standalone APK auth
	APIToken    string `json:"api_token,omitempty"`     // static token for APK auth (auto-generated UUID)
	APITokenUID int64  `json:"api_token_uid,omitempty"` // user ID mapped to this token

	// Device identification
	DeviceID string `json:"device_id,omitempty"` // hardware-derived 12-char hex ID

	// Setup state
	SetupComplete bool `json:"setup_complete"` // true after web wizard finishes

	// JWT auth
	JWTSigningKey string `json:"jwt_signing_key,omitempty"` // base64-encoded 32-byte key

	// Tunnel settings
	TunnelMode      string `json:"tunnel_mode,omitempty"`      // "disabled", "quick", "named"
	CloudflaredPath string `json:"cloudflared_path,omitempty"` // path to cloudflared binary
	TunnelName      string `json:"tunnel_name,omitempty"`      // for named tunnels
	TunnelURL       string `json:"tunnel_url,omitempty"`       // last known public URL
	TunnelAutoStart bool   `json:"tunnel_autostart,omitempty"` // start tunnel on launch

	// Inbox: folder where files received from the Telegram bot are stored
	// (when no topic→agent binding directs them elsewhere).
	InboxDir string `json:"inbox_dir,omitempty"`

	// TelegramProxy routes the Telegram Bot API through a proxy so the bot keeps
	// working where the Telegram IPs are throttled/blocked at the ISP (e.g. RU).
	// Accepts socks5://host:port, socks5h://host:port, http://host:port (both
	// supported natively by net/http). Empty ⇒ fall back to *_PROXY env vars.
	TelegramProxy string `json:"telegram_proxy,omitempty"`
	// TelegramAPIURL optionally overrides the Telegram Bot API base URL (e.g. a
	// reachable mirror). Empty ⇒ official https://api.telegram.org.
	TelegramAPIURL string `json:"telegram_api_url,omitempty"`

	// Cloud relay (P2). RelayJWT хранится отдельно через keystore — в JSON
	// сохраняется только expiry, чтобы UI мог предупреждать о скором истечении.
	RelayBaseURL   string `json:"relay_base_url,omitempty"`
	RelayJWT       string `json:"-"`
	RelayJWTExpiry int64  `json:"relay_jwt_expiry,omitempty"`
	// Незавершённый cloud-pairing хранится до истечения короткого кода. Это
	// позволяет агенту забрать выданный device-JWT, даже если пользователь
	// закрыл окно настройки или приложение перезапустилось в ожидании Telegram.
	PendingPairCode      string `json:"pending_pair_code,omitempty"`
	PendingPairExpiresAt int64  `json:"pending_pair_expires_at,omitempty"`

	// DisableAutoUpdate выключает фоновую самопроверку обновлений
	// (https://remotai.ru/download/latest.json). По умолчанию включено.
	DisableAutoUpdate bool `json:"disable_auto_update,omitempty"`

	// DetectAgentQuestions — распознавание вопросов агента по экрану терминала
	// («ждёт ответа» + кнопки 1/2/3 на телефоне). ВЫКЛЮЧЕНО по умолчанию с
	// 2.49.4 по решению владельца: «ловит просто так».
	//
	// Почему так вышло: единственный универсальный источник — текст на экране, а
	// текст врёт. Живой случай: агент печатал ОТВЕТ ПРО вопросы — таблицу со
	// строками «Ok to proceed? (y)» и «1. Yes / 2. … / 3. No», — и терминал
	// объявил, что ждёт подтверждения, хотя работа была закончена. Отличить
	// рассказ о меню от самого меню по тексту нельзя: у Claude Code, Codex и
	// прочих нет ни маркера «это вопрос», ни протокола, который мы могли бы
	// прочитать (Agent SDK есть только у Claude и требует запускать агента не в
	// терминале). Механизм оставлен под флагом — включается в настройках.
	DetectAgentQuestions bool `json:"detect_agent_questions,omitempty"`

	// ShellIntegration — разметка команд в терминале (OSC 133, ST-10): шелл
	// отмечает приглашение, команду и её вывод, и в приложении их можно
	// копировать по отдельности. ВЫКЛЮЧЕНО по умолчанию: включается после
	// проверки на устройстве владельца. Действует на НОВЫЕ терминалы.
	// Пользовательская конфигурация шелла подключается как обычно
	// (internal/pty/shell_integration.go).
	ShellIntegration bool `json:"shell_integration,omitempty"`

	// ScreenshotHotkey — занимать ли клавишу PrtScr на самом компьютере, чтобы
	// снимок экрана падал файлом в ~/Remotai/files (оттуда его читает агент).
	// ВЫКЛЮЧЕНО по умолчанию, и это не осторожность, а вежливость: PrtScr в
	// Windows достаётся первому, кто её попросил, и молча отбирать привычную
	// клавишу у системных «Ножниц» или у сторонней программы человека нельзя.
	// Пока Windows: про другие системы см. screenshot_hotkey_other.go.
	ScreenshotHotkey bool `json:"screenshot_hotkey,omitempty"`

	// AgentSummaries — пересказывать вывод агента в уведомлении: «что сделал» и
	// «о чём спрашивает» вместо «агент закончил». ВЫКЛЮЧЕНО по умолчанию, и это
	// не осторожность ради осторожности: пересказ делает языковая модель, а
	// значит текст терминала уходит в OpenRouter по ключу человека. Такое
	// включают сознательно, прочитав, что именно уходит (карточка настройки
	// говорит это прямым текстом). Механика — internal/web/agent_summary.go,
	// маскирование секретов перед отправкой — internal/agentsummary.
	AgentSummaries bool `json:"agent_summaries,omitempty"`

	// PeerAccess — что можно ДРУГОМУ УСТРОЙСТВУ этого же аккаунта (серверу,
	// дотянувшемуся до компьютера через облако): off | diag | full.
	// Пусто = diag: состояние компьютера и управление VPN, без терминалов,
	// файлов и экрана. Подробности и белый список — internal/relay/peer_access.go.
	PeerAccess string `json:"peer_access,omitempty"`

	// VPNWatchdog — сторож зависшего VPN: заметив, что облака нет и релей не
	// отвечает, выключает VPN-клиент и проверяет связь снова (а если не помогло —
	// возвращает его назад). По умолчанию ВКЛЮЧЁН: выключать он имеет право
	// только то, что и так уже не работает. Отключается значением "false".
	VPNWatchdog string `json:"vpn_watchdog,omitempty"`
}

// PeerAccessMode — режим доступа для соседних устройств аккаунта.
// Значение нормализуется в internal/relay (там же живёт белый список путей),
// здесь — только сырая настройка.
func (c *Config) PeerAccessMode() string { return c.PeerAccess }

// VPNWatchdogEnabled — включён ли сторож зависшего VPN (по умолчанию да).
func (c *Config) VPNWatchdogEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(c.VPNWatchdog), "false")
}

// Defaults returns a Config with default values.
func Defaults() *Config {
	return &Config{
		Mode:                 ModeOwnBot,
		TelegramUserID:       "",
		ConnectionToken:      "",
		RelayURL:             RelayURL,
		RelayBaseURL:         DefaultRelayURL,
		ClaudePath:           "claude",
		ClaudeModel:          "sonnet",
		ClaudePermissionMode: "bypassPermissions",
		CodexPath:            "",
		CodexModel:           "gpt-5.3-codex",
		CodexApprovalMode:    "full-auto",
		CodexReasoning:       "medium",
		DefaultAgent:         "claude",
		DefaultCwd:           "",
		NotificationsEnabled: "true",
		WebPort:              "8080",
	}
}

// IsCentralBot returns true if using relay mode.
func (c *Config) IsCentralBot() bool { return c.Mode == ModeCentralBot }

// RelayHTTPBase возвращает https-URL relay для REST API.
// Производное от RelayBaseURL или RelayURL (с заменой wss/ws → https/http).
func (c *Config) RelayHTTPBase() string {
	if c.RelayBaseURL != "" {
		return strings.TrimRight(c.RelayBaseURL, "/")
	}
	u := c.RelayURL
	if u == "" {
		return ""
	}
	u = strings.TrimRight(u, "/")
	switch {
	case strings.HasPrefix(u, "wss://"):
		u = "https://" + u[len("wss://"):]
	case strings.HasPrefix(u, "ws://"):
		u = "http://" + u[len("ws://"):]
	}
	// если задан полный WS-путь вроде wss://relay/connect — отрезаем хвост
	if idx := strings.LastIndex(u, "/"); idx > strings.Index(u, "://")+2 {
		base := u[:idx]
		// сохраняем только если в хвосте действительно WS-путь
		tail := u[idx:]
		if tail == "/connect" || tail == "/v1/agent/connect" {
			return base
		}
	}
	return u
}

// RelayAgentWS возвращает wss-URL endpoint для outbound WS подключения агента.
func (c *Config) RelayAgentWS() string { return c.relayWSPath("/v1/agent/connect") }

// RelayStreamWS возвращает wss-URL для outbound стрим-подключения агента
// (PTY / Remote Desktop reverse-tunnel).
func (c *Config) RelayStreamWS() string { return c.relayWSPath("/v1/agent/stream") }

func (c *Config) relayWSPath(path string) string {
	base := c.RelayHTTPBase()
	if base == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + base[len("https://"):] + path
	case strings.HasPrefix(base, "http://"):
		return "ws://" + base[len("http://"):] + path
	}
	return ""
}

// IsOwnBot returns true if using own BotFather bot.
func (c *Config) IsOwnBot() bool { return c.Mode == ModeOwnBot }

// IsAgentAllowed checks if an agent type is in the allowed list (empty = all allowed).
func (c *Config) IsAgentAllowed(agentType string) bool {
	if len(c.AllowedAgents) == 0 {
		return true
	}
	for _, a := range c.AllowedAgents {
		if a == agentType {
			return true
		}
	}
	return false
}

// Port returns the web port as int.
func (c *Config) Port() int {
	p, err := strconv.Atoi(c.WebPort)
	if err != nil || p <= 0 {
		return 8080
	}
	return p
}

// configPath returns the path to config.json: next to the exe in portable
// mode, otherwise the per-user data dir (см. internal/paths).
func configPath() string {
	return filepath.Join(paths.Base(), "config.json")
}

// envPath returns the path to .env file (same dir as config.json).
func envPath() string {
	return filepath.Join(paths.Base(), ".env")
}

// saveMu serializes Save/Update so two writers can't race on the .bak swap.
var saveMu sync.Mutex

// Load reads config from config.json. Returns false if missing/broken.
// On a corrupt (unparseable) file it quarantines the bad copy and tries the
// last-known-good config.json.bak instead of silently reverting to defaults
// (which would wipe the bot token / JWT key / device identity).
func (c *Config) Load() bool {
	p := configPath()
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	if err := json.Unmarshal(data, c); err != nil {
		log.Printf("[CONFIG] %s is corrupt (%v) — quarantining and trying .bak", p, err)
		if bad, qerr := atomicfile.Quarantine(p); qerr == nil {
			log.Printf("[CONFIG] moved corrupt file to %s", bad)
		}
		if bdata, berr := os.ReadFile(p + ".bak"); berr == nil {
			if json.Unmarshal(bdata, c) == nil {
				log.Printf("[CONFIG] recovered configuration from %s.bak", p)
				_ = c.Save() // re-materialize the primary from backup
				return true
			}
		}
		return false
	}
	return true
}

// Save atomically writes config to config.json (temp+fsync+rename) and keeps the
// previous good copy as config.json.bak for crash recovery.
func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	p := configPath()
	// Preserve the last-known-good file as .bak before overwriting.
	if old, rerr := os.ReadFile(p); rerr == nil && json.Valid(old) {
		_ = atomicfile.WriteFile(p+".bak", old, 0600)
	}
	return atomicfile.WriteFile(p, data, 0600)
}

// Update mutates the global config under the package lock and persists it
// atomically. Use this instead of mutating the pointer from Get()/GetNoSetup()
// and calling Save() — that pattern races marshaling against concurrent writers.
func Update(mutate func(*Config)) error {
	_mu.Lock()
	if _config == nil {
		_config = Defaults()
		_config.Load()
	}
	// Copy-on-write: мутируем КОПИЮ и публикуем новый указатель. Иначе мутация
	// полей _config in-place гонится с читателями, которые держат указатель из
	// Get()/GetNoSetup() и читают поля без блокировки (data race, аудит 2026-06-20).
	// После публикации указуемая структура неизменяема — читать её поля безопасно.
	nc := *_config
	mutate(&nc)
	_config = &nc
	data, err := json.MarshalIndent(_config, "", "  ")
	_mu.Unlock()
	if err != nil {
		return err
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	p := configPath()
	if old, rerr := os.ReadFile(p); rerr == nil && json.Valid(old) {
		_ = atomicfile.WriteFile(p+".bak", old, 0600)
	}
	return atomicfile.WriteFile(p, data, 0600)
}

// TelegramProxyURL returns the configured proxy (config.json wins, else the
// TELEGRAM_PROXY env var). Empty means "use *_PROXY env / direct".
func (c *Config) TelegramProxyURL() string {
	if c.TelegramProxy != "" {
		return c.TelegramProxy
	}
	return os.Getenv("TELEGRAM_PROXY")
}

// FindCLI searches for a CLI tool in PATH.
func FindCLI(name string) string {
	path, err := exec.LookPath(name)
	if err == nil {
		return path
	}
	if runtime.GOOS == "windows" {
		for _, ext := range []string{".cmd", ".exe", ".bat"} {
			path, err = exec.LookPath(name + ext)
			if err == nil {
				return path
			}
		}
	}
	return ""
}

// ── Menu options for setup wizard ────────────────────────────────────

type menuOption struct {
	Key  string
	Desc string
}

var modes = []menuOption{
	{"central_bot", localize.Text("Central Bot — через наш бот @TGControlBot (проще)")},
	{"own_bot", localize.Text("Own Bot — свой бот через BotFather (приватнее)")},
}

var claudeModels = []menuOption{
	{"sonnet", localize.Text("Sonnet 4.5 — быстрая и умная (рекомендуется)")},
	{"opus", localize.Text("Opus 4.6 — самая умная, дороже")},
	{"haiku", localize.Text("Haiku 4.5 — самая быстрая и дешёвая")},
}

var claudePermissionModes = []menuOption{
	{"bypassPermissions", localize.Text("Полный автомат — без подтверждений (рекомендуется)")},
	{"default", localize.Text("Стандартный — спрашивает подтверждение")},
	{"plan", localize.Text("Plan — только планирование, без выполнения")},
}

var codexModels = []menuOption{
	{"gpt-5.3-codex", localize.Text("GPT-5.3-Codex — самая умная (рекомендуется)")},
	{"gpt-5.3-codex-spark", localize.Text("GPT-5.3-Codex-Spark — быстрая, 1000+ tok/s")},
	{"gpt-5.2-codex", localize.Text("GPT-5.2-Codex — предыдущее поколение, дешевле")},
}

var codexReasoningOptions = []menuOption{
	{"xhigh", localize.Text("Максимум — самый умный, медленный")},
	{"high", localize.Text("Высокий — для сложных задач (рекомендуется)")},
	{"medium", localize.Text("Средний — баланс скорости и качества")},
	{"low", localize.Text("Низкий — быстрее, проще")},
	{"minimal", localize.Text("Минимальный — самый быстрый")},
}

var codexApprovalModes = []menuOption{
	{"full-auto", localize.Text("Автомат с песочницей (рекомендуется)")},
	{"bypass", localize.Text("Полный автомат без песочницы (опасно!)")},
	{"suggest", localize.Text("Спрашивает перед незнакомыми командами")},
	{"auto", localize.Text("Не спрашивает, ошибки возвращает модели")},
}

// ── Interactive helpers ──────────────────────────────────────────────

func pick(prompt string, options []menuOption, defaultKey string) string {
	fmt.Printf("\n  %s\n", prompt)
	for i, opt := range options {
		marker := ""
		if opt.Key == defaultKey {
			marker = " *"
		}
		fmt.Printf("    [%d] %s — %s%s\n", i+1, opt.Key, opt.Desc, marker)
	}
	for {
		defaultHint := ""
		if defaultKey != "" {
			for i, opt := range options {
				if opt.Key == defaultKey {
					defaultHint = fmt.Sprintf(" [%d]", i+1)
					break
				}
			}
		}
		fmt.Printf(localize.Text("  Выбор%s: "), defaultHint)
		var choice string
		if _, err := fmt.Scanln(&choice); errors.Is(err, io.EOF) {
			stdinClosed = true
			return defaultKey
		}
		choice = strings.TrimSpace(choice)

		if choice == "" && defaultKey != "" {
			return defaultKey
		}
		if n, err := strconv.Atoi(choice); err == nil {
			idx := n - 1
			if idx >= 0 && idx < len(options) {
				return options[idx].Key
			}
		}
		for _, opt := range options {
			if choice == opt.Key {
				return opt.Key
			}
		}
		fmt.Println(localize.Text("    Неверный выбор, попробуйте ещё раз."))
	}
}

// stdinClosed — ввода больше не будет (EOF): запуск без консоли, служба, CI.
// Циклы «спросить ещё раз» обязаны на нём остановиться. Без этого мастер на
// пустом stdin переспрашивал Telegram ID бесконечно: 13.09.2026 так висел
// CI — 18 млн строк лога за 10 минут на каждый прогон.
var stdinClosed bool

func input(prompt string, defaultVal string) string {
	hint := ""
	if defaultVal != "" {
		hint = fmt.Sprintf(" [%s]", defaultVal)
	}
	fmt.Printf("  %s%s: ", prompt, hint)
	var val string
	if _, err := fmt.Scanln(&val); errors.Is(err, io.EOF) {
		stdinClosed = true
	}
	val = strings.TrimSpace(val)
	if val == "" {
		return defaultVal
	}
	return val
}

// ── Setup wizard ─────────────────────────────────────────────────────

// RunSetup runs the interactive first-launch setup.
func RunSetup(force bool) *Config {
	cfg := Defaults()
	if !force && cfg.Load() {
		return cfg
	}

	fmt.Println()
	fmt.Println(strings.Repeat("=", 52))
	fmt.Println(localize.Text("   TGControl — Настройка"))
	fmt.Println(strings.Repeat("=", 52))

	// 0. Mode
	fmt.Println(localize.Text("\n  [0/4] Режим работы"))
	cfg.Mode = pick(localize.Text("Как подключаться к Telegram:"), modes, "central_bot")

	// 1. CLI agents
	fmt.Println(localize.Text("\n  [1/4] CLI-агенты"))
	if found := FindCLI("claude"); found != "" {
		fmt.Printf("    Claude CLI: %s\n", found)
		use := input(localize.Text("Использовать? (y/n)"), "y")
		if strings.ToLower(use) == "y" || use == "" || strings.ToLower(use) == "д" {
			cfg.ClaudePath = found
		} else {
			cfg.ClaudePath = input(localize.Text("Путь к claude"), "claude")
		}
	} else {
		fmt.Println(localize.Text("    Claude CLI: не найден в PATH"))
		cfg.ClaudePath = input(localize.Text("Путь к claude (или Enter = 'claude')"), "claude")
	}

	if found := FindCLI("codex"); found != "" {
		fmt.Printf("    Codex CLI:  %s\n", found)
		use := input(localize.Text("Использовать? (y/n)"), "y")
		if strings.ToLower(use) == "y" || use == "" || strings.ToLower(use) == "д" {
			cfg.CodexPath = found
		} else {
			cfg.CodexPath = input(localize.Text("Путь к codex (или Enter = пропустить)"), "")
		}
	} else {
		fmt.Println(localize.Text("    Codex CLI:  не найден (необязательно)"))
		cfg.CodexPath = input(localize.Text("Путь к codex (или Enter = пропустить)"), "")
	}

	// 2. Claude settings
	fmt.Println(localize.Text("\n  [2/4] Настройки Claude"))
	cfg.ClaudeModel = pick(localize.Text("Модель:"), claudeModels, "sonnet")
	cfg.ClaudePermissionMode = pick(localize.Text("Режим разрешений:"), claudePermissionModes, "bypassPermissions")

	// 3. Codex settings
	if cfg.CodexPath != "" {
		fmt.Println(localize.Text("\n  [3/4] Настройки Codex"))
		cfg.CodexModel = pick(localize.Text("Модель:"), codexModels, "gpt-5.3-codex")
		cfg.CodexReasoning = pick("Reasoning effort:", codexReasoningOptions, "high")
		cfg.CodexApprovalMode = pick(localize.Text("Режим работы:"), codexApprovalModes, "full-auto")
	} else {
		fmt.Println(localize.Text("\n  [3/4] Настройки Codex — пропущено (CLI не настроен)"))
	}

	// 4. Connection
	if cfg.IsCentralBot() {
		setupCentralBot(cfg)
	} else {
		setupOwnBot(cfg)
	}

	finishSetup(cfg)
	return cfg
}

func setupCentralBot(cfg *Config) {
	fmt.Println("\n  [4/4] Central Bot")
	fmt.Println(localize.Text("    Используется наш бот @TGControlBot"))
	fmt.Println(localize.Text("    Вам нужен только ваш Telegram User ID."))
	fmt.Println(localize.Text("    Узнать: отправьте /start боту @userinfobot"))

	userID := input(localize.Text("Ваш Telegram User ID"), "")
	for !stdinClosed {
		if _, err := strconv.ParseInt(userID, 10, 64); err == nil {
			break
		}
		fmt.Println(localize.Text("    ID должен быть числом!"))
		userID = input(localize.Text("Ваш Telegram User ID"), "")
	}

	cfg.TelegramUserID = userID
	// Generate connection token
	cfg.ConnectionToken = generateToken()
	fmt.Printf("    User ID: %s\n", userID)
	fmt.Printf("    Token: %s...\n", cfg.ConnectionToken[:8])
}

func setupOwnBot(cfg *Config) {
	fmt.Println("\n  [4/4] Telegram Bot")

	ep := envPath()
	if _, err := os.Stat(ep); err == nil {
		fmt.Printf(localize.Text("    .env найден: %s\n"), ep)
		reconf := input(localize.Text("Перенастроить .env? (y/n)"), "n")
		if strings.ToLower(reconf) != "y" && strings.ToLower(reconf) != "д" {
			return
		}
	} else {
		fmt.Println(localize.Text("    .env не найден — создаём."))
	}

	token := input("Bot Token (@BotFather)", "")
	for token == "" && !stdinClosed {
		fmt.Println(localize.Text("    Токен обязателен!"))
		token = input("Bot Token (@BotFather)", "")
	}

	users := input(localize.Text("Allowed User IDs (через запятую, Enter = все)"), "")

	lines := []string{fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s", token)}
	if users != "" {
		lines = append(lines, fmt.Sprintf("ALLOWED_USERS=%s", users))
	}
	os.WriteFile(ep, []byte(strings.Join(lines, "\n")+"\n"), 0600)
	fmt.Printf(localize.Text("    Сохранено: %s\n"), ep)
}

func finishSetup(cfg *Config) {
	if err := cfg.Save(); err != nil {
		fmt.Printf(localize.Text("  Ошибка сохранения: %v\n"), err)
		return
	}

	fmt.Println()
	fmt.Println(strings.Repeat("-", 52))
	modeName := "Own Bot"
	if cfg.IsCentralBot() {
		modeName = "Central Bot"
	}
	fmt.Printf(localize.Text("  Режим: %s\n"), modeName)
	fmt.Printf("  Claude:  %s\n", cfg.ClaudePath)
	fmt.Printf(localize.Text("  Модель:  %s | Режим: %s\n"), cfg.ClaudeModel, cfg.ClaudePermissionMode)
	if cfg.CodexPath != "" {
		fmt.Printf("  Codex:   %s\n", cfg.CodexPath)
		fmt.Printf(localize.Text("  Модель:  %s | Reasoning: %s | Режим: %s\n"),
			cfg.CodexModel, cfg.CodexReasoning, cfg.CodexApprovalMode)
	} else {
		fmt.Println(localize.Text("  Codex:   (не настроен)"))
	}
	if cfg.IsCentralBot() {
		fmt.Printf("  User ID: %s\n", cfg.TelegramUserID)
	}
	fmt.Printf(localize.Text("  Конфиг:  %s\n"), configPath())
	fmt.Println(strings.Repeat("-", 52))
	fmt.Println(localize.Text("  Готово! Запуск бота..."))
	fmt.Println()
}

func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail, but handle gracefully
		for i := range b {
			b[i] = byte(i * 17)
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// DefaultInboxDir returns the default location for the bot inbox folder
// (%USERPROFILE%\Downloads\TGControl-Inbox on Windows, ~/Downloads/TGControl-Inbox elsewhere).
func DefaultInboxDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Fallback: next to the executable.
		exe, _ := os.Executable()
		return filepath.Join(filepath.Dir(exe), "TGControl-Inbox")
	}
	return filepath.Join(home, "Downloads", "TGControl-Inbox")
}

// EnsureInboxDir returns the configured inbox directory, creating it if missing.
// Falls back to DefaultInboxDir() when InboxDir is unset.
func (c *Config) EnsureInboxDir() (string, error) {
	dir := c.InboxDir
	if dir == "" {
		dir = DefaultInboxDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create inbox dir %q: %w", dir, err)
	}
	return dir, nil
}

// EnsureAPIToken generates and saves an API token if one doesn't exist yet.
func (c *Config) EnsureAPIToken(defaultUID int64) {
	if c.APIToken != "" {
		return
	}
	_ = Update(func(cc *Config) {
		if cc.APIToken == "" {
			cc.APIToken = generateToken()
		}
		if cc.APITokenUID == 0 {
			cc.APITokenUID = defaultUID
		}
	})
}

// IsConfigured returns true if the app has been set up (has bot token or central bot config).
func (c *Config) IsConfigured() bool {
	if c.SetupComplete {
		return true
	}
	// Legacy: check if own_bot mode has a bot token in env
	if c.IsOwnBot() {
		return os.Getenv("TELEGRAM_BOT_TOKEN") != ""
	}
	// Central bot needs a user ID and connection token
	return c.IsCentralBot() && c.TelegramUserID != "" && c.ConnectionToken != ""
}

// ── Singleton ────────────────────────────────────────────────────────

var (
	_config *Config
	_once   sync.Once
	_mu     sync.RWMutex
)

// Get returns the global config (runs setup wizard if needed).
func Get() *Config {
	_once.Do(func() {
		_mu.Lock()
		_config = Defaults()
		if !_config.Load() {
			_mu.Unlock()
			_config = RunSetup(false)
			return
		}
		_mu.Unlock()
	})
	_mu.RLock()
	defer _mu.RUnlock()
	return _config
}

// GetNoSetup returns the global config without triggering the setup wizard.
// Returns defaults if config.json doesn't exist.
func GetNoSetup() *Config {
	_once.Do(func() {
		_mu.Lock()
		defer _mu.Unlock()
		_config = Defaults()
		_config.Load() // ignore if missing
	})
	_mu.RLock()
	defer _mu.RUnlock()
	return _config
}

// Reload re-reads config.json from disk and returns the updated config.
func Reload() *Config {
	_mu.Lock()
	defer _mu.Unlock()
	_config = Defaults()
	_config.Load()
	return _config
}
