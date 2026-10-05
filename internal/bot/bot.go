// Package bot implements the Telegram bot for TGControl.
package bot

import (
	"context"
	"fmt"
	"html"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/config"
	"tgcontrol/internal/observability"
	"tgcontrol/internal/orchestrator"
	"tgcontrol/internal/sessions"
	"tgcontrol/internal/web"
)

const (
	sessionPrefix       = "\U0001F4CC " // 📌
	sessionActivePrefix = "\u2705 "     // ✅
	maxKBSessions       = 6
	maxNameLen          = 24
	dirsPerPage         = 6
	filesPerPage        = 8
)

// Bot wraps the Telegram bot with session management.
type Bot struct {
	b         *bot.Bot
	store     *sessions.Store
	history   *sessions.History
	topics    *sessions.TopicStore
	webServer *web.Server
	allowed   map[int64]bool

	// Per-user wizard state
	wizMu   sync.RWMutex
	wizards map[int64]*wizard

	// Per-user path registry
	pathMu    sync.RWMutex
	pathStore map[int64]map[int]string

	// Per-user upload target
	uploadMu      sync.RWMutex
	uploadTargets map[int64]string

	// Running agents: "uid:session" -> cancel func
	stopMu     sync.Mutex
	stopChans  map[string]chan struct{}
	agentMu    sync.Mutex
	agentLocks map[string]*sync.Mutex

	// Last known Telegram target per user for miniapp-originated sends.
	chatMu      sync.RWMutex
	chatTargets map[int64]chatTarget

	// Discovered sessions cache (for /discover import flow)
	discMu          sync.RWMutex
	discoveredCache map[int64][]agents.DiscoveredSession

	// Liveness — proof the bot is actually reaching Telegram (for the watchdog
	// and the health report). The library's getUpdates loop never returns on a
	// network outage, so we track contact ourselves.
	lastContact atomic.Int64 // unix-nanos of last successful Telegram contact
	healthy     atomic.Bool
}

type chatTarget struct {
	ChatID   int64
	ThreadID int
}

type wizard struct {
	Type    string // "new_session", "project", "rename"
	Step    string // "name", "cwd", "parent", "folder_name", "upload"
	Agent   string
	Name    string
	OldName string
	Parent  string
	Cwd     string
}

// New creates a new Bot instance.
func New(store *sessions.Store, history *sessions.History, topics *sessions.TopicStore, webServer *web.Server, allowedUsers map[int64]bool) *Bot {
	return &Bot{
		store:         store,
		history:       history,
		topics:        topics,
		webServer:     webServer,
		allowed:       allowedUsers,
		wizards:       make(map[int64]*wizard),
		pathStore:     make(map[int64]map[int]string),
		uploadTargets: make(map[int64]string),
		stopChans:     make(map[string]chan struct{}),
		agentLocks:    make(map[string]*sync.Mutex),
		chatTargets:   make(map[int64]chatTarget),
	}
}

// Start initializes the bot and starts polling. It returns nil only when the
// parent ctx is cancelled (real shutdown). If the watchdog determines Telegram
// has been unreachable for too long it cancels an internal context, causing
// Start to return a non-nil error so the supervisor in main.go re-creates the
// bot (possibly picking up a newly-available proxy/route).
func (tb *Bot) Start(ctx context.Context) error {
	cfg := config.Get()
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" && cfg.IsOwnBot() {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN not set")
	}

	agents.DetectOnce()

	const pollTimeout = 50 * time.Second
	client := newTelegramHTTPClient(cfg.TelegramProxyURL(), pollTimeout)
	if px := cfg.TelegramProxyURL(); px != "" {
		log.Printf("[TGBOT] routing Telegram API through proxy %s", px)
	}

	opts := []bot.Option{
		bot.WithHTTPClient(pollTimeout, client),
		bot.WithSkipGetMe(), // don't block init when Telegram is unreachable
		bot.WithMiddlewares(tb.recoverMiddleware, tb.contactMiddleware),
		bot.WithErrorsHandler(tb.onPollError),
		bot.WithDefaultHandler(tb.withTarget(tb.onDefault)),
	}
	if cfg.TelegramAPIURL != "" {
		opts = append(opts, bot.WithServerURL(cfg.TelegramAPIURL))
	}

	b, err := bot.New(token, opts...)
	if err != nil {
		return fmt.Errorf("bot init: %w", err)
	}
	tb.b = b

	// Protected paths оркестратора: эскалация в этот чат кнопками ✅/↩️.
	// Экземпляр Orchestrator создаётся внутри internal/agents, поэтому хук
	// пакетный; без него действия по защищённым путям запрещены молча-отказом.
	orchestrator.SetDefaultConfirmFunc(tb.orchestratorConfirm)

	tb.registerHandlers()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tb.setCommands(runCtx)

	go tb.watchdog(runCtx, cancel)

	log.Println("Bot starting...")
	b.Start(runCtx) // blocks until runCtx is cancelled (parent or watchdog)
	if ctx.Err() != nil {
		return nil // parent shutdown — stop the supervisor
	}
	return fmt.Errorf("telegram unreachable — restarting bot")
}

// recoverMiddleware isolates a handler panic to the single update instead of
// crashing the whole process. The library dispatches each handler in a bare
// goroutine with no recovery of its own, so without this any nil-deref / index
// panic in a handler takes down the bot, web server, tunnel and PTY at once.
func (tb *Bot) recoverMiddleware(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		defer observability.RecoverPanic("bot-handler")
		next(ctx, b, update)
	}
}

// contactMiddleware records a successful Telegram contact on every received
// update (proof the polling connection is alive).
func (tb *Bot) contactMiddleware(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		tb.markContact()
		next(ctx, b, update)
	}
}

// onPollError replaces the library's default error logger and additionally flips
// the health flag so /diag and the health card can surface the silent outage.
func (tb *Bot) onPollError(err error) {
	tb.healthy.Store(false)
	log.Printf("[TGBOT] [ERROR] %v", err)
}

func (tb *Bot) markContact() {
	tb.lastContact.Store(time.Now().UnixNano())
	tb.healthy.Store(true)
}

// Liveness reports whether the bot has recently reached Telegram and when the
// last successful contact happened. Wired into the web health report.
func (tb *Bot) Liveness() (bool, time.Time) {
	ns := tb.lastContact.Load()
	var t time.Time
	if ns > 0 {
		t = time.Unix(0, ns)
	}
	return tb.healthy.Load(), t
}

// watchdog actively probes Telegram (GetMe) every minute. The library's polling
// loop never returns on network errors, so this is the only way to detect a
// sustained outage and force a reconnect. After maxUnhealthy of failed probes it
// cancels the run context, making Start return so the supervisor recreates the
// bot from scratch.
func (tb *Bot) watchdog(ctx context.Context, cancel context.CancelFunc) {
	defer observability.RecoverPanic("bot-watchdog")
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	const maxUnhealthy = 4 * time.Minute
	var failingSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeCtx, pcancel := context.WithTimeout(ctx, 15*time.Second)
			_, err := tb.b.GetMe(probeCtx)
			pcancel()
			if err == nil {
				tb.markContact()
				failingSince = time.Time{}
				continue
			}
			tb.healthy.Store(false)
			if failingSince.IsZero() {
				failingSince = time.Now()
			} else if time.Since(failingSince) >= maxUnhealthy {
				log.Printf("[TGBOT] no contact with Telegram for %s — recreating bot", time.Since(failingSince).Round(time.Second))
				cancel()
				return
			}
		}
	}
}

func (tb *Bot) registerHandlers() {
	wrap := tb.withTarget

	// Commands
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/start", bot.MatchTypeExact, wrap(tb.cmdStart))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/help", bot.MatchTypeExact, wrap(tb.cmdHelp))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/new", bot.MatchTypePrefix, wrap(tb.cmdNew))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/list", bot.MatchTypeExact, wrap(tb.cmdList))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/use ", bot.MatchTypePrefix, wrap(tb.cmdUse))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/use", bot.MatchTypeExact, wrap(tb.cmdUse))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/close", bot.MatchTypePrefix, wrap(tb.cmdClose))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/status", bot.MatchTypeExact, wrap(tb.cmdStatus))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/rename", bot.MatchTypePrefix, wrap(tb.cmdRename))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/last", bot.MatchTypeExact, wrap(tb.cmdLast))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/clone", bot.MatchTypePrefix, wrap(tb.cmdClone))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/cwd", bot.MatchTypePrefix, wrap(tb.cmdCwd))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/project", bot.MatchTypeExact, wrap(tb.cmdProject))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/files", bot.MatchTypeExact, wrap(tb.cmdFiles))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/config", bot.MatchTypeExact, wrap(tb.cmdConfig))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/model", bot.MatchTypeExact, wrap(tb.cmdConfig))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/mode", bot.MatchTypeExact, wrap(tb.cmdMode))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/clear", bot.MatchTypeExact, wrap(tb.cmdClear))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/compact", bot.MatchTypeExact, wrap(tb.cmdCompact))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/cost", bot.MatchTypeExact, wrap(tb.cmdCost))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/continue", bot.MatchTypePrefix, wrap(tb.cmdContinue))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/retry", bot.MatchTypeExact, wrap(tb.cmdRetry))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/agent", bot.MatchTypeExact, wrap(tb.cmdAgent))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/cancel", bot.MatchTypeExact, wrap(tb.cmdCancel))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/discover", bot.MatchTypeExact, wrap(tb.cmdDiscover))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/bind", bot.MatchTypePrefix, wrap(tb.cmdBind))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/unbind", bot.MatchTypeExact, wrap(tb.cmdUnbind))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/topics", bot.MatchTypeExact, wrap(tb.cmdTopics))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/leaving", bot.MatchTypeExact, wrap(tb.cmdLeaving))
	tb.b.RegisterHandler(bot.HandlerTypeMessageText, "/diag", bot.MatchTypeExact, wrap(tb.cmdDiag))

	// Russian aliases
	for _, alias := range []struct {
		prefix  string
		handler bot.HandlerFunc
	}{
		{"/новая", tb.cmdNew},
		{"/сессии", tb.cmdList},
		{"/статус", tb.cmdStatus},
		{"/папка", tb.cmdCwd},
		{"/помощь", tb.cmdHelp},
		{"/отмена", tb.cmdCancel},
		{"/переименовать", tb.cmdRename},
		{"/клон", tb.cmdClone},
		{"/предыдущая", tb.cmdLast},
		{"/проект", tb.cmdProject},
		{"/файлы", tb.cmdFiles},
		{"/настройки", tb.cmdConfig},
		{"/обнаружить", tb.cmdDiscover},
		{"/привязать", tb.cmdBind},
		{"/отвязать", tb.cmdUnbind},
		{"/топики", tb.cmdTopics},
	} {
		tb.b.RegisterHandler(bot.HandlerTypeMessageText, alias.prefix, bot.MatchTypePrefix, wrap(alias.handler))
	}

	// Callback handlers by prefix
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "se:", bot.MatchTypePrefix, wrap(tb.onSessionCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "st:", bot.MatchTypePrefix, wrap(tb.onStatusCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "fb:", bot.MatchTypePrefix, wrap(tb.onBrowserCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "fm:", bot.MatchTypePrefix, wrap(tb.onFileMgrCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "wz:", bot.MatchTypePrefix, wrap(tb.onWizardCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "xa:", bot.MatchTypePrefix, wrap(tb.onActionCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "qr:", bot.MatchTypePrefix, wrap(tb.onQuickCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "cf:", bot.MatchTypePrefix, wrap(tb.onConfigCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "tp:", bot.MatchTypePrefix, wrap(tb.onTopicCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "dc:", bot.MatchTypePrefix, wrap(tb.onDiscoverCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "sh:", bot.MatchTypePrefix, wrap(tb.onShareCB))
	tb.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "ap:", bot.MatchTypePrefix, wrap(tb.onApproveCB))
}

func (tb *Bot) setCommands(ctx context.Context) {
	commands := []models.BotCommand{
		{Command: "new", Description: "Создать сессию"},
		{Command: "list", Description: "Список сессий"},
		{Command: "status", Description: "Текущая сессия"},
		{Command: "model", Description: "Сменить модель"},
		{Command: "mode", Description: "Режим разрешений"},
		{Command: "agent", Description: "Сменить агента"},
		{Command: "clear", Description: "Очистить сессию"},
		{Command: "compact", Description: "Сбросить контекст"},
		{Command: "cost", Description: "Стоимость сессии"},
		{Command: "continue", Description: "Продолжить ответ"},
		{Command: "retry", Description: "Повторить запрос"},
		{Command: "files", Description: "Файловый менеджер"},
		{Command: "cwd", Description: "Рабочая директория"},
		{Command: "cancel", Description: "Остановить агента"},
		{Command: "discover", Description: "Найти запущенные сессии"},
		{Command: "bind", Description: "Привязать топик к агенту"},
		{Command: "unbind", Description: "Отвязать топик"},
		{Command: "topics", Description: "Список привязок топиков"},
		{Command: "leaving", Description: "Самопроверка перед уходом"},
		{Command: "diag", Description: "Диагностика (логи, состояние)"},
		{Command: "help", Description: "Справка"},
	}
	tb.b.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: commands,
	})
}

// ── Helpers ──────────────────────────────────────────────────────────

func (tb *Bot) authorized(uid int64) bool {
	if len(tb.allowed) == 0 {
		return true
	}
	return tb.allowed[uid]
}

func esc(text string) string {
	return html.EscapeString(text)
}

func getUID(update *models.Update) int64 {
	if update.Message != nil && update.Message.From != nil {
		return update.Message.From.ID
	}
	if update.CallbackQuery != nil && update.CallbackQuery.From.ID != 0 {
		return update.CallbackQuery.From.ID
	}
	return 0
}

func getChatID(update *models.Update) int64 {
	if update.Message != nil {
		return update.Message.Chat.ID
	}
	if update.CallbackQuery != nil && update.CallbackQuery.Message.Message != nil {
		return update.CallbackQuery.Message.Message.Chat.ID
	}
	return 0
}

func getThreadID(update *models.Update) int {
	if update.Message != nil {
		return update.Message.MessageThreadID
	}
	if update.CallbackQuery != nil && update.CallbackQuery.Message.Message != nil {
		return update.CallbackQuery.Message.Message.MessageThreadID
	}
	return 0
}

func cmdArgs(text string) []string {
	parts := strings.Fields(text)
	if len(parts) > 1 {
		return parts[1:]
	}
	return nil
}

func stopKey(uid int64, session string) string {
	return fmt.Sprintf("%d:%s", uid, session)
}

func (tb *Bot) withTarget(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		tb.rememberTarget(getUID(update), getChatID(update), getThreadID(update))
		next(ctx, b, update)
	}
}

func (tb *Bot) rememberTarget(uid, chatID int64, threadID int) {
	if uid == 0 || chatID == 0 {
		return
	}
	tb.chatMu.Lock()
	tb.chatTargets[uid] = chatTarget{ChatID: chatID, ThreadID: threadID}
	tb.chatMu.Unlock()
}

// ── Path registry ────────────────────────────────────────────────────

func (tb *Bot) pid(uid int64, path string) int {
	tb.pathMu.Lock()
	defer tb.pathMu.Unlock()

	if tb.pathStore[uid] == nil {
		tb.pathStore[uid] = make(map[int]string)
	}
	for k, v := range tb.pathStore[uid] {
		if v == path {
			return k
		}
	}
	// Bound growth: this is a transient id↔path cache for inline keyboards in a
	// process that runs for weeks. If it grows pathologically large, reset it —
	// stale ids only back old keyboards the user is no longer looking at.
	if len(tb.pathStore[uid]) >= 1000 {
		tb.pathStore[uid] = make(map[int]string)
	}
	idx := len(tb.pathStore[uid])
	tb.pathStore[uid][idx] = path
	return idx
}

func (tb *Bot) pget(uid int64, idx int) string {
	tb.pathMu.RLock()
	defer tb.pathMu.RUnlock()

	if tb.pathStore[uid] == nil {
		return ""
	}
	return tb.pathStore[uid][idx]
}

// ── Wizard state ─────────────────────────────────────────────────────

func (tb *Bot) setWizard(uid int64, w *wizard) {
	tb.wizMu.Lock()
	defer tb.wizMu.Unlock()
	tb.wizards[uid] = w
}

func (tb *Bot) getWizard(uid int64) *wizard {
	tb.wizMu.RLock()
	defer tb.wizMu.RUnlock()
	return tb.wizards[uid]
}

func (tb *Bot) clearWizard(uid int64) {
	tb.wizMu.Lock()
	defer tb.wizMu.Unlock()
	delete(tb.wizards, uid)

	tb.uploadMu.Lock()
	delete(tb.uploadTargets, uid)
	tb.uploadMu.Unlock()
}

// ── Upload target ────────────────────────────────────────────────────

func (tb *Bot) setUploadTarget(uid int64, path string) {
	tb.uploadMu.Lock()
	defer tb.uploadMu.Unlock()
	tb.uploadTargets[uid] = path
}

func (tb *Bot) getUploadTarget(uid int64) string {
	tb.uploadMu.RLock()
	defer tb.uploadMu.RUnlock()
	return tb.uploadTargets[uid]
}

func (tb *Bot) clearUploadTarget(uid int64) {
	tb.uploadMu.Lock()
	defer tb.uploadMu.Unlock()
	delete(tb.uploadTargets, uid)
}

// ── Auto name ────────────────────────────────────────────────────────

func (tb *Bot) autoName(uid int64, agentType string) string {
	sessList := tb.store.List(int(uid))
	maxNum := 0
	prefix := agentType + "-"
	for _, sess := range sessList {
		if strings.HasPrefix(sess.Name, prefix) {
			suffix := sess.Name[len(prefix):]
			if n, err := strconv.Atoi(suffix); err == nil && n > maxNum {
				maxNum = n
			}
		}
	}
	return fmt.Sprintf("%s%d", prefix, maxNum+1)
}

// ── Agent IDs list ───────────────────────────────────────────────────

func agentIDs() string {
	detected := agents.GetDetected()
	ids := make([]string, len(detected))
	for i, d := range detected {
		ids[i] = d.ID
	}
	return strings.Join(ids, ", ")
}

func helpText() string {
	return fmt.Sprintf(`🤖 <b>TGControl</b> — удалённое управление CLI-агентами
Отправляйте текст — он пойдёт в активного агента (%s).

<b>⚡ Быстрые действия:</b>
/continue — продолжить ответ агента
/retry — повторить последний запрос
/cancel — остановить агента

<b>🤖 Агент и модель:</b>
/model — сменить модель (sonnet/opus/haiku)
/mode — режим разрешений
/agent — сменить агента сессии
/config — все настройки

<b>💬 Контекст:</b>
/clear — очистить сессию и историю
/compact — сбросить контекст (как в Claude Code)
/cost — стоимость текущей сессии

<b>📋 Сессии:</b>
/new — создать сессию
/list — список сессий
/use <code>имя</code> — переключить
/close — закрыть
/last — предыдущая
/status — подробности
/rename, /clone — переименовать, клонировать

<b>📁 Файлы:</b>
/files — файловый менеджер
/cwd — рабочая директория
/project — создать проект
Прикрепите файл — он сохранится в папку сессии.

<b>🔗 Топики (ACP):</b>
/bind <code>агент</code> — привязать топик к агенту
/unbind — отвязать топик
/topics — список привязок

<b>🔍 Обнаружение сессий:</b>
/discover — найти запущенные Claude/Codex сессии на ПК и подключиться к ним`, agentIDs())
}
