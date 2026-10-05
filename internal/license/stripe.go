package license

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// StripeConfig holds Stripe API keys.
type StripeConfig struct {
	SecretKey      string // sk_live_... or sk_test_...
	WebhookSecret  string // whsec_...
	PublishableKey string // pk_live_... or pk_test_...

	// Stripe Price IDs for each tier/interval
	PriceProMonthly  string // price_...
	PriceProAnnual   string // price_...
	PriceTeamMonthly string // price_...
	PriceTeamAnnual  string // price_...

	// Portal
	PortalConfigID string // bpc_... (optional)

	// URLs
	SuccessURL string // redirect after successful checkout
	CancelURL  string // redirect after cancelled checkout
}

// StripeClient handles Stripe API calls.
type StripeClient struct {
	config  StripeConfig
	manager *Manager
	client  *http.Client
}

// NewStripeClient creates a Stripe client.
func NewStripeClient(cfg StripeConfig, manager *Manager) *StripeClient {
	return &StripeClient{
		config:  cfg,
		manager: manager,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// IsConfigured returns true if Stripe API keys are set.
func (s *StripeClient) IsConfigured() bool {
	return s.config.SecretKey != ""
}

// ── Checkout ────────────────────────────────────────────────────────

// CreateCheckoutURL creates a Stripe Checkout session for a given tier.
func (s *StripeClient) CreateCheckoutURL(tier Tier, annual bool, email, deviceID string) (string, error) {
	priceID := s.getPriceID(tier, annual)
	if priceID == "" {
		return "", fmt.Errorf("no price configured for tier %s", tier)
	}

	params := url.Values{
		"mode":                                   {"subscription"},
		"line_items[0][price]":                   {priceID},
		"line_items[0][quantity]":                {"1"},
		"success_url":                            {s.config.SuccessURL + "?session_id={CHECKOUT_SESSION_ID}"},
		"cancel_url":                             {s.config.CancelURL},
		"subscription_data[metadata][device_id]": {deviceID},
		"subscription_data[metadata][tier]":      {string(tier)},
	}

	if email != "" {
		params.Set("customer_email", email)
	}

	// Allow promotion codes
	params.Set("allow_promotion_codes", "true")

	// 14-day trial for Pro
	if tier == TierPro {
		params.Set("subscription_data[trial_period_days]", "14")
	}

	resp, err := s.apiPost("/v1/checkout/sessions", params)
	if err != nil {
		return "", err
	}

	sessionURL, _ := resp["url"].(string)
	if sessionURL == "" {
		return "", fmt.Errorf("no checkout URL in response")
	}

	return sessionURL, nil
}

// CreatePortalURL creates a Stripe Customer Portal session for managing subscriptions.
func (s *StripeClient) CreatePortalURL(customerID, returnURL string) (string, error) {
	params := url.Values{
		"customer":   {customerID},
		"return_url": {returnURL},
	}

	if s.config.PortalConfigID != "" {
		params.Set("configuration", s.config.PortalConfigID)
	}

	resp, err := s.apiPost("/v1/billing_portal/sessions", params)
	if err != nil {
		return "", err
	}

	portalURL, _ := resp["url"].(string)
	if portalURL == "" {
		return "", fmt.Errorf("no portal URL in response")
	}

	return portalURL, nil
}

// ── Webhook ─────────────────────────────────────────────────────────

// StripeEvent represents a Stripe webhook event.
type StripeEvent struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// VerifyWebhookSignature validates the Stripe webhook signature.
func (s *StripeClient) VerifyWebhookSignature(payload []byte, sigHeader string) bool {
	if s.config.WebhookSecret == "" {
		return false
	}

	// Parse sig header: t=timestamp,v1=signature
	parts := strings.Split(sigHeader, ",")
	var timestamp, signature string
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp = kv[1]
		case "v1":
			signature = kv[1]
		}
	}

	if timestamp == "" || signature == "" {
		return false
	}

	// Reject events older than 5 minutes (replay protection)
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if time.Since(time.Unix(ts, 0)) > 5*time.Minute {
		log.Println("[STRIPE] Webhook rejected: timestamp too old")
		return false
	}

	// Compute expected signature
	signedPayload := timestamp + "." + string(payload)
	mac := hmac.New(sha256.New, []byte(s.config.WebhookSecret))
	mac.Write([]byte(signedPayload))
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

// HandleWebhook processes a verified Stripe webhook event.
func (s *StripeClient) HandleWebhook(event StripeEvent) {
	log.Printf("[STRIPE] Webhook: %s (id=%s)", event.Type, event.ID)

	switch event.Type {
	case "customer.subscription.created",
		"customer.subscription.updated":
		s.handleSubscriptionUpdate(event.Data)

	case "customer.subscription.deleted":
		s.handleSubscriptionDeleted(event.Data)

	case "invoice.payment_failed":
		s.handlePaymentFailed(event.Data)

	case "checkout.session.completed":
		s.handleCheckoutCompleted(event.Data)
	}
}

func (s *StripeClient) handleSubscriptionUpdate(data json.RawMessage) {
	var wrapper struct {
		Object struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Customer string `json:"customer"`
			Metadata struct {
				DeviceID string `json:"device_id"`
				Tier     string `json:"tier"`
			} `json:"metadata"`
			CurrentPeriodEnd int64  `json:"current_period_end"`
			TrialEnd         *int64 `json:"trial_end"`
			Items            struct {
				Data []struct {
					Price struct {
						ID string `json:"id"`
					} `json:"price"`
				} `json:"data"`
			} `json:"items"`
		} `json:"object"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		log.Printf("[STRIPE] Parse subscription: %v", err)
		return
	}

	sub := wrapper.Object
	if sub.Status != "active" && sub.Status != "trialing" {
		return
	}

	// Tier is derived from the Stripe price ID (authoritative — set by Stripe),
	// not from client-supplied metadata.tier which a forged/edited webhook could
	// spoof to unlock a higher tier. Metadata is only a fallback when the price
	// isn't one we recognize.
	tier := s.tierFromPriceID(sub.Items.Data)
	if tier == "" {
		if c := Tier(sub.Metadata.Tier); c == TierPro || c == TierTeam {
			tier = c
			log.Printf("[STRIPE] price unrecognized — falling back to metadata tier %s", tier)
		} else {
			tier = TierPro
		}
	}

	// A subscription with no period end would become a far-future zero-time and
	// never expire — refuse to activate on it.
	if sub.CurrentPeriodEnd <= 0 {
		log.Printf("[STRIPE] subscription %s missing current_period_end — skipping activation", sub.ID)
		return
	}
	validUntil := time.Unix(sub.CurrentPeriodEnd, 0)

	s.manager.Activate(
		sub.ID,
		tier,
		"", // email from customer, not in subscription
		sub.Customer,
		sub.ID,
		validUntil,
	)

	if sub.Metadata.DeviceID != "" {
		s.manager.ActivateDevice(sub.Metadata.DeviceID)
	}
}

func (s *StripeClient) handleSubscriptionDeleted(data json.RawMessage) {
	log.Println("[STRIPE] Subscription deleted — reverting to free tier")
	s.manager.Deactivate()
}

func (s *StripeClient) handlePaymentFailed(data json.RawMessage) {
	log.Println("[STRIPE] Payment failed — license will expire at period end")
	// Don't immediately deactivate — let grace period handle it
}

func (s *StripeClient) handleCheckoutCompleted(data json.RawMessage) {
	var wrapper struct {
		Object struct {
			CustomerEmail string `json:"customer_email"`
			Subscription  string `json:"subscription"`
			Customer      string `json:"customer"`
		} `json:"object"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return
	}

	log.Printf("[STRIPE] Checkout completed: customer=%s email=%s", wrapper.Object.Customer, wrapper.Object.CustomerEmail)
	// The subscription.created event will handle activation
}

// ── Helpers ─────────────────────────────────────────────────────────

func (s *StripeClient) getPriceID(tier Tier, annual bool) string {
	switch tier {
	case TierPro:
		if annual {
			return s.config.PriceProAnnual
		}
		return s.config.PriceProMonthly
	case TierTeam:
		if annual {
			return s.config.PriceTeamAnnual
		}
		return s.config.PriceTeamMonthly
	}
	return ""
}

func (s *StripeClient) tierFromPriceID(items []struct {
	Price struct {
		ID string `json:"id"`
	} `json:"price"`
}) Tier {
	if len(items) == 0 {
		return "" // unknown — let caller fall back
	}
	priceID := items[0].Price.ID
	switch priceID {
	case s.config.PriceTeamMonthly, s.config.PriceTeamAnnual:
		return TierTeam
	case s.config.PriceProMonthly, s.config.PriceProAnnual:
		return TierPro
	default:
		return "" // unrecognized price — let caller fall back
	}
}

func (s *StripeClient) apiPost(path string, params url.Values) (map[string]any, error) {
	req, err := http.NewRequest("POST", "https://api.stripe.com"+path, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+s.config.SecretKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe API: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("stripe API error (%d): %s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse stripe response: %w", err)
	}
	return result, nil
}
