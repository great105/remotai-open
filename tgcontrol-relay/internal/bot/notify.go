package bot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/metrics"
	"tgcontrol-relay/internal/notify"
)

// Уведомление «AI-агент ждёт ответа» и ответ прямо из чата.
//
// Сообщение формирует релейный notifier (server/notifier.go) и отдаёт сюда как
// notify.Notice. Здесь — только текст, клавиатура и обработка нажатий.

// callbackPtyInput / callbackNotifyPref — префиксы callback_data. Формат ответа:
// "pi:<device>:<pty>:<key>" ≈ 38 байт при 12-hex device и 16-hex pty (лимит
// Telegram — 64 байта, имена ПК в data класть нельзя).
const (
	callbackPtyInput    = "pi:"
	callbackNotifyPref  = "nt:"
	callbackSupportSend = "sp:"
	callbackDataLimit   = 64
)

// Ключи тумблеров /notify внутри callback_data: "nt:<что>:<on|off>". Старый
// формат "nt:on" / "nt:off" остаётся рабочим — сообщения /notify живут в истории
// чата вечно, и нажатие в прошлогоднем сообщении обязано что-то делать.
const (
	notifyPrefQuestions = "q"
	notifyPrefErrors    = "e"
)

// agentLabels — те же подписи агентов, что в клиенте (apk/src/notifications.ts).
var agentLabels = map[string]string{
	"claude":       "Claude",
	"codex":        "Codex",
	"gemini":       "Gemini",
	"kimi":         "Kimi",
	"aider":        "Aider",
	"opencode":     "OpenCode",
	"copilot":      "Copilot",
	"cursor-agent": "Cursor",
	"cline":        "Cline",
	"kilo":         "Kilo",
	"amazon-q":     "Amazon Q",
}

func agentLabel(kind string) string {
	if l, ok := agentLabels[kind]; ok {
		return l
	}
	return ""
}

// SendAgentWaiting пишет пользователю в личный чат о том, что агент ждёт ответа
// (или что в терминале ошибка). Вызывается сервером через колбэк UserNotify.
func (b *Bot) SendAgentWaiting(ctx context.Context, chatID int64, n notify.Notice) error {
	if b == nil || b.b == nil {
		return errors.New("bot not configured")
	}
	// Ошибка в терминале — отдельный повод со своим выключателем: триггер широкий
	// (любая строка, начинающаяся с error/FAIL/Traceback), текста ошибки в
	// событии нет, а гасить его вместе с вопросами агента нельзя — ради вопросов
	// канал и существует. По умолчанию выключено (миграция 0019).
	// Возвращаем nil, а не ошибку: это осознанное молчание, а не сбой доставки
	// (notifier посчитал бы его "failed" и залил лог). Цена — молчание попадает в
	// метрику как "sent"; честнее было бы гасить ошибки ещё в notifier рядом с
	// проверкой NotifyEnabled, но там же придётся читать и этот флаг.
	if n.Kind == notify.KindError && !b.errorNoticesEnabled(ctx, chatID) {
		return nil
	}
	text, cut := clampTG(noticeText(n))
	params := &tgbot.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: b.noticeKeyboard(n),
	}
	if cut {
		// Обрезанный HTML почти наверняка невалиден — как и в b.send, лучше
		// доставить сообщение с видимыми тегами, чем получить 400 и тишину.
		params.ParseMode = ""
	}
	msg, err := b.b.SendMessage(ctx, params)
	if err != nil {
		// Бот заблокирован пользователем — уведомления выключаем сами, иначе
		// релей будет биться в стену на каждом вопросе агента.
		if isBlockedByUser(err) {
			b.disableNotifyForChat(ctx, chatID, err)
		}
		return err
	}
	// Свободный текст, присланный в чат, уходит В ЭТОТ терминал: тип вопроса
	// «text» кнопками не ответить, а «да» в чате раньше получало инструкцию про
	// код подключения (см. rememberReplyTarget и defaultHandler).
	b.rememberReplyTarget(chatID, msg, n)
	return nil
}

// errorNoticesEnabled — включены ли у этого чата сообщения про ошибки в
// терминале. Ошибку чтения трактуем как «выключено», как и notifier для главного
// флага: молчание дешевле неожиданной ночной рассылки.
func (b *Bot) errorNoticesEnabled(ctx context.Context, chatID int64) bool {
	u, err := db.GetUserByTelegram(ctx, b.db, chatID)
	if err != nil {
		return false
	}
	on, err := db.NotifyErrorsEnabled(ctx, b.db, u.ID)
	if err != nil {
		log.Printf("[NOTIFY] чтение флага ошибок user=%d: %v", u.ID, err)
		return false
	}
	return on
}

// noticeText — тело сообщения. Всё пользовательское (имя ПК, имя терминала,
// текст вопроса) проходит через esc: hostname с «<» роняет отправку (Telegram
// 400 can't parse entities).
func noticeText(n notify.Notice) string {
	title := agentLabel(n.Agent)
	if title == "" {
		title = n.FgProcess
	}
	if title == "" {
		title = "Терминал"
	}
	ptyName := n.PtyName
	if ptyName == "" {
		ptyName = "без имени"
	}
	device := n.DeviceName
	if device == "" {
		device = "компьютер"
	}

	var sb strings.Builder
	if n.Kind == notify.KindError {
		// Формулировка ровно такой силы, какая есть у повода: триггер ловит
		// строку с error/FAIL/Traceback — то есть с некоторой вероятностью и
		// обычный красный вывод тестов. Прежнее «⚠️ Ошибка в терминале» обещало
		// знание, которого у сообщения нет.
		fmt.Fprintf(&sb, "⚠️ В терминале «<b>%s</b>» мелькнула строка, похожая на ошибку\n", esc(ptyName))
		fmt.Fprintf(&sb, "🖥 <b>%s</b> · %s", esc(device), esc(title))
		hint := strings.TrimSpace(n.Hint)
		if hint != "" {
			fmt.Fprintf(&sb, "\n\n%s", esc(hint))
		}
		if hint == "" {
			// Оговорка только когда строки правда нет (совсем старый агент):
			// печатать «текста ошибки у меня нет» СРАЗУ ПОД этим текстом —
			// значит противоречить самому себе в одном сообщении.
			sb.WriteString("\n\n<i>Самого текста ошибки у меня нет — он виден только в терминале.</i>")
		}
		sb.WriteString("\n\n<i>Не нужны такие сообщения — /notify.</i>")
	} else {
		fmt.Fprintf(&sb, "⏳ <b>%s</b> ждёт ответа\n", esc(title))
		fmt.Fprintf(&sb, "🖥 <b>%s</b> · терминал «<b>%s</b>»", esc(device), esc(ptyName))
		if hint := strings.TrimSpace(n.Hint); hint != "" {
			fmt.Fprintf(&sb, "\n\n%s", esc(hint))
		}
	}
	if !n.AgentOnline {
		sb.WriteString("\n\n<i>Компьютер сейчас не в сети — ответить из чата не получится.</i>")
	} else if n.Kind == notify.KindWaiting && !n.CanReply {
		sb.WriteString("\n\n<i>Обновите Remotai на компьютере, чтобы отвечать прямо из чата.</i>")
	} else if canReplyByText(n) {
		// Вопросы типа text кнопками не ответить (клавишу угадывать нельзя), а
		// раньше сообщение об этом молчало — человек писал «да» в чат и получал
		// инструкцию про код подключения.
		sb.WriteString("\n\n<i>Ответить можно текстом: напишите ответ в этот чат (или ответом на это сообщение) — я передам его агенту.</i>")
	}
	return sb.String()
}

// canReplyByText — можно ли отдать агенту свободный текст из чата. Метка эпизода
// обязательна: с ней ПК не применит ответ к ДРУГОМУ вопросу (expect_status_at),
// без неё писать в терминал наугад нельзя.
func canReplyByText(n notify.Notice) bool {
	return n.Kind == notify.KindWaiting && n.CanReply &&
		n.StatusAt > 0 && n.DeviceID != "" && n.PtyID != ""
}

// noticeKeyboard — ряд ответов по типу вопроса (ровно те же клавиши, что на
// экране терминала) плюс кнопка «Открыть терминал». Ряд ответов рисуем только
// когда ответ реально дойдёт: ПК на связи и версия агента умеет принимать ввод.
func (b *Bot) noticeKeyboard(n notify.Notice) models.ReplyMarkup {
	var rows [][]models.InlineKeyboardButton
	if n.Kind == notify.KindWaiting && n.CanReply {
		if row := b.replyRow(n); len(row) > 0 {
			rows = append(rows, row)
		}
	}
	if url := b.ptyDeepLink(n.DeviceID, n.PtyID); url != "" {
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "⌨️ Открыть терминал", WebApp: &models.WebAppInfo{URL: url}},
		})
	}
	if len(rows) == 0 {
		return nil
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (b *Bot) replyRow(n notify.Notice) []models.InlineKeyboardButton {
	type btn struct{ text, key string }
	var want []btn
	switch n.HintKind {
	case "choice":
		want = []btn{{"1", "1"}, {"2", "2"}, {"3", "3"}, {"Esc", "esc"}}
	case "enter":
		want = []btn{{"↵ Enter", "enter"}}
	case "yes_no":
		want = []btn{{"✅ Да", "y"}, {"❌ Нет", "n"}}
	default:
		// Тип вопроса не распознан (text / пусто) — угадывать клавишу опасно,
		// человек ответит с экрана терминала.
		return nil
	}
	row := make([]models.InlineKeyboardButton, 0, len(want))
	for _, w := range want {
		data, ok := inputCallbackData(n.DeviceID, n.PtyID, w.key, n.StatusAt)
		if !ok {
			return nil
		}
		row = append(row, models.InlineKeyboardButton{Text: w.text, CallbackData: data})
	}
	return row
}

// inputCallbackData собирает callback_data ответа:
// "pi:<device>:<pty>:<key>[:<status_at>]". ok=false, если id содержат
// разделитель или строка не влезает даже без метки эпизода — тогда кнопок не
// будет вовсе (лучше без кнопок, чем битое нажатие).
func inputCallbackData(deviceID, ptyID, key string, statusAt int64) (string, bool) {
	if deviceID == "" || ptyID == "" ||
		strings.Contains(deviceID, ":") || strings.Contains(ptyID, ":") {
		return "", false
	}
	base := callbackPtyInput + deviceID + ":" + ptyID + ":" + key
	if statusAt > 0 {
		// Метка эпизода — защита «ответ уходит в тот вопрос, который видели».
		// Если из-за длины id она не влезает в лимит Telegram, отправляем без
		// неё: кнопка без гарантии лучше отсутствующей кнопки.
		if withMark := base + ":" + strconv.FormatInt(statusAt, 10); len(withMark) <= callbackDataLimit {
			return withMark, true
		}
	}
	if len(base) > callbackDataLimit {
		return "", false
	}
	return base, true
}

// ptyDeepLink — web_app-ссылка на конкретный терминал. Когда известен ПК,
// маршрут сначала переключает cloud-контекст через DeviceList, а уже потом
// открывает PTY. Это не зависит от того, какой компьютер был выбран последним.
func (b *Bot) ptyDeepLink(deviceID, ptyID string) string {
	if ptyID == "" {
		return ""
	}
	if deviceID == "" {
		return b.miniAppRoute("/pty/" + urlPathEscape(ptyID))
	}
	next := "/pty/" + urlPathEscape(ptyID)
	return b.miniAppRoute("/devices?select=" + urlQueryEscape(deviceID) + "&next=" + urlQueryEscape(next))
}

// cbPtyInput — нажатие «Да» / «Нет» / «1..3» / «Enter» под уведомлением.
//
// Ответить Telegram надо в пределах ~30 секунд, иначе у человека вечно крутится
// спиннер — отсюда жёсткий таймаут на поход к ПК.
func (b *Bot) cbPtyInput(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	cq := upd.CallbackQuery
	if cq == nil {
		return
	}
	parts := strings.Split(strings.TrimPrefix(cq.Data, callbackPtyInput), ":")
	if len(parts) < 3 {
		b.answerCBMsg(ctx, tg, cq.ID, "Не понял кнопку", true)
		return
	}
	deviceID, ptyID, key := parts[0], parts[1], parts[2]
	var statusAt int64
	if len(parts) >= 4 {
		statusAt, _ = strconv.ParseInt(parts[3], 10, 64)
	}

	// Аккаунта нет — честный отказ; ошибка БД — НЕ отказ: занятая SQLite не
	// повод говорить человеку «вас тут нет» и портить метрику denied.
	u, err := db.GetUserByTelegram(ctx, b.db, cq.From.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		metrics.AgentReply("denied")
		b.answerCBMsg(ctx, tg, cq.ID, "Аккаунт не найден — откройте приложение заново", true)
		return
	case err != nil:
		metrics.AgentReply("error")
		log.Printf("[NOTIFY] pty input: пользователь tg=%d: %v", cq.From.ID, err)
		b.answerCBMsg(ctx, tg, cq.ID, "Не получилось проверить аккаунт — попробуйте ещё раз", true)
		return
	}
	allowed, err := db.UserCanAccessDevice(ctx, b.db, deviceID, u.ID)
	if err != nil {
		// Та же развилка: «не смогли проверить» ≠ «нет доступа». Схлопывание врало
		// владельцу про его же машину, а в мониторинге выглядело всплеском отказов.
		metrics.AgentReply("error")
		log.Printf("[NOTIFY] pty input: доступ device=%s user=%d: %v", deviceID, u.ID, err)
		b.answerCBMsg(ctx, tg, cq.ID, "Не получилось проверить доступ — попробуйте ещё раз", true)
		return
	}
	if !allowed {
		metrics.AgentReply("denied")
		b.answerCBMsg(ctx, tg, cq.ID, "Нет доступа к этому компьютеру", true)
		return
	}
	if b.PtyInput == nil {
		metrics.AgentReply("error")
		b.answerCBMsg(ctx, tg, cq.ID, "Ответы из чата сейчас недоступны", true)
		return
	}

	// Двойной тап: EditMessageText снимет кнопки не мгновенно, а сообщение живёт
	// в истории вечно — без этой защиты второе «n» улетело бы уже в СЛЕДУЮЩИЙ
	// вопрос агента. Резерв берём ПОСЛЕ всех проверок, прямо перед отправкой:
	// раньше он захватывался в начале и на ранних выходах не снимался — человек
	// нажимал «Да», в терминал не уходило ничего, а второе нажатие получало
	// «уже ответили», и карточка навсегда теряла возможность ответить.
	if !b.claimAnswer(cq) {
		b.answerCBMsg(ctx, tg, cq.ID, "Ответ уже отправляли — откройте терминал и проверьте", true)
		return
	}

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// UserID обязателен: право писать в этот терминал сервер проверяет у себя
	// (SendPtyInput), проверка выше — ранний отказ с понятным текстом.
	err = b.PtyInput(cctx, notify.InputRequest{
		UserID: u.ID, DeviceID: deviceID, PtyID: ptyID, Key: key, ExpectStatusAt: statusAt,
	})
	// release — снять резерв двойного тапа. Ставим его ТОЛЬКО там, где ввод точно
	// не попал в терминал (ПК офлайн, терминал закрыт, вопрос сменился, отказ по
	// частоте): тогда кнопка снова рабочая. Неясный исход резерв не снимает.
	release := false
	switch {
	case err == nil:
		metrics.AgentReply("ok")
		label := notify.KeyLabel(key)
		b.answerCBMsg(ctx, tg, cq.ID, "Отправлено: "+label, false)
		b.markAnswered(ctx, tg, cq, label)
		// Вопрос закрыт кнопкой — снимаем и адресата текста, иначе следующее
		// «спасибо» в чат получило бы «на этот вопрос уже отвечали».
		b.forgetReplyTargetCB(cq)
	case errors.Is(err, notify.ErrPromptChanged):
		release = true
		metrics.AgentReply("denied")
		b.answerCBMsg(ctx, tg, cq.ID,
			"Агент уже спрашивает о другом — ответ не отправлен. Откройте терминал", true)
	case errors.Is(err, notify.ErrOffline):
		release = true
		metrics.AgentReply("offline")
		b.answerCBMsg(ctx, tg, cq.ID, "Компьютер не в сети — ответ не доставлен", true)
	case errors.Is(err, notify.ErrUnsupported):
		release = true
		metrics.AgentReply("unsupported")
		b.answerCBMsg(ctx, tg, cq.ID, "Обновите Remotai на компьютере: эта версия не принимает ответы из Telegram", true)
	case errors.Is(err, notify.ErrPtyGone):
		release = true
		metrics.AgentReply("unsupported")
		b.answerCBMsg(ctx, tg, cq.ID, "Терминал уже закрыт", true)
	case errors.Is(err, notify.ErrKeyNotAllowed):
		release = true
		metrics.AgentReply("denied")
		b.answerCBMsg(ctx, tg, cq.ID, "Такой ответ отправить нельзя", true)
	case isRateLimited(err):
		// ПК отбил ввод по частоте — в терминал не ушло ничего, повторить можно.
		release = true
		metrics.AgentReply("denied")
		b.answerCBMsg(ctx, tg, cq.ID, "Слишком часто — попробуйте через минуту", true)
	case errors.Is(err, notify.ErrSubscriptionRequired), errors.Is(err, notify.ErrSubscriptionUnavailable):
		release = true
		metrics.AgentReply("denied")
		b.answerCBMsg(ctx, tg, cq.ID, err.Error(), true)
	case errors.Is(err, notify.ErrNoAccess):
		// Доступ отозвали между проверкой выше и отправкой: в терминал не ушло
		// ничего.
		release = true
		metrics.AgentReply("denied")
		b.answerCBMsg(ctx, tg, cq.ID, "Нет доступа к этому компьютеру", true)
	default:
		// Таймаут похода к ПК или обрыв: ПК мог записать ввод и не успеть
		// ответить. Утверждать «не отправлено» нельзя — человек нажмёт ещё раз, и
		// в терминал уйдёт ВТОРОЙ «y». Резерв тоже оставляем взятым.
		metrics.AgentReply("error")
		log.Printf("[NOTIFY] pty input device=%s pty=%s key=%s: %v", deviceID, ptyID, key, err)
		b.answerCBMsg(ctx, tg, cq.ID, "Ответ мог не дойти — откройте терминал и проверьте", true)
	}
	if release {
		b.releaseAnswer(cq)
	}
}

// isRateLimited — ПК ответил «слишком часто» (429 / code rate_limited в
// api_pty.go). Основной путь — sentinel notify.ErrRateLimited из
// server.agentInputError; проверка текста осталась запасной на случай, когда
// отказ по частоте прилетает общей ошибкой (чужой прокси, старый релей).
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, notify.ErrRateLimited) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "rate_limited") || strings.Contains(s, "агент ответил 429")
}

// answeredKey — сообщение-уведомление, под которым уже нажали кнопку ответа.
type answeredKey struct {
	chat int64
	msg  int
}

// claimAnswer резервирует право ответить под этим сообщением. false — под ним
// уже нажимали. Недоступное сообщение (Telegram не отдал message) пропускаем:
// дедуплицировать нечем, а терять ответ хуже.
func (b *Bot) claimAnswer(cq *models.CallbackQuery) bool {
	if cq.Message.Type != models.MaybeInaccessibleMessageTypeMessage || cq.Message.Message == nil {
		return true
	}
	return b.claimAnswerKey(answeredKey{chat: cq.Message.Message.Chat.ID, msg: cq.Message.Message.ID})
}

// claimAnswerKey — тот же резерв для ответа ТЕКСТОМ: CallbackQuery у него нет, а
// дедупликация обязана быть общей — под одним вопросом отвечают один раз, хоть
// кнопкой, хоть фразой в чат.
func (b *Bot) claimAnswerKey(key answeredKey) bool {
	now := time.Now()
	b.answeredMu.Lock()
	defer b.answeredMu.Unlock()
	if b.answered == nil {
		b.answered = make(map[answeredKey]time.Time)
	}
	if _, seen := b.answered[key]; seen {
		return false
	}
	// Карта растёт по числу отвеченных уведомлений — чистим редко и оптом.
	if len(b.answered) > 512 {
		for k, t := range b.answered {
			if now.Sub(t) > 6*time.Hour {
				delete(b.answered, k)
			}
		}
	}
	b.answered[key] = now
	return true
}

// releaseAnswer снимает резерв, если ответ ТОЧНО не дошёл до ПК (офлайн,
// закрытый терминал, сменившийся вопрос, отказ по частоте). При неясном исходе
// (таймаут, обрыв) резерв обязан остаться: ввод мог примениться, и повтор
// отправил бы в терминал второй ответ.
func (b *Bot) releaseAnswer(cq *models.CallbackQuery) {
	if cq.Message.Type != models.MaybeInaccessibleMessageTypeMessage || cq.Message.Message == nil {
		return
	}
	b.releaseAnswerKey(answeredKey{chat: cq.Message.Message.Chat.ID, msg: cq.Message.Message.ID})
}

func (b *Bot) releaseAnswerKey(key answeredKey) {
	b.answeredMu.Lock()
	delete(b.answered, key)
	b.answeredMu.Unlock()
}

// ── Ответ агенту свободным текстом ───────────────────────────────────────────
//
// Кнопки закрывают только распознанные вопросы (да/нет, 1–3, Enter). На вопрос
// типа text («Как назвать функцию?») кнопки не рисуются вовсе, и человек
// естественно пишет ответ фразой в чат. Чтобы фраза попала в тот терминал, про
// который ему написали, последнее уведомление привязывается к чату.
//
// Почему свободный текст здесь можно, а под кнопкой нельзя (notify/notice.go):
// callback_data приходит из сообщения, которое живёт в истории вечно и задаётся
// нами же, поэтому там разрешён только словарь клавиш. Текст же набирает живой
// человек в своём чате прямо сейчас — это ровно то же действие, что ввод с
// экрана терминала в мини-аппе, и права на ПК проверяются на релее
// (BotAgentRequest) при каждой отправке.

// chatReplyTarget — терминал из последнего уведомления «агент ждёт ответа».
type chatReplyTarget struct {
	deviceID   string
	deviceName string
	ptyID      string
	ptyName    string
	statusAt   int64 // эпизод вопроса, который человек ВИДЕЛ (expect_status_at)
	at         time.Time
}

// replyTargetTTL — сколько помним адресата. Ограничение только по памяти:
// правильность ответа держит не время, а метка эпизода (ПК отобьёт устаревший
// ответ кодом prompt_changed).
const replyTargetTTL = 6 * time.Hour

// rememberReplyTarget привязывает отправленное уведомление к чату: и по id
// сообщения (ответ reply'ем на конкретный вопрос), и как «последнее» (человек
// просто пишет в чат).
func (b *Bot) rememberReplyTarget(chatID int64, msg *models.Message, n notify.Notice) {
	if msg == nil || !canReplyByText(n) {
		return
	}
	key := answeredKey{chat: chatID, msg: msg.ID}
	target := chatReplyTarget{
		deviceID: n.DeviceID, deviceName: n.DeviceName,
		ptyID: n.PtyID, ptyName: n.PtyName,
		statusAt: n.StatusAt, at: time.Now(),
	}
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	if b.replyTargets == nil {
		b.replyTargets = make(map[answeredKey]chatReplyTarget)
		b.replyLatest = make(map[int64]answeredKey)
	}
	// Карта растёт по числу уведомлений — чистим редко и оптом, как b.answered.
	if len(b.replyTargets) > 512 {
		for k, t := range b.replyTargets {
			if time.Since(t.at) > replyTargetTTL {
				delete(b.replyTargets, k)
			}
		}
	}
	b.replyTargets[key] = target
	b.replyLatest[chatID] = key
}

// lookupReplyTarget ищет адресата свободного текста. replyToMsgID > 0 — человек
// ответил reply'ем на конкретное уведомление, и тогда берём именно его: он мог
// ответить на вопрос, который вышел не последним.
func (b *Bot) lookupReplyTarget(chatID int64, replyToMsgID int) (answeredKey, chatReplyTarget, bool) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	key := answeredKey{chat: chatID, msg: replyToMsgID}
	if replyToMsgID <= 0 {
		latest, ok := b.replyLatest[chatID]
		if !ok {
			return answeredKey{}, chatReplyTarget{}, false
		}
		key = latest
	}
	target, ok := b.replyTargets[key]
	if !ok || time.Since(target.at) > replyTargetTTL {
		return answeredKey{}, chatReplyTarget{}, false
	}
	return key, target, true
}

// forgetReplyTargetCB — то же для ответа кнопкой: ключ берём из сообщения, под
// которым нажали.
func (b *Bot) forgetReplyTargetCB(cq *models.CallbackQuery) {
	if cq.Message.Type != models.MaybeInaccessibleMessageTypeMessage || cq.Message.Message == nil {
		return
	}
	b.forgetReplyTarget(answeredKey{chat: cq.Message.Message.Chat.ID, msg: cq.Message.Message.ID})
}

// forgetReplyTarget снимает адресата после успешно отправленного текста: вопрос
// закрыт, и следующая случайная фраза в чат не должна улетать в терминал.
func (b *Bot) forgetReplyTarget(key answeredKey) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	delete(b.replyTargets, key)
	if latest, ok := b.replyLatest[key.chat]; ok && latest == key {
		delete(b.replyLatest, key.chat)
	}
}

// markAnswered гасит кнопки ответа у уже отвеченного сообщения: карточка живёт
// в истории вечно, а повторное нажатие отправило бы «n» в СЛЕДУЮЩИЙ вопрос
// агента. Кнопку «Открыть терминал» оставляем.
func (b *Bot) markAnswered(ctx context.Context, tg *tgbot.Bot, cq *models.CallbackQuery, label string) {
	if tg == nil {
		tg = b.b
	}
	if tg == nil || cq.Message.Type != models.MaybeInaccessibleMessageTypeMessage || cq.Message.Message == nil {
		return
	}
	msg := cq.Message.Message
	// Telegram отдаёт текст уже без разметки — отправляем его обратно как
	// обычный текст (ParseMode пустой), иначе «<» в имени ПК уронит edit.
	text, _ := clampTG(msg.Text + "\n\n✅ Отправлено: " + label)
	// ReplyMarkup задаём ВСЕГДА: без него Telegram оставит прежнюю клавиатуру, и
	// кнопки ответа никуда не денутся. Пустой inline_keyboard = снять кнопки.
	kb := b.keepOpenButton(msg.ReplyMarkup)
	if kb == nil {
		kb = &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
	}
	params := &tgbot.EditMessageTextParams{
		ChatID:      msg.Chat.ID,
		MessageID:   msg.ID,
		Text:        text,
		ReplyMarkup: kb,
	}
	if _, err := tg.EditMessageText(ctx, params); err != nil {
		log.Printf("[NOTIFY] edit answered message %d: %v", msg.ID, err)
	}
}

// keepOpenButton оставляет из прежней клавиатуры только ряды без ответов
// (кнопка web_app «Открыть терминал»).
func (b *Bot) keepOpenButton(kb *models.InlineKeyboardMarkup) *models.InlineKeyboardMarkup {
	if kb == nil {
		return nil
	}
	var rows [][]models.InlineKeyboardButton
	for _, row := range kb.InlineKeyboard {
		keep := true
		for _, btn := range row {
			if strings.HasPrefix(btn.CallbackData, callbackPtyInput) {
				keep = false
				break
			}
		}
		if keep && len(row) > 0 {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// cmdNotify — /notify: два независимых тумблера, вопросы агента и ошибки в
// терминале. Раньше настройка была одна и звалась «уведомления о вопросах
// агента», но гасила и рассылку ошибок — то есть обещание расходилось с делом.
func (b *Bot) cmdNotify(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	chatID := upd.Message.Chat.ID
	u, err := db.GetUserByTelegram(ctx, b.db, upd.Message.From.ID)
	if err != nil {
		b.send(ctx, tg, &tgbot.SendMessageParams{
			ChatID: chatID,
			Text:   "У вас ещё нет подключённых компьютеров. Нажмите /start, чтобы добавить.",
		})
		return
	}
	questions, err := db.NotifyEnabled(ctx, b.db, u.ID)
	if err != nil {
		b.send(ctx, tg, &tgbot.SendMessageParams{ChatID: chatID, Text: "Ошибка БД: " + err.Error()})
		return
	}
	errorsOn, err := db.NotifyErrorsEnabled(ctx, b.db, u.ID)
	if err != nil {
		b.send(ctx, tg, &tgbot.SendMessageParams{ChatID: chatID, Text: "Ошибка БД: " + err.Error()})
		return
	}
	b.send(ctx, tg, &tgbot.SendMessageParams{
		ChatID:      chatID,
		Text:        notifyPrefText(questions, errorsOn),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: notifyPrefKeyboard(questions, errorsOn),
	})
}

// cbNotifyPref — кнопки тумблеров под /notify. Формат data: "nt:<q|e>:<on|off>",
// legacy "nt:on" / "nt:off" = вопросы агента (старые сообщения в истории чата).
func (b *Bot) cbNotifyPref(ctx context.Context, tg *tgbot.Bot, upd *models.Update) {
	cq := upd.CallbackQuery
	if cq == nil {
		return
	}
	arg := strings.TrimPrefix(cq.Data, callbackNotifyPref)
	which, state := notifyPrefQuestions, arg
	if pref, rest, found := strings.Cut(arg, ":"); found {
		which, state = pref, rest
	}
	on := state == "on"
	u, err := db.GetUserByTelegram(ctx, b.db, cq.From.ID)
	if err != nil {
		b.answerCBMsg(ctx, tg, cq.ID, "Аккаунт не найден", true)
		return
	}
	switch which {
	case notifyPrefErrors:
		err = db.SetNotifyErrorsEnabled(ctx, b.db, u.ID, on)
	default:
		err = db.SetNotifyEnabled(ctx, b.db, u.ID, on)
	}
	if err != nil {
		b.answerCBMsg(ctx, tg, cq.ID, "Не удалось сохранить", true)
		return
	}
	label := "Вопросы агента"
	if which == notifyPrefErrors {
		label = "Ошибки в терминале"
	}
	if on {
		b.answerCBMsg(ctx, tg, cq.ID, label+": включено", false)
	} else {
		b.answerCBMsg(ctx, tg, cq.ID, label+": выключено", false)
	}
	// Перечитываем оба флага: в сообщении показаны они оба, и второй мог измениться
	// другим нажатием.
	questions, qErr := db.NotifyEnabled(ctx, b.db, u.ID)
	errorsOn, eErr := db.NotifyErrorsEnabled(ctx, b.db, u.ID)
	if qErr != nil || eErr != nil {
		return
	}
	if tg == nil {
		tg = b.b
	}
	if tg == nil || cq.Message.Type != models.MaybeInaccessibleMessageTypeMessage || cq.Message.Message == nil {
		return
	}
	if _, err := tg.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:      cq.Message.Message.Chat.ID,
		MessageID:   cq.Message.Message.ID,
		Text:        notifyPrefText(questions, errorsOn),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: notifyPrefKeyboard(questions, errorsOn),
	}); err != nil {
		log.Printf("[NOTIFY] edit /notify message: %v", err)
	}
}

func onOffWord(on bool) string {
	if on {
		return "включены"
	}
	return "выключены"
}

func notifyPrefText(questions, errorsOn bool) string {
	return "<b>Уведомления в этом чате</b>\n\n" +
		"• Вопросы агента: <b>" + onOffWord(questions) + "</b>\n" +
		"• Ошибки в терминале: <b>" + onOffWord(errorsOn) + "</b>\n\n" +
		"Когда AI-агент на компьютере ждёт вашего ответа, а приложение закрыто, я напишу сюда — " +
		"ответить можно кнопками или обычным текстом в чат.\n" +
		"Сообщения про ошибки — отдельно и по умолчанию выключены: компьютер присылает только " +
		"имя терминала, а «похоже на ошибку» — это любая строка вроде <code>FAIL</code> в выводе тестов."
}

func notifyPrefKeyboard(questions, errorsOn bool) models.ReplyMarkup {
	row := func(which, label string, on bool) []models.InlineKeyboardButton {
		text, state := "🔔 Включить "+label, "on"
		if on {
			text, state = "🔕 Выключить "+label, "off"
		}
		return []models.InlineKeyboardButton{{
			Text:         text,
			CallbackData: callbackNotifyPref + which + ":" + state,
		}}
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		row(notifyPrefQuestions, "вопросы агента", questions),
		row(notifyPrefErrors, "ошибки", errorsOn),
	}}
}

// answerCBMsg — ACK на инлайн-кнопку с текстом. alert=true показывает диалог
// (для отказов), иначе — всплывающую подсказку.
func (b *Bot) answerCBMsg(ctx context.Context, tg *tgbot.Bot, id, text string, alert bool) {
	if tg == nil {
		tg = b.b
	}
	if tg == nil {
		return
	}
	if _, err := tg.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{
		CallbackQueryID: id, Text: text, ShowAlert: alert,
	}); err != nil {
		log.Printf("[BOT] answer callback %s failed: %v", id, err)
	}
}

// isBlockedByUser — Telegram 403: пользователь заблокировал бота или удалил чат.
func isBlockedByUser(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "bot was blocked") ||
		strings.Contains(s, "user is deactivated") ||
		strings.Contains(s, "chat not found")
}

// disableNotifyForChat выключает уведомления заблокировавшему боту пользователю.
func (b *Bot) disableNotifyForChat(ctx context.Context, chatID int64, cause error) {
	u, err := db.GetUserByTelegram(ctx, b.db, chatID)
	if err != nil {
		return
	}
	if err := db.SetNotifyEnabled(ctx, b.db, u.ID, false); err != nil {
		log.Printf("[NOTIFY] disable notify for user=%d: %v", u.ID, err)
		return
	}
	log.Printf("[NOTIFY] notifications disabled for user=%d (telegram said: %v)", u.ID, cause)
}

// ── Сообщение от агента («remotai send») ─────────────────────────────────────

// SendAgentMessage доставляет текст, который агент на ПК прислал владельцу
// через POST /v1/agent/send. Одно сообщение без клавиатуры; auth, rate limit
// и проверка флага — на сервере (server/agent_send.go), здесь только текст и
// отправка. Вызывается сервером через колбэк UserSendText.
func (b *Bot) SendAgentMessage(ctx context.Context, chatID int64, deviceName, text string) error {
	if b == nil || b.b == nil {
		return errors.New("bot not configured")
	}
	msg, cut := clampTG(agentMessageText(deviceName, text))
	params := &tgbot.SendMessageParams{
		ChatID:    chatID,
		Text:      msg,
		ParseMode: models.ParseModeHTML,
	}
	if cut {
		// Как и в b.send: обрезанный HTML лучше доставить с видимыми тегами,
		// чем получить 400 и тишину.
		params.ParseMode = ""
	}
	if _, err := b.b.SendMessage(ctx, params); err != nil {
		if isBlockedByUser(err) {
			// Бот заблокирован — канал мёртв, гасим уведомления целиком, иначе
			// каждый «remotai send» будет биться в стену.
			b.disableNotifyForChat(ctx, chatID, err)
		}
		return err
	}
	return nil
}

// agentMessageText — «📨 <b>ПК</b>:\nтекст». Имя ПК и текст приезжают с машины
// пользователя: без esc «<» уронит отправку (Telegram 400 can't parse
// entities). Обрезка до лимита TG — clampTG у вызывающего.
func agentMessageText(deviceName, text string) string {
	if strings.TrimSpace(deviceName) == "" {
		deviceName = "компьютер"
	}
	return fmt.Sprintf("📨 <b>%s</b>:\n%s", esc(deviceName), esc(text))
}

// ── «Компьютер обновился» ─────────────────────────────────────────────────────

// SendUpdateNotice пишет пользователю, что его компьютер обновился. Одно
// спокойное сообщение без клавиатуры; дедуп и проверка флага — на сервере
// (server/notifier.go, fireAgentUpdated), здесь только текст и отправка.
// Вызывается сервером через колбэк UserNotifyText.
func (b *Bot) SendUpdateNotice(ctx context.Context, chatID int64, deviceName, version string) error {
	if b == nil || b.b == nil {
		return errors.New("bot not configured")
	}
	text, cut := clampTG(updateNoticeText(deviceName, version))
	params := &tgbot.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	}
	if cut {
		// Как и в b.send: обрезанный HTML лучше доставить с видимыми тегами,
		// чем получить 400 и тишину.
		params.ParseMode = ""
	}
	if _, err := b.b.SendMessage(ctx, params); err != nil {
		if isBlockedByUser(err) {
			// Бот заблокирован — канал мёртв, гасим уведомления целиком, иначе
			// каждое обновление будет биться в стену.
			b.disableNotifyForChat(ctx, chatID, err)
		}
		return err
	}
	return nil
}

// updateNoticeText — «✅ <b>ПК</b> обновился до v2.46.5». Имя ПК приезжает с
// машины пользователя: без esc «<» в имени уронит отправку (Telegram 400
// can't parse entities).
func updateNoticeText(deviceName, version string) string {
	if strings.TrimSpace(deviceName) == "" {
		deviceName = "компьютер"
	}
	return fmt.Sprintf("✅ <b>%s</b> обновился до v%s", esc(deviceName), esc(version))
}

// ── Ответ поддержки в личный чат ──────────────────────────────────────────────

// SendSupportReply пишет пользователю, что поддержка ответила на его обращение.
// Вызывается сервером через колбэк UserNotifySupport (server/support.go): там
// живут дедупликация и проверка флага, здесь — только текст и кнопка.
//
// Кнопка ведёт в чат поддержки мини-аппа: до неё же ведёт бейдж непрочитанного в
// приложении, так что человек попадает в одну и ту же переписку с любой стороны.
func (b *Bot) SendSupportReply(ctx context.Context, chatID int64, preview string) error {
	if b == nil || b.b == nil {
		return errors.New("bot not configured")
	}
	var sb strings.Builder
	sb.WriteString("💬 <b>Поддержка Remotai ответила</b>")
	// Превью — текст живого человека: без esc любой «<» в ответе уронил бы
	// отправку (Telegram 400 can't parse entities).
	if p := strings.TrimSpace(preview); p != "" {
		fmt.Fprintf(&sb, "\n\n%s", esc(p))
	}
	text, cut := clampTG(sb.String())
	params := &tgbot.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	}
	if cut {
		// Обрезанный HTML почти наверняка невалиден — как и в b.send, лучше
		// доставить сообщение с видимыми тегами, чем получить 400 и тишину.
		params.ParseMode = ""
	}
	if url := b.miniAppRoute("/support"); url != "" {
		params.ReplyMarkup = &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "💬 Открыть чат поддержки", WebApp: &models.WebAppInfo{URL: url}},
			}},
		}
	}
	if _, err := b.b.SendMessage(ctx, params); err != nil {
		if isBlockedByUser(err) {
			// Бот заблокирован — мёртвы ОБА канала, гасим и вопросы агента, и
			// ответы поддержки: иначе релей будет биться в стену на каждом ответе.
			b.disableNotifyForChat(ctx, chatID, err)
			b.disableSupportNotifyForChat(ctx, chatID, err)
		}
		return err
	}
	return nil
}

// disableSupportNotifyForChat выключает сообщения про ответ поддержки
// заблокировавшему боту пользователю (флаг users.tg_notify_support).
func (b *Bot) disableSupportNotifyForChat(ctx context.Context, chatID int64, cause error) {
	u, err := db.GetUserByTelegram(ctx, b.db, chatID)
	if err != nil {
		return
	}
	if err := db.SetSupportNotifyEnabled(ctx, b.db, u.ID, false); err != nil {
		log.Printf("[SUPPORT] disable reply notify for user=%d: %v", u.ID, err)
		return
	}
	log.Printf("[SUPPORT] reply notifications disabled for user=%d (telegram said: %v)", u.ID, cause)
}
