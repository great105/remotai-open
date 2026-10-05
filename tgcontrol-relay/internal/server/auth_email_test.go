package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
)

// fakeMailer перехватывает письма, чтобы тест мог вытащить код.
type fakeMailer struct {
	lastTo      string
	lastSubject string
	lastBody    string
	sent        int
	fail        bool
}

func (f *fakeMailer) Send(to, subject, body string) error {
	if f.fail {
		return errFakeSMTP
	}
	f.lastTo, f.lastSubject, f.lastBody = to, subject, body
	f.sent++
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

const errFakeSMTP = errString("fake smtp failure")

func emailCfg() *config.Config {
	c := testCfg()
	c.SMTPHost = "smtp.test"
	c.SMTPFrom = "Remotai <login@test>"
	c.EmailCodeTTL = 10 * time.Minute
	c.EmailMaxAttempts = 5
	return c
}

func extractCode(t *testing.T, m *fakeMailer) string {
	t.Helper()
	// Тело: "Ваш код входа в Remotai:\n\n123456\n\n..."
	parts := strings.Split(m.lastBody, "\n\n")
	if len(parts) < 2 || len(parts[1]) != 6 {
		t.Fatalf("cannot extract code from body: %q", m.lastBody)
	}
	return parts[1]
}

func postJSON(t *testing.T, url string, body string, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func TestAuthProvidersByRegion(t *testing.T) {
	// RU: tg + email + vk + yandex; google/apple отсутствуют.
	ru := emailCfg()
	ru.VKClientID, ru.VKClientSecret = "vk123", "vks"
	ru.YandexClientID, ru.YandexSecret = "ya123", "yas"
	ru.GoogleClientID, ru.GoogleSecret = "g123", "gs" // заданы, но регион ru → не должен попасть
	ids := map[string]string{}
	for _, p := range ru.AuthProviders() {
		ids[p.ID] = p.Kind
	}
	for _, want := range []string{"telegram", "email", "vk", "yandex"} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("ru: provider %s missing: %v", want, ids)
		}
	}
	if _, ok := ids["google"]; ok {
		t.Fatalf("ru: google must be hidden: %v", ids)
	}

	// Global: tg + email + google; vk/yandex отсутствуют.
	gl := emailCfg()
	gl.Region = "global"
	gl.VKClientID, gl.VKClientSecret = "vk123", "vks"
	gl.GoogleClientID, gl.GoogleSecret = "g123", "gs"
	ids = map[string]string{}
	for _, p := range gl.AuthProviders() {
		ids[p.ID] = p.Kind
	}
	if _, ok := ids["google"]; !ok {
		t.Fatalf("global: google missing: %v", ids)
	}
	if _, ok := ids["vk"]; ok {
		t.Fatalf("global: vk must be hidden: %v", ids)
	}

	// Без SMTP и кредов: только telegram.
	bare := testCfg()
	if got := bare.AuthProviders(); len(got) != 1 || got[0].ID != "telegram" {
		t.Fatalf("bare cfg: want only telegram, got %v", got)
	}
}

func TestEmailLoginEndToEnd(t *testing.T) {
	d := initSQLite(t)
	defer d.Close()
	cfg := emailCfg()
	cfg.BetaFree = true
	srv := New(cfg, d)
	fm := &fakeMailer{}
	srv.Mailer = fm
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	ctx := context.Background()

	// 1) start: код уходит письмом.
	resp := postJSON(t, ts.URL+"/v1/auth/email/start", `{"email":"User@Test.ru"}`, "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("start status %d", resp.StatusCode)
	}
	var start struct {
		LoginToken string `json:"login_token"`
	}
	json.NewDecoder(resp.Body).Decode(&start)
	if start.LoginToken == "" {
		t.Fatalf("empty login_token")
	}
	if fm.sent != 1 || fm.lastTo != "user@test.ru" { // email нормализован в lower
		t.Fatalf("mail not sent or bad recipient: %+v", fm)
	}
	code := extractCode(t, fm)

	// 2) Неверный код → 401, сессия жива (попытка списана).
	resp2 := postJSON(t, ts.URL+"/v1/auth/email/verify",
		`{"login_token":"`+start.LoginToken+`","code":"000000"}`, "")
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("wrong code: want 401, got %d", resp2.StatusCode)
	}
	// Код "000000" мог случайно совпасть с настоящим — тогда тест недетерминирован.
	if code == "000000" {
		t.Skip("flaky: generated code equals the wrong guess")
	}

	// 3) Верный код → JWT, аккаунт создан, identity прикреплена.
	resp3 := postJSON(t, ts.URL+"/v1/auth/email/verify",
		`{"login_token":"`+start.LoginToken+`","code":"`+code+`"}`, "")
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("verify status %d", resp3.StatusCode)
	}
	var out struct {
		JWT    string `json:"jwt"`
		UserID int64  `json:"user_id"`
	}
	json.NewDecoder(resp3.Body).Decode(&out)
	if out.JWT == "" || out.UserID == 0 {
		t.Fatalf("bad verify response: %+v", out)
	}
	u, err := db.GetUserByIdentity(ctx, d, "email", "user@test.ru")
	if err != nil || u.ID != out.UserID {
		t.Fatalf("identity not linked: u=%+v err=%v", u, err)
	}

	// 4) JWT работает на /v1/me.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+out.JWT)
	meResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if meResp.StatusCode != 200 {
		meResp.Body.Close()
		t.Fatalf("me status %d", meResp.StatusCode)
	}
	var me struct {
		Permanent      bool   `json:"permanent"`
		Identities     int    `json:"identities_count"`
		Provider       string `json:"login_provider"`
		Display        string `json:"login_display"`
		Tier           string `json:"tier"`
		EffectiveTier  string `json:"effective_tier"`
		Beta           bool   `json:"beta"`
		BillingEnabled bool   `json:"billing_enabled"`
		MaxDevices     int    `json:"max_devices"`
	}
	if err := json.NewDecoder(meResp.Body).Decode(&me); err != nil {
		meResp.Body.Close()
		t.Fatalf("decode me: %v", err)
	}
	meResp.Body.Close()
	if !me.Permanent || me.Identities != 1 || me.Provider != "email" || me.Display != "user@test.ru" {
		t.Fatalf("identity truth missing from /me: %+v", me)
	}
	if me.Tier != "free" || me.EffectiveTier != "free" || me.Beta || me.BillingEnabled || me.MaxDevices != cfg.FreeMaxDevices {
		t.Fatalf("legacy beta must not grant account access: %+v", me)
	}

	// 5) Single-use: повторный verify → 401.
	resp5 := postJSON(t, ts.URL+"/v1/auth/email/verify",
		`{"login_token":"`+start.LoginToken+`","code":"`+code+`"}`, "")
	resp5.Body.Close()
	if resp5.StatusCode != 401 {
		t.Fatalf("second verify: want 401, got %d", resp5.StatusCode)
	}
}

func TestEmailLoginMergesAnonAccount(t *testing.T) {
	d := initSQLite(t)
	defer d.Close()
	srv := New(emailCfg(), d)
	fm := &fakeMailer{}
	srv.Mailer = fm
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	ctx := context.Background()

	// Анонимный аккаунт с привязанным ПК.
	anon, err := db.CreateAnonUser(ctx, d)
	if err != nil {
		t.Fatalf("anon: %v", err)
	}
	if err := db.InsertDevice(ctx, d, &db.Device{
		ID: "dev-email-merge", UserID: anon.ID, Name: "AnonPC", Platform: "windows",
	}); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	anonJWT, _, err := srv.JWT.IssueUser(anon.ID, anon.TelegramID, anon.Tier)
	if err != nil {
		t.Fatalf("anon jwt: %v", err)
	}

	resp := postJSON(t, ts.URL+"/v1/auth/email/start", `{"email":"merge@test.ru"}`, "")
	var start struct {
		LoginToken string `json:"login_token"`
	}
	json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	code := extractCode(t, fm)

	// Verify с анонимным Bearer → слияние.
	resp2 := postJSON(t, ts.URL+"/v1/auth/email/verify",
		`{"login_token":"`+start.LoginToken+`","code":"`+code+`"}`, anonJWT)
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("verify status %d", resp2.StatusCode)
	}
	var out struct {
		UserID int64 `json:"user_id"`
	}
	json.NewDecoder(resp2.Body).Decode(&out)

	devs, err := db.ListDevicesByUser(ctx, d, out.UserID)
	if err != nil || len(devs) != 1 || devs[0].ID != "dev-email-merge" {
		t.Fatalf("device not merged: %+v err=%v", devs, err)
	}
	if _, err := db.GetUserByID(ctx, d, anon.ID); err == nil {
		t.Fatalf("anon account should be deleted after merge")
	}
}

func TestEmailStartRateLimitPerEmail(t *testing.T) {
	d := initSQLite(t)
	defer d.Close()
	srv := New(emailCfg(), d)
	srv.Mailer = &fakeMailer{}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	for i := 0; i < emailStartRateMax; i++ {
		resp := postJSON(t, ts.URL+"/v1/auth/email/start", `{"email":"flood@test.ru"}`, "")
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("start #%d: status %d", i, resp.StatusCode)
		}
	}
	resp := postJSON(t, ts.URL+"/v1/auth/email/start", `{"email":"flood@test.ru"}`, "")
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("over-limit start: want 429, got %d", resp.StatusCode)
	}
}

func TestEmailStartRejectsBadInput(t *testing.T) {
	d := initSQLite(t)
	defer d.Close()
	srv := New(emailCfg(), d)
	srv.Mailer = &fakeMailer{}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	for _, body := range []string{`{"email":"not-an-email"}`, `{"email":""}`, `{}`} {
		resp := postJSON(t, ts.URL+"/v1/auth/email/start", body, "")
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("start %s: want 400, got %d", body, resp.StatusCode)
		}
	}
}

func TestOAuthLoginEndToEnd(t *testing.T) {
	// Фейковый userinfo для vk/yandex/google: отвечает по пути.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "vk"):
			w.Write([]byte(`{"user":{"user_id":"vk-1","first_name":"Иван","last_name":"Петров"}}`))
		case strings.Contains(r.URL.Path, "yandex"):
			if r.Header.Get("Authorization") != "OAuth good-token" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"id":"ya-1","real_name":"Ян Декс"}`))
		default: // google
			if r.Header.Get("Authorization") != "Bearer good-token" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"sub":"g-1","name":"G User"}`))
		}
	}))
	defer fake.Close()

	oldVK, oldYa, oldG := vkUserInfoURL, yandexUserInfoURL, googleUserInfoURL
	vkUserInfoURL = fake.URL + "/vk"
	yandexUserInfoURL = fake.URL + "/yandex"
	googleUserInfoURL = fake.URL + "/google"
	defer func() { vkUserInfoURL, yandexUserInfoURL, googleUserInfoURL = oldVK, oldYa, oldG }()

	d := initSQLite(t)
	defer d.Close()
	cfg := emailCfg()
	cfg.VKClientID, cfg.VKClientSecret = "vk123", "vks"
	cfg.YandexClientID, cfg.YandexSecret = "ya123", "yas"
	srv := New(cfg, d)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	ctx := context.Background()

	for _, tc := range []struct {
		provider string
		uid      string
	}{
		{"vk", "vk-1"},
		{"yandex", "ya-1"},
	} {
		resp := postJSON(t, ts.URL+"/v1/auth/oauth/"+tc.provider, `{"access_token":"good-token"}`, "")
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", tc.provider, resp.StatusCode)
		}
		var out struct {
			JWT    string `json:"jwt"`
			UserID int64  `json:"user_id"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		if out.JWT == "" {
			t.Fatalf("%s: no jwt", tc.provider)
		}
		if _, err := db.GetUserByIdentity(ctx, d, tc.provider, tc.uid); err != nil {
			t.Fatalf("%s: identity not stored: %v", tc.provider, err)
		}
		// Повторный вход тем же токеном → тот же аккаунт.
		resp2 := postJSON(t, ts.URL+"/v1/auth/oauth/"+tc.provider, `{"access_token":"good-token"}`, "")
		var out2 struct {
			UserID int64 `json:"user_id"`
		}
		json.NewDecoder(resp2.Body).Decode(&out2)
		resp2.Body.Close()
		if out2.UserID != out.UserID {
			t.Fatalf("%s: re-login gave different account: %d vs %d", tc.provider, out2.UserID, out.UserID)
		}
		// Невалидный токен → 401 (фейк yandex/google требует good-token; vk принимает любой).
		if tc.provider != "vk" {
			resp3 := postJSON(t, ts.URL+"/v1/auth/oauth/"+tc.provider, `{"access_token":"bad-token"}`, "")
			resp3.Body.Close()
			if resp3.StatusCode != 401 {
				t.Fatalf("%s bad token: want 401, got %d", tc.provider, resp3.StatusCode)
			}
		}
	}

	// Google выключен регионом ru → 404.
	resp := postJSON(t, ts.URL+"/v1/auth/oauth/google", `{"access_token":"good-token"}`, "")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("google on ru region: want 404, got %d", resp.StatusCode)
	}
}

// Полный redirect-флоу: begin выдаёт authorize_url, callback (из браузера)
// меняет code на токен у фейк-провайдера, poll отдаёт JWT. Single-use.
func TestOAuthRedirectFlow(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				w.WriteHeader(400)
				return
			}
			// VK ID обязан прислать PKCE-verifier и device_id на exchange.
			if r.Form.Get("code_verifier") == "" || r.Form.Get("device_id") == "" {
				w.WriteHeader(400)
				return
			}
			w.Write([]byte(`{"access_token":"at-123"}`))
		case "/userinfo":
			w.Write([]byte(`{"user":{"user_id":"vk-flow-1","first_name":"Флоу"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()

	oldTok, oldInfo := vkTokenURL, vkUserInfoURL
	vkTokenURL = fake.URL + "/token"
	vkUserInfoURL = fake.URL + "/userinfo"
	defer func() { vkTokenURL, vkUserInfoURL = oldTok, oldInfo }()

	d := initSQLite(t)
	defer d.Close()
	cfg := emailCfg()
	cfg.VKClientID, cfg.VKClientSecret = "vk123", "vks"
	cfg.PublicURL = "https://relay.test"
	srv := New(cfg, d)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	ctx := context.Background()

	// 1) begin → state + authorize_url с PKCE и redirect на релей.
	resp := postJSON(t, ts.URL+"/v1/auth/oauth/vk/begin", `{}`, "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("begin status %d", resp.StatusCode)
	}
	var begin struct {
		State        string `json:"state"`
		AuthorizeURL string `json:"authorize_url"`
	}
	json.NewDecoder(resp.Body).Decode(&begin)
	if begin.State == "" || !strings.Contains(begin.AuthorizeURL, "code_challenge=") {
		t.Fatalf("bad begin: %+v", begin)
	}
	if !strings.Contains(begin.AuthorizeURL, url.QueryEscape("https://relay.test/v1/auth/oauth/vk/callback")) {
		t.Fatalf("authorize_url without relay redirect_uri: %s", begin.AuthorizeURL)
	}

	// 2) До callback poll → pending.
	pollURL := ts.URL + "/v1/auth/oauth/poll?state=" + begin.State
	pResp, err := http.Get(pollURL)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	var pending struct {
		Confirmed bool `json:"confirmed"`
		Expired   bool `json:"expired"`
	}
	json.NewDecoder(pResp.Body).Decode(&pending)
	pResp.Body.Close()
	if pending.Confirmed || pending.Expired {
		t.Fatalf("poll before callback should be pending: %+v", pending)
	}

	// 3) Браузер возвращается от провайдера на callback.
	cbResp, err := http.Get(ts.URL + "/v1/auth/oauth/vk/callback?code=code-1&state=" + begin.State)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	cbResp.Body.Close()
	if cbResp.StatusCode != 200 {
		t.Fatalf("callback status %d", cbResp.StatusCode)
	}

	// 4) Poll → JWT, аккаунт создан с identity.
	jwtReq, _ := http.NewRequest("GET", pollURL, nil)
	jwtResp, err := http.DefaultClient.Do(jwtReq)
	if err != nil {
		t.Fatalf("poll2: %v", err)
	}
	var out struct {
		Confirmed bool   `json:"confirmed"`
		JWT       string `json:"jwt"`
		UserID    int64  `json:"user_id"`
	}
	json.NewDecoder(jwtResp.Body).Decode(&out)
	jwtResp.Body.Close()
	if !out.Confirmed || out.JWT == "" {
		t.Fatalf("poll after callback bad: %+v", out)
	}
	if _, err := db.GetUserByIdentity(ctx, d, "vk", "vk-flow-1"); err != nil {
		t.Fatalf("identity not stored: %v", err)
	}

	// 5) Single-use.
	third, err := http.Get(pollURL)
	if err != nil {
		t.Fatalf("poll3: %v", err)
	}
	var exp struct {
		Confirmed bool `json:"confirmed"`
		Expired   bool `json:"expired"`
	}
	json.NewDecoder(third.Body).Decode(&exp)
	third.Body.Close()
	if exp.Confirmed || !exp.Expired {
		t.Fatalf("third poll should be expired: %+v", exp)
	}

	// 6) Тот же redirect-флоу в режиме link не меняет аккаунт и не выдаёт
	// новый JWT: он только подтверждает привязку identity.
	noAuthLink := postJSON(t, ts.URL+"/v1/auth/oauth/vk/begin?link=1", `{}`, "")
	noAuthLink.Body.Close()
	if noAuthLink.StatusCode != http.StatusUnauthorized {
		t.Fatalf("link begin without auth: want 401, got %d", noAuthLink.StatusCode)
	}
	linkBeginResp := postJSON(t, ts.URL+"/v1/auth/oauth/vk/begin?link=1", `{}`, out.JWT)
	var linkBegin struct {
		State string `json:"state"`
	}
	json.NewDecoder(linkBeginResp.Body).Decode(&linkBegin)
	linkBeginResp.Body.Close()
	if linkBeginResp.StatusCode != http.StatusOK || linkBegin.State == "" {
		t.Fatalf("link begin bad: status=%d state=%q", linkBeginResp.StatusCode, linkBegin.State)
	}
	linkCallback, err := http.Get(ts.URL + "/v1/auth/oauth/vk/callback?code=code-link&state=" + linkBegin.State)
	if err != nil {
		t.Fatalf("link callback: %v", err)
	}
	linkCallback.Body.Close()
	if linkCallback.StatusCode != http.StatusOK {
		t.Fatalf("link callback status %d", linkCallback.StatusCode)
	}
	linkPoll, err := http.Get(ts.URL + "/v1/auth/oauth/poll?state=" + linkBegin.State)
	if err != nil {
		t.Fatalf("link poll: %v", err)
	}
	var linked struct {
		Confirmed bool   `json:"confirmed"`
		Linked    bool   `json:"linked"`
		Provider  string `json:"provider"`
		JWT       string `json:"jwt"`
	}
	json.NewDecoder(linkPoll.Body).Decode(&linked)
	linkPoll.Body.Close()
	if !linked.Confirmed || !linked.Linked || linked.Provider != "vk" || linked.JWT != "" {
		t.Fatalf("link poll must not switch account: %+v", linked)
	}
}

// Привязка второго способа входа к существующему аккаунту.
func TestLinkIdentity(t *testing.T) {
	d := initSQLite(t)
	defer d.Close()
	srv := New(emailCfg(), d)
	fm := &fakeMailer{}
	srv.Mailer = fm
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	ctx := context.Background()

	login := func(email string) (jwt string, userID int64) {
		resp := postJSON(t, ts.URL+"/v1/auth/email/start", `{"email":"`+email+`"}`, "")
		var st struct {
			LoginToken string `json:"login_token"`
		}
		json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		code := extractCode(t, fm)
		resp2 := postJSON(t, ts.URL+"/v1/auth/email/verify",
			`{"login_token":"`+st.LoginToken+`","code":"`+code+`"}`, "")
		defer resp2.Body.Close()
		if resp2.StatusCode != 200 {
			t.Fatalf("verify %s: status %d", email, resp2.StatusCode)
		}
		var out struct {
			JWT    string `json:"jwt"`
			UserID int64  `json:"user_id"`
		}
		json.NewDecoder(resp2.Body).Decode(&out)
		return out.JWT, out.UserID
	}

	jwt1, uid1 := login("first@test.ru")

	// Привязываем вторую почту к тому же аккаунту.
	resp := postJSON(t, ts.URL+"/v1/auth/email/start", `{"email":"second@test.ru"}`, "")
	var st2 struct {
		LoginToken string `json:"login_token"`
	}
	json.NewDecoder(resp.Body).Decode(&st2)
	resp.Body.Close()
	code2 := extractCode(t, fm)
	linkResp := postJSON(t, ts.URL+"/v1/me/identities/link",
		`{"provider":"email","login_token":"`+st2.LoginToken+`","code":"`+code2+`"}`, jwt1)
	linkResp.Body.Close()
	if linkResp.StatusCode != 200 {
		t.Fatalf("link status %d", linkResp.StatusCode)
	}

	// Список способов: обе почты.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/me/identities", nil)
	req.Header.Set("Authorization", "Bearer "+jwt1)
	listResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list struct {
		Identities []struct {
			Provider string `json:"provider"`
			UID      string `json:"uid"`
		} `json:"identities"`
	}
	json.NewDecoder(listResp.Body).Decode(&list)
	listResp.Body.Close()
	if len(list.Identities) != 2 {
		t.Fatalf("want 2 identities, got %+v", list.Identities)
	}

	// Вход второй почтой ведёт в ТОТ ЖЕ аккаунт.
	_, uid2 := login("second@test.ru")
	if uid2 != uid1 {
		t.Fatalf("second email gave different account: %d vs %d", uid2, uid1)
	}

	// Чужую identity привязать нельзя: third@test.ru это отдельный аккаунт.
	_, uid3 := login("third@test.ru")
	resp3 := postJSON(t, ts.URL+"/v1/auth/email/start", `{"email":"third@test.ru"}`, "")
	var st3 struct {
		LoginToken string `json:"login_token"`
	}
	json.NewDecoder(resp3.Body).Decode(&st3)
	resp3.Body.Close()
	code3 := extractCode(t, fm)
	conflict := postJSON(t, ts.URL+"/v1/me/identities/link",
		`{"provider":"email","login_token":"`+st3.LoginToken+`","code":"`+code3+`"}`, jwt1)
	conflict.Body.Close()
	if conflict.StatusCode != 409 {
		t.Fatalf("foreign identity: want 409, got %d", conflict.StatusCode)
	}
	// Аккаунт third не пострадал.
	if _, err := db.GetUserByID(ctx, d, uid3); err != nil {
		t.Fatalf("third account broken: %v", err)
	}
}
