package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"tgcontrol-relay/internal/db"
)

func TestSelfHostedExpiredAccountHasFullAccessWithoutStartingTrial(t *testing.T) {
	ts, s, initData := gateTestServer(t, false)
	s.Config.SelfHosted = true
	token, uid := serverAccessToken(t, s)
	if _, err := s.DB.Exec(`UPDATE users SET trial_end = datetime('now','-1 day') WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		status, out := accessRequest(t, ts.URL, token, method)
		if status != 200 || out["allowed"] != true || out["tier"] != "team" || out["trial_available"] != false || out["self_hosted"] != true {
			t.Fatalf("%s: status=%d body=%v", method, status, out)
		}
	}
	before, err := db.GetUserByID(t.Context(), s.DB, uid)
	if err != nil {
		t.Fatal(err)
	}
	access, err := s.cloudAccess(t.Context(), uid)
	if err != nil || !access.Allowed {
		t.Fatalf("cloud: %v %v", access, err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/me", nil)
	req.Header.Set("Authorization", "tma "+initData)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var me map[string]any
	if err := json.NewDecoder(res.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || me["effective_tier"] != "team" || me["billing_enabled"] != false || me["cloud_allowed"] != true || me["max_devices"] != float64(0) {
		t.Fatalf("me: status=%d body=%v", res.StatusCode, me)
	}
	after, err := db.GetUserByID(t.Context(), s.DB, uid)
	if err != nil || !before.TrialEnd.Time.Equal(after.TrialEnd.Time) {
		t.Fatal("self-hosting changed trial")
	}
}

func TestSelfHostedKeepsAuthenticationAndHasNoPaidPlans(t *testing.T) {
	ts, s, _ := gateTestServer(t, false)
	s.Config.SelfHosted = true
	res, err := http.Get(ts.URL + "/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /me: %d", res.StatusCode)
	}
	if _, err := s.cloudAccess(t.Context(), -999); err == nil {
		t.Fatal("unknown account received access")
	}
	res, err = http.Get(ts.URL + "/v1/pricing")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Billing    bool   `json:"billing_enabled"`
		SelfHosted bool   `json:"self_hosted"`
		Trial      int    `json:"trial_days"`
		Plans      []plan `json:"plans"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Billing || !out.SelfHosted || out.Trial != 0 || len(out.Plans) != 1 || out.Plans[0].Monthly != 0 {
		t.Fatalf("pricing: %+v", out)
	}
}

func TestSelfHostedPairingHasNoSubscriptionDeviceQuota(t *testing.T) {
	_, s, _ := gateTestServer(t, false)
	s.Config.SelfHosted = true
	_, uid := serverAccessToken(t, s)
	// Exceed both the managed free and Fleet quotas, without bypassing binding.
	for i := 0; i < 30; i++ {
		dev := &db.Device{ID: fmt.Sprintf("self-hosted-%d", i), UserID: uid, Name: "test", Hostname: "test", Platform: "linux"}
		primary, over, err := db.BindDeviceForPairing(t.Context(), s.DB, dev, uid, s.maxDevicesForTier("free"))
		if err != nil || over || !primary {
			t.Fatalf("device %d: primary=%v over=%v err=%v", i, primary, over, err)
		}
	}
}
