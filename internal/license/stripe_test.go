package license

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

const testWebhookSecret = "whsec_testsecret_0123456789"

func testStripeConfig() StripeConfig {
	return StripeConfig{
		SecretKey:        "sk_test_123",
		WebhookSecret:    testWebhookSecret,
		PriceProMonthly:  "price_pro_m",
		PriceProAnnual:   "price_pro_a",
		PriceTeamMonthly: "price_team_m",
		PriceTeamAnnual:  "price_team_a",
	}
}

// signPayload builds a Stripe-Signature header the way Stripe does:
// t=<unix>,v1=<hex hmac-sha256 of "t.payload" keyed by the webhook secret>.
func signPayload(secret string, payload []byte, ts time.Time) string {
	timestamp := fmt.Sprintf("%d", ts.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + string(payload)))
	return "t=" + timestamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyWebhookSignature(t *testing.T) {
	payload := []byte(`{"id":"evt_1","type":"customer.subscription.created","data":{}}`)
	s := NewStripeClient(testStripeConfig(), newTestManager(t, nil))

	cases := []struct {
		name      string
		secret    string // secret the CLIENT is configured with
		header    string
		wantValid bool
	}{
		{"valid signature", testWebhookSecret, signPayload(testWebhookSecret, payload, time.Now()), true},
		{"signed with wrong secret", testWebhookSecret, signPayload("whsec_other", payload, time.Now()), false},
		{"payload tampered after signing", testWebhookSecret, signPayload(testWebhookSecret, []byte(`{"id":"evt_2"}`), time.Now()), false},
		{"timestamp older than 5 min", testWebhookSecret, signPayload(testWebhookSecret, payload, time.Now().Add(-6*time.Minute)), false},
		{"missing v1", testWebhookSecret, fmt.Sprintf("t=%d", time.Now().Unix()), false},
		{"missing t", testWebhookSecret, "v1=deadbeef", false},
		{"empty header", testWebhookSecret, "", false},
		{"garbage timestamp", testWebhookSecret, "t=notanumber,v1=deadbeef", false},
		{"extra fields tolerated", testWebhookSecret,
			signPayload(testWebhookSecret, payload, time.Now()) + ",v0=ignored,garbage", true},
	}
	for _, c := range cases {
		s.config.WebhookSecret = c.secret
		if got := s.VerifyWebhookSignature(payload, c.header); got != c.wantValid {
			t.Errorf("%s: VerifyWebhookSignature = %v, want %v", c.name, got, c.wantValid)
		}
	}

	// No webhook secret configured: nothing verifies, ever.
	s.config.WebhookSecret = ""
	if s.VerifyWebhookSignature(payload, signPayload(testWebhookSecret, payload, time.Now())) {
		t.Error("empty WebhookSecret must reject every signature")
	}
}

func TestGetPriceID(t *testing.T) {
	s := NewStripeClient(testStripeConfig(), newTestManager(t, nil))
	cases := []struct {
		tier   Tier
		annual bool
		want   string
	}{
		{TierPro, false, "price_pro_m"},
		{TierPro, true, "price_pro_a"},
		{TierTeam, false, "price_team_m"},
		{TierTeam, true, "price_team_a"},
		{TierFree, false, ""},
		{Tier("bogus"), true, ""},
	}
	for _, c := range cases {
		if got := s.getPriceID(c.tier, c.annual); got != c.want {
			t.Errorf("getPriceID(%q, %v) = %q, want %q", c.tier, c.annual, got, c.want)
		}
	}
}

func TestIsConfigured(t *testing.T) {
	if !NewStripeClient(testStripeConfig(), newTestManager(t, nil)).IsConfigured() {
		t.Error("client with SecretKey should be configured")
	}
	cfg := testStripeConfig()
	cfg.SecretKey = ""
	if NewStripeClient(cfg, newTestManager(t, nil)).IsConfigured() {
		t.Error("client without SecretKey must report not configured")
	}
}

// subscriptionEvent builds the data payload of a customer.subscription.* event.
func subscriptionEvent(t *testing.T, status, priceID, metaTier, deviceID string, periodEnd int64) json.RawMessage {
	t.Helper()
	trial := "null"
	obj := fmt.Sprintf(`{
		"object": {
			"id": "sub_123",
			"status": %q,
			"customer": "cus_123",
			"metadata": {"device_id": %q, "tier": %q},
			"current_period_end": %d,
			"trial_end": %s,
			"items": {"data": [{"price": {"id": %q}}]}
		}
	}`, status, deviceID, metaTier, periodEnd, trial, priceID)
	return json.RawMessage(obj)
}

func TestHandleSubscriptionUpdateActivates(t *testing.T) {
	m := newTestManager(t, nil)
	s := NewStripeClient(testStripeConfig(), m)
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Unix()

	s.HandleWebhook(StripeEvent{
		ID:   "evt_1",
		Type: "customer.subscription.created",
		Data: subscriptionEvent(t, "active", "price_team_m", "", "dev-1", periodEnd),
	})

	l := m.Get()
	if l.Tier != TierTeam {
		t.Errorf("tier derived from price ID = %q, want team", l.Tier)
	}
	if l.CustomerID != "cus_123" || l.SubscriptionID != "sub_123" || !l.Active {
		t.Errorf("license after activation: %+v", l)
	}
	if len(l.ActiveDevices) != 1 || l.ActiveDevices[0] != "dev-1" {
		t.Errorf("device from metadata not activated: %v", l.ActiveDevices)
	}
	if l.ValidUntil.Unix() != periodEnd {
		t.Errorf("ValidUntil = %d, want %d", l.ValidUntil.Unix(), periodEnd)
	}
}

func TestHandleSubscriptionUpdateMetadataFallback(t *testing.T) {
	// Price ID is authoritative: an unknown price falls back to metadata.tier,
	// but a recognized price must WIN over spoofed metadata claiming a higher tier.
	m := newTestManager(t, nil)
	s := NewStripeClient(testStripeConfig(), m)
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Unix()

	s.HandleWebhook(StripeEvent{
		ID: "evt_2", Type: "customer.subscription.updated",
		Data: subscriptionEvent(t, "active", "price_unknown", "team", "", periodEnd),
	})
	if got := m.GetTier(); got != TierTeam {
		t.Errorf("unknown price + metadata team: tier = %q, want team", got)
	}

	s.HandleWebhook(StripeEvent{
		ID: "evt_3", Type: "customer.subscription.updated",
		Data: subscriptionEvent(t, "active", "price_pro_m", "team", "", periodEnd),
	})
	if got := m.GetTier(); got != TierPro {
		t.Errorf("recognized pro price with spoofed team metadata: tier = %q, want pro", got)
	}
}

func TestHandleSubscriptionUpdateRefused(t *testing.T) {
	periodEnd := time.Now().Add(30 * 24 * time.Hour).Unix()
	cases := []struct {
		name      string
		status    string
		periodEnd int64
	}{
		{"past_due status ignored", "past_due", periodEnd},
		{"canceled status ignored", "canceled", periodEnd},
		{"missing current_period_end refused", "active", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestManager(t, nil)
			s := NewStripeClient(testStripeConfig(), m)
			s.HandleWebhook(StripeEvent{
				ID: "evt_x", Type: "customer.subscription.created",
				Data: subscriptionEvent(t, c.status, "price_pro_m", "", "", c.periodEnd),
			})
			if got := m.GetTier(); got != TierFree {
				t.Errorf("tier = %q, want free (activation refused)", got)
			}
		})
	}
}

func TestHandleSubscriptionDeletedRevertsToFree(t *testing.T) {
	m := newTestManager(t, nil)
	s := NewStripeClient(testStripeConfig(), m)
	m.Activate("key", TierPro, "", "cus_123", "sub_123", time.Now().Add(30*24*time.Hour))

	s.HandleWebhook(StripeEvent{
		ID:   "evt_del",
		Type: "customer.subscription.deleted",
		Data: json.RawMessage(`{"object":{"id":"sub_123"}}`),
	})
	if got := m.GetTier(); got != TierFree {
		t.Errorf("after deletion tier = %q, want free", got)
	}
}
