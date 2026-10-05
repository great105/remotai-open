package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tgcontrol-relay/internal/billing"
	"tgcontrol-relay/internal/db"
)

// Real HTTP handlers and SQLite, with only the external bank replaced.
// This does not claim a live bank charge or receipt acceptance.
func paidProvider(t *testing.T, e *payEnv, status *atomic.Int32) {
	t.Helper()
	kassa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := "pending"
		if r.Method == http.MethodPost {
			var order map[string]any
			if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
				t.Error(err)
			}
			if order["save_payment_method"] == true || order["capture"] != true {
				t.Errorf("unexpected purchase mode: %v", order)
			}
			amount, _ := order["amount"].(map[string]any)
			if amount["value"] != "900.00" || amount["currency"] != "RUB" {
				t.Errorf("wrong Pro price: %v", amount)
			}
			receipt, _ := order["receipt"].(map[string]any)
			customer, _ := receipt["customer"].(map[string]any)
			if customer["email"] != "buyer@example.com" || r.Header.Get("Idempotence-Key") == "" {
				t.Error("receipt email or idempotency key missing")
			}
		} else if status.Load() == 1 {
			state = "succeeded"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "purchase-pro", "status": state, "paid": state == "succeeded",
			"amount":         map[string]string{"value": "900.00", "currency": "RUB"},
			"confirmation":   map[string]string{"confirmation_url": "https://payment.example.test/confirm"},
			"payment_method": map[string]any{"type": "bank_card", "saved": true, "id": "must-not-save"},
		})
	}))
	t.Cleanup(kassa.Close)
	client := billing.New("unit_shop", "unit_secret")
	client.HTTP = kassa.Client()
	client.HTTP.Transport = rewriteTo{base: kassa.URL, rt: kassa.Client().Transport}
	e.srv.BillingClient = client
}

func TestPaidPurchaseCheckoutWebhookAndRetry(t *testing.T) {
	e := newPayEnv(t)
	var status atomic.Int32
	paidProvider(t, e, &status)
	code, out := e.post(t, e.plain, "/v1/billing/checkout", `{"tier":"pro","method":"bank_card","email":"buyer@example.com"}`)
	if code != http.StatusOK || out["confirmation_url"] == nil {
		t.Fatalf("checkout failed: %d %v", code, out)
	}
	if e.sub(t, e.plain)["paid_until"] != nil {
		t.Fatal("pending payment granted access")
	}
	status.Store(1)
	// Fail the grant after the provider has charged. A retry must recover.
	if _, err := e.d.Exec(`CREATE TRIGGER reject_grant BEFORE INSERT ON subscriptions BEGIN SELECT RAISE(ABORT, 'simulated database failure'); END`); err != nil {
		t.Fatal(err)
	}
	event := `{"event":"payment.succeeded","object":{"id":"purchase-pro"}}`
	if code, _ := e.post(t, "", "/v1/billing/webhook", event); code != http.StatusInternalServerError {
		t.Fatalf("provider was not asked to retry: %d", code)
	}
	if _, err := e.d.Exec(`DROP TRIGGER reject_grant`); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.post(t, "", "/v1/billing/webhook", event); code != http.StatusOK {
		t.Fatalf("retry failed: %d", code)
	}
	sub := e.sub(t, e.plain)
	if sub["tier"] != "pro" || sub["auto_renew"] != false || sub["card"] != nil || sub["period_days"] != float64(30) {
		t.Fatalf("wrong access or saved card: %v", sub)
	}
	until, err := time.Parse(time.RFC3339, sub["paid_until"].(string))
	if err != nil || time.Until(until) < paidPeriod-time.Minute || time.Until(until) > paidPeriod {
		t.Fatalf("wrong paid period: %v %v", until, err)
	}
	if code, out := e.post(t, "", "/v1/billing/webhook", event); code != http.StatusOK || out["duplicate"] != true {
		t.Fatalf("duplicate not recognized: %d %v", code, out)
	}
	if e.sub(t, e.plain)["paid_until"] != sub["paid_until"] {
		t.Fatal("duplicate webhook extended access twice")
	}
}

func TestCheckoutDoesNotExposeUnrecordedPayment(t *testing.T) {
	e := newPayEnv(t)
	var status atomic.Int32
	paidProvider(t, e, &status)
	if _, err := e.d.Exec(`CREATE TRIGGER reject_order BEFORE INSERT ON payments BEGIN SELECT RAISE(ABORT, 'simulated storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	code, out := e.post(t, e.plain, "/v1/billing/checkout", `{"tier":"pro","email":"buyer@example.com"}`)
	if code != http.StatusServiceUnavailable || out["confirmation_url"] != nil {
		t.Fatalf("untracked checkout escaped: %d %v", code, out)
	}
}

func TestOldMissedPaymentReconcilesInCabinet(t *testing.T) {
	e := newPayEnv(t)
	var status atomic.Int32
	status.Store(1)
	paidProvider(t, e, &status)
	u, err := db.GetUserByTelegram(context.Background(), e.d, 6002)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordPayment(context.Background(), e.d, db.Payment{PaymentID: "purchase-pro", UserID: u.ID, Tier: "pro", AmountMinor: 90000, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Exec(`UPDATE payments SET created_at = ? WHERE payment_id = 'purchase-pro'`, time.Now().Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if sub := e.sub(t, e.plain); sub["paid_until"] == nil || sub["tier"] != "pro" {
		t.Fatalf("old payment was lost: %v", sub)
	}
}

func TestDisabledBillingRejectsCheckout(t *testing.T) {
	e := newPayEnv(t)
	e.srv.Config.BillingEnabled = false
	code, _ := e.post(t, e.plain, "/v1/billing/checkout", `{"tier":"pro","email":"buyer@example.com"}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("disabled checkout returned %d", code)
	}
}
