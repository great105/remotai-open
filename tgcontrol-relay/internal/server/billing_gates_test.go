package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"tgcontrol-relay/internal/billing"
	"tgcontrol-relay/internal/db"
)

// Гейты на входе в оплату. 01.09.2026 все три оказались закрыты не перед тем,
// перед кем надо, и вместе давали ноль оплат: проверочный платёж владельцу был
// запрещён как основателю, а обычный человек упирался в почту, которую негде
// было ввести. Проверяются одним набором, потому что человек проходит их
// подряд и застревает на первом же.
type payEnv struct {
	ts    *httptest.Server
	d     *sql.DB
	srv   *Server
	admin string // JWT администратора (он же основатель — как у владельца)
	plain string // JWT обычного платящего человека
}

const payAdminTG = int64(5001)

func newPayEnv(t *testing.T) *payEnv {
	t.Helper()
	d := initSQLite(t)
	t.Cleanup(func() { d.Close() })
	cfg := testCfg()
	cfg.AdminIDs = []int64{payAdminTG}
	cfg.BillingEnabled = true
	// Непустые реквизиты: без них checkout отвечает 501 «оплата не подключена»
	// ещё до гейтов, и проверять было бы нечего. До сети тесты не доходят —
	// все проверки ниже срабатывают раньше обращения к кассе.
	cfg.YooKassaShopID = "1451249"
	cfg.YooKassaSecretKey = "test_secret"
	srv := New(cfg, d)
	// Every provider request stays on localhost, including error-path tests.
	kassa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"description":"simulated provider unavailable"}`, http.StatusServiceUnavailable)
	}))
	t.Cleanup(kassa.Close)
	client := billing.New("unit_shop", "unit_secret")
	client.HTTP = kassa.Client()
	client.HTTP.Transport = rewriteTo{base: kassa.URL, rt: kassa.Client().Transport}
	srv.BillingClient = client
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	admin, err := db.UpsertUser(ctx, d, payAdminTG, "owner", "Владелец", "ru")
	if err != nil {
		t.Fatalf("upsert admin: %v", err)
	}
	// Владелец — основатель: именно это сочетание и ломало проверочный платёж.
	if _, err := d.Exec(`UPDATE users SET founder = 1 WHERE id = ?`, admin.ID); err != nil {
		t.Fatalf("founder: %v", err)
	}
	plain, err := db.UpsertUser(ctx, d, 6002, "buyer", "Покупатель", "ru")
	if err != nil {
		t.Fatalf("upsert plain: %v", err)
	}
	e := &payEnv{ts: ts, d: d, srv: srv}
	e.admin = issue(t, srv, admin)
	e.plain = issue(t, srv, plain)
	return e
}

func issue(t *testing.T, srv *Server, u *db.User) string {
	t.Helper()
	tok, _, err := srv.JWT.IssueUser(u.ID, u.TelegramID, u.Tier)
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}
	return tok
}

func (e *payEnv) post(t *testing.T, jwt, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+path, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *payEnv) sub(t *testing.T, jwt string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/v1/billing/subscription", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET subscription: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscription: код %d", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// Кнопку проверочного платежа рисует сервер, и видеть её должен только
// администратор: идентификатор полки виден в исходниках приложения, «нет на
// витрине» защитой не считается.
func TestCanTestPayOnlyAdmin(t *testing.T) {
	e := newPayEnv(t)
	if got := e.sub(t, e.admin)["can_test_pay"]; got != true {
		t.Errorf("администратор не видит проверочный платёж: %v", got)
	}
	if got := e.sub(t, e.plain)["can_test_pay"]; got != false {
		t.Errorf("обычный пользователь видит служебную полку: %v", got)
	}
}

// ⚠ Проверка живыми деньгами нужна именно владельцу, а он основатель. До
// 01.09.2026 запрет «основателям платить не за что» отвечал 409 и на служебную
// полку — кнопка была, нажатие не работало.
func TestFounderCanPayHiddenTierButNotReal(t *testing.T) {
	e := newPayEnv(t)

	code, body := e.post(t, e.admin, "/v1/billing/checkout", `{"tier":"pro"}`)
	if code != http.StatusConflict {
		t.Errorf("основателю продали Про: код %d, %v", code, body)
	}

	// Служебную полку тот же человек проходит: до кассы его останавливает уже
	// только почта — то есть гейт основателя больше не срабатывает.
	code, body = e.post(t, e.admin, "/v1/billing/checkout", `{"tier":"test"}`)
	if code == http.StatusConflict {
		t.Fatalf("проверочный платёж всё ещё запрещён основателю: %v", body)
	}
	if code != http.StatusBadRequest || body["code"] != "email_required" {
		t.Fatalf("ожидали запрос почты, получили %d %v", code, body)
	}
}

// Служебная полка закрыта на сервере, а не только спрятана с витрины.
func TestHiddenTierForbiddenForOthers(t *testing.T) {
	e := newPayEnv(t)
	code, body := e.post(t, e.plain, "/v1/billing/checkout", `{"tier":"test"}`)
	if code != http.StatusForbidden {
		t.Errorf("чужой купил доступ за 10 ₽: код %d, %v", code, body)
	}
}

// Почта: спрашиваем один раз. Без неё — машинный код, по которому приложение
// показывает поле, а не тупик «что-то пошло не так».
func TestBillingEmailAskedOnceAndRemembered(t *testing.T) {
	e := newPayEnv(t)
	ctx := context.Background()

	code, body := e.post(t, e.plain, "/v1/billing/checkout", `{"tier":"pro"}`)
	if code != http.StatusBadRequest || body["code"] != "email_required" {
		t.Fatalf("без почты ожидали email_required, получили %d %v", code, body)
	}
	// ⚠ Текст этого отказа видит ТОЛЬКО тот, чьё приложение не понимает код
	// email_required, — то есть не обновилось. Для него поле почты не появится
	// никогда, и текст обязан говорить, что делать, а не только что не так.
	if msg, _ := body["error"].(string); !strings.Contains(msg, "Обновите приложение") {
		t.Errorf("старому приложению не сказано, как выйти из тупика: %q", msg)
	}

	// Кривой адрес отсекается до кассы, отдельным кодом.
	code, body = e.post(t, e.plain, "/v1/billing/checkout", `{"tier":"pro","email":"почта-без-собаки"}`)
	if code != http.StatusBadRequest || body["code"] != "email_invalid" {
		t.Fatalf("кривой адрес принят: %d %v", code, body)
	}

	// Настоящий адрес запоминается: дальше идёт обращение к кассе (её в тесте
	// нет — важно, что почта осела в аккаунте).
	e.post(t, e.plain, "/v1/billing/checkout", `{"tier":"pro","email":"buyer@example.com"}`)
	u, err := db.GetUserByTelegram(ctx, e.d, 6002)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u.BillingEmail != "buyer@example.com" {
		t.Fatalf("почта не запомнена: %q", u.BillingEmail)
	}

	// Второй раз не спрашиваем — гейт почты пройден без поля в запросе.
	code, body = e.post(t, e.plain, "/v1/billing/checkout", `{"tier":"pro"}`)
	if code == http.StatusBadRequest && body["code"] == "email_required" {
		t.Fatal("почту спросили повторно, хотя она сохранена")
	}
}

// Срок в описании платежа человек видит в банке и в чеке. У служебной полки он
// суточный: «Проверка оплаты, 1 месяц» было бы неправдой.
func TestPaymentDescriptionPeriod(t *testing.T) {
	if got := planPeriodName("test"); got != "сутки" {
		t.Errorf("служебная полка: %q", got)
	}
	if got := planPeriodName("pro"); got != "30 дней" {
		t.Errorf("обычная полка: %q", got)
	}
}

// rewriteTo перенаправляет запросы клиента кассы на тестовый сервер: адрес
// api.yookassa.ru зашит в пакете billing константой.
type rewriteTo struct {
	base string
	rt   http.RoundTripper
}

func (r rewriteTo) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(r.base)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
	return r.rt.RoundTrip(req)
}

// ⚠ Худшее, что может случиться на экране денег: списали и не открыли доступ.
//
// Единственный путь зачисления — уведомление от ЮKassa на адрес, заданный в
// личном кабинете магазина. Проверить, задан ли он, из кода нельзя: API
// /v3/webhooks по ключу магазина отвечает «Authentication type is not allowed»
// (проверено живым запросом 01.09.2026). Значит на доставку полагаться нельзя —
// кабинет обязан досмотреть свои незакрытые платежи сам.
func TestCabinetSettlesPaymentWithoutWebhook(t *testing.T) {
	e := newPayEnv(t)
	ctx := context.Background()

	var asked int
	kassa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"pay-1","status":"succeeded","paid":true,
			"amount":{"value":"599.00","currency":"RUB"},
			"payment_method":{"type":"sbp","saved":false}}`))
	}))
	defer kassa.Close()
	c := billing.New("1451249", "test_secret")
	c.HTTP = kassa.Client()
	c.HTTP.Transport = rewriteTo{base: kassa.URL, rt: kassa.Client().Transport}
	e.srv.BillingClient = c

	u, err := db.GetUserByTelegram(ctx, e.d, 6002)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	// Человек оплатил, ЮKassa нам об этом не сказала.
	if err := db.RecordPayment(ctx, e.d, db.Payment{
		PaymentID: "pay-1", UserID: u.ID, Tier: "pro", AmountMinor: 59900, Status: "pending",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	out := e.sub(t, e.plain)
	if out["paid_until"] == nil {
		t.Fatalf("оплата не зачтена при открытии кабинета: %v", out)
	}
	if out["tier"] != "pro" {
		t.Errorf("доступ не открылся: tier=%v", out["tier"])
	}

	// Повторное открытие не продлевает второй раз за те же деньги.
	before, err := db.GetSubscription(ctx, e.d, u.ID)
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	e.sub(t, e.plain)
	after, err := db.GetSubscription(ctx, e.d, u.ID)
	if err != nil {
		t.Fatalf("sub2: %v", err)
	}
	if !before.PaidUntil.Time.Equal(after.PaidUntil.Time) {
		t.Fatalf("подписка продлилась дважды за один платёж: %v → %v",
			before.PaidUntil.Time, after.PaidUntil.Time)
	}
	// И кассу второй раз не дёргаем: закрытый платёж больше не досматривается.
	if asked > 1 {
		t.Errorf("закрытый платёж спрашивали у кассы %d раз(а)", asked)
	}
}

// Сохранённая почта должна быть ВИДНА человеку и меняться без начала оплаты.
//
// Владелец 02.09.2026 после первого живого платежа: «почту надо сохранять,
// чтобы не вводить 2 раза». Она сохранялась и до этого — но нигде не
// показывалась, а невидимое сохранение человек считает отсутствующим.
func TestBillingEmailVisibleAndChangeable(t *testing.T) {
	e := newPayEnv(t)

	if got := e.sub(t, e.plain)["billing_email"]; got != "" {
		t.Fatalf("почты быть не должно: %v", got)
	}

	code, body := e.post(t, e.plain, "/v1/billing/email", `{"email":"buyer@example.com"}`)
	if code != http.StatusOK {
		t.Fatalf("почта не сохранилась: %d %v", code, body)
	}
	if got := e.sub(t, e.plain)["billing_email"]; got != "buyer@example.com" {
		t.Errorf("кабинет не показывает сохранённую почту: %v", got)
	}

	// Адрес меняют — вторая запись должна победить.
	if code, _ := e.post(t, e.plain, "/v1/billing/email", `{"email":"other@example.com"}`); code != http.StatusOK {
		t.Fatalf("почта не меняется: %d", code)
	}
	if got := e.sub(t, e.plain)["billing_email"]; got != "other@example.com" {
		t.Errorf("показана старая почта: %v", got)
	}

	// Мусор до кассы не доходит.
	code, body = e.post(t, e.plain, "/v1/billing/email", `{"email":"без-собаки"}`)
	if code != http.StatusBadRequest || body["code"] != "email_invalid" {
		t.Errorf("кривой адрес принят: %d %v", code, body)
	}
}
