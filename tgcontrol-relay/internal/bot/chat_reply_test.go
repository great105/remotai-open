package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/notify"
)

// Уведомление о вопросе привязывается к чату: свободный текст должен уходить в
// тот терминал, про который человеку написали.
func TestRememberAndLookupReplyTarget(t *testing.T) {
	b := testBot()
	notice := notify.Notice{
		Kind: notify.KindWaiting, DeviceID: "dev1", DeviceName: "DESKTOP",
		PtyID: "pty1", PtyName: "build", HintKind: "text",
		StatusAt: 1721937600000, AgentOnline: true, CanReply: true,
	}
	b.rememberReplyTarget(42, &models.Message{ID: 7}, notice)

	// Просто фраза в чат → последнее уведомление.
	key, target, ok := b.lookupReplyTarget(42, 0)
	if !ok || target.ptyID != "pty1" || target.statusAt != notice.StatusAt {
		t.Fatalf("последний адресат не найден: ok=%v target=%+v", ok, target)
	}
	// Reply на конкретное сообщение → тот же адресат по id сообщения.
	if _, _, ok := b.lookupReplyTarget(42, 7); !ok {
		t.Fatalf("reply на уведомление не нашёл адресата")
	}
	// Reply на чужое сообщение адресата не даёт: писать в терминал наугад нельзя.
	if _, _, ok := b.lookupReplyTarget(42, 999); ok {
		t.Fatalf("reply на постороннее сообщение не должен давать адресата")
	}
	// После ответа адресат снимается — случайная фраза в чат в терминал не уедет.
	b.forgetReplyTarget(key)
	if _, _, ok := b.lookupReplyTarget(42, 0); ok {
		t.Fatalf("адресат остался после forgetReplyTarget")
	}
}

// Без метки эпизода (или на офлайн-ПК, или у старого агента) текстом отвечать
// нельзя: ПК не сможет проверить, что отвечают на видимый вопрос.
func TestReplyTargetRequiresEpisodeMark(t *testing.T) {
	base := notify.Notice{
		Kind: notify.KindWaiting, DeviceID: "dev1", PtyID: "pty1",
		StatusAt: 100, AgentOnline: true, CanReply: true,
	}
	if !canReplyByText(base) {
		t.Fatalf("нормальный вопрос обязан принимать текст")
	}
	noMark := base
	noMark.StatusAt = 0
	if canReplyByText(noMark) {
		t.Fatalf("без status_at текстовый ответ запрещён")
	}
	oldAgent := base
	oldAgent.CanReply = false
	if canReplyByText(oldAgent) {
		t.Fatalf("старый агент не принимает ввод — текст запрещён")
	}
	errNotice := base
	errNotice.Kind = notify.KindError
	if canReplyByText(errNotice) {
		t.Fatalf("на сообщение об ошибке отвечать нечему")
	}

	b := testBot()
	b.rememberReplyTarget(1, &models.Message{ID: 2}, noMark)
	if _, _, ok := b.lookupReplyTarget(1, 0); ok {
		t.Fatalf("адресат без метки эпизода не должен запоминаться")
	}
}

// Несвежий адресат забывается: карта живёт только в памяти релея.
func TestReplyTargetExpires(t *testing.T) {
	b := testBot()
	b.rememberReplyTarget(5, &models.Message{ID: 1}, notify.Notice{
		Kind: notify.KindWaiting, DeviceID: "d", PtyID: "p",
		StatusAt: 1, AgentOnline: true, CanReply: true,
	})
	b.replyMu.Lock()
	key := b.replyLatest[5]
	stale := b.replyTargets[key]
	stale.at = time.Now().Add(-replyTargetTTL - time.Minute)
	b.replyTargets[key] = stale
	b.replyMu.Unlock()
	if _, _, ok := b.lookupReplyTarget(5, 0); ok {
		t.Fatalf("просроченный адресат обязан отбраковываться")
	}
}

// Ответ кнопкой и ответ текстом делят один резерв: под одним вопросом человек
// отвечает один раз, иначе второй ответ уедет в следующий вопрос агента.
func TestClaimAnswerSharedBetweenButtonAndText(t *testing.T) {
	b := &Bot{answered: map[answeredKey]time.Time{}}
	key := answeredKey{chat: 1, msg: 2}
	if !b.claimAnswerKey(key) {
		t.Fatalf("первый ответ обязан проходить")
	}
	if b.claimAnswerKey(key) {
		t.Fatalf("второй ответ под тем же вопросом проходить не должен")
	}
	b.releaseAnswerKey(key)
	if !b.claimAnswerKey(key) {
		t.Fatalf("после releaseAnswerKey ответить снова можно")
	}
}

// Сообщение про ошибку не должно утверждать больше, чем знает: строки ошибки в
// событии нет, а выключается оно теперь отдельно от вопросов агента.
func TestErrorNoticeTextIsHonest(t *testing.T) {
	text := noticeText(notify.Notice{
		Kind: notify.KindError, DeviceName: "DESKTOP", PtyName: "build",
		Agent: "claude", AgentOnline: true,
	})
	if !strings.Contains(text, "похожая на ошибку") {
		t.Fatalf("текст обязан говорить, что это лишь похоже на ошибку: %q", text)
	}
	if !strings.Contains(text, "/notify") {
		t.Fatalf("текст обязан показывать, где выключить такие сообщения: %q", text)
	}
	// На ошибку текстом не отвечают — предложения писать ответ быть не должно.
	if strings.Contains(text, "напишите ответ") {
		t.Fatalf("ошибка не принимает текстовый ответ: %q", text)
	}
}

// Вопрос типа text кнопок не получает — значит, в сообщении обязано быть сказано,
// что ответить можно текстом.
func TestWaitingNoticeOffersTextReply(t *testing.T) {
	text := noticeText(notify.Notice{
		Kind: notify.KindWaiting, DeviceID: "dev1", DeviceName: "DESKTOP",
		PtyID: "pty1", PtyName: "build",
		Agent: "codex", Hint: "Требуется ввод", HintKind: "text",
		StatusAt: 7, AgentOnline: true, CanReply: true,
	})
	if !strings.Contains(text, "текстом") {
		t.Fatalf("нет предложения ответить текстом: %q", text)
	}
}

// Оба тумблера /notify независимы и оба видны в сообщении.
func TestNotifyPrefUI(t *testing.T) {
	text := notifyPrefText(true, false)
	if !strings.Contains(text, "Вопросы агента: <b>включены</b>") ||
		!strings.Contains(text, "Ошибки в терминале: <b>выключены</b>") {
		t.Fatalf("состояние тумблеров не показано: %q", text)
	}
	kb, _ := notifyPrefKeyboard(true, false).(*models.InlineKeyboardMarkup)
	if kb == nil || len(kb.InlineKeyboard) != 2 {
		t.Fatalf("ждали два ряда тумблеров: %+v", kb)
	}
	if got := kb.InlineKeyboard[0][0].CallbackData; got != callbackNotifyPref+notifyPrefQuestions+":off" {
		t.Fatalf("вопросы: неожиданная callback_data %q", got)
	}
	if got := kb.InlineKeyboard[1][0].CallbackData; got != callbackNotifyPref+notifyPrefErrors+":on" {
		t.Fatalf("ошибки: неожиданная callback_data %q", got)
	}
	for _, row := range kb.InlineKeyboard {
		if len(row[0].CallbackData) > callbackDataLimit {
			t.Fatalf("callback_data длиннее лимита: %q", row[0].CallbackData)
		}
	}
}

// Причина отказа ПК приходит машинным кодом — человек обязан прочитать её, а не
// «не удалось создать терминал».
func TestAgentErrorText(t *testing.T) {
	limit := agentErrorText(403, []byte(`{"code":"pty_limit","error":"Достигнут лимит терминалов."}`), "фолбэк")
	if !strings.Contains(limit, "лимит терминалов") {
		t.Fatalf("pty_limit не объяснён: %q", limit)
	}
	changed := agentErrorText(409, []byte(`{"code":"prompt_changed"}`), "фолбэк")
	if !strings.Contains(changed, "о другом") {
		t.Fatalf("prompt_changed не объяснён: %q", changed)
	}
	// Старый агент: роута нет, net/http отдаёт текст без кода — это «обновите».
	old := agentErrorText(404, []byte("404 page not found\n"), "фолбэк")
	if !strings.Contains(old, "Обновите Remotai") {
		t.Fatalf("старый агент: %q", old)
	}
	// Русский текст самого ПК важнее общего фолбэка.
	own := agentErrorText(400, []byte(`{"error":"Эта рабочая папка недоступна."}`), "фолбэк")
	if own != "Эта рабочая папка недоступна." {
		t.Fatalf("текст ПК потерян: %q", own)
	}
	if got := agentErrorText(500, []byte("{}"), "фолбэк"); got != "фолбэк" {
		t.Fatalf("фолбэк не сработал: %q", got)
	}
}

// /run переиспользует свой терминал и не пишет в занятый.
func TestFindBotRunPty(t *testing.T) {
	b := testBot()
	answer := ""
	b.AgentRequest = func(_ context.Context, _ int64, _, _, path string, _ []byte) (int, []byte, error) {
		if !strings.HasPrefix(path, "/api/pty?") {
			t.Fatalf("ждали список терминалов, получили %q", path)
		}
		return 200, []byte(answer), nil
	}

	answer = `{"sessions":[{"id":"p1","name":"other","alive":true},{"id":"p2","name":"Telegram","alive":true,"status":"idle"}]}`
	session, busy, err := b.findBotRunPty(context.Background(), 1, "dev1")
	if err != nil || session == nil || session.ID != "p2" || busy {
		t.Fatalf("свободный терминал бота не найден: %+v busy=%v err=%v", session, busy, err)
	}

	answer = `{"sessions":[{"id":"p2","name":"Telegram","alive":true,"status":"working"}]}`
	session, busy, err = b.findBotRunPty(context.Background(), 1, "dev1")
	if err != nil || session == nil || !busy {
		t.Fatalf("работающая команда обязана считаться занятостью: %+v busy=%v err=%v", session, busy, err)
	}

	answer = `{"sessions":[{"id":"p2","name":"Telegram","alive":true,"status":"idle","agent_kind":"claude"}]}`
	if _, busy, _ = b.findBotRunPty(context.Background(), 1, "dev1"); !busy {
		t.Fatalf("терминал с AI-агентом занят — команду туда писать нельзя")
	}

	answer = `{"sessions":[{"id":"p1","name":"other","alive":true}]}`
	session, busy, err = b.findBotRunPty(context.Background(), 1, "dev1")
	if err != nil || session != nil || busy {
		t.Fatalf("своего терминала нет — ждали nil: %+v busy=%v err=%v", session, busy, err)
	}
}

// Меню для владельца ПК — вместо онбординга «скачайте программу».
func TestMenuTextListsCommands(t *testing.T) {
	b := testBot()
	b.IsOnline = func(deviceID string) bool { return deviceID == "d1" }
	text := b.menuText([]*db.Device{{ID: "d1"}, {ID: "d2"}})
	if !strings.Contains(text, "2 компьютера") || !strings.Contains(text, "в сети: 1") {
		t.Fatalf("нет счётчика компьютеров: %q", text)
	}
	for _, cmd := range []string{"/pc", "/term", "/run", "/notify", "/support"} {
		if !strings.Contains(text, cmd) {
			t.Fatalf("в меню нет %s: %q", cmd, text)
		}
	}
	// Меню уходит с ParseMode HTML — «<команда>» обязана быть экранирована.
	if strings.Contains(text, "<команда>") {
		t.Fatalf("угловые скобки не экранированы: %q", text)
	}
}

// Команду боту в терминал не отправляем, а путь вида /home/user — отправляем:
// различаем их сущностью bot_command от Telegram, а не первым «/».
func TestStartsWithCommand(t *testing.T) {
	cmd := &models.Message{
		Text:     "/foo bar",
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: 4}},
	}
	if !startsWithCommand(cmd) {
		t.Fatalf("команда не распознана")
	}
	if startsWithCommand(&models.Message{Text: "/home/user/project"}) {
		t.Fatalf("путь без сущности bot_command командой считать нельзя")
	}
	// Команда не в начале строки — это обычный текст.
	notFirst := &models.Message{
		Text:     "смотри /foo",
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 7, Length: 4}},
	}
	if startsWithCommand(notFirst) {
		t.Fatalf("команда не в начале — это ответ агенту")
	}
}

func TestPlural(t *testing.T) {
	cases := map[int]string{1: "1 компьютер", 2: "2 компьютера", 5: "5 компьютеров", 11: "11 компьютеров", 21: "21 компьютер"}
	for n, want := range cases {
		if got := plural(n, "компьютер", "компьютера", "компьютеров"); got != want {
			t.Fatalf("plural(%d) = %q, ждали %q", n, got, want)
		}
	}
}
