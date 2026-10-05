package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"tgcontrol-relay/internal/db"
)

// supportTestEnv — сервер с тестовой БД, юзером и его JWT.
type supportTestEnv struct {
	ts   *httptest.Server
	d    *sql.DB
	srv  *Server
	jwt  string
	user *db.User
}

func newSupportTestEnv(t *testing.T) *supportTestEnv {
	t.Helper()
	d := initSQLite(t)
	t.Cleanup(func() { d.Close() })
	cfg := testCfg()
	cfg.AdminToken = "test-admin-token"
	cfg.AdminIDs = []int64{7777}
	srv := New(cfg, d)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	u, err := db.UpsertUser(ctx, d, 4242, "supuser", "Саппорт", "ru")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	jwt, _, err := srv.JWT.IssueUser(u.ID, u.TelegramID, u.Tier)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	return &supportTestEnv{ts: ts, d: d, srv: srv, jwt: jwt, user: u}
}

func (e *supportTestEnv) do(t *testing.T, method, path, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func (e *supportTestEnv) userHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + e.jwt,
		"Content-Type":  "application/json",
	}
}

func (e *supportTestEnv) adminHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer test-admin-token",
		"Content-Type":  "application/json",
	}
}

func TestSupportUserFlow(t *testing.T) {
	e := newSupportTestEnv(t)

	// Без auth → 401.
	if code, _ := e.do(t, "GET", "/v1/support/messages", "", nil); code != 401 {
		t.Fatalf("no-auth status = %d, want 401", code)
	}

	// Пустая переписка до первого сообщения.
	code, body := e.do(t, "GET", "/v1/support/messages", "", e.userHeaders())
	if code != 200 {
		t.Fatalf("list empty status = %d: %s", code, body)
	}
	var empty struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &empty); err != nil {
		t.Fatalf("list empty decode: %v", err)
	}
	if len(empty.Messages) != 0 {
		t.Fatalf("messages = %d, want 0", len(empty.Messages))
	}

	// Отправка сообщения → {ok, id}.
	code, body = e.do(t, "POST", "/v1/support/messages",
		`{"text":"не открывается терминал","meta":{"client_version":"2.30.0","platform":"android","mode":"cloud"}}`,
		e.userHeaders())
	if code != 200 {
		t.Fatalf("post status = %d: %s", code, body)
	}
	var posted struct {
		OK bool  `json:"ok"`
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &posted); err != nil || !posted.OK || posted.ID == 0 {
		t.Fatalf("post resp = %s, err %v", body, err)
	}
	threadWithMeta, err := db.GetSupportThreadByUser(context.Background(), e.d, e.user.ID)
	if err != nil || !strings.Contains(threadWithMeta.Meta, `"client_version":"2.30.0"`) ||
		!strings.Contains(threadWithMeta.Meta, `"platform":"android"`) {
		t.Fatalf("support context not saved: thread=%+v err=%v", threadWithMeta, err)
	}
	// Метаданные — снимок первого обращения: последующие сообщения не должны
	// незаметно переписать среду, в которой возникла проблема.
	if code, _ := e.do(t, "POST", "/v1/support/messages",
		`{"text":"дополнение","meta":{"client_version":"9.9.9"}}`, e.userHeaders()); code != 200 {
		t.Fatalf("second support message status = %d", code)
	}
	threadAfterSecond, _ := db.GetSupportThreadByUser(context.Background(), e.d, e.user.ID)
	if threadAfterSecond.Meta != threadWithMeta.Meta {
		t.Fatalf("support context overwritten: first=%s second=%s", threadWithMeta.Meta, threadAfterSecond.Meta)
	}

	// Пустой текст → 400.
	if code, _ := e.do(t, "POST", "/v1/support/messages", `{"text":"  "}`, e.userHeaders()); code != 400 {
		t.Fatalf("empty text status = %d, want 400", code)
	}

	// Ответ админа бампит непрочитанные юзера.
	thread, err := db.GetSupportThreadByUser(context.Background(), e.d, e.user.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if _, err := db.PostAdminSupportMessage(context.Background(), e.d, thread.ID, "уже чиним"); err != nil {
		t.Fatalf("admin post: %v", err)
	}
	code, body = e.do(t, "GET", "/v1/support/unread", "", e.userHeaders())
	if code != 200 {
		t.Fatalf("unread status = %d", code)
	}
	var unread struct {
		Count int `json:"count"`
	}
	json.Unmarshal(body, &unread)
	if unread.Count != 1 {
		t.Fatalf("unread = %d, want 1", unread.Count)
	}

	// GET messages возвращает переписку по контракту и ОБНУЛЯЕТ непрочитанные.
	code, body = e.do(t, "GET", "/v1/support/messages", "", e.userHeaders())
	if code != 200 {
		t.Fatalf("list status = %d", code)
	}
	var listed struct {
		Messages []struct {
			ID        int64  `json:"id"`
			Sender    string `json:"sender"`
			Text      string `json:"text"`
			CreatedAt string `json:"created_at"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(listed.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(listed.Messages))
	}
	if listed.Messages[0].Sender != "user" || listed.Messages[0].Text != "не открывается терминал" {
		t.Fatalf("msg[0] = %+v", listed.Messages[0])
	}
	if listed.Messages[1].Sender != "user" || listed.Messages[2].Sender != "admin" {
		t.Fatalf("msg[1] = %+v", listed.Messages[1])
	}
	if _, err := time.Parse(time.RFC3339, listed.Messages[0].CreatedAt); err != nil {
		t.Fatalf("created_at %q не RFC3339: %v", listed.Messages[0].CreatedAt, err)
	}
	// Бейдж сброшен.
	code, body = e.do(t, "GET", "/v1/support/unread", "", e.userHeaders())
	json.Unmarshal(body, &unread)
	if unread.Count != 0 {
		t.Fatalf("unread after list = %d, want 0", unread.Count)
	}
}

func TestSupportAdminFlow(t *testing.T) {
	e := newSupportTestEnv(t)
	ctx := context.Background()

	// Без admin-auth → 403; с юзерским JWT — тоже 403.
	if code, _ := e.do(t, "GET", "/v1/admin/support/threads", "", nil); code != 403 {
		t.Fatalf("no-auth admin status = %d, want 403", code)
	}
	if code, _ := e.do(t, "GET", "/v1/admin/support/threads", "", e.userHeaders()); code != 403 {
		t.Fatalf("user-jwt admin status = %d, want 403", code)
	}

	// Юзер пишет два сообщения.
	if _, _, err := db.PostUserSupportMessage(ctx, e.d, e.user.ID, "первый вопрос"); err != nil {
		t.Fatalf("post 1: %v", err)
	}
	if _, _, err := db.PostUserSupportMessage(ctx, e.d, e.user.ID, "второй вопрос"); err != nil {
		t.Fatalf("post 2: %v", err)
	}

	// Список тредов.
	code, body := e.do(t, "GET", "/v1/admin/support/threads", "", e.adminHeaders())
	if code != 200 {
		t.Fatalf("threads status = %d: %s", code, body)
	}
	var threads struct {
		Threads []struct {
			ID          int64   `json:"id"`
			Username    string  `json:"username"`
			Status      string  `json:"status"`
			UnreadAdmin int     `json:"unread_admin"`
			LastText    string  `json:"last_text"`
			LastMsgAt   *string `json:"last_msg_at"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(body, &threads); err != nil {
		t.Fatalf("threads decode: %v", err)
	}
	if len(threads.Threads) != 1 {
		t.Fatalf("threads = %d, want 1", len(threads.Threads))
	}
	th := threads.Threads[0]
	if th.Username != "supuser" || th.UnreadAdmin != 2 || th.LastText != "второй вопрос" || th.LastMsgAt == nil {
		t.Fatalf("thread = %+v", th)
	}

	// Чтение переписки снимает непрочитанные админа.
	code, body = e.do(t, "GET", "/v1/admin/support/threads/"+itoa64(th.ID)+"/messages", "", e.adminHeaders())
	if code != 200 {
		t.Fatalf("admin messages status = %d: %s", code, body)
	}
	var msgs struct {
		Messages []struct {
			Sender string `json:"sender"`
			Text   string `json:"text"`
		} `json:"messages"`
	}
	json.Unmarshal(body, &msgs)
	if len(msgs.Messages) != 2 || msgs.Messages[0].Sender != "user" {
		t.Fatalf("messages = %+v", msgs)
	}
	if n, _ := db.SupportUnreadForUser(ctx, e.d, e.user.ID); n != 0 {
		t.Fatalf("unread_user = %d, want 0 пока админ не ответил", n)
	}
	if got, _ := db.GetSupportThread(ctx, e.d, th.ID); got.UnreadAdmin != 0 {
		t.Fatalf("unread_admin после GET = %d, want 0", got.UnreadAdmin)
	}

	// Ответ админа → юзер видит непрочитанное.
	code, body = e.do(t, "POST", "/v1/admin/support/threads/"+itoa64(th.ID)+"/messages", `{"text":"ответ админа"}`, e.adminHeaders())
	if code != 200 {
		t.Fatalf("admin post status = %d: %s", code, body)
	}
	if n, _ := db.SupportUnreadForUser(ctx, e.d, e.user.ID); n != 1 {
		t.Fatalf("unread_user после ответа = %d, want 1", n)
	}

	// Toggle close → closed, повторный → open.
	code, body = e.do(t, "POST", "/v1/admin/support/threads/"+itoa64(th.ID)+"/close", "", e.adminHeaders())
	if code != 200 {
		t.Fatalf("close status = %d: %s", code, body)
	}
	var toggled struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	json.Unmarshal(body, &toggled)
	if !toggled.OK || toggled.Status != "closed" {
		t.Fatalf("toggle = %s", body)
	}
	code, body = e.do(t, "POST", "/v1/admin/support/threads/"+itoa64(th.ID)+"/close", "", e.adminHeaders())
	json.Unmarshal(body, &toggled)
	if toggled.Status != "open" {
		t.Fatalf("toggle 2 = %s", body)
	}

	// Несуществующий тред → 404.
	if code, _ := e.do(t, "GET", "/v1/admin/support/threads/9999/messages", "", e.adminHeaders()); code != 404 {
		t.Fatalf("missing thread status = %d, want 404", code)
	}
}

func TestSupportNotifiesAdmin(t *testing.T) {
	e := newSupportTestEnv(t)
	notified := make(chan string, 1)
	e.srv.AdminNotify = func(text string) { notified <- text }

	code, body := e.do(t, "POST", "/v1/support/messages",
		`{"text":"проверка уведомления"}`, e.userHeaders())
	if code != http.StatusOK {
		t.Fatalf("post status = %d: %s", code, body)
	}
	select {
	case msg := <-notified:
		if !strings.Contains(msg, "проверка уведомления") ||
			!strings.Contains(msg, "https://remotai.ru/admin") {
			t.Fatalf("unexpected notification: %q", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("admin notification was not sent")
	}
}

func TestEventsEndpointFlow(t *testing.T) {
	e := newSupportTestEnv(t)

	// Лендинг пишет анонимно.
	code, body := e.do(t, "POST", "/v1/events",
		`{"kind":"landing_visit","utm":{"utm_source":"test"}}`,
		map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK {
		t.Fatalf("landing event status = %d: %s", code, body)
	}
	var n int
	if err := e.d.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='landing_visit'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("landing events = %d, err=%v", n, err)
	}

	// Мусорные события и register_source без auth не принимаются.
	if code, _ := e.do(t, "POST", "/v1/events", `{"kind":"unknown"}`,
		map[string]string{"Content-Type": "application/json"}); code != http.StatusBadRequest {
		t.Fatalf("unknown event status = %d, want 400", code)
	}
	if code, _ := e.do(t, "POST", "/v1/events",
		`{"kind":"register_source","source":"apk"}`,
		map[string]string{"Content-Type": "application/json"}); code != http.StatusBadRequest {
		t.Fatalf("anonymous register_source status = %d, want 400", code)
	}

	// Авторизованный first-touch пишет users.source и ровно одну веху register.
	code, body = e.do(t, "POST", "/v1/events",
		`{"kind":"register_source","source":"apk","utm":{"utm_campaign":"release"}}`,
		e.userHeaders())
	if code != http.StatusOK {
		t.Fatalf("register_source status = %d: %s", code, body)
	}
	var source, utm string
	if err := e.d.QueryRow(`SELECT source, utm_json FROM users WHERE id=?`, e.user.ID).
		Scan(&source, &utm); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if source != "apk" || !strings.Contains(utm, `"utm_campaign":"release"`) {
		t.Fatalf("source=%q utm=%q", source, utm)
	}

	// Повтор не затирает first-touch и не плодит register.
	code, body = e.do(t, "POST", "/v1/events",
		`{"kind":"register_source","source":"web"}`, e.userHeaders())
	if code != http.StatusOK || !strings.Contains(string(body), `"deduped":true`) {
		t.Fatalf("dedupe status=%d body=%s", code, body)
	}
	if err := e.d.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='register' AND user_id=?`, e.user.ID).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("register events = %d, err=%v", n, err)
	}
}

func TestAdminStatsEndpoint(t *testing.T) {
	e := newSupportTestEnv(t)
	ctx := context.Background()

	// Немного данных: событие, счётчик фичи, источник.
	uid := e.user.ID
	if err := db.InsertEvent(ctx, e.d, &uid, "register", "tg", map[string]string{"utm_source": "chan"}, nil); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := db.InsertEvent(ctx, e.d, nil, "landing_visit", "", nil, nil); err != nil {
		t.Fatalf("insert landing: %v", err)
	}
	if err := db.BumpFeatureCounter(ctx, e.d, uid, "terminal"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if _, err := db.SetUserSourceIfEmpty(ctx, e.d, uid, "tg", map[string]string{"utm_source": "chan"}); err != nil {
		t.Fatalf("set source: %v", err)
	}

	code, body := e.do(t, "GET", "/v1/admin/stats", "", e.adminHeaders())
	if code != 200 {
		t.Fatalf("stats status = %d: %s", code, body)
	}
	var st struct {
		Funnel struct {
			LandingVisit int64 `json:"landing_visit"`
			Register     int64 `json:"register"`
		} `json:"funnel"`
		RegsByDay []struct {
			Day string `json:"day"`
			N   int64  `json:"n"`
		} `json:"regs_by_day"`
		Active struct {
			DAU int64 `json:"dau"`
		} `json:"active"`
		Sources []struct {
			Name string `json:"name"`
			N    int64  `json:"n"`
		} `json:"sources"`
		UTM []struct {
			Name string `json:"name"`
			N    int64  `json:"n"`
		} `json:"utm"`
		Features []struct {
			Feature string `json:"feature"`
			N7      int64  `json:"n7"`
			N30     int64  `json:"n30"`
		} `json:"features"`
		RecentUsers []struct {
			Username     string `json:"username"`
			Source       string `json:"source"`
			DevicesCount int64  `json:"devices_count"`
		} `json:"recent_users"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("stats decode: %v", err)
	}
	if st.Funnel.LandingVisit != 1 || st.Funnel.Register != 1 {
		t.Fatalf("funnel = %+v", st.Funnel)
	}
	if len(st.RegsByDay) == 0 {
		t.Fatal("regs_by_day пуст")
	}
	if st.Active.DAU < 1 {
		t.Fatalf("dau = %d, want >= 1 (юзер только что создан с last_seen)", st.Active.DAU)
	}
	if len(st.Sources) == 0 || st.Sources[0].Name != "tg" {
		t.Fatalf("sources = %+v", st.Sources)
	}
	if len(st.UTM) != 1 || st.UTM[0].Name != "chan" || st.UTM[0].N != 1 {
		t.Fatalf("utm = %+v", st.UTM)
	}
	if len(st.Features) != 1 || st.Features[0].Feature != "terminal" || st.Features[0].N7 != 1 {
		t.Fatalf("features = %+v", st.Features)
	}
	if len(st.RecentUsers) == 0 || st.RecentUsers[0].Username != "supuser" || st.RecentUsers[0].Source != "tg" {
		t.Fatalf("recent_users = %+v", st.RecentUsers)
	}
}

func TestAdminPageServed(t *testing.T) {
	e := newSupportTestEnv(t)
	code, body := e.do(t, "GET", "/admin", "", nil)
	if code != 200 {
		t.Fatalf("/admin status = %d", code)
	}
	if !strings.Contains(string(body), "telegram-web-app.js") {
		t.Fatal("в HTML нет Telegram Web App SDK")
	}
	if !strings.Contains(string(body), `https://t.me/TestBot?start=admin`) {
		t.Fatal("имя бота не подставлено в admin deep-link")
	}
	if strings.Contains(string(body), "__BOT_USERNAME__") {
		t.Fatal("плейсхолдер __BOT_USERNAME__ не заменён")
	}
	// Admin API по tma с битой подписью → 403.
	h := map[string]string{"Authorization": "tma id=7777&auth_date=1&hash=bad"}
	if code, _ := e.do(t, "GET", "/v1/admin/stats", "", h); code != 403 {
		t.Fatalf("bad tma status = %d, want 403", code)
	}

	// Основной production-вход: подписанный Mini App initData allowlisted админа.
	adminInitData := buildInitData(t, e.srv.Config.BotToken, 7777, "admin")
	h = map[string]string{"Authorization": "tma " + adminInitData}
	if code, _ := e.do(t, "GET", "/v1/admin/stats", "", h); code != 200 {
		t.Fatalf("Mini App admin status = %d, want 200", code)
	}
	otherInitData := buildInitData(t, e.srv.Config.BotToken, 8888, "other")
	h = map[string]string{"Authorization": "tma " + otherInitData}
	if code, _ := e.do(t, "GET", "/v1/admin/stats", "", h); code != 403 {
		t.Fatalf("non-admin Mini App status = %d, want 403", code)
	}
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
