package license

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestManager builds a Manager backed by a temp file, bypassing NewManager's
// ~/.tgcontrol-license.json location so tests never touch the real license.
func newTestManager(t *testing.T, lic *License) *Manager {
	t.Helper()
	if lic == nil {
		lic = &License{Tier: TierFree, Active: true, DeviceSlots: 1, OfflineGrace: 7}
	}
	return &Manager{license: lic, path: filepath.Join(t.TempDir(), "license.json")}
}

func TestEffectiveTierBetaUplift(t *testing.T) {
	// BetaFree is a compile-time const today; these cases pin the intended
	// behavior so flipping it is a deliberate, test-visible change.
	free := newTestManager(t, nil)
	pro := newTestManager(t, &License{Tier: TierPro, Active: true, OfflineGrace: 7})

	if BetaFree {
		if got := free.EffectiveTier(); got != TierPro {
			t.Errorf("free EffectiveTier = %q, want %q during beta", got, TierPro)
		}
		if got := free.GetLimits(); got != Limits[TierPro] {
			t.Errorf("free GetLimits = %+v, want Pro limits during beta", got)
		}
	} else {
		if got := free.EffectiveTier(); got != TierFree {
			t.Errorf("free EffectiveTier = %q, want %q after beta", got, TierFree)
		}
	}
	if got := pro.EffectiveTier(); got != TierPro {
		t.Errorf("pro EffectiveTier = %q, want %q (paid tiers never change)", got, TierPro)
	}
}

func TestGetLimitsUnknownTierFallsBackToFree(t *testing.T) {
	m := newTestManager(t, &License{Tier: Tier("bogus"), Active: true})
	if got := m.GetLimits(); got != Limits[TierFree] {
		t.Errorf("GetLimits for unknown tier = %+v, want Free limits", got)
	}
}

func TestIsActive(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		lic  *License
		want bool
	}{
		{"free always active", &License{Tier: TierFree, Active: true}, true},
		{"free without ValidUntil", &License{Tier: TierFree, Active: true, OfflineGrace: 7}, true},
		{"inactive flag", &License{Tier: TierPro, Active: false, ValidUntil: now.Add(24 * time.Hour)}, false},
		{"pro within validity", &License{Tier: TierPro, Active: true, ValidUntil: now.Add(24 * time.Hour), OfflineGrace: 7}, true},
		{"pro expired inside grace", &License{Tier: TierPro, Active: true, ValidUntil: now.Add(-3 * 24 * time.Hour), OfflineGrace: 7}, true},
		{"pro expired past grace", &License{Tier: TierPro, Active: true, ValidUntil: now.Add(-8 * 24 * time.Hour), OfflineGrace: 7}, false},
		{"pro zero ValidUntil never expires", &License{Tier: TierPro, Active: true, OfflineGrace: 7}, true},
	}
	for _, c := range cases {
		m := newTestManager(t, c.lic)
		if got := m.IsActive(); got != c.want {
			t.Errorf("%s: IsActive() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCanUseFeature(t *testing.T) {
	pro := newTestManager(t, &License{
		Tier: TierPro, Active: true,
		ValidUntil: time.Now().Add(24 * time.Hour), OfflineGrace: 7,
	})
	if !pro.CanUseFeature("orchestrator") || !pro.CanUseFeature("researcher") {
		t.Error("active pro should have orchestrator and researcher")
	}

	expired := newTestManager(t, &License{
		Tier: TierPro, Active: true,
		ValidUntil: time.Now().Add(-30 * 24 * time.Hour), OfflineGrace: 7,
	})
	if expired.CanUseFeature("orchestrator") {
		t.Error("expired pro must fall back to free limits (no orchestrator)")
	}
	if !expired.CanUseFeature("file_write") {
		t.Error("file_write is never gated, even for an expired license")
	}
	if !expired.CanUseFeature("unknown-feature-xyz") {
		t.Error("unknown features default to allowed")
	}
}

func TestActivateDeviceSlots(t *testing.T) {
	m := newTestManager(t, &License{Tier: TierPro, Active: true, OfflineGrace: 7})

	// Pro allows 5 devices; repeats are idempotent, the 6th distinct device fails.
	for range Limits[TierPro].MaxDevices {
		if !m.ActivateDevice("dev-first") {
			t.Fatal("re-activating the same device must stay true")
		}
	}
	if got := len(m.Get().ActiveDevices); got != 1 {
		t.Fatalf("idempotent activation: got %d devices, want 1", got)
	}
	for _, d := range []string{"d2", "d3", "d4", "d5"} {
		if !m.ActivateDevice(d) {
			t.Fatalf("device %s should fit in pro slots", d)
		}
	}
	if m.ActivateDevice("d6-overflow") {
		t.Error("6th device must be rejected on a 5-slot pro license")
	}

	// Activation is persisted: a fresh manager on the same file sees the devices.
	m2 := &Manager{path: m.path, license: &License{}}
	m2.load()
	if got := len(m2.Get().ActiveDevices); got != Limits[TierPro].MaxDevices {
		t.Errorf("reloaded devices = %d, want %d", got, Limits[TierPro].MaxDevices)
	}
}

func TestActivatePersistsAndDeactivateReverts(t *testing.T) {
	m := newTestManager(t, nil)
	validUntil := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	m.Activate("key-1", TierTeam, "u@example.com", "cus_1", "sub_1", validUntil)

	l := m.Get()
	if l.Tier != TierTeam || l.LicenseKey != "key-1" || l.CustomerID != "cus_1" || !l.Active {
		t.Fatalf("after Activate: %+v", l)
	}
	if l.DeviceSlots != Limits[TierTeam].MaxDevices {
		t.Errorf("DeviceSlots = %d, want %d for team", l.DeviceSlots, Limits[TierTeam].MaxDevices)
	}

	// Reload from disk and confirm the round trip.
	m2 := &Manager{path: m.path, license: &License{}}
	m2.load()
	l2 := m2.Get()
	if l2.Tier != TierTeam || l2.Email != "u@example.com" || l2.SubscriptionID != "sub_1" {
		t.Fatalf("reloaded license: %+v", l2)
	}
	if !l2.ValidUntil.Equal(validUntil) {
		t.Errorf("ValidUntil round trip: got %s, want %s", l2.ValidUntil, validUntil)
	}

	m2.Deactivate()
	l3 := m2.Get()
	if l3.Tier != TierFree || l3.LicenseKey != "" || l3.SubscriptionID != "" || !l3.Active {
		t.Fatalf("after Deactivate: %+v", l3)
	}
}

func TestLoadCorruptFileIsQuarantined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "license.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{path: path, license: &License{Tier: TierFree, Active: true}}
	m.load()

	if m.Get().Tier != TierFree {
		t.Errorf("corrupt file must keep the default license, got tier %q", m.Get().Tier)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("corrupt file should be renamed away")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Error("corrupt file should be preserved as .corrupt for recovery")
	}
}

func TestStatusSnapshot(t *testing.T) {
	m := newTestManager(t, &License{
		Tier: TierPro, Active: true, Email: "u@example.com",
		DeviceID: "dev-1", DeviceSlots: 5, OfflineGrace: 7,
		ValidUntil: time.Now().Add(24 * time.Hour),
	})
	st := m.Status()

	if st["tier"] != TierPro || st["effective_tier"] != TierPro {
		t.Errorf("status tiers: %v / %v", st["tier"], st["effective_tier"])
	}
	if st["beta"] != BetaFree {
		t.Errorf("status beta = %v, want %v", st["beta"], BetaFree)
	}
	if st["active"] != true || st["email"] != "u@example.com" || st["device_id"] != "dev-1" {
		t.Errorf("status fields: %v", st)
	}
	if _, ok := st["valid_until"].(string); !ok {
		t.Error("valid_until should be rendered when set")
	}
	if _, ok := st["limits"]; !ok {
		t.Error("limits must be present in status")
	}
}
