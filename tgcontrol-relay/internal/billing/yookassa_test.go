package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Подменяем адрес API на тестовый сервер: клиент ходит по apiBase, поэтому
// тесты держат свой httptest и переписывают HTTP-транспорт.
func withServer(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := New("1451249", "test_secret")
	c.HTTP = srv.Client()
	c.HTTP.Transport = rewrite{base: srv.URL, rt: srv.Client().Transport}
	t.Cleanup(srv.Close)
	return c, srv
}

type rewrite struct {
	base string
	rt   http.RoundTripper
}

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.Scheme = "http"
	u.Host = strings.TrimPrefix(r.base, "http://")
	req2 := req.Clone(req.Context())
	req2.URL = &u
	return r.rt.RoundTrip(req2)
}

// Главное свойство: чек уходит с каждым платежом. Без него боевой магазин
// отвечает 400 «Receipt is missing or illegal» — проверено живым запросом
// 01.09.2026, и забыть об этом нельзя.
func TestCreateAlwaysSendsReceipt(t *testing.T) {
	var got map[string]any
	c, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"p1","status":"pending"}`))
	})

	_, err := c.Create(context.Background(), CreateRequest{
		Amount: 59900, Description: "Про, 1 месяц", Email: "a@b.ru",
		ReturnURL: "https://remotai.ru/app/",
	})
	if err != nil {
		t.Fatalf("создание платежа: %v", err)
	}
	receipt, ok := got["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("чек не ушёл: %+v", got)
	}
	items, _ := receipt["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("в чеке должна быть одна позиция: %+v", receipt)
	}
	if cust, _ := receipt["customer"].(map[string]any); cust["email"] != "a@b.ru" {
		t.Errorf("почта покупателя не попала в чек: %+v", receipt["customer"])
	}
	if got["amount"].(map[string]any)["value"] != "599.00" {
		t.Errorf("сумма в рублях с копейками: %+v", got["amount"])
	}
}

// Копейки не должны превращаться в дроби с хвостом.
func TestMoneyFormatting(t *testing.T) {
	for _, c := range []struct {
		in   Money
		want string
	}{{59900, "599.00"}, {199000, "1990.00"}, {1000, "10.00"}, {5, "0.05"}, {0, "0.00"}} {
		if got := c.in.String(); got != c.want {
			t.Errorf("%d копеек → %q, ожидали %q", int64(c.in), got, c.want)
		}
	}
}

// ⚠ Самое ценное поведение: магазину ещё не включили автоплатежи (403 «This
// store can't make recurring payments» — реальный ответ боевого магазина
// 01.09.2026). Разовая оплата от этого страдать не должна: повторяем без
// сохранения карты, а не роняем платёж человеку в лицо.
func TestCreateFallsBackWhenRecurringForbidden(t *testing.T) {
	var bodies []map[string]any
	c, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		if _, saving := b["save_payment_method"]; saving {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"error","code":"forbidden","description":"This store can't make recurring payments. Contact the YooMoney manager to learn more"}`))
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"p2","status":"pending"}`))
	})

	p, err := c.Create(context.Background(), CreateRequest{
		Amount: 59900, Description: "Про", Email: "a@b.ru",
		ReturnURL: "https://remotai.ru/app/", SaveCard: true,
	})
	if err != nil {
		t.Fatalf("платёж обязан пройти без сохранения карты: %v", err)
	}
	if p.ID != "p2" {
		t.Errorf("вернулся не тот платёж: %+v", p)
	}
	if len(bodies) != 2 {
		t.Fatalf("ожидали две попытки (с картой и без), было %d", len(bodies))
	}
	if _, saving := bodies[1]["save_payment_method"]; saving {
		t.Error("во второй попытке сохранение карты обязано быть снято")
	}
	if !c.RecurringDisabled() {
		t.Error("клиент обязан запомнить отказ, чтобы не биться в него каждый раз")
	}

	// И следующий платёж уже не тратит лишний запрос на заведомый отказ.
	bodies = nil
	if _, err := c.Create(context.Background(), CreateRequest{
		Amount: 59900, Email: "a@b.ru", ReturnURL: "https://remotai.ru/app/", SaveCard: true,
	}); err != nil {
		t.Fatalf("второй платёж: %v", err)
	}
	if len(bodies) != 1 {
		t.Errorf("после запомненного отказа должна быть ОДНА попытка, было %d", len(bodies))
	}
}

// Прочие отказы не глотаем: 400 про чек обязан дойти до вызывающего как есть.
func TestCreateSurfacesOtherErrors(t *testing.T) {
	c, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","code":"invalid_request","description":"Receipt is missing or illegal"}`))
	})
	_, err := c.Create(context.Background(), CreateRequest{Amount: 100, Email: "a@b.ru"})
	if err == nil {
		t.Fatal("ошибка обязана вернуться вызывающему")
	}
	if !strings.Contains(err.Error(), "Receipt") {
		t.Errorf("объяснение ЮKassa потеряно: %v", err)
	}
}

// Автосписание идёт без confirmation — человек в нём не участвует.
func TestChargeUsesSavedMethod(t *testing.T) {
	var got map[string]any
	c, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"p3","status":"succeeded","paid":true}`))
	})
	if _, err := c.Charge(context.Background(), "pm-42", CreateRequest{
		Amount: 59900, Description: "Про, продление", Email: "a@b.ru",
	}); err != nil {
		t.Fatalf("автосписание: %v", err)
	}
	if got["payment_method_id"] != "pm-42" {
		t.Errorf("не указан сохранённый способ оплаты: %+v", got)
	}
	if _, has := got["confirmation"]; has {
		t.Error("в автосписании подтверждение человеку не запрашивается")
	}
}

// Без ключей молчаливо «работать» нельзя: это привело бы к тихо неработающей
// кассе на бою.
func TestNotConfigured(t *testing.T) {
	c := New("", "")
	if c.Configured() {
		t.Fatal("пустой магазин не может считаться настроенным")
	}
	if _, err := c.Create(context.Background(), CreateRequest{Amount: 1}); err != ErrNotConfigured {
		t.Errorf("ожидали ErrNotConfigured, получили %v", err)
	}
}
