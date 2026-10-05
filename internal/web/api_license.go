package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"tgcontrol/internal/config"
	"tgcontrol/internal/license"
)

// ── License API Endpoints ───────────────────────────────────────────

// GET /api/license — get current license status and limits
func (s *Server) apiLicenseStatus(w http.ResponseWriter, r *http.Request, uid int64) {
	if s.licenseManager == nil {
		jsonResp(w, map[string]any{
			"tier":   "free",
			"active": true,
			"limits": license.Limits[license.TierFree],
		})
		return
	}
	status := s.licenseManager.Status()
	// Local feature limits are not an account subscription or a free cloud beta.
	status["beta"] = false
	jsonResp(w, status)
}

// POST /api/license/activate — activate a license key manually
func (s *Server) apiLicenseActivate(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		LicenseKey string `json:"license_key"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	if req.LicenseKey == "" {
		jsonError(w, "License key required", 400)
		return
	}

	// For now, manual key validation is a placeholder.
	// In production, validate against the server or Stripe.
	log.Printf("[LICENSE] Manual activation attempt: key=%s...", req.LicenseKey[:min(len(req.LicenseKey), 8)])
	jsonError(w, "Ручная активация пока недоступна.", 501)
}

// POST /api/license/checkout — create Stripe checkout URL
func (s *Server) apiLicenseCheckout(w http.ResponseWriter, r *http.Request, uid int64) {
	if s.stripeClient == nil || !s.stripeClient.IsConfigured() {
		jsonError(w, "Оплата пока недоступна.", 501)
		return
	}

	var req struct {
		Tier   string `json:"tier"`   // "pro" or "team"
		Annual bool   `json:"annual"` // monthly or annual billing
		Email  string `json:"email"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	tier := license.Tier(req.Tier)
	if tier != license.TierPro && tier != license.TierTeam {
		jsonError(w, "Invalid tier: must be 'pro' or 'team'", 400)
		return
	}

	deviceID := config.GetOrCreateDeviceID()
	checkoutURL, err := s.stripeClient.CreateCheckoutURL(tier, req.Annual, req.Email, deviceID)
	if err != nil {
		log.Printf("[LICENSE] Checkout creation failed: %v", err)
		jsonError(w, "Failed to create checkout session", 500)
		return
	}

	jsonResp(w, map[string]string{"url": checkoutURL})
}

// POST /api/license/portal — create Stripe customer portal URL
func (s *Server) apiLicensePortal(w http.ResponseWriter, r *http.Request, uid int64) {
	if s.stripeClient == nil || !s.stripeClient.IsConfigured() {
		jsonError(w, "Управление подпиской пока недоступно.", 501)
		return
	}

	lic := s.licenseManager.Get()
	if lic.CustomerID == "" {
		jsonError(w, "No active subscription", 400)
		return
	}

	// Use a safe return URL (never trust Referer directly)
	cfg := config.GetNoSetup()
	returnURL := fmt.Sprintf("http://localhost:%d/miniapp/#/settings", cfg.Port())

	portalURL, err := s.stripeClient.CreatePortalURL(lic.CustomerID, returnURL)
	if err != nil {
		log.Printf("[LICENSE] Portal creation failed: %v", err)
		jsonError(w, "Failed to create portal session", 500)
		return
	}

	jsonResp(w, map[string]string{"url": portalURL})
}

// GET /api/license/pricing — return pricing info for all tiers
func (s *Server) apiLicensePricing(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, map[string]any{
		"beta":            false,
		"billing_enabled": s.stripeClient != nil && s.stripeClient.IsConfigured() && !license.BetaFree,
		// Полки и цены канона. Локальное управление бесплатно навсегда —
		// поэтому у полки «Локально» нет ни цены, ни облачных устройств.
		"currency":   "RUB",
		"trial_days": 30,
		"note":       "Локальная работа на своём компьютере бесплатна навсегда. Серверы, SSH/SFTP, проброс портов и облачный доступ входят в Про. Аренда VPS оплачивается отдельно.",
		"tiers": []map[string]any{
			{
				"id":            "local",
				"name":          license.PlanNames[license.TierFree],
				"price_monthly": license.PricingRUB[license.TierFree],
				"price_annual":  license.PricingAnnualRUB[license.TierFree],
				"cloud_devices": 0,
				"limits":        license.Limits[license.TierFree],
			},
			{
				"id":                "pro",
				"name":              license.PlanNames[license.TierPro],
				"price_monthly":     license.PricingRUB[license.TierPro],
				"price_annual":      license.PricingAnnualRUB[license.TierPro],
				"price_monthly_usd": license.Pricing[license.TierPro],
				"price_annual_usd":  license.PricingAnnual[license.TierPro],
				"cloud_devices":     license.Limits[license.TierPro].MaxDevices,
				"trial_days":        30,
				"limits":            license.Limits[license.TierPro],
			},
			{
				"id":                "fleet",
				"name":              license.PlanNames[license.TierTeam],
				"price_monthly":     license.PricingRUB[license.TierTeam],
				"price_annual":      license.PricingAnnualRUB[license.TierTeam],
				"price_monthly_usd": license.Pricing[license.TierTeam],
				"price_annual_usd":  license.PricingAnnual[license.TierTeam],
				"cloud_devices":     license.Limits[license.TierTeam].MaxDevices,
				"limits":            license.Limits[license.TierTeam],
			},
		},
	})
}

// POST /api/webhooks/stripe — Stripe webhook handler (no auth, sig verification)
func (s *Server) apiStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if s.stripeClient == nil || !s.stripeClient.IsConfigured() {
		http.Error(w, "Not configured", 404)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB max
	if err != nil {
		http.Error(w, "Read error", 400)
		return
	}

	// Verify webhook signature
	sigHeader := r.Header.Get("Stripe-Signature")
	if sigHeader == "" || !s.stripeClient.VerifyWebhookSignature(body, sigHeader) {
		log.Println("[STRIPE] Webhook signature verification failed")
		http.Error(w, "Invalid signature", 401)
		return
	}

	var event license.StripeEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "Parse error", 400)
		return
	}

	s.stripeClient.HandleWebhook(event)

	w.WriteHeader(200)
	w.Write([]byte(`{"received": true}`))
}

// ── Feature Flag Middleware ──────────────────────────────────────────

// requireFeature wraps a handler and checks if the feature is available for the current tier.
func (s *Server) requireFeature(feature string, handler func(w http.ResponseWriter, r *http.Request, uid int64)) func(w http.ResponseWriter, r *http.Request, uid int64) {
	return func(w http.ResponseWriter, r *http.Request, uid int64) {
		if s.licenseManager != nil && !s.licenseManager.CanUseFeature(feature) {
			log.Printf("[LICENSE] Feature %q denied for tier %s", feature, s.licenseManager.GetTier())
			jsonErrorCode(w, http.StatusForbidden, "license_required", "Upgrade required", nil)
			return
		}
		handler(w, r, uid)
	}
}

// requireTier wraps a handler and checks if the user has at least the given tier.
func (s *Server) requireTier(minTier license.Tier, handler func(w http.ResponseWriter, r *http.Request, uid int64)) func(w http.ResponseWriter, r *http.Request, uid int64) {
	tierOrder := map[license.Tier]int{
		license.TierFree: 0,
		license.TierPro:  1,
		license.TierTeam: 2,
	}
	return func(w http.ResponseWriter, r *http.Request, uid int64) {
		if s.licenseManager != nil {
			currentTier := s.licenseManager.EffectiveTier()
			if tierOrder[currentTier] < tierOrder[minTier] {
				log.Printf("[LICENSE] Tier %s denied, requires %s", currentTier, minTier)
				jsonErrorCode(w, http.StatusForbidden, "license_required", "Upgrade required", nil)
				return
			}
		}
		handler(w, r, uid)
	}
}

// ── Route Registration ──────────────────────────────────────────────

func (s *Server) registerLicenseRoutes() {
	// Public (no auth)
	s.mux.HandleFunc("GET /api/license/pricing", s.apiLicensePricing)
	s.mux.HandleFunc("POST /api/webhooks/stripe", s.apiStripeWebhook)

	// Protected
	s.mux.HandleFunc("GET /api/license", s.authWrap(s.apiLicenseStatus))
	s.mux.HandleFunc("POST /api/license/activate", s.authWrap(s.apiLicenseActivate))
	s.mux.HandleFunc("POST /api/license/checkout", s.authWrap(s.rateLimitAuth(s.apiLimiter, s.apiLicenseCheckout)))
	s.mux.HandleFunc("POST /api/license/portal", s.authWrap(s.apiLicensePortal))
}

// initLicense initializes the license manager and Stripe client.
func (s *Server) initLicense() {
	s.licenseManager = license.NewManager()

	// Set device ID (thread-safe)
	s.licenseManager.SetDeviceID(config.GetOrCreateDeviceID())

	// Initialize Stripe if keys are available
	stripeKey := os.Getenv("STRIPE_SECRET_KEY")
	if stripeKey != "" {
		stripeCfg := license.StripeConfig{
			SecretKey:        stripeKey,
			WebhookSecret:    os.Getenv("STRIPE_WEBHOOK_SECRET"),
			PublishableKey:   os.Getenv("STRIPE_PUBLISHABLE_KEY"),
			PriceProMonthly:  os.Getenv("STRIPE_PRICE_PRO_MONTHLY"),
			PriceProAnnual:   os.Getenv("STRIPE_PRICE_PRO_ANNUAL"),
			PriceTeamMonthly: os.Getenv("STRIPE_PRICE_TEAM_MONTHLY"),
			PriceTeamAnnual:  os.Getenv("STRIPE_PRICE_TEAM_ANNUAL"),
			SuccessURL:       os.Getenv("STRIPE_SUCCESS_URL"),
			CancelURL:        os.Getenv("STRIPE_CANCEL_URL"),
		}
		if stripeCfg.SuccessURL == "" {
			stripeCfg.SuccessURL = "http://localhost:8080/miniapp/#/settings"
		}
		if stripeCfg.CancelURL == "" {
			stripeCfg.CancelURL = "http://localhost:8080/miniapp/#/settings"
		}
		s.stripeClient = license.NewStripeClient(stripeCfg, s.licenseManager)
		log.Println("[LICENSE] Stripe configured")
	}
}
