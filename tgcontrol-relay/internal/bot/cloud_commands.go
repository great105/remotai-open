package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/notify"
)

type botPtyList struct {
	Sessions []botPty `json:"sessions"`
}

type botPty struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Shell     string `json:"shell"`
	Alive     bool   `json:"alive"`
	AgentKind string `json:"agent_kind"`
	Status    string `json:"status"`
	StatusAt  int64  `json:"status_at"`
	Hint      string `json:"hint"`
	HintKind  string `json:"hint_kind"`
}

// ClientVersion — версия выпущенного клиента; её ставит main при старте.
// Пустая строка и "dev" означают «метку не добавлять» (тесты, локальный запуск).
var ClientVersion = "dev"

// withClientVersion дописывает к адресу мини-аппа метку версии.
//
// Метка нужна не нам, а ПРОТИВ КЭША КЛИЕНТА. Telegram держит index.html мини-аппа
// у себя, и пока адрес не менялся, выпущенная починка может не доехать до
// человека вовсе. Живой случай 04.08.2026: владелец на iPad третий раз подряд
// присылал скриншот уже исправленного дефекта — его клиент оставался сборкой
// двухдневной давности, и это было видно по тому, что новый клиент шлёт в лог
// строку диагностики, а её не было ни одной. Метка ставится ПЕРЕД hash: маршрут
// в мини-аппе живёт именно там.
func withClientVersion(u string) string {
	if ClientVersion == "" || ClientVersion == "dev" || u == "" {
		return u
	}
	hash := ""
	if i := strings.Index(u, "#"); i >= 0 {
		hash, u = u[i:], u[:i]
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "v=" + url.QueryEscape(ClientVersion) + hash
}

// miniAppURL — адрес мини-аппа для кнопок бота (с меткой версии).
func (b *Bot) miniAppURL() string {
	return withClientVersion(strings.TrimSpace(b.cfg.MiniAppURL))
}

func (b *Bot) miniAppRoute(route string) string {
	base := strings.TrimRight(strings.TrimSpace(b.cfg.MiniAppURL), "/")
	if !strings.HasPrefix(base, "https://") || !strings.HasPrefix(route, "/") {
		return ""
	}
	return withClientVersion(base + "/#" + route)
}

func (b *Bot) deviceDeepLink(deviceID string) string {
	if deviceID == "" {
		return ""
	}
	return b.miniAppRoute("/devices?select=" + url.QueryEscape(deviceID))
}

func urlQueryEscape(value string) string { return url.QueryEscape(value) }

func urlPathEscape(value string) string { return url.PathEscape(value) }

func shortAgo(at time.Time) string {
	d := time.Since(at)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	default:
		return fmt.Sprintf("%d дн назад", int(d.Hours()/24))
	}
}

func truncateRunes(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max-1]) + "…"
}

func (b *Bot) replyTarget(upd *models.Update) (chatID int64, threadID int) {
	if upd == nil || upd.Message == nil {
		return 0, 0
	}
	return upd.Message.Chat.ID, upd.Message.MessageThreadID
}

func (b *Bot) cloudUser(ctx context.Context, upd *models.Update) (*db.User, []*db.Device, error) {
	if upd == nil || upd.Message == nil || upd.Message.From == nil {
		return nil, nil, fmt.Errorf("message user missing")
	}
	u, err := db.GetUserByTelegram(ctx, b.db, upd.Message.From.ID)
	if err != nil {
		return nil, nil, err
	}
	devices, err := db.ListDevicesForUser(ctx, b.db, u.ID)
	return u, devices, err
}

func (b *Bot) cmdTerm(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	chatID, threadID := b.replyTarget(upd)
	if chatID == 0 {
		return
	}
	u, devices, err := b.cloudUser(ctx, upd)
	if err != nil || len(devices) == 0 {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text: "Сначала подключите компьютер: /start.",
		})
		return
	}
	if b.AgentRequest == nil {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text:        "Терминалы временно недоступны. Откройте приложение кнопкой ниже.",
			ReplyMarkup: b.miniAppKeyboard(),
		})
		return
	}

	var text strings.Builder
	text.WriteString("<b>Терминалы:</b>\n")
	var rows [][]models.InlineKeyboardButton
	foundOnline := false
	foundSessions := false
	for _, device := range devices {
		if b.IsOnline == nil || !b.IsOnline(device.ID) {
			continue
		}
		foundOnline = true
		status, raw, reqErr := b.AgentRequest(ctx, u.ID, device.ID, http.MethodGet, "/api/pty?sort=last_active&limit=8", nil)
		if reqErr != nil || status != http.StatusOK {
			fmt.Fprintf(&text, "\n🖥 <b>%s</b> · не ответил\n", esc(device.Name))
			continue
		}
		var list botPtyList
		if json.Unmarshal(raw, &list) != nil {
			continue
		}
		fmt.Fprintf(&text, "\n🖥 <b>%s</b>\n", esc(device.Name))
		if len(list.Sessions) == 0 {
			text.WriteString("• пока нет терминалов\n")
			continue
		}
		for _, session := range list.Sessions {
			foundSessions = true
			label := session.Name
			if label == "" {
				label = path.Base(strings.ReplaceAll(session.Shell, "\\", "/"))
			}
			if label == "." || label == "/" || label == "" {
				label = "Терминал"
			}
			state := "готов"
			switch {
			case !session.Alive:
				state = "завершён"
			case session.Status == "waiting":
				state = "ждёт ответа"
			case session.Status == "working":
				state = "работает"
			case session.Status == "error":
				state = "ошибка"
			}
			fmt.Fprintf(&text, "• %s · %s\n", esc(label), state)

			var row []models.InlineKeyboardButton
			if openURL := b.ptyDeepLink(device.ID, session.ID); openURL != "" {
				row = append(row, models.InlineKeyboardButton{
					Text:   "⌨️ " + truncateRunes(label, 28),
					WebApp: &models.WebAppInfo{URL: openURL},
				})
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
			if session.Alive && session.Status == "waiting" {
				reply := b.replyRow(notify.Notice{
					DeviceID: device.ID, PtyID: session.ID,
					HintKind: session.HintKind, StatusAt: session.StatusAt,
				})
				if len(reply) > 0 {
					rows = append(rows, reply)
				}
			}
			if len(rows) >= 20 {
				break
			}
		}
		if len(rows) >= 20 {
			break
		}
	}
	if !foundOnline {
		text.WriteString("\nВсе компьютеры сейчас не в сети.")
	} else if !foundSessions {
		text.WriteString("\nСоздайте терминал в приложении или выполните <code>/run команда</code>.")
	}
	var markup models.ReplyMarkup
	if len(rows) > 0 {
		markup = &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	} else {
		markup = b.miniAppKeyboard()
	}
	b.send(ctx, tg, &tgbot.SendMessageParams{
		ChatID: chatID, MessageThreadID: threadID, Text: text.String(),
		ParseMode: models.ParseModeHTML, ReplyMarkup: markup,
	})
}

func commandText(message, command string) string {
	fields := strings.Fields(message)
	if len(fields) < 2 {
		return ""
	}
	head := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(head, '@'); at >= 0 {
		head = head[:at]
	}
	if !strings.EqualFold(head, command) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(message, fields[0]))
}

// botRunPtyName — имя терминала, который бот держит для /run на каждом ПК.
// Одно на компьютер: раньше каждая команда из чата создавала новый терминал, и
// после десятка /run на ПК висел десяток никем не закрытых сессий (а затем
// приходил отказ по лимиту без объяснения причины).
const botRunPtyName = "Telegram"

// ptyTextMaxBytes — предел одной строки, которую бот пишет в терминал. У ПК свой
// лимит (ptyInputMaxBytes), здесь — чтобы не гонять заведомо отбиваемое тело.
const ptyTextMaxBytes = 4000

// findBotRunPty ищет на ПК терминал бота. busy — терминал есть, но занят: в нём
// идёт команда или живёт AI-агент, и вторая строка ввода смешалась бы с первой.
func (b *Bot) findBotRunPty(ctx context.Context, userID int64, deviceID string) (session *botPty, busy bool, err error) {
	status, raw, reqErr := b.AgentRequest(ctx, userID, deviceID, http.MethodGet, "/api/pty?alive=1&sort=last_active&limit=30", nil)
	if reqErr != nil {
		return nil, false, reqErr
	}
	if status != http.StatusOK {
		return nil, false, fmt.Errorf("список терминалов: статус %d", status)
	}
	var list botPtyList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, false, err
	}
	for i := range list.Sessions {
		s := list.Sessions[i]
		if s.Name != botRunPtyName || !s.Alive {
			continue
		}
		// "working" — в терминале идёт команда, "waiting"/"ready"/"stalled" — там
		// AI-агент. И то, и другое означает: писать туда команду нельзя.
		if s.AgentKind != "" || s.Status == "working" || s.Status == "waiting" || s.Status == "stalled" || s.Status == "ready" {
			return &s, true, nil
		}
		return &s, false, nil
	}
	return nil, false, nil
}

// agentErrorText переводит отказ ПК в человеческую фразу для чата. Разбираем
// машинный code из тела, а не текст: тексты меняются, коды — контракт
// (internal/web/api_pty.go). fallback — когда кода нет вовсе.
func agentErrorText(status int, raw []byte, fallback string) string {
	var e struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	switch e.Code {
	case "pty_limit":
		return "На компьютере достигнут лимит терминалов. Закройте ненужный — и повторите."
	case "pty_not_found":
		return "Этот терминал уже закрыт."
	case "pty_dead":
		return "Терминал завершился — команда не отправлена."
	case "prompt_changed":
		return "Агент уже спрашивает о другом — ответ не отправлен. Откройте терминал и посмотрите новый вопрос."
	case "rate_limited":
		return "Слишком часто — попробуйте через минуту."
	case "too_large":
		return "Слишком длинный текст для терминала."
	case "bad_encoding":
		return "В тексте есть символы, которые терминал не примет."
	case "invalid_cwd":
		return "Рабочая папка недоступна — откройте терминал в приложении и выберите другую."
	case "license_required", "pro_required", "team_required":
		return "Эта возможность недоступна на текущем плане."
	}
	// Текст ошибки от самого ПК уже по-русски (jsonErrorCode в api_pty.go), но
	// доверяем ему только если это не служебная англоязычная фраза net/http.
	if msg := strings.TrimSpace(e.Error); msg != "" && !strings.Contains(msg, "page not found") {
		return msg
	}
	switch status {
	case http.StatusNotFound:
		// Роута на ПК нет вовсе — это старая версия, а не «закрытый терминал».
		return "Обновите Remotai на компьютере: эта версия не понимает команду из чата."
	case http.StatusForbidden:
		return "Компьютер отказал в доступе. Проверьте, что он всё ещё привязан к вашему аккаунту."
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return "Компьютер не отвечает. Включите его и повторите."
	}
	return fallback
}

// ptyTextResult — исход отправки свободного текста в терминал.
type ptyTextResult struct {
	ok bool
	// unclear — исход неизвестен (таймаут, обрыв): ПК мог применить ввод, поэтому
	// повторять нельзя и резерв ответа не снимаем.
	unclear bool
	message string
}

// sendPtyText отдаёт агенту свободный текст из чата. Метка эпизода обязательна:
// с ней ПК не применит ответ к ДРУГОМУ вопросу (409 prompt_changed), то есть
// «отвечаю на то, что видел» гарантирует сам компьютер.
func (b *Bot) sendPtyText(ctx context.Context, userID int64, target chatReplyTarget, text string) ptyTextResult {
	if b.AgentRequest == nil {
		return ptyTextResult{message: "Ответы из чата сейчас недоступны. Откройте терминал в приложении."}
	}
	body, err := json.Marshal(map[string]any{
		"data": text, "enter": true, "expect_status_at": target.statusAt,
	})
	if err != nil {
		return ptyTextResult{message: "Не удалось собрать ответ агенту."}
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, raw, reqErr := b.AgentRequest(cctx, userID, target.deviceID,
		http.MethodPost, "/api/pty/"+url.PathEscape(target.ptyID)+"/input", body)
	if reqErr != nil {
		// Дошло или нет — неизвестно. Утверждать «не отправлено» нельзя: человек
		// напишет ответ ещё раз, и агент получит его дважды.
		return ptyTextResult{unclear: true, message: "Ответ мог не дойти — откройте терминал и проверьте."}
	}
	if status != http.StatusOK {
		return ptyTextResult{message: agentErrorText(status, raw, "Не удалось передать ответ агенту.")}
	}
	return ptyTextResult{ok: true}
}

func (b *Bot) cmdRun(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	chatID, threadID := b.replyTarget(upd)
	if chatID == 0 {
		return
	}
	command := commandText(upd.Message.Text, "run")
	if command == "" {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text:      "Напишите команду после <code>/run</code>.\nНапример: <code>/run git status</code>",
			ParseMode: models.ParseModeHTML,
		})
		return
	}
	if len([]byte(command)) > ptyTextMaxBytes {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text: "Команда слишком длинная — максимум 4000 байт.",
		})
		return
	}
	u, devices, err := b.cloudUser(ctx, upd)
	if err != nil || len(devices) == 0 {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text: "Сначала подключите компьютер: /start.",
		})
		return
	}
	var target *db.Device
	for _, device := range devices {
		if b.IsOnline != nil && b.IsOnline(device.ID) {
			target = device
			break
		}
	}
	if target == nil || b.AgentRequest == nil {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text: "Нет компьютера в сети. Включите ПК и повторите /run.",
		})
		return
	}

	// Переиспользуем СВОЙ терминал: «Telegram» на этом ПК. Раньше каждая команда
	// создавала новый, и человек, погоняв команды из чата, получал на ПК десяток
	// живых сессий и отказ по лимиту.
	existing, busy, findErr := b.findBotRunPty(ctx, u.ID, target.ID)
	if findErr != nil {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text: "Компьютер «" + target.Name + "» не ответил на запрос терминалов. Повторите /run.",
		})
		return
	}
	if busy && existing != nil {
		// Вторая строка ввода в занятый терминал ушла бы в stdin работающей
		// программы (или в чат AI-агента) — это не запуск команды, а порча ввода.
		what := "выполняется предыдущая команда"
		if existing.AgentKind != "" {
			what = "работает " + agentRunLabel(existing.AgentKind)
		}
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text: fmt.Sprintf("⏳ В терминале «<b>%s</b>» на <b>%s</b> %s — команда не отправлена.\nПосмотрите вывод кнопкой ниже или дождитесь конца.",
				esc(botRunPtyName), esc(target.Name), esc(what)),
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: b.ptyKeyboard(target.ID, existing.ID, "⌨️ Смотреть вывод"),
		})
		return
	}

	ptyID, reused := "", false
	if existing != nil {
		ptyID, reused = existing.ID, true
	} else {
		createBody, _ := json.Marshal(map[string]any{"cols": 100, "rows": 30})
		status, raw, reqErr := b.AgentRequest(ctx, u.ID, target.ID, http.MethodPost, "/api/pty", createBody)
		if reqErr != nil {
			b.send(ctx, tg, &tgbot.SendMessageParams{
				ChatID: chatID, MessageThreadID: threadID,
				Text: "Компьютер «" + target.Name + "» не ответил. Повторите /run.",
			})
			return
		}
		if status != http.StatusOK {
			// Причину ПК присылает машинным кодом (например pty_limit) — до этого
			// она терялась, и человек читал «Не удалось создать терминал» без выхода.
			b.send(ctx, tg, &tgbot.SendMessageParams{
				ChatID: chatID, MessageThreadID: threadID,
				Text: "Не удалось создать терминал на компьютере «" + esc(target.Name) + "».\n" +
					esc(agentErrorText(status, raw, "Компьютер отказал без объяснения причины.")),
				ParseMode:   models.ParseModeHTML,
				ReplyMarkup: b.deviceRouteKeyboard(target.ID, "/pty", "⌨️ Открыть терминалы"),
			})
			return
		}
		var created struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &created) != nil || created.ID == "" {
			b.send(ctx, tg, &tgbot.SendMessageParams{
				ChatID: chatID, MessageThreadID: threadID,
				Text: "Компьютер вернул некорректный ответ. Обновите Remotai.",
			})
			return
		}
		ptyID = created.ID
		// Имя — чтобы следующий /run нашёл этот же терминал (и человек в списке
		// видел, откуда он взялся). Не критично: неудача лишь заведёт второй.
		nameBody, _ := json.Marshal(map[string]any{"name": botRunPtyName})
		if st, _, err := b.AgentRequest(ctx, u.ID, target.ID, http.MethodPatch,
			"/api/pty/"+url.PathEscape(ptyID), nameBody); err != nil || st != http.StatusOK {
			log.Printf("[BOT] /run: не удалось назвать терминал device=%s pty=%s status=%d err=%v",
				target.ID, ptyID, st, err)
		}
	}

	inputBody, _ := json.Marshal(map[string]any{"data": command, "enter": true})
	status, raw, reqErr := b.AgentRequest(ctx, u.ID, target.ID, http.MethodPost, "/api/pty/"+url.PathEscape(ptyID)+"/input", inputBody)
	if reqErr != nil || status != http.StatusOK {
		reason := "Компьютер не ответил."
		if reqErr == nil {
			reason = agentErrorText(status, raw, "Компьютер отказал без объяснения причины.")
		}
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text:        "Команду отправить не удалось: " + esc(reason) + "\nОткройте терминал кнопкой ниже.",
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: b.ptyKeyboard(target.ID, ptyID, "⌨️ Открыть терминал"),
		})
		return
	}
	text := fmt.Sprintf("✅ Команда запущена в терминале «<b>%s</b>» на <b>%s</b>.", esc(botRunPtyName), esc(target.Name))
	if reused {
		text += "\n<i>Это тот же терминал, что и в прошлый раз — новых на ПК не плодим.</i>"
	}
	if len(devices) > 1 {
		text += "\n<i>Выбран первый компьютер в сети. Другой ПК можно открыть через /pc.</i>"
	}
	b.send(ctx, tg, &tgbot.SendMessageParams{
		ChatID: chatID, MessageThreadID: threadID, Text: text,
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: b.ptyKeyboard(target.ID, ptyID, "⌨️ Смотреть вывод"),
	})
}

// agentRunLabel — подпись агента для фразы «работает Claude». Незнакомый вид
// агента показываем как «AI-агент», а не сырым идентификатором.
func agentRunLabel(kind string) string {
	if label := agentLabel(kind); label != "" {
		return label
	}
	return "AI-агент"
}

// deviceRouteKeyboard — кнопка «открыть раздел приложения на этом ПК»: маршрут
// сначала переключает облачный контекст на нужный компьютер (см. ptyDeepLink).
func (b *Bot) deviceRouteKeyboard(deviceID, route, label string) models.ReplyMarkup {
	target := b.miniAppRoute("/devices?select=" + urlQueryEscape(deviceID) + "&next=" + urlQueryEscape(route))
	if deviceID == "" {
		target = b.miniAppRoute(route)
	}
	if target == "" {
		return nil
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		{Text: label, WebApp: &models.WebAppInfo{URL: target}},
	}}}
}

func (b *Bot) ptyKeyboard(deviceID, ptyID, label string) models.ReplyMarkup {
	deepLink := b.ptyDeepLink(deviceID, ptyID)
	if deepLink == "" {
		return nil
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		{Text: label, WebApp: &models.WebAppInfo{URL: deepLink}},
	}}}
}
