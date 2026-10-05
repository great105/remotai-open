package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/notify"
	"tgcontrol-relay/internal/protocol"
)

// Тесты релейного notifier'а. Telegram здесь не нужен: сервер отдаёт готовую
// notify.Notice через колбэк UserNotify, его и подменяем каналом.

type notifyEnv struct {
	srv      *Server
	ts       *httptest.Server
	wsBase   string
	initData string
	devJWT   string
	notices  chan notify.Notice
}

const notifyDeviceID = "dev-notify"

func newNotifyEnv(t *testing.T) *notifyEnv {
	t.Helper()
	const botToken = "1234:ABCDEF"
	cfg := &config.Config{
		BotToken:        botToken,
		JWTSecret:       "test-secret-please-change-1234567890",
		JWTTTL:          time.Hour,
		PairCodeTTL:     time.Minute,
		PairMaxAttempts: 5,
		FreeMaxDevices:  3,
		ProMaxDevices:   10,
		TeamMaxDevices:  50,
		// Transport fixtures have a real trial; BETA_FREE no longer grants access.
		TrialDays:   30,
		BotUsername: "TestBot",
		CORSOrigins: []string{"*"},

		NotifyTG:         true,
		NotifyDelay:      50 * time.Millisecond,
		NotifyMaxPerHour: 100,
		// agent_version в тестовом пейринге — "e2e", версионный гейт кнопок
		// ответа для него не применим.
		NotifyMinAgent: "any",
	}
	d := initSQLite(t)
	t.Cleanup(func() { d.Close() })

	srv := New(cfg, d)
	notices := make(chan notify.Notice, 16)
	srv.UserNotify = func(ctx context.Context, chatID int64, n notify.Notice) error {
		notices <- n
		return nil
	}
	srv.Notifier = NewNotifier(srv)
	srv.Notifier.ptyCooldown = time.Minute

	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	devJWT, initData := pairTestDevice(t, ts, botToken, notifyDeviceID, 6161)
	if _, err := d.Exec(`UPDATE users SET trial_end=datetime('now','+30 days')`); err != nil {
		t.Fatal(err)
	}
	return &notifyEnv{
		srv: srv, ts: ts, wsBase: "ws" + strings.TrimPrefix(ts.URL, "http"),
		initData: initData, devJWT: devJWT, notices: notices,
	}
}

// fakeAgent — тестовый ПК: держит control-WS, отвечает на Cmd и умеет слать
// события pty.
type fakeAgent struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	inputs  chan agentInput // то, что дошло до POST /api/pty/{id}/input
	// stateDelay — насколько тормозит ответ на GET /api/pty/{id}/state. Нужен,
	// чтобы попасть событием ровно в окно, пока fire() ждёт ПК.
	stateDelay atomic.Int64
}

type agentInput struct {
	key            string
	expectStatusAt int64
}

// dialAgent поднимает соединение агента. inputStatus/inputBody задают ответ на
// /input, чтобы проверить старую версию агента (404) и закрытый терминал.
func (e *notifyEnv) dialAgent(t *testing.T, inputStatus int, inputBody string) *fakeAgent {
	t.Helper()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+e.devJWT)
	conn, _, err := websocket.DefaultDialer.Dial(e.wsBase+"/v1/agent/connect", hdr)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	if err := conn.WriteJSON(protocol.Hello{
		Type: protocol.MsgHello, ProtocolVersion: protocol.ProtocolVersion,
		DeviceID: notifyDeviceID, JWT: e.devJWT, Platform: "linux", Hostname: "h",
	}); err != nil {
		t.Fatalf("agent hello: %v", err)
	}
	var welcome protocol.Welcome
	if err := conn.ReadJSON(&welcome); err != nil || welcome.Type != protocol.MsgWelcome {
		t.Fatalf("agent welcome: %v (%+v)", err, welcome)
	}

	ag := &fakeAgent{conn: conn, inputs: make(chan agentInput, 8)}
	t.Cleanup(func() { conn.Close() })
	go func() {
		for {
			var raw json.RawMessage
			if err := conn.ReadJSON(&raw); err != nil {
				return
			}
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &head) != nil || head.Type != protocol.MsgCmd {
				continue
			}
			var cmd protocol.Cmd
			if json.Unmarshal(raw, &cmd) != nil {
				continue
			}
			res := protocol.CmdResult{Type: protocol.MsgCmdResult, RequestID: cmd.RequestID}
			switch {
			case strings.HasSuffix(cmd.Path, "/state"):
				if d := ag.stateDelay.Load(); d > 0 {
					time.Sleep(time.Duration(d))
				}
				res.StatusCode = http.StatusOK
				res.Body = []byte(`{"status":"waiting","status_at":1721937600000,` +
					`"hint":"Подтвердите: y/n","hint_kind":"yes_no","name":"build","alive":true}`)
			case strings.HasSuffix(cmd.Path, "/input"):
				var in struct {
					Key            string `json:"key"`
					ExpectStatusAt int64  `json:"expect_status_at"`
				}
				_ = json.Unmarshal(cmd.Body, &in)
				select {
				case ag.inputs <- agentInput{key: in.Key, expectStatusAt: in.ExpectStatusAt}:
				default:
				}
				res.StatusCode = inputStatus
				res.Body = []byte(inputBody)
			default:
				res.StatusCode = http.StatusNotFound
				res.Body = []byte("404 page not found\n")
			}
			ag.writeMu.Lock()
			_ = conn.WriteJSON(res)
			ag.writeMu.Unlock()
		}
	}()
	return ag
}

func (a *fakeAgent) sendPtyEvent(t *testing.T, payload map[string]any) {
	t.Helper()
	a.sendPtyEventAt(t, payload, time.Now())
}

// sendPtyEventAt — отправка с явным created_at (моментом, когда агент отдал
// событие релею). Обе метки — created_at и payload.ts — идут по часам ПК,
// поэтому сдвинув их вместе, получаем ПК с уехавшими часами.
func (a *fakeAgent) sendPtyEventAt(t *testing.T, payload map[string]any, createdAt time.Time) {
	t.Helper()
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if err := a.conn.WriteJSON(protocol.Event{
		Type: protocol.MsgEvent, Channel: "events", Payload: payload,
		CreatedAt: createdAt.Unix(),
	}); err != nil {
		t.Fatalf("send event: %v", err)
	}
}

func waitingPayload(ptyID, hint string) map[string]any {
	return map[string]any{
		"type": "pty_event", "event": "waiting_input", "pty_id": ptyID,
		"name": "build", "agent": "claude", "fg_process": "node",
		"hint": hint, "hint_kind": "yes_no", "ts": time.Now().UnixMilli(),
	}
}

// errorPayload — «в терминале мелькнула ошибка»: другой kind на ТОМ ЖЕ терминале.
func errorPayload(ptyID string) map[string]any {
	return map[string]any{
		"type": "pty_event", "event": "error", "pty_id": ptyID,
		"name": "build", "agent": "claude", "fg_process": "node",
		"hint": "npm ERR! code E404", "ts": time.Now().UnixMilli(),
	}
}

func (e *notifyEnv) expectNotice(t *testing.T, within time.Duration) notify.Notice {
	t.Helper()
	select {
	case n := <-e.notices:
		return n
	case <-time.After(within):
		t.Fatalf("уведомление не пришло за %s", within)
		return notify.Notice{}
	}
}

func (e *notifyEnv) expectSilence(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case n := <-e.notices:
		t.Fatalf("ждали тишину, а пришло уведомление: %+v", n)
	case <-time.After(within):
	}
}

// Базовый сценарий: агент задал вопрос, приложение закрыто → одно сообщение с
// вопросом и кнопками ответа.
func TestNotifierSendsWaitingNotice(t *testing.T) {
	e := newNotifyEnv(t)
	ag := e.dialAgent(t, http.StatusOK, `{"ok":true}`)

	ag.sendPtyEvent(t, waitingPayload("pty1", "Подтвердите: y/n"))

	n := e.expectNotice(t, 3*time.Second)
	if n.Kind != notify.KindWaiting || n.PtyID != "pty1" || n.DeviceID != notifyDeviceID {
		t.Fatalf("не то уведомление: %+v", n)
	}
	if n.Hint == "" || n.HintKind != "yes_no" {
		t.Fatalf("вопрос потерян: %+v", n)
	}
	if !n.AgentOnline || !n.CanReply {
		t.Fatalf("ПК на связи — ждали кнопки ответа: %+v", n)
	}
	if n.Agent != "claude" || n.PtyName != "build" {
		t.Fatalf("метаданные терминала потеряны: %+v", n)
	}
	// status_at переспрошен у ПК и уедет в кнопку: ответ из этого сообщения не
	// должен примениться к следующему вопросу агента.
	if n.StatusAt != 1721937600000 {
		t.Fatalf("метка эпизода не доехала: %+v", n)
	}
}

// Человек в приложении (жив control-WS) — дублировать вопрос в Telegram нельзя.
func TestNotifierSilentWhenClientPresent(t *testing.T) {
	e := newNotifyEnv(t)
	ag := e.dialAgent(t, http.StatusOK, `{"ok":true}`)

	client, _, err := websocket.DefaultDialer.Dial(
		e.wsBase+"/v1/client/"+notifyDeviceID+"/ws?initData="+url.QueryEscape(e.initData), nil)
	if err != nil {
		t.Fatalf("client ws dial: %v", err)
	}
	defer client.Close()
	// Дожидаемся, пока хендлер успеет отметить присутствие.
	deadline := time.Now().Add(2 * time.Second)
	for !e.srv.Hub.ClientActive(notifyDeviceID, ownerUserID(t, e)) {
		if time.Now().After(deadline) {
			t.Fatalf("присутствие клиента не зарегистрировалось")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ag.sendPtyEvent(t, waitingPayload("pty1", "Подтвердите: y/n"))
	e.expectSilence(t, 700*time.Millisecond)
}

// Латч на терминал: один эпизод простоя = одно сообщение, даже если агент
// повторит событие (переподключение, реплей).
func TestNotifierLatchesEpisodePerTerminal(t *testing.T) {
	e := newNotifyEnv(t)
	ag := e.dialAgent(t, http.StatusOK, `{"ok":true}`)

	ag.sendPtyEvent(t, waitingPayload("pty1", "Подтвердите: y/n"))
	e.expectNotice(t, 3*time.Second)

	for i := 0; i < 3; i++ {
		ag.sendPtyEvent(t, waitingPayload("pty1", "Подтвердите: y/n"))
	}
	e.expectSilence(t, 500*time.Millisecond)
}

// Кулдаун держится на ТЕРМИНАЛЕ, а не на вопросе. hint агент пересчитывает по
// видимому хвосту — он «дрожит» при перерисовке, а waiting_input и error
// чередуются: каждый раз это новый эпизод. Пока отметка отправки лежала внутри
// эпизода, она умирала вместе с ним, и за ночь набегало под сотню сообщений.
func TestNotifierCooldownSurvivesQuestionChange(t *testing.T) {
	e := newNotifyEnv(t)
	ag := e.dialAgent(t, http.StatusOK, `{"ok":true}`)

	ag.sendPtyEvent(t, waitingPayload("pty1", "Подтвердите: y/n"))
	e.expectNotice(t, 3*time.Second)

	// hint A → hint B → hint A: три разных эпизода на одном терминале.
	ag.sendPtyEvent(t, waitingPayload("pty1", "Продолжить? (y/n)"))
	e.expectSilence(t, 400*time.Millisecond)
	ag.sendPtyEvent(t, waitingPayload("pty1", "Подтвердите: y/n"))
	e.expectSilence(t, 400*time.Millisecond)
	// И другой kind на том же терминале — тот же кулдаун.
	ag.sendPtyEvent(t, errorPayload("pty1"))
	e.expectSilence(t, 400*time.Millisecond)

	// Кулдаун держит именно ЭТОТ терминал: соседний молчать не обязан.
	ag.sendPtyEvent(t, waitingPayload("pty2", "Подтвердите: y/n"))
	if n := e.expectNotice(t, 3*time.Second); n.PtyID != "pty2" {
		t.Fatalf("ждали уведомление по второму терминалу, пришло: %+v", n)
	}
}

// Повтор события, пока fire() ходит к ПК, не должен давать второго сообщения:
// отметка «эпизод в работе» ставится ДО блокирующих проверок.
func TestNotifierNoDoubleSendWhileFiring(t *testing.T) {
	e := newNotifyEnv(t)
	ag := e.dialAgent(t, http.StatusOK, `{"ok":true}`)
	ag.stateDelay.Store(int64(400 * time.Millisecond))

	ag.sendPtyEvent(t, waitingPayload("pty1", "Подтвердите: y/n"))
	// holdDelay = 50 мс, ответ ПК тормозит 400 мс — попадаем ровно в окно,
	// пока fire() ждёт /state.
	time.Sleep(200 * time.Millisecond)
	ag.sendPtyEvent(t, waitingPayload("pty1", "Подтвердите: y/n"))

	e.expectNotice(t, 3*time.Second)
	e.expectSilence(t, time.Second)
}

// Уехавшие часы ПК (спящий ноутбук, VM без NTP, WSL) не должны выглядеть
// реплеем истории: возраст события считаем по часам самого ПК (created_at и ts
// проставляет он же), а свежесть — по времени прихода на релей.
func TestNotifierSkewedAgentClockStillNotifies(t *testing.T) {
	e := newNotifyEnv(t)
	ag := e.dialAgent(t, http.StatusOK, `{"ok":true}`)

	behind := time.Now().Add(-3 * time.Hour) // часы ПК отстают на три часа
	payload := waitingPayload("pty1", "Подтвердите: y/n")
	payload["ts"] = behind.UnixMilli()
	ag.sendPtyEventAt(t, payload, behind)

	if n := e.expectNotice(t, 3*time.Second); n.PtyID != "pty1" {
		t.Fatalf("не то уведомление: %+v", n)
	}
}

// Шум наружу не уходит: «просто освободился» (пустой hint), finished и старый
// реплей истории уведомлением не становятся.
func TestNotifierIgnoresNoise(t *testing.T) {
	e := newNotifyEnv(t)
	ag := e.dialAgent(t, http.StatusOK, `{"ok":true}`)

	ag.sendPtyEvent(t, waitingPayload("pty-empty", "")) // агент освободился
	ag.sendPtyEvent(t, map[string]any{
		"type": "pty_event", "event": "finished", "pty_id": "pty-fin",
		"name": "build", "ts": time.Now().UnixMilli(),
	})
	stale := waitingPayload("pty-stale", "Подтвердите: y/n")
	stale["ts"] = time.Now().Add(-10 * time.Minute).UnixMilli() // реплей истории
	ag.sendPtyEvent(t, stale)

	e.expectSilence(t, 500*time.Millisecond)
}

// Ответ кнопкой доходит до ПК ровно тем ключом, который нажали.
func TestSendPtyInputDeliversKey(t *testing.T) {
	e := newNotifyEnv(t)
	ag := e.dialAgent(t, http.StatusOK, `{"ok":true}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := notify.InputRequest{
		UserID: ownerUserID(t, e), DeviceID: notifyDeviceID, PtyID: "pty1",
		Key: "y", ExpectStatusAt: 1721937600000,
	}
	if err := e.srv.SendPtyInput(ctx, req); err != nil {
		t.Fatalf("SendPtyInput: %v", err)
	}
	select {
	case got := <-ag.inputs:
		if got.key != "y" {
			t.Fatalf("до ПК дошёл ключ %q, ждали \"y\"", got.key)
		}
		// Метка эпизода обязана доехать: без неё ПК не отличит «отвечаю на этот
		// вопрос» от «отвечаю на вопрос, который агент уже сменил».
		if got.expectStatusAt != req.ExpectStatusAt {
			t.Fatalf("expect_status_at=%d, ждали %d", got.expectStatusAt, req.ExpectStatusAt)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("ввод не дошёл до агента")
	}

	// Произвольные байты через этот путь не проходят никогда.
	bad := notify.InputRequest{
		UserID: req.UserID, DeviceID: notifyDeviceID, PtyID: "pty1", Key: "rm -rf /\r",
	}
	if err := e.srv.SendPtyInput(ctx, bad); !errors.Is(err, notify.ErrKeyNotAllowed) {
		t.Fatalf("SendPtyInput с произвольной строкой = %v, ждали ErrKeyNotAllowed", err)
	}
}

// Право писать в терминал проверяет САМ сервер: чужой аккаунт (и вызывающий,
// забывший проставить UserID) до ПК не доходит, даже если проверку у себя не
// сделал.
func TestSendPtyInputChecksAccess(t *testing.T) {
	e := newNotifyEnv(t)
	ag := e.dialAgent(t, http.StatusOK, `{"ok":true}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stranger, err := db.UpsertUser(ctx, e.srv.DB, 777001, "stranger", "Чужой", "ru")
	if err != nil {
		t.Fatalf("создать чужого пользователя: %v", err)
	}
	cases := []struct {
		name string
		uid  int64
	}{
		{"чужой аккаунт", stranger.ID},
		{"вызывающий не указал пользователя", 0},
	}
	for _, c := range cases {
		err := e.srv.SendPtyInput(ctx, notify.InputRequest{
			UserID: c.uid, DeviceID: notifyDeviceID, PtyID: "pty1", Key: "y",
		})
		if !errors.Is(err, notify.ErrNoAccess) {
			t.Fatalf("%s: %v, ждали ErrNoAccess", c.name, err)
		}
	}
	select {
	case in := <-ag.inputs:
		t.Fatalf("ввод без прав дошёл до ПК: %+v", in)
	case <-time.After(300 * time.Millisecond):
	}
}

// Честные ошибки: выключенный ПК и старая версия агента различимы боту.
func TestSendPtyInputErrors(t *testing.T) {
	e := newNotifyEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := notify.InputRequest{
		UserID: ownerUserID(t, e), DeviceID: notifyDeviceID, PtyID: "pty1", Key: "y",
	}

	if err := e.srv.SendPtyInput(ctx, req); !errors.Is(err, notify.ErrOffline) {
		t.Fatalf("офлайн-ПК: %v, ждали ErrOffline", err)
	}

	// Агент старой версии: роут не зарегистрирован → net/http отдаёт текстовый 404.
	e.dialAgent(t, http.StatusNotFound, "404 page not found\n")
	if err := e.srv.SendPtyInput(ctx, req); !errors.Is(err, notify.ErrUnsupported) {
		t.Fatalf("старый агент: %v, ждали ErrUnsupported", err)
	}
}

// Машинные коды агента (api_pty.go) → понятные боту причины отказа.
func TestSendPtyInputAgentCodes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"закрытый терминал", http.StatusNotFound, `{"error":"session not found","code":"pty_not_found"}`, notify.ErrPtyGone},
		{"процесс умер", http.StatusGone, `{"error":"session is not running","code":"pty_dead"}`, notify.ErrPtyGone},
		{"вопрос сменился", http.StatusConflict, `{"error":"asking about something else","code":"prompt_changed"}`, notify.ErrPromptChanged},
		{"слишком часто", http.StatusTooManyRequests, `{"error":"too many requests","code":"rate_limited"}`, notify.ErrRateLimited},
		{"слишком часто без кода", http.StatusTooManyRequests, `{"error":"Too many requests"}`, notify.ErrRateLimited},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newNotifyEnv(t)
			e.dialAgent(t, c.status, c.body)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := e.srv.SendPtyInput(ctx, notify.InputRequest{
				UserID:   ownerUserID(t, e),
				DeviceID: notifyDeviceID, PtyID: "pty1", Key: "y", ExpectStatusAt: 1,
			})
			if !errors.Is(err, c.want) {
				t.Fatalf("%s: %v, ждали %v", c.name, err, c.want)
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"2.30.0", "2.30.0", true},
		{"2.30.1", "2.30.0", true},
		{"3.0.0", "2.30.0", true},
		{"2.29.9", "2.30.0", false},
		{"v2.31.0", "2.30.0", true},
		{"2.30.0-rc1", "2.30.0", true},
		{"e2e", "2.30.0", false},
		{"", "2.30.0", false},
		{"2.9", "2.30.0", false},
	}
	for _, c := range cases {
		if got := versionAtLeast(c.have, c.want); got != c.ok {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

// ownerUserID — id владельца тестового устройства.
func ownerUserID(t *testing.T, e *notifyEnv) int64 {
	t.Helper()
	var uid int64
	if err := e.srv.DB.QueryRow(`SELECT user_id FROM devices WHERE id = ?`, notifyDeviceID).Scan(&uid); err != nil {
		t.Fatalf("owner lookup: %v", err)
	}
	return uid
}
