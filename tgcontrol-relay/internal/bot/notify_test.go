package bot

import (
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/notify"
)

func testBot() *Bot {
	return &Bot{cfg: &config.Config{MiniAppURL: "https://remotai.ru/tg/"}}
}

// callback_data ограничен 64 байтами. Реальные id (12 hex у ПК, 16 hex у
// терминала) обязаны влезать ВМЕСТЕ с меткой эпизода.
func TestInputCallbackDataFitsTelegramLimit(t *testing.T) {
	data, ok := inputCallbackData("a7a22f3d10bf", "0123456789abcdef", "y", 1721937600000)
	if !ok {
		t.Fatalf("реальные id не влезли в лимит")
	}
	if len(data) > callbackDataLimit {
		t.Fatalf("callback_data %d байт (%q), лимит %d", len(data), data, callbackDataLimit)
	}
	if !strings.HasPrefix(data, callbackPtyInput) || strings.Count(data, ":") != 4 {
		t.Fatalf("неожиданный формат: %q", data)
	}

	// Метка не влезла → кнопка остаётся, но без гарантии «тот же вопрос».
	long := strings.Repeat("d", 40)
	data, ok = inputCallbackData(long, "0123456789abcdef", "y", 1721937600000)
	if !ok || strings.Count(data, ":") != 3 {
		t.Fatalf("длинный device_id: ok=%v data=%q — ждали кнопку без метки", ok, data)
	}

	// Разделитель внутри id ломает разбор — таких кнопок быть не должно.
	if _, ok := inputCallbackData("dev:1", "pty1", "y", 0); ok {
		t.Fatalf("device_id с ':' обязан отбраковываться")
	}
	// Без запаса не влезает вообще — кнопок нет.
	if _, ok := inputCallbackData(strings.Repeat("d", 60), "pty1", "y", 0); ok {
		t.Fatalf("слишком длинный device_id обязан отбраковываться")
	}
}

// Имя ПК с «<» роняло отправку сообщения (Telegram 400 can't parse entities).
func TestNoticeTextEscapesHTML(t *testing.T) {
	text := noticeText(notify.Notice{
		Kind: notify.KindWaiting, DeviceName: "DESKTOP<1>", PtyName: "build & test",
		Agent: "claude", Hint: "Подтвердите: y/n", HintKind: "yes_no", AgentOnline: true, CanReply: true,
	})
	if strings.Contains(text, "DESKTOP<1>") || strings.Contains(text, "build & test") {
		t.Fatalf("текст не экранирован: %q", text)
	}
	if !strings.Contains(text, "Claude") || !strings.Contains(text, "Подтвердите: y/n") {
		t.Fatalf("в тексте нет агента или вопроса: %q", text)
	}
}

// Офлайн-ПК и старый агент обязаны честно объяснять, почему кнопок ответа нет.
func TestNoticeTextExplainsWhyNoButtons(t *testing.T) {
	offline := noticeText(notify.Notice{Kind: notify.KindWaiting, PtyName: "build", AgentOnline: false})
	if !strings.Contains(offline, "не в сети") {
		t.Fatalf("офлайн-ПК не объяснён: %q", offline)
	}
	old := noticeText(notify.Notice{Kind: notify.KindWaiting, PtyName: "build", AgentOnline: true, CanReply: false})
	if !strings.Contains(old, "Обновите Remotai") {
		t.Fatalf("старый агент не объяснён: %q", old)
	}
}

// Кнопки ответа появляются только когда ответ реально дойдёт и тип вопроса
// понятен; кнопка «Открыть терминал» — всегда.
func TestNoticeKeyboardRows(t *testing.T) {
	b := testBot()
	base := notify.Notice{
		Kind: notify.KindWaiting, DeviceID: "dev1", PtyID: "pty1",
		HintKind: "yes_no", AgentOnline: true, CanReply: true,
	}
	kb, _ := b.noticeKeyboard(base).(*models.InlineKeyboardMarkup)
	if kb == nil || len(kb.InlineKeyboard) != 2 || len(kb.InlineKeyboard[0]) != 2 {
		t.Fatalf("ждали ряд [Да][Нет] + «Открыть»: %+v", kb)
	}

	offline := base
	offline.AgentOnline, offline.CanReply = false, false
	kb, _ = b.noticeKeyboard(offline).(*models.InlineKeyboardMarkup)
	if kb == nil || len(kb.InlineKeyboard) != 1 {
		t.Fatalf("офлайн-ПК: ждали только «Открыть»: %+v", kb)
	}

	unknown := base
	unknown.HintKind = "text" // тип вопроса не распознан — клавишу не угадываем
	kb, _ = b.noticeKeyboard(unknown).(*models.InlineKeyboardMarkup)
	if kb == nil || len(kb.InlineKeyboard) != 1 {
		t.Fatalf("нераспознанный вопрос: ждали только «Открыть»: %+v", kb)
	}

	// Ошибка в терминале — отвечать нечего.
	errNotice := base
	errNotice.Kind = notify.KindError
	kb, _ = b.noticeKeyboard(errNotice).(*models.InlineKeyboardMarkup)
	if kb == nil || len(kb.InlineKeyboard) != 1 {
		t.Fatalf("ошибка: ждали только «Открыть»: %+v", kb)
	}
}

// Текст «ПК обновился»: имя компьютера экранируется, пустое имя заменяется.
func TestUpdateNoticeText(t *testing.T) {
	got := updateNoticeText("Офисный ПК", "2.46.5")
	if got != "✅ <b>Офисный ПК</b> обновился до v2.46.5" {
		t.Fatalf("неожиданный текст: %q", got)
	}
	if !strings.Contains(updateNoticeText("", "2.46.5"), "компьютер") {
		t.Fatalf("пустое имя не заменено: %q", updateNoticeText("", "2.46.5"))
	}
	if strings.Contains(updateNoticeText("DESKTOP<1>", "2.46.5"), "DESKTOP<1>") {
		t.Fatalf("имя не экранировано: %q", updateNoticeText("DESKTOP<1>", "2.46.5"))
	}
}

// Текст «сообщение от агента»: имя компьютера и сам текст экранируются,
// пустое имя заменяется.
func TestAgentMessageText(t *testing.T) {
	got := agentMessageText("Офисный ПК", "бэкап готов")
	if got != "📨 <b>Офисный ПК</b>:\nбэкап готов" {
		t.Fatalf("неожиданный текст: %q", got)
	}
	if !strings.Contains(agentMessageText("", "x"), "компьютер") {
		t.Fatalf("пустое имя не заменено: %q", agentMessageText("", "x"))
	}
	got = agentMessageText("DESKTOP<1>", "a < b")
	if strings.Contains(got, "DESKTOP<1>") || strings.Contains(got, "a < b") {
		t.Fatalf("не экранировано: %q", got)
	}
}

func TestPtyDeepLink(t *testing.T) {
	b := testBot()
	if got := b.ptyDeepLink("dev1", "pty1"); got != "https://remotai.ru/tg/#/devices?select=dev1&next=%2Fpty%2Fpty1" {
		t.Fatalf("ссылка с выбором device: %q", got)
	}
	if got := b.ptyDeepLink("", "pty1"); got != "https://remotai.ru/tg/#/pty/pty1" {
		t.Fatalf("legacy-ссылка без device: %q", got)
	}
	// web_app-кнопки принимают только HTTPS.
	b.cfg.MiniAppURL = "http://localhost:5173/tg/"
	if got := b.ptyDeepLink("dev1", "pty1"); got != "" {
		t.Fatalf("не-HTTPS URL должен отключать кнопку, получили %q", got)
	}
}

// После ответа ряд с клавишами исчезает, «Открыть терминал» остаётся.
func TestKeepOpenButton(t *testing.T) {
	b := testBot()
	kb := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{{Text: "Да", CallbackData: callbackPtyInput + "d:p:y"}, {Text: "Нет", CallbackData: callbackPtyInput + "d:p:n"}},
		{{Text: "Открыть", WebApp: &models.WebAppInfo{URL: "https://remotai.ru/tg/"}}},
	}}
	got := b.keepOpenButton(kb)
	if got == nil || len(got.InlineKeyboard) != 1 || got.InlineKeyboard[0][0].Text != "Открыть" {
		t.Fatalf("ждали одну кнопку «Открыть»: %+v", got)
	}
	if b.keepOpenButton(&models.InlineKeyboardMarkup{}) != nil {
		t.Fatalf("пустая клавиатура должна давать nil, иначе Telegram получит пустой markup")
	}
}
