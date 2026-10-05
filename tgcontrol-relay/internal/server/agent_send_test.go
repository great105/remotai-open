package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"tgcontrol-relay/internal/db"
)

// Тесты POST /v1/agent/send («remotai send»). Telegram подменяем каналом через
// колбэк UserSendText, как pty-notifier подменяет UserNotify.

type agentTextMsg struct {
	chatID     int64
	deviceName string
	text       string
}

func (e *notifyEnv) captureAgentTexts() chan agentTextMsg {
	ch := make(chan agentTextMsg, 32)
	e.srv.UserSendText = func(ctx context.Context, chatID int64, deviceName, text string) error {
		ch <- agentTextMsg{chatID: chatID, deviceName: deviceName, text: text}
		return nil
	}
	return ch
}

type agentSendResp struct {
	status int
	body   map[string]any
}

func (e *notifyEnv) agentSend(t *testing.T, token, body string) agentSendResp {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/agent/send", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return agentSendResp{status: resp.StatusCode, body: out}
}

func ownerTelegramID(t *testing.T, e *notifyEnv) int64 {
	t.Helper()
	var tg int64
	if err := e.srv.DB.QueryRow(`SELECT telegram_id FROM users WHERE id = ?`, ownerUserID(t, e)).Scan(&tg); err != nil {
		t.Fatalf("owner telegram_id: %v", err)
	}
	return tg
}

// Auth: без токена и с битым токеном — 401; живой device JWT чужого
// владельца (user_id в токене не совпадает с хозяином ПК) — 403.
func TestAgentSendAuthFailures(t *testing.T) {
	e := newNotifyEnv(t)
	e.captureAgentTexts()

	if r := e.agentSend(t, "", `{"text":"x"}`); r.status != http.StatusUnauthorized {
		t.Fatalf("без токена: %d, ждали 401", r.status)
	}
	if r := e.agentSend(t, "garbage", `{"text":"x"}`); r.status != http.StatusUnauthorized {
		t.Fatalf("битый токен: %d, ждали 401", r.status)
	}
	foreign, _, err := e.srv.JWT.IssueDevice(notifyDeviceID, 999999)
	if err != nil {
		t.Fatalf("issue foreign device jwt: %v", err)
	}
	if r := e.agentSend(t, foreign, `{"text":"x"}`); r.status != http.StatusForbidden {
		t.Fatalf("чужой токен: %d, ждали 403", r.status)
	}
}

// Базовый сценарий: текст доходит до hook'а с правильными chatID, именем ПК и
// текстом; ответ ok:true sent:["text"].
func TestAgentSendTextOK(t *testing.T) {
	e := newNotifyEnv(t)
	texts := e.captureAgentTexts()

	r := e.agentSend(t, e.devJWT, `{"text":"бэкап готов"}`)
	if r.status != http.StatusOK || r.body["ok"] != true {
		t.Fatalf("ответ: %d %+v", r.status, r.body)
	}
	sent, _ := r.body["sent"].([]any)
	if len(sent) != 1 || sent[0] != "text" {
		t.Fatalf("sent = %v, ждали [text]", r.body["sent"])
	}

	var devName string
	if err := e.srv.DB.QueryRow(`SELECT name FROM devices WHERE id = ?`, notifyDeviceID).Scan(&devName); err != nil {
		t.Fatalf("device name: %v", err)
	}
	select {
	case m := <-texts:
		if m.chatID != ownerTelegramID(t, e) {
			t.Fatalf("chatID = %d, ждали tg владельца", m.chatID)
		}
		if m.deviceName != devName || m.text != "бэкап готов" {
			t.Fatalf("hook получил %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("hook UserSendText не вызван")
	}
}

// Пустой запрос и файл без пути — 400 bad_request.
func TestAgentSendBadRequest(t *testing.T) {
	e := newNotifyEnv(t)
	e.captureAgentTexts()

	for _, body := range []string{`{}`, `{"text":"  "}`, `{"file":{"path":""}}`} {
		r := e.agentSend(t, e.devJWT, body)
		if r.status != http.StatusBadRequest || r.body["code"] != "bad_request" {
			t.Fatalf("%s: %d %+v, ждали 400 bad_request", body, r.status, r.body)
		}
	}
}

// /notify off — не ошибка: 200 ok:false muted:true, hook НЕ вызывается.
func TestAgentSendMuted(t *testing.T) {
	e := newNotifyEnv(t)
	texts := e.captureAgentTexts()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.SetNotifyEnabled(ctx, e.srv.DB, ownerUserID(t, e), false); err != nil {
		t.Fatalf("mute: %v", err)
	}

	r := e.agentSend(t, e.devJWT, `{"text":"бэкап готов"}`)
	if r.status != http.StatusOK || r.body["ok"] != false || r.body["muted"] != true {
		t.Fatalf("ответ: %d %+v, ждали ok:false muted:true", r.status, r.body)
	}
	select {
	case m := <-texts:
		t.Fatalf("hook вызван при выключенных уведомлениях: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

// Лимит 30/час на устройство: в тесте подменяем пакетную переменную на 2.
func TestAgentSendRateLimit(t *testing.T) {
	old := agentSendPerHour
	agentSendPerHour = 2
	t.Cleanup(func() { agentSendPerHour = old })

	e := newNotifyEnv(t)
	e.captureAgentTexts()

	for i := 1; i <= 2; i++ {
		if r := e.agentSend(t, e.devJWT, `{"text":"x"}`); r.status != http.StatusOK {
			t.Fatalf("запрос %d: %d %+v, ждали 200", i, r.status, r.body)
		}
	}
	r := e.agentSend(t, e.devJWT, `{"text":"x"}`)
	if r.status != http.StatusTooManyRequests || r.body["code"] != "rate_limited" {
		t.Fatalf("третий запрос: %d %+v, ждали 429 rate_limited", r.status, r.body)
	}
}

// Файл при офлайн агенте — 502 pc_offline (файл живёт на ПК, забрать не у кого).
func TestAgentSendFileOffline(t *testing.T) {
	e := newNotifyEnv(t)
	e.captureAgentTexts()
	e.srv.UserSendFile = func(ctx context.Context, chatID int64, filename string, r io.Reader) error {
		return nil
	}

	r := e.agentSend(t, e.devJWT, `{"file":{"path":"/tmp/report.bin"}}`)
	if r.status != http.StatusBadGateway || r.body["code"] != "pc_offline" {
		t.Fatalf("ответ: %d %+v, ждали 502 pc_offline", r.status, r.body)
	}
}
