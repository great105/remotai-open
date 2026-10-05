package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/notify"
)

// supportNotice — что ушло бы в личный чат бота.
type supportNotice struct {
	chatID  int64
	preview string
}

// captureSupportNotices подменяет колбэк доставки на канал. Буфер с запасом:
// проверки «второго сообщения не было» не должны блокировать отправителя.
func captureSupportNotices(e *supportTestEnv) chan supportNotice {
	out := make(chan supportNotice, 8)
	e.srv.UserNotifySupport = func(ctx context.Context, chatID int64, preview string) error {
		out <- supportNotice{chatID: chatID, preview: preview}
		return nil
	}
	return out
}

// adminReply — ответ админа через ручку админки (именно она обязана уведомлять).
func (e *supportTestEnv) adminReply(t *testing.T, threadID int64, text string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	code, resp := e.do(t, "POST", "/v1/admin/support/threads/"+itoa64(threadID)+"/messages",
		string(body), e.adminHeaders())
	if code != http.StatusOK {
		t.Fatalf("admin reply status = %d: %s", code, resp)
	}
}

func waitNotice(t *testing.T, ch chan supportNotice) supportNotice {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(3 * time.Second):
		t.Fatal("уведомление об ответе поддержки не отправлено")
		return supportNotice{}
	}
}

func expectNoNotice(t *testing.T, ch chan supportNotice, why string) {
	t.Helper()
	select {
	case n := <-ch:
		t.Fatalf("%s, но сообщение ушло: %+v", why, n)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestSupportReplyNotifiesUser(t *testing.T) {
	e := newSupportTestEnv(t)
	ctx := context.Background()
	notices := captureSupportNotices(e)

	threadID, _, err := db.PostUserSupportMessage(ctx, e.d, e.user.ID, "не подключается ПК")
	if err != nil {
		t.Fatalf("post user message: %v", err)
	}

	e.adminReply(t, threadID, "смотрю логи, ответлю через минуту")
	got := waitNotice(t, notices)
	if got.chatID != e.user.TelegramID {
		t.Fatalf("chat_id = %d, want %d", got.chatID, e.user.TelegramID)
	}
	if got.preview != "смотрю логи, ответлю через минуту" {
		t.Fatalf("preview = %q", got.preview)
	}

	// Серия ответов подряд = один пинг: отметка треда уже стоит.
	e.adminReply(t, threadID, "и ещё проверьте автозапуск")
	expectNoNotice(t, notices, "второй ответ в пределах окна не должен уведомлять")

	// Человек прочитал переписку → отметка снята, следующий ответ снова доходит.
	if code, body := e.do(t, "GET", "/v1/support/messages", "", e.userHeaders()); code != http.StatusOK {
		t.Fatalf("user list status = %d: %s", code, body)
	}
	e.adminReply(t, threadID, "получилось?")
	if got := waitNotice(t, notices); got.preview != "получилось?" {
		t.Fatalf("preview после прочтения = %q", got.preview)
	}
}

func TestSupportReplyNotifyRespectsFlags(t *testing.T) {
	e := newSupportTestEnv(t)
	ctx := context.Background()
	notices := captureSupportNotices(e)

	threadID, _, err := db.PostUserSupportMessage(ctx, e.d, e.user.ID, "вопрос")
	if err != nil {
		t.Fatalf("post user message: %v", err)
	}

	// Флаг поддержки выключен — молчим.
	if err := db.SetSupportNotifyEnabled(ctx, e.d, e.user.ID, false); err != nil {
		t.Fatalf("set support notify: %v", err)
	}
	e.adminReply(t, threadID, "ответ при выключенном флаге")
	expectNoNotice(t, notices, "support_reply выключен")

	// Выключенные уведомления о вопросах агента ответ поддержки НЕ глушат:
	// это разные каналы и разные флаги.
	if err := db.SetSupportNotifyEnabled(ctx, e.d, e.user.ID, true); err != nil {
		t.Fatalf("re-enable support notify: %v", err)
	}
	if err := db.SetNotifyEnabled(ctx, e.d, e.user.ID, false); err != nil {
		t.Fatalf("disable agent notify: %v", err)
	}
	e.adminReply(t, threadID, "ответ при выключенных вопросах агента")
	if got := waitNotice(t, notices); got.preview != "ответ при выключенных вопросах агента" {
		t.Fatalf("preview = %q", got.preview)
	}
}

func TestSupportReplyNotifySkipsAnonAccount(t *testing.T) {
	e := newSupportTestEnv(t)
	ctx := context.Background()
	notices := captureSupportNotices(e)

	// Анонимный аккаунт (пейринг по QR): telegram_id отрицательный, личного чата
	// нет — писать физически некуда.
	anon, err := db.UpsertUser(ctx, e.d, -991, "", "", "ru")
	if err != nil {
		t.Fatalf("upsert anon: %v", err)
	}
	threadID, _, err := db.PostUserSupportMessage(ctx, e.d, anon.ID, "аноним пишет")
	if err != nil {
		t.Fatalf("post anon message: %v", err)
	}
	e.adminReply(t, threadID, "ответ анониму")
	expectNoNotice(t, notices, "у анонимного аккаунта нет Telegram")
}

func TestNotifyPrefsEndpoint(t *testing.T) {
	e := newSupportTestEnv(t)
	ctx := context.Background()
	// Канал доступен: бот настроен и релей уведомляет о вопросах агента.
	e.srv.Config.NotifyTG = true
	e.srv.UserNotify = func(context.Context, int64, notify.Notice) error { return nil }
	_ = captureSupportNotices(e)

	if code, _ := e.do(t, "GET", "/v1/me/notify", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", code)
	}

	code, body := e.do(t, "GET", "/v1/me/notify", "", e.userHeaders())
	if code != http.StatusOK {
		t.Fatalf("get status = %d: %s", code, body)
	}
	var prefs notifyPrefsJSON
	if err := json.Unmarshal(body, &prefs); err != nil {
		t.Fatalf("decode prefs: %v", err)
	}
	if !prefs.TelegramLinked || !prefs.AgentWaiting || !prefs.SupportReply ||
		!prefs.AgentWaitingAvailable || !prefs.SupportReplyAvailable {
		t.Fatalf("defaults = %+v, want всё включено и доступно", prefs)
	}

	// Частичный PUT: второй флаг не трогаем.
	code, body = e.do(t, "PUT", "/v1/me/notify", `{"agent_waiting":false}`, e.userHeaders())
	if code != http.StatusOK {
		t.Fatalf("put status = %d: %s", code, body)
	}
	if err := json.Unmarshal(body, &prefs); err != nil {
		t.Fatalf("decode put: %v", err)
	}
	if prefs.AgentWaiting || !prefs.SupportReply {
		t.Fatalf("после PUT agent_waiting=false: %+v", prefs)
	}
	if on, err := db.NotifyEnabled(ctx, e.d, e.user.ID); err != nil || on {
		t.Fatalf("tg_notify = %v (err %v), want false", on, err)
	}
	if on, err := db.SupportNotifyEnabled(ctx, e.d, e.user.ID); err != nil || !on {
		t.Fatalf("tg_notify_support = %v (err %v), want true", on, err)
	}

	// Пустое тело — это не «выключить всё», а ошибка запроса.
	code, body = e.do(t, "PUT", "/v1/me/notify", `{}`, e.userHeaders())
	if code != http.StatusBadRequest {
		t.Fatalf("empty put status = %d, want 400", code)
	}
	var errResp struct {
		Code string `json:"code"`
	}
	json.Unmarshal(body, &errResp)
	if errResp.Code != "notify_no_fields" {
		t.Fatalf("empty put code = %q", errResp.Code)
	}

	code, body = e.do(t, "PUT", "/v1/me/notify", `{"support_reply":false}`, e.userHeaders())
	if code != http.StatusOK {
		t.Fatalf("put support status = %d: %s", code, body)
	}
	if err := json.Unmarshal(body, &prefs); err != nil {
		t.Fatalf("decode put support: %v", err)
	}
	if prefs.AgentWaiting || prefs.SupportReply {
		t.Fatalf("после второго PUT: %+v", prefs)
	}
	if on, err := db.SupportNotifyEnabled(ctx, e.d, e.user.ID); err != nil || on {
		t.Fatalf("tg_notify_support = %v (err %v), want false", on, err)
	}
}
