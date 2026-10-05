// Package bot — единственный Telegram-бот в облачном режиме.
// Обрабатывает /start [pair_CODE], текстовый ввод кода и приветствие.
package bot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol-relay/internal/auth"
	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/metrics"
	"tgcontrol-relay/internal/notify"
)

type Bot struct {
	cfg *config.Config
	db  *sql.DB
	jwt *auth.Issuer
	b   *tgbot.Bot

	// PtyInput — «нажать клавишу в терминале ПК» под инлайн-кнопкой уведомления.
	// Проводится из main.go (server.SendPtyInput), чтобы bot не импортировал
	// server. nil — кнопки ответа не работают, человек это увидит в алерте.
	PtyInput notify.PtyInputFunc

	// Live relay callbacks are injected from cmd/relay so the bot remains
	// independent of package server. They power /pc, /term and /run.
	IsOnline     func(deviceID string) bool
	AgentRequest func(ctx context.Context, userID int64, deviceID, method, path string, body []byte) (status int, response []byte, err error)

	// answered — уведомления, под которыми уже нажали кнопку ответа (защита от
	// двойного тапа, см. claimAnswer в notify.go).
	answeredMu sync.Mutex
	answered   map[answeredKey]time.Time

	// replyTargets/replyLatest — куда уходит свободный текст из чата: терминал из
	// последнего уведомления «агент ждёт ответа» (см. rememberReplyTarget).
	// Только в памяти: после рестарта релея человек ответит кнопкой или из
	// приложения, а писать в терминал по несвежему адресату нельзя.
	replyMu      sync.Mutex
	replyTargets map[answeredKey]chatReplyTarget
	replyLatest  map[int64]answeredKey

	// supportDrafts — тексты, которые человек написал боту «в поддержку», но ещё
	// не подтвердил кнопкой (см. cbSupportSend). Ключ — сообщение ЧЕЛОВЕКА:
	// callback_data 64 байта, сам текст в неё не влезает.
	supportMu     sync.Mutex
	supportDrafts map[answeredKey]supportDraft
}

// supportDraft — необработанное сообщение, предложенное к отправке в поддержку.
type supportDraft struct {
	text string
	at   time.Time
}

// Telegram запоминает allowed_updates между вызовами getUpdates. Если когда-то
// бот запускался только с "message", последующий запрос без этого поля НЕ
// возвращает подписку ко всем типам: callback_query продолжает отбрасываться на
// стороне Telegram. Поэтому список задаём явно — иначе инлайн-кнопки выглядят
// нажатыми, но relay вообще не получает событие.
var relayAllowedUpdates = tgbot.AllowedUpdates{"message", "callback_query"}

// New собирает бот; при пустом BOT_TOKEN возвращает nil, nil (mute mode).
func New(cfg *config.Config, d *sql.DB, jwt *auth.Issuer) (*Bot, error) {
	if cfg.BotToken == "" {
		return nil, nil
	}
	b := &Bot{
		cfg: cfg, db: d, jwt: jwt,
		answered:      make(map[answeredKey]time.Time),
		replyTargets:  make(map[answeredKey]chatReplyTarget),
		replyLatest:   make(map[int64]answeredKey),
		supportDrafts: make(map[answeredKey]supportDraft),
	}
	opts := []tgbot.Option{
		tgbot.WithDefaultHandler(b.defaultHandler),
		tgbot.WithAllowedUpdates(relayAllowedUpdates),
	}
	tg, err := tgbot.New(cfg.BotToken, opts...)
	if err != nil {
		return nil, err
	}
	tg.RegisterHandler(tgbot.HandlerTypeMessageText, "/start", tgbot.MatchTypePrefix, b.cmdStart)
	tg.RegisterHandler(tgbot.HandlerTypeMessageText, "/devices", tgbot.MatchTypeExact, b.cmdDevices)
	tg.RegisterHandler(tgbot.HandlerTypeMessageText, "/pc", tgbot.MatchTypeExact, b.cmdDevices)
	tg.RegisterHandler(tgbot.HandlerTypeMessageText, "/term", tgbot.MatchTypeExact, b.cmdTerm)
	tg.RegisterHandler(tgbot.HandlerTypeMessageText, "/run", tgbot.MatchTypePrefix, b.cmdRun)
	tg.RegisterHandler(tgbot.HandlerTypeMessageText, "/help", tgbot.MatchTypeExact, b.cmdHelp)
	tg.RegisterHandler(tgbot.HandlerTypeMessageText, "/notify", tgbot.MatchTypeExact, b.cmdNotify)
	tg.RegisterHandler(tgbot.HandlerTypeMessageText, "/support", tgbot.MatchTypePrefix, b.cmdSupport)
	tg.RegisterHandler(tgbot.HandlerTypeCallbackQueryData, "pair_prompt", tgbot.MatchTypeExact, b.cbPairPrompt)
	// Все инлайн-кнопки регистрируем явно: пейринг и ответы на уведомления
	// должны иметь собственный проверяемый маршрут, а не зависеть от fallback.
	tg.RegisterHandler(tgbot.HandlerTypeCallbackQueryData, callbackPtyInput, tgbot.MatchTypePrefix, b.cbPtyInput)
	tg.RegisterHandler(tgbot.HandlerTypeCallbackQueryData, callbackNotifyPref, tgbot.MatchTypePrefix, b.cbNotifyPref)
	tg.RegisterHandler(tgbot.HandlerTypeCallbackQueryData, callbackSupportSend, tgbot.MatchTypePrefix, b.cbSupportSend)
	b.b = tg
	return b, nil
}

// Start запускает polling; блокирующий вызов, до отмены ctx.
func (b *Bot) Start(ctx context.Context) error {
	if b == nil || b.b == nil {
		return errors.New("bot not configured")
	}
	if err := b.setupMenu(ctx); err != nil {
		log.Printf("[BOT] setMenu failed (non-fatal): %v", err)
	}
	log.Printf("[BOT] starting polling for @%s", b.cfg.BotUsername)
	b.b.Start(ctx)
	return nil
}

// SendMessage отправляет текст в произвольный чат. Используется сервером для
// уведомлений админу о новых сообщениях поддержки (ADMIN_CHAT_ID).
func (b *Bot) SendMessage(ctx context.Context, chatID int64, text string) error {
	if b == nil || b.b == nil {
		return errors.New("bot not configured")
	}
	text, _ = clampTG(text)
	_, err := b.b.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text})
	return err
}

// SendDocument отправляет файл облачному пользователю. Поток приходит с
// временного файла релея: байты забираются с конкретного ПК чанками и не
// требуют локального own_bot на самом компьютере.
func (b *Bot) SendDocument(ctx context.Context, chatID int64, filename string, r io.Reader) error {
	if b == nil || b.b == nil {
		return errors.New("bot not configured")
	}
	_, err := b.b.SendDocument(ctx, &tgbot.SendDocumentParams{
		ChatID: chatID,
		Document: &models.InputFileUpload{
			Filename: filename,
			Data:     r,
		},
	})
	return err
}

func (b *Bot) setupMenu(ctx context.Context) error {
	// Список команд регистрируем ВСЕГДА: раньше пустой MINIAPP_URL выходил из
	// функции раньше SetMyCommands, и бот оставался вообще без команд в меню.
	if b.cfg.MiniAppURL != "" {
		if _, err := b.b.SetChatMenuButton(ctx, &tgbot.SetChatMenuButtonParams{
			MenuButton: models.MenuButtonWebApp{
				Type:   "web_app",
				Text:   "Открыть",
				WebApp: models.WebAppInfo{URL: b.miniAppURL()},
			},
		}); err != nil {
			return err
		}
	}
	_, err := b.b.SetMyCommands(ctx, &tgbot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "start", Description: "Главное меню"},
			{Command: "pc", Description: "Мои компьютеры"},
			{Command: "term", Description: "Терминалы на компьютерах"},
			{Command: "run", Description: "Выполнить команду на ПК"},
			{Command: "notify", Description: "Уведомления о вопросах агента"},
			{Command: "support", Description: "Написать в поддержку"},
			{Command: "help", Description: "Справка"},
		},
	})
	return err
}

func (b *Bot) miniAppKeyboard() models.ReplyMarkup {
	if b.cfg.MiniAppURL == "" {
		return nil
	}
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{{Text: "🖥 Открыть Remotai", WebApp: &models.WebAppInfo{URL: b.miniAppURL()}}},
		},
	}
}

func (b *Bot) cmdStart(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	args := strings.Fields(upd.Message.Text)
	if len(args) >= 2 && strings.HasPrefix(args[1], "pair_") {
		code := strings.TrimPrefix(args[1], "pair_")
		b.handlePairCode(ctx, upd, code)
		return
	}
	if len(args) >= 2 && strings.HasPrefix(args[1], "login_") {
		nonce := strings.TrimPrefix(args[1], "login_")
		b.handleLoginCode(ctx, upd, nonce)
		return
	}
	if len(args) >= 2 && args[1] == "admin" {
		b.handleAdminStart(ctx, tg, upd)
		return
	}
	// /start src_<метка> — переход по рекламной ссылке на бота: запоминаем
	// UTM-источник (первое касание), дальше — обычное приветствие.
	if len(args) >= 2 && strings.HasPrefix(args[1], "src_") {
		b.saveStartSource(ctx, upd, strings.TrimPrefix(args[1], "src_"))
	}
	// В /help /start заявлен «главным меню», а показывал онбординг «скачайте
	// программу» даже владельцу трёх подключённых ПК. Меню — только тем, у кого
	// уже есть компьютеры; инструкция — тем, у кого их нет.
	if devices := b.userDevices(ctx, upd); len(devices) > 0 {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID:      upd.Message.Chat.ID,
			Text:        b.menuText(devices),
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: b.miniAppKeyboard(),
		})
		return
	}
	b.send(ctx, tg, &tgbot.SendMessageParams{
		ChatID:    upd.Message.Chat.ID,
		Text:      welcomeText(),
		ParseMode: models.ParseModeHTML,
		ReplyMarkup: &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "🔗 Подключить ПК", CallbackData: "pair_prompt"}},
				{{Text: "🖥 Открыть Remotai", WebApp: &models.WebAppInfo{URL: b.miniAppURL()}}},
			},
		},
	})
}

// userDevices — компьютеры этого человека (владеемые + допущенные грантом).
// Ошибки БД и «аккаунта ещё нет» = пустой список: обе ветки ведут в онбординг.
func (b *Bot) userDevices(ctx context.Context, upd *models.Update) []*db.Device {
	if upd == nil || upd.Message == nil || upd.Message.From == nil {
		return nil
	}
	u, err := db.GetUserByTelegram(ctx, b.db, upd.Message.From.ID)
	if err != nil {
		return nil
	}
	devices, err := db.ListDevicesForUser(ctx, b.db, u.ID)
	if err != nil {
		return nil
	}
	return devices
}

// menuText — главное меню для человека, у которого ПК уже подключены. Команды
// в тексте Telegram делает нажимаемыми, поэтому отдельные кнопки под каждую не
// нужны (инлайн-кнопка не умеет «отправить команду за меня»).
func (b *Bot) menuText(devices []*db.Device) string {
	online := 0
	for _, d := range devices {
		if b.IsOnline != nil && b.IsOnline(d.ID) {
			online++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🖥 <b>%s</b>, из них в сети: %d\n\n", plural(len(devices), "компьютер", "компьютера", "компьютеров"), online)
	sb.WriteString("/pc — мои компьютеры и их статус\n")
	sb.WriteString("/term — терминалы на компьютерах\n")
	sb.WriteString("/run &lt;команда&gt; — выполнить на ПК\n")
	sb.WriteString("/notify — уведомления в этом чате\n")
	sb.WriteString("/support — написать в поддержку\n\n")
	sb.WriteString("Код с экрана компьютера тоже присылайте сюда — добавлю ещё один ПК.")
	return sb.String()
}

// plural — «1 компьютер / 2 компьютера / 5 компьютеров» с самим числом.
func plural(n int, one, few, many string) string {
	word := many
	switch {
	case n%10 == 1 && n%100 != 11:
		word = one
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 10 || n%100 >= 20):
		word = few
	}
	return fmt.Sprintf("%d %s", n, word)
}

// handleAdminStart выдаёт защищённую Web App-кнопку только allowlisted админу.
// Открытая через неё /admin получает Telegram initData, которое relay затем
// проверяет по BOT_TOKEN и ADMIN_IDS — BotFather Login Widget domain не нужен.
func (b *Bot) handleAdminStart(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	userID := upd.Message.From.ID
	allowed := false
	for _, id := range b.cfg.AdminIDs {
		if userID == id {
			allowed = true
			break
		}
	}
	if !allowed {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: upd.Message.Chat.ID,
			Text:   "Нет доступа к админ-кабинету.",
		})
		return
	}
	adminURL := strings.TrimRight(b.cfg.PublicURL, "/") + "/admin"
	b.send(ctx, tg, &tgbot.SendMessageParams{
		ChatID: upd.Message.Chat.ID,
		Text:   "Админ-кабинет Remotai:",
		ReplyMarkup: &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "Открыть админку", WebApp: &models.WebAppInfo{URL: adminURL}}},
			},
		},
	})
}

func (b *Bot) cmdDevices(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	uid := upd.Message.From.ID
	chatID := upd.Message.Chat.ID
	threadID := upd.Message.MessageThreadID
	u, err := db.GetUserByTelegram(ctx, b.db, uid)
	if err != nil {
		b.send(ctx, tg, &tgbot.SendMessageParams{ChatID: chatID, MessageThreadID: threadID, Text: "У вас ещё нет подключённых компьютеров."})
		return
	}
	devs, err := db.ListDevicesForUser(ctx, b.db, u.ID) // владеемые + допущенные грантом
	if err != nil || len(devs) == 0 {
		b.send(ctx, tg, &tgbot.SendMessageParams{ChatID: chatID, MessageThreadID: threadID, Text: "У вас ещё нет подключённых компьютеров.\nНажмите /start, чтобы добавить."})
		return
	}
	var sb strings.Builder
	sb.WriteString("<b>Ваши компьютеры:</b>\n")
	var rows [][]models.InlineKeyboardButton
	// Список ограничиваем: грантов может быть больше тарифного лимита, а
	// сообщение длиннее 4096 символов Telegram просто не примет.
	const maxRows = 30
	for i, d := range devs {
		if i == maxRows {
			sb.WriteString(fmt.Sprintf("…и ещё %d — весь список в приложении.\n", len(devs)-maxRows))
			break
		}
		online := b.IsOnline != nil && b.IsOnline(d.ID)
		state := "⚫ не в сети"
		if online {
			state = "🟢 в сети"
		} else if d.LastSeenAt.Valid {
			state = "⚫ был " + shortAgo(d.LastSeenAt.Time)
		}
		sb.WriteString(fmt.Sprintf("• <b>%s</b> · %s · %s\n", esc(d.Name), esc(d.Platform), state))
		if url := b.deviceDeepLink(d.ID); url != "" {
			rows = append(rows, []models.InlineKeyboardButton{{
				Text:   "Открыть · " + truncateRunes(d.Name, 32),
				WebApp: &models.WebAppInfo{URL: url},
			}})
		}
	}
	var markup models.ReplyMarkup
	if len(rows) > 0 {
		markup = &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	} else {
		markup = b.miniAppKeyboard()
	}
	b.send(ctx, tg, &tgbot.SendMessageParams{
		ChatID: chatID, MessageThreadID: threadID, Text: sb.String(), ParseMode: models.ParseModeHTML,
		ReplyMarkup: markup,
	})
}

func (b *Bot) cmdHelp(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	b.send(ctx, tg, &tgbot.SendMessageParams{
		ChatID:    upd.Message.Chat.ID,
		ParseMode: models.ParseModeHTML,
		Text: `<b>Remotai</b>

Команды:
/start — главное меню
/pc — список ПК и их онлайн-статус
/term — активные терминалы на всех ПК
/run &lt;текст&gt; — выполнить команду на первом доступном ПК
/notify — уведомления, когда AI-агент ждёт ответа
/support &lt;текст&gt; — написать в поддержку Remotai

Когда придёт «агент ждёт ответа», можно ответить прямо в этом чате: нажать кнопку или написать ответ текстом.

Чтобы подключить новый ПК:
1. Установите Remotai на компьютер — <a href="https://remotai.ru">remotai.ru</a>.
2. Программа покажет код подключения.
3. Введите этот код в этом чате.

Сайт: https://remotai.ru`,
	})
}

func (b *Bot) defaultHandler(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	if upd.CallbackQuery != nil {
		// Известные callback_data регистрируются отдельными хендлерами в New.
		// Неизвестную кнопку всё равно ACK-аем, чтобы Telegram не держал спиннер.
		b.answerCB(ctx, tg, upd.CallbackQuery.ID)
		return
	}
	if upd.Message == nil {
		return
	}
	// Фото (скриншот QR) декодировать нечем — но молчать нельзя: раньше бот на
	// фото не отвечал вообще, и человек ждал реакции.
	if upd.Message.Text == "" {
		if len(upd.Message.Photo) > 0 || upd.Message.Document != nil {
			b.sendFallback(ctx, tg, upd.Message.Chat.ID,
				"QR-код с картинки я прочитать не могу. Пришлите 8 символов кода с экрана компьютера — например <code>FX42-9KQ7</code>.")
		}
		return
	}
	text := upd.Message.Text
	// Неизвестные команды (/чтото) агенту не отправляем никогда — ни как ответ на
	// уведомление, ни как обычную фразу.
	command := startsWithCommand(upd.Message)
	// 1) Ответ агенту reply'ем на конкретное уведомление — самый явный адресат,
	//    важнее даже похожего на код текста: агент вполне мог спросить про код.
	if !command && upd.Message.ReplyToMessage != nil {
		if b.routeTextToAgent(ctx, tg, upd, text, upd.Message.ReplyToMessage.ID) {
			return
		}
	}
	// 2) Код может прийти внутри фразы («мой код FX42-9KQ7», «вот: fx429kq7»):
	//    раньше требовалось сообщение, состоящее ровно из кода, а всё остальное
	//    оставалось без ответа.
	if code, ok := extractCode(text); ok {
		b.handlePairCode(ctx, upd, code)
		return
	}
	// 3) Обычная фраза, а последним я писал «агент ждёт ответа» — это ответ ему.
	if !command && b.routeTextToAgent(ctx, tg, upd, text, 0) {
		return
	}
	b.sendUnknown(ctx, tg, upd, text)
}

// startsWithCommand — сообщение начинается с команды боту. Смотрим сущность
// bot_command от Telegram, а не первый «/»: ответ агенту вполне может быть путём
// вида /home/user/project, и такой текст обязан дойти до терминала.
func startsWithCommand(msg *models.Message) bool {
	for _, e := range msg.Entities {
		if e.Type == models.MessageEntityTypeBotCommand && e.Offset == 0 {
			return true
		}
	}
	return false
}

// sendUnknown — ответ на сообщение, которое не оказалось ни кодом, ни ответом
// агенту. Раньше здесь у ВСЕХ был онбординг «пришлите код подключения» — его
// получал и владелец трёх ПК, написавший «после обновления терминал не
// подключается». Теперь: у кого ПК есть — меню, у кого нет — инструкция, и в
// обоих случаях кнопка «отправить это в поддержку» (жалоба не должна пропадать).
func (b *Bot) sendUnknown(ctx context.Context, tg *tgbot.Bot, upd *models.Update, text string) {
	chatID := upd.Message.Chat.ID
	devices := b.userDevices(ctx, upd)
	var sb strings.Builder
	if len(devices) > 0 {
		sb.WriteString("Не понял сообщение — вот что я умею:\n\n")
		sb.WriteString(b.menuText(devices))
	} else {
		sb.WriteString("Не понял сообщение. Пришлите код подключения с экрана компьютера " +
			"(8 символов, например <code>FX42-9KQ7</code>) — или откройте приложение кнопкой ниже.")
	}
	params := &tgbot.SendMessageParams{
		ChatID: chatID, Text: sb.String(), ParseMode: models.ParseModeHTML,
	}
	rows := b.supportOfferRows(chatID, upd.Message.ID, text)
	if kb, ok := b.miniAppKeyboard().(*models.InlineKeyboardMarkup); ok && kb != nil {
		rows = append(kb.InlineKeyboard, rows...)
	}
	if len(rows) > 0 {
		params.ReplyMarkup = &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	b.send(ctx, tg, params)
}

// routeTextToAgent отдаёт свободный текст агенту, который ждёт ответа.
// false — адресата нет (или он несвежий), и сообщение разбирается как обычно.
func (b *Bot) routeTextToAgent(ctx context.Context, tg *tgbot.Bot, upd *models.Update, text string, replyToMsgID int) bool {
	chatID := upd.Message.Chat.ID
	key, target, ok := b.lookupReplyTarget(chatID, replyToMsgID)
	if !ok || b.AgentRequest == nil {
		return false
	}
	answer := strings.TrimRight(text, " \t\r\n")
	if answer == "" {
		return false
	}
	openKeyboard := b.ptyKeyboard(target.deviceID, target.ptyID, "⌨️ Открыть терминал")
	reply := func(body string) {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID, Text: body, ParseMode: models.ParseModeHTML,
			ReplyMarkup: openKeyboard,
		})
	}
	if strings.ContainsAny(answer, "\n\r") {
		// Каждый перевод строки в терминале — это Enter: многострочная вставка
		// выполнила бы несколько команд вместо одного ответа.
		reply("Отправлю в терминал только одну строку — многострочный текст лучше вставить в самом терминале.")
		return true
	}
	if len([]byte(answer)) > ptyTextMaxBytes {
		reply("Слишком длинный ответ для терминала — максимум 4000 байт.")
		return true
	}
	u, err := db.GetUserByTelegram(ctx, b.db, upd.Message.From.ID)
	if err != nil {
		return false
	}
	// Резерв общий с кнопками: под одним вопросом отвечают один раз, иначе второй
	// ответ уедет в СЛЕДУЮЩИЙ вопрос агента.
	if !b.claimAnswerKey(key) {
		reply("На этот вопрос уже отвечали — откройте терминал и посмотрите, что там сейчас.")
		return true
	}
	res := b.sendPtyText(ctx, u.ID, target, answer)
	switch {
	case res.ok:
		b.forgetReplyTarget(key)
		metrics.AgentReply("ok")
		ptyName := target.ptyName
		if ptyName == "" {
			ptyName = "без имени"
		}
		device := target.deviceName
		if device == "" {
			device = "компьютер"
		}
		reply(fmt.Sprintf("✅ Отправлено агенту в терминал «<b>%s</b>» · %s", esc(ptyName), esc(device)))
	case res.unclear:
		// Исход неизвестен: резерв НЕ снимаем, иначе повтор отправит второй ответ.
		metrics.AgentReply("error")
		reply(esc(res.message))
	default:
		b.releaseAnswerKey(key)
		metrics.AgentReply("denied")
		reply(esc(res.message))
	}
	return true
}

func (b *Bot) cbPairPrompt(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	if upd.CallbackQuery == nil {
		return
	}
	b.answerCB(ctx, tg, upd.CallbackQuery.ID)
	b.send(ctx, tg, &tgbot.SendMessageParams{
		ChatID:    upd.CallbackQuery.From.ID,
		Text:      "Введите 8-значный код с экрана компьютера (например <code>FX42-9KQ7</code>):",
		ParseMode: models.ParseModeHTML,
	})
}

// ── Поддержка прямо из чата бота ─────────────────────────────────────────────
//
// В приложении строка «Поддержка в Telegram» ведёт именно сюда, и человек пишет
// боту жалобу. До этого она никуда не попадала: тред создаёт только POST
// /v1/support/messages из мини-аппа, а бот отвечал «пришлите код подключения» —
// человек был уверен, что обратился в поддержку, и ждал ответа. Теперь бот
// пишет в тот же support_threads и уведомляет админа.

// supportDraftTTL — сколько живёт предложение «отправить это в поддержку».
const supportDraftTTL = 6 * time.Hour

// supportTextMaxRunes — тот же предел, что у POST /v1/support/messages.
const supportTextMaxRunes = 4000

// cmdSupport — /support [текст]: с текстом сразу создаёт обращение, без текста
// объясняет, как написать, и даёт кнопку в чат поддержки приложения.
func (b *Bot) cmdSupport(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil || upd.Message.From == nil {
		return
	}
	chatID := upd.Message.Chat.ID
	text := commandText(upd.Message.Text, "support")
	if text == "" {
		params := &tgbot.SendMessageParams{
			ChatID:    chatID,
			ParseMode: models.ParseModeHTML,
			Text: "Напишите обращение вместе с командой — например:\n" +
				"<code>/support после обновления терминал не подключается</code>\n\n" +
				"Я передам его в поддержку Remotai. Ответ придёт сюда и в чат поддержки в приложении.",
		}
		if url := b.miniAppRoute("/support"); url != "" {
			params.ReplyMarkup = &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{{
					{Text: "💬 Открыть чат поддержки", WebApp: &models.WebAppInfo{URL: url}},
				}},
			}
		}
		b.send(ctx, tg, params)
		return
	}
	if utf8.RuneCountInString(text) > supportTextMaxRunes {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID,
			Text:   "Обращение слишком длинное — не больше 4000 символов.",
		})
		return
	}
	if err := b.postSupportMessage(ctx, upd.Message.From, text); err != nil {
		log.Printf("[SUPPORT] bot: обращение от tg=%d: %v", upd.Message.From.ID, err)
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID,
			Text:   "Не получилось отправить обращение. Попробуйте ещё раз через минуту.",
		})
		return
	}
	b.sendSupportAccepted(ctx, tg, chatID)
}

// sendSupportAccepted — подтверждение принятого обращения (одинаковое для
// /support и для кнопки под непонятым сообщением).
func (b *Bot) sendSupportAccepted(ctx context.Context, tg *tgbot.Bot, chatID int64) {
	params := &tgbot.SendMessageParams{
		ChatID: chatID,
		Text:   "✅ Обращение отправлено в поддержку Remotai. Ответ придёт в этот чат.",
	}
	if url := b.miniAppRoute("/support"); url != "" {
		params.ReplyMarkup = &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "💬 Открыть чат поддержки", WebApp: &models.WebAppInfo{URL: url}},
			}},
		}
	}
	b.send(ctx, tg, params)
}

// supportOfferRows — кнопка «отправить это в поддержку» под непонятым
// сообщением. Сам текст в callback_data не влезает (64 байта), поэтому он ждёт
// подтверждения в памяти под ключом сообщения человека.
func (b *Bot) supportOfferRows(chatID int64, msgID int, text string) [][]models.InlineKeyboardButton {
	body := strings.TrimSpace(text)
	if body == "" || msgID <= 0 || utf8.RuneCountInString(body) > supportTextMaxRunes {
		return nil
	}
	key := answeredKey{chat: chatID, msg: msgID}
	b.supportMu.Lock()
	if b.supportDrafts == nil {
		b.supportDrafts = make(map[answeredKey]supportDraft)
	}
	// Карта растёт по числу непонятых сообщений — чистим редко и оптом.
	if len(b.supportDrafts) > 512 {
		for k, d := range b.supportDrafts {
			if time.Since(d.at) > supportDraftTTL {
				delete(b.supportDrafts, k)
			}
		}
		// Поток свежих сообщений (спам в бота) уборкой по времени не режется —
		// поэтому есть жёсткий предел: память релея важнее старых черновиков.
		if len(b.supportDrafts) > 2048 {
			b.supportDrafts = make(map[answeredKey]supportDraft)
		}
	}
	b.supportDrafts[key] = supportDraft{text: body, at: time.Now()}
	b.supportMu.Unlock()
	return [][]models.InlineKeyboardButton{{
		{Text: "✉️ Отправить это в поддержку", CallbackData: callbackSupportSend + strconv.Itoa(msgID)},
	}}
}

// cbSupportSend — нажали «Отправить это в поддержку» под своим же сообщением.
func (b *Bot) cbSupportSend(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	cq := upd.CallbackQuery
	if cq == nil {
		return
	}
	msgID, convErr := strconv.Atoi(strings.TrimPrefix(cq.Data, callbackSupportSend))
	chatID := cq.From.ID
	if cq.Message.Type == models.MaybeInaccessibleMessageTypeMessage && cq.Message.Message != nil {
		// В группе chat_id ≠ id пользователя, а черновик лежит под чатом сообщения.
		chatID = cq.Message.Message.Chat.ID
	}
	key := answeredKey{chat: chatID, msg: msgID}
	b.supportMu.Lock()
	draft, ok := b.supportDrafts[key]
	if ok {
		delete(b.supportDrafts, key)
	}
	b.supportMu.Unlock()
	if convErr != nil || !ok || time.Since(draft.at) > supportDraftTTL {
		b.answerCBMsg(ctx, tg, cq.ID, "Это сообщение я уже не помню. Напишите /support и текст обращения", true)
		return
	}
	if err := b.postSupportMessage(ctx, &cq.From, draft.text); err != nil {
		log.Printf("[SUPPORT] bot: обращение кнопкой от tg=%d: %v", cq.From.ID, err)
		// Черновик возвращаем: человек нажмёт ещё раз, а не будет писать заново.
		b.supportMu.Lock()
		if b.supportDrafts == nil {
			b.supportDrafts = make(map[answeredKey]supportDraft)
		}
		b.supportDrafts[key] = draft
		b.supportMu.Unlock()
		b.answerCBMsg(ctx, tg, cq.ID, "Не получилось отправить — попробуйте ещё раз", true)
		return
	}
	b.answerCBMsg(ctx, tg, cq.ID, "Отправлено в поддержку", false)
	b.sendSupportAccepted(ctx, tg, chatID)
}

// postSupportMessage кладёт обращение в тот же тред, что и чат поддержки в
// приложении (db.PostUserSupportMessage), и уведомляет админа.
func (b *Bot) postSupportMessage(ctx context.Context, from *models.User, text string) error {
	if from == nil {
		return errors.New("support: неизвестный отправитель")
	}
	u, err := db.UpsertUser(ctx, b.db, from.ID, from.Username, from.FirstName, from.LanguageCode)
	if err != nil {
		return err
	}
	if _, _, err := db.PostUserSupportMessage(ctx, b.db, u.ID, text); err != nil {
		return err
	}
	// Контекст обращения: админке важно видеть, что человек писал из бота, а не из
	// приложения (в приложении meta приходит с версией и режимом связи).
	if err := db.SetSupportThreadMetaIfEmpty(ctx, b.db, u.ID, `{"source":"telegram_bot"}`); err != nil {
		log.Printf("[SUPPORT] bot: сохранить контекст обращения: %v", err)
	}
	b.notifyAdminSupport(ctx, u, text)
	return nil
}

// notifyAdminSupport — то же уведомление, что server.notifyAdminSupport, для
// обращений, пришедших в бот: ADMIN_CHAT_ID должен видеть оба канала.
func (b *Bot) notifyAdminSupport(ctx context.Context, u *db.User, text string) {
	if b.cfg.AdminChatID == 0 || u == nil {
		return
	}
	name := "id " + strconv.FormatInt(u.ID, 10)
	switch {
	case u.Username != "":
		name = "@" + u.Username
	case u.FirstName != "":
		name = u.FirstName
	}
	msg := "🆘 Поддержка (Telegram-бот)\n" + name + ": " + truncateRunes(text, 200) +
		"\nОтветить: " + strings.TrimRight(b.cfg.PublicURL, "/") + "/admin"
	if err := b.SendMessage(ctx, b.cfg.AdminChatID, msg); err != nil {
		log.Printf("[SUPPORT] bot: уведомление админу: %v", err)
	}
}

// sendFallback — ответ на непонятное сообщение с кнопкой в мини-апп.
func (b *Bot) sendFallback(ctx context.Context, tg *tgbot.Bot, chatID int64, text string) {
	params := &tgbot.SendMessageParams{ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML}
	if kb := b.miniAppKeyboard(); kb != nil {
		params.ReplyMarkup = kb
	}
	b.send(ctx, tg, params)
}

// send — единая точка отправки сообщений бота. Раньше почти все ответы уходили
// как `_, _ = tg.SendMessage(...)`: 400 «can't parse entities» на имени ПК с
// «<», 403 от заблокировавшего бота и 429 rate limit не попадали в лог вообще,
// и жалоба «ввёл код — ничего не пришло» была невоспроизводима.
// tg == nil → отправляем через b.b (у части хендлеров нет параметра tg).
func (b *Bot) send(ctx context.Context, tg *tgbot.Bot, params *tgbot.SendMessageParams) {
	if tg == nil {
		tg = b.b
	}
	if tg == nil || params == nil {
		return
	}
	if text, cut := clampTG(params.Text); cut {
		// Обрезанный HTML может остаться с незакрытым тегом — снимаем ParseMode:
		// доставленное сообщение с видимыми тегами лучше, чем 400 и тишина.
		params.Text = text
		params.ParseMode = ""
		log.Printf("[BOT] message to %v truncated to %d runes", params.ChatID, tgMessageLimit)
	}
	if _, err := tg.SendMessage(ctx, params); err != nil {
		log.Printf("[BOT] send chat=%v failed: %v", params.ChatID, err)
	}
}

// answerCB — ACK на инлайн-кнопку: без ответа Telegram крутит спиннер 30 секунд,
// поэтому ошибку тоже надо видеть.
func (b *Bot) answerCB(ctx context.Context, tg *tgbot.Bot, id string) {
	if tg == nil {
		tg = b.b
	}
	if tg == nil {
		return
	}
	if _, err := tg.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{CallbackQueryID: id}); err != nil {
		log.Printf("[BOT] answer callback %s failed: %v", id, err)
	}
}

// extractCode вытаскивает код подключения из произвольного текста.
func extractCode(text string) (string, bool) {
	// Кандидаты: слова из алфавита кодов длиной 8-9 (с дефисом или без).
	for _, w := range codeWordRe.FindAllString(text, -1) {
		if looksLikeCode(w) {
			return w, true
		}
	}
	return "", false
}

var codeWordRe = regexp.MustCompile(`(?i)[A-Z0-9]{4}-?[A-Z0-9]{4}`)

func (b *Bot) handlePairCode(ctx context.Context, upd *models.Update, code string) {
	uid := upd.Message.From.ID
	chatID := upd.Message.Chat.ID
	normCode := db.NormalizeCode(code)
	// Лимит попыток
	u, err := db.UpsertUser(ctx, b.db, uid, upd.Message.From.Username, upd.Message.From.FirstName, upd.Message.From.LanguageCode)
	if err != nil {
		b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: "Ошибка БД: " + err.Error()})
		return
	}
	if att, _ := db.RecentAttemptsByUser(ctx, b.db, u.ID); att >= b.cfg.PairMaxAttempts {
		metrics.PairFail()
		b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: "Слишком много попыток. Подождите 10 минут."})
		return
	}
	rec, err := db.GetPairingCode(ctx, b.db, normCode)
	if err != nil {
		if errors.Is(err, db.ErrCodeNotFound) {
			metrics.PairFail()
			b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: "❌ Код не найден. Проверьте, что вы ввели правильно."})
			return
		}
		b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: "Ошибка: " + err.Error()})
		return
	}
	// Анти-брутфорс: код сгорел после серии неудачных попыток (db.MaxPairCodeAttempts).
	if rec.Attempts >= db.MaxPairCodeAttempts {
		metrics.PairFail()
		b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: "❌ Код заблокирован после неверных попыток. Получите новый в программе на ПК."})
		return
	}
	// Многоразовый код в пределах TTL — повторный ввод с другого аккаунта = грант.
	if time.Now().UTC().After(rec.ExpiresAt) {
		metrics.PairFail()
		_ = db.IncAttempts(ctx, b.db, normCode) // неудачная попытка (анти-брутфорс)
		b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: "❌ Код устарел. Получите новый в программе на ПК."})
		return
	}

	name := rec.Hostname
	if name == "" {
		name = "Мой ПК"
	}
	dev := &db.Device{
		ID: rec.DeviceID, UserID: u.ID, Name: name,
		Hostname: rec.Hostname, Platform: rec.Platform, AgentVersion: rec.AgentVersion,
	}
	primary, overLimit, err := db.BindDeviceForPairing(ctx, b.db, dev, u.ID, tierMax(b.cfg, u.EffectiveTier(false)))
	if overLimit {
		metrics.PairFail()
		_ = db.IncAttempts(ctx, b.db, normCode) // неудачная попытка (анти-брутфорс)
		// Отвязки в чате нет вообще (в /pc только «Открыть · имя»), поэтому шлём
		// туда, где кнопка «Отвязать» действительно есть — в «Мои компьютеры»
		// приложения. Прежний текст «откройте /pc и отвяжите» врал про место.
		// Маршрут по историческим причинам зовётся /infrastructure, но раздела
		// с таким названием в интерфейсе нет — человеку показываем ровно то имя,
		// которое он увидит на экране, иначе он ищет несуществующий раздел.
		params := &tgbot.SendMessageParams{
			ChatID: chatID, ParseMode: models.ParseModeHTML,
			Text: fmt.Sprintf("⛔ Достигнут лимит компьютеров: %d.\n\nОтвяжите ненужный: откройте приложение → «Мои компьютеры» → компьютер → «Отвязать», затем пришлите код снова.",
				tierMax(b.cfg, u.EffectiveTier(false))),
		}
		if url := b.miniAppRoute("/infrastructure"); url != "" {
			params.ReplyMarkup = &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{{
					{Text: "🖥 Открыть «Мои компьютеры»", WebApp: &models.WebAppInfo{URL: url}},
				}},
			}
		}
		b.send(ctx, nil, params)
		return
	}
	if err != nil {
		metrics.PairFail()
		_ = db.IncAttempts(ctx, b.db, normCode) // неудачная попытка (анти-брутфорс)
		b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: "Не удалось подключить: " + err.Error()})
		return
	}
	// Владелец (первый ввод этого кода) → ПК заберёт device-JWT через status-poll.
	if primary && !rec.ConsumedAt.Valid {
		tok, _, e := b.jwt.IssueDevice(rec.DeviceID, u.ID)
		if e != nil {
			b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: "Ошибка JWT: " + e.Error()})
			return
		}
		if _, e := db.ConsumePairingCode(ctx, b.db, normCode, u.ID, tok); e != nil {
			b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: "Не удалось активировать код: " + e.Error()})
			return
		}
	}
	metrics.PairOK()
	b.send(ctx, nil, &tgbot.SendMessageParams{
		ChatID:      chatID,
		ParseMode:   models.ParseModeHTML,
		Text:        fmt.Sprintf("✅ Компьютер <b>%s</b> подключён!\n\nЖмите кнопку ниже, чтобы открыть Remotai.", esc(name)),
		ReplyMarkup: b.miniAppKeyboard(),
	})
}

// saveStartSource сохраняет UTM-источник из /start src_<метка>: юзер
// приходит из Telegram, поэтому source="tg", а метка уходит в utm_source.
// Проставляется только если источник ещё пуст — первое касание не затираем.
// Ошибки не фатальны: аналитика не должна ломать приветствие.
func (b *Bot) saveStartSource(ctx context.Context, upd *models.Update, tag string) {
	if tag == "" {
		return
	}
	from := upd.Message.From
	u, err := db.UpsertUser(ctx, b.db, from.ID, from.Username, from.FirstName, from.LanguageCode)
	if err != nil {
		log.Printf("[BOT] src_ upsert user: %v", err)
		return
	}
	if _, err := db.SetUserSourceIfEmpty(ctx, b.db, u.ID, "tg", map[string]string{"utm_source": tag}); err != nil {
		log.Printf("[BOT] src_ save source: %v", err)
	}
}

// handleLoginCode подтверждает Telegram-вход: из /start login_<nonce> известен
// telegram_id (Telegram аутентифицировал отправителя), привязываем nonce к
// аккаунту. Клиент (APK/web/exe) затем заберёт durable user-JWT через poll.
func (b *Bot) handleLoginCode(ctx context.Context, upd *models.Update, nonce string) {
	chatID := upd.Message.Chat.ID
	from := upd.Message.From
	u, err := db.UpsertUser(ctx, b.db, from.ID, from.Username, from.FirstName, from.LanguageCode)
	if err != nil {
		b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: "Ошибка БД: " + err.Error()})
		return
	}
	if err := db.ConfirmLoginToken(ctx, b.db, nonce, u.ID); err != nil {
		text := "❌ Ссылка для входа недействительна. Откройте вход в приложении заново и нажмите «Запустить» на новом сообщении бота (не на этом)."
		if errors.Is(err, db.ErrLoginTokenExpired) {
			text = "⌛ Время на вход вышло — ссылка живёт 5 минут. Откройте вход в приложении заново."
		}
		b.send(ctx, nil, &tgbot.SendMessageParams{ChatID: chatID, Text: text})
		return
	}
	b.send(ctx, nil, &tgbot.SendMessageParams{
		ChatID:      chatID,
		ParseMode:   models.ParseModeHTML,
		Text:        "✅ Вход подтверждён! Вернитесь в приложение Remotai — оно продолжит само.",
		ReplyMarkup: b.miniAppKeyboard(),
	})
}

// helpers ----

// tgMessageLimit — жёсткий лимит Telegram на длину текста сообщения.
const tgMessageLimit = 4096

// clampTG режет текст по лимиту Telegram (по рунам, чтобы не порвать UTF-8).
// Второе значение — признак обрезки: вызывающий снимает ParseMode, потому что
// обрезанный HTML почти наверняка невалиден.
func clampTG(s string) (string, bool) {
	r := []rune(s)
	if len(r) <= tgMessageLimit {
		return s, false
	}
	return string(r[:tgMessageLimit-1]) + "…", true
}

func looksLikeCode(s string) bool {
	s = db.NormalizeCode(s)
	if len(s) != 9 {
		return false
	}
	if s[4] != '-' {
		return false
	}
	for i, r := range s {
		if i == 4 {
			continue
		}
		if !strings.ContainsRune("ABCDEFGHJKLMNPQRSTUVWXYZ23456789", r) {
			return false
		}
	}
	return true
}

func welcomeText() string {
	return `👋 <b>Remotai</b>

Управление вашим компьютером прямо из Telegram — терминал, файлы, удалённый рабочий стол, AI-агенты.

Чтобы начать:
1. Скачайте программу с <a href="https://remotai.ru">remotai.ru</a>
2. Запустите её на ПК.
3. Введите код, который вам покажет программа.`
}

// esc экранирует текст для ParseMode HTML. Раньше здесь был EscapeMarkdown,
// из-за чего пользователь видел «DESKTOP\-6PRVSTU», а hostname с «<» ронял
// отправку сообщения (Telegram 400 can't parse entities).
func esc(s string) string { return html.EscapeString(s) }

func tierMax(c *config.Config, tier string) int {
	if c.SelfHosted {
		return 0
	}
	switch tier {
	case "pro":
		return c.ProMaxDevices
	case "team", "fleet":
		return c.TeamMaxDevices
	default:
		return c.FreeMaxDevices
	}
}
