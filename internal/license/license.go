// Package license handles license validation, tier-based feature flags,
// device management, and Stripe billing integration.
package license

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tgcontrol/internal/atomicfile"
)

// Tier represents a subscription tier.
type Tier string

const (
	TierFree Tier = "free"
	TierPro  Tier = "pro"
	TierTeam Tier = "team"
)

// License holds the current license state for this device.
type License struct {
	// Core fields
	Tier           Tier   `json:"tier"`
	LicenseKey     string `json:"license_key,omitempty"`
	Email          string `json:"email,omitempty"`
	CustomerID     string `json:"customer_id,omitempty"`     // Stripe customer ID
	SubscriptionID string `json:"subscription_id,omitempty"` // Stripe subscription ID

	// Device management
	DeviceID      string   `json:"device_id"`
	DeviceSlots   int      `json:"device_slots"` // max devices for this license
	ActiveDevices []string `json:"active_devices,omitempty"`

	// Validation
	ValidUntil   time.Time `json:"valid_until"`   // subscription end date
	LastVerified time.Time `json:"last_verified"` // last online validation
	OfflineGrace int       `json:"offline_grace"` // days allowed offline (default 7)

	// Status
	Active      bool       `json:"active"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
	TrialEnd    *time.Time `json:"trial_end,omitempty"`
}

// TierLimits defines feature limits for each tier.
type TierLimits struct {
	MaxDevices           int  `json:"max_devices"`
	MaxConcurrentAgents  int  `json:"max_concurrent_agents"`
	MaxPTYTerminals      int  `json:"max_pty_terminals"`
	RemoteDesktopMaxFPS  int  `json:"remote_desktop_max_fps"`
	RemoteDesktopMaxRes  int  `json:"remote_desktop_max_res"` // max width
	RemoteDesktopQuality int  `json:"remote_desktop_quality"` // max JPEG quality
	FileManagerWrite     bool `json:"file_manager_write"`
	OrchestratorEnabled  bool `json:"orchestrator_enabled"`
	ResearcherEnabled    bool `json:"researcher_enabled"`
	AutoUpdate           bool `json:"auto_update"`
	AllAgents            bool `json:"all_agents"` // false = only claude+shell
}

// BetaFree включает режим бесплатной беты: всем пользователям действуют
// Pro-лимиты (EffectiveTier), а UI помечает Pro-фичи бейджем «бесплатно в бете».
// Выключить при запуске монетизации — тогда Free-лимиты вступят в силу,
// и это не будет «отъёмом»: граница платного была объявлена с первого дня.
const BetaFree = true

// Транспорт подключения — чем определяется, платное оно или нет.
// Канон продукта: «Дома — бесплатно навсегда. Из любой точки — по подписке.»
// Граница денег проходит по РАССТОЯНИЮ, а не по числу машин или функций.
const (
	TransportLAN   = "lan"   // пульт и машина в одной сети (или сам ПК) — не ограничивается
	TransportRelay = "relay" // через облачный релей — тут и живут лимиты тарифа
)

// Limits maps each tier to its feature limits.
//
// Принцип (docs/monetization-strategy-2026-06.md): платится облако и
// параллельность, базовый продукт не урезается. Лимиты качества стрима
// относятся ТОЛЬКО к подключениям через relay — для LAN они снимаются
// в LimitsForTransport. Безопасность, автообновление и запись файлов
// не гейтятся никогда.
var Limits = map[Tier]TierLimits{
	TierFree: {
		MaxDevices:           1,
		MaxConcurrentAgents:  2,
		MaxPTYTerminals:      2,
		RemoteDesktopMaxFPS:  12,
		RemoteDesktopMaxRes:  1280,
		RemoteDesktopQuality: 65,
		FileManagerWrite:     true,
		OrchestratorEnabled:  false,
		ResearcherEnabled:    false,
		AutoUpdate:           true,
		AllAgents:            true,
	},
	TierPro: {
		MaxDevices:           5,
		MaxConcurrentAgents:  0, // unlimited
		MaxPTYTerminals:      0, // unlimited
		RemoteDesktopMaxFPS:  30,
		RemoteDesktopMaxRes:  1920,
		RemoteDesktopQuality: 85,
		FileManagerWrite:     true,
		OrchestratorEnabled:  true,
		ResearcherEnabled:    true,
		AutoUpdate:           true,
		AllAgents:            true,
	},
	TierTeam: {
		// Канон: «Флит» — 25 облачных устройств (было 10, расходилось с витриной).
		MaxDevices:           25,
		MaxConcurrentAgents:  0,
		MaxPTYTerminals:      0,
		RemoteDesktopMaxFPS:  30,
		RemoteDesktopMaxRes:  1920,
		RemoteDesktopQuality: 85,
		FileManagerWrite:     true,
		OrchestratorEnabled:  true,
		ResearcherEnabled:    true,
		AutoUpdate:           true,
		AllAgents:            true,
	},
}

// Цены — из `ПРОДВИЖЕНИЕ/КАНОН-ПРОДУКТА.md`. Источник правды для приложения —
// `GET /v1/pricing` на релее; здесь те же значения на случай офлайна, и они
// обязаны совпадать. Раньше тут стояли $9/$29, и человек видел на сайте одно,
// а в приложении другое.
//
// Год = десять месяцев (два в подарок).

// PricingRUB (месяц, в копейках) — основная валюта продукта.
var PricingRUB = map[Tier]int{
	TierFree: 0,
	TierPro:  90000,  // 900 ₽/мес
	TierTeam: 199000, // 1 990 ₽/мес — полка «Флит»
}

// PricingAnnualRUB (год, в копейках)
var PricingAnnualRUB = map[Tier]int{
	TierFree: 0,
	TierPro:  900000,  // 9 000 ₽/год
	TierTeam: 1990000, // 19 900 ₽/год
}

// Pricing (monthly, in cents USD) — глобальная витрина, пока не подключена.
var Pricing = map[Tier]int{
	TierFree: 0,
	TierPro:  1800, // $18/mo
	TierTeam: 3900, // $39/mo
}

// PricingAnnual (annual, in cents USD — 2 months free)
var PricingAnnual = map[Tier]int{
	TierFree: 0,
	TierPro:  18000, // $180/yr
	TierTeam: 39000, // $390/yr
}

// PlanNames — названия полок, как они звучат на витрине и в приложении.
var PlanNames = map[Tier]string{
	TierFree: "Локально",
	TierPro:  "Про",
	TierTeam: "Флит",
}

// ── License Manager ─────────────────────────────────────────────────

// Manager handles license state, persistence, and validation.
type Manager struct {
	mu      sync.RWMutex
	license *License
	path    string
}

// NewManager creates a license manager and loads the stored license.
func NewManager() *Manager {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".tgcontrol-license.json")

	m := &Manager{
		path: path,
		license: &License{
			Tier:         TierFree,
			Active:       true,
			DeviceSlots:  1,
			OfflineGrace: 7,
		},
	}
	m.load()
	return m
}

// Get returns the current license.
func (m *Manager) Get() *License {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.license
}

// GetLimits returns feature limits for the effective tier
// (во время беты Free получает Pro-лимиты).
func (m *Manager) GetLimits() TierLimits {
	limits, ok := Limits[m.EffectiveTier()]
	if !ok {
		return Limits[TierFree]
	}
	return limits
}

// LimitsForTransport возвращает лимиты, действующие для конкретного подключения.
//
// Локальное подключение не ограничивается НИКОГДА, ни на одном тарифе: локальная
// сеть не стоит нам ничего, платится облако. Пока этой развилки не было, лимиты
// качества экрана и число терминалов резали и домашние подключения — то есть
// обещание «дома бесплатно и без ограничений» было неправдой (блокер №1 канона).
//
// Ноль в полях RemoteDesktop*/Max* означает «без ограничения»: приёмники
// (newAdaptiveCtrlWithLimits и проверки лимитов) трактуют 0 именно так.
func (m *Manager) LimitsForTransport(transport string) TierLimits {
	limits := m.GetLimits()
	if transport == TransportRelay {
		return limits
	}
	limits.RemoteDesktopMaxFPS = 0
	limits.RemoteDesktopMaxRes = 0
	limits.RemoteDesktopQuality = 0
	limits.MaxPTYTerminals = 0
	limits.MaxConcurrentAgents = 0
	return limits
}

// EffectiveTier returns the tier whose limits actually apply:
// during the free beta, Free is uplifted to Pro.
func (m *Manager) EffectiveTier() Tier {
	tier := m.GetTier()
	if BetaFree && tier == TierFree {
		return TierPro
	}
	return tier
}

// GetTier returns the current tier.
func (m *Manager) GetTier() Tier {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.license.Tier
}

// SetDeviceID sets the device ID on the license (thread-safe).
func (m *Manager) SetDeviceID(deviceID string) {
	m.mu.Lock()
	m.license.DeviceID = deviceID
	m.mu.Unlock()
}

// IsActive returns true if the license is active and not expired.
func (m *Manager) IsActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.license.Active {
		return false
	}
	if m.license.Tier == TierFree {
		return true // free tier always active
	}
	// Check expiration with offline grace
	if !m.license.ValidUntil.IsZero() {
		grace := time.Duration(m.license.OfflineGrace) * 24 * time.Hour
		if time.Now().After(m.license.ValidUntil.Add(grace)) {
			return false
		}
	}
	return true
}

// CanUseFeature checks if a specific feature is available in the current tier.
// If a paid license is expired, falls back to free tier limits.
func (m *Manager) CanUseFeature(feature string) bool {
	limits := m.GetLimits()
	if !m.IsActive() && m.GetTier() != TierFree {
		// Expired paid license — use free tier limits
		limits = Limits[TierFree]
	}
	switch feature {
	case "file_write":
		return limits.FileManagerWrite
	case "orchestrator":
		return limits.OrchestratorEnabled
	case "researcher":
		return limits.ResearcherEnabled
	case "auto_update":
		return limits.AutoUpdate
	case "all_agents":
		return limits.AllAgents
	default:
		return true
	}
}

// ActivateDevice adds a device to the active devices list.
func (m *Manager) ActivateDevice(deviceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Already activated?
	for _, d := range m.license.ActiveDevices {
		if d == deviceID {
			return true
		}
	}

	// Effective tier inline: m.mu уже взят на запись, EffectiveTier() брать нельзя
	// (RWMutex не реентерабелен).
	tier := m.license.Tier
	if BetaFree && tier == TierFree {
		tier = TierPro
	}
	limits := Limits[tier]
	if limits.MaxDevices > 0 && len(m.license.ActiveDevices) >= limits.MaxDevices {
		return false
	}

	m.license.ActiveDevices = append(m.license.ActiveDevices, deviceID)
	m.save()
	return true
}

// Activate sets a license key and tier (called after Stripe payment or manual activation).
func (m *Manager) Activate(key string, tier Tier, email, customerID, subscriptionID string, validUntil time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.license.LicenseKey = key
	m.license.Tier = tier
	m.license.Email = email
	m.license.CustomerID = customerID
	m.license.SubscriptionID = subscriptionID
	m.license.ValidUntil = validUntil
	m.license.LastVerified = time.Now()
	m.license.Active = true
	m.license.CancelledAt = nil

	if limits, ok := Limits[tier]; ok {
		m.license.DeviceSlots = limits.MaxDevices
	}

	m.save()
	log.Printf("[LICENSE] Activated: tier=%s email=%s valid_until=%s", tier, email, validUntil.Format("2006-01-02"))
}

// Deactivate completely deactivates the license (fallback to free).
func (m *Manager) Deactivate() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.license.Tier = TierFree
	m.license.Active = true
	m.license.LicenseKey = ""
	m.license.CustomerID = ""
	m.license.SubscriptionID = ""
	m.license.DeviceSlots = 1
	m.license.CancelledAt = nil
	m.save()
	log.Println("[LICENSE] Deactivated — reverted to free tier")
}

// Status returns a summary for the API.
func (m *Manager) Status() map[string]any {
	// Snapshot the struct (and the devices slice) under the lock so we don't
	// read fields a concurrent webhook is mutating in place. GetLimits/IsActive
	// take the lock themselves, so they're called after releasing it.
	m.mu.RLock()
	l := *m.license
	devices := append([]string(nil), m.license.ActiveDevices...)
	m.mu.RUnlock()

	limits := m.GetLimits()
	active := m.IsActive()

	result := map[string]any{
		"tier":           l.Tier,
		"effective_tier": m.EffectiveTier(),
		"beta":           BetaFree,
		"active":         active,
		"email":          l.Email,
		"device_id":      l.DeviceID,
		"device_slots":   l.DeviceSlots,
		"active_devices": devices,
		"limits":         limits,
	}

	if !l.ValidUntil.IsZero() {
		result["valid_until"] = l.ValidUntil.Format(time.RFC3339)
	}
	if l.CancelledAt != nil {
		result["cancelled_at"] = l.CancelledAt.Format(time.RFC3339)
	}
	if l.TrialEnd != nil {
		result["trial_end"] = l.TrialEnd.Format(time.RFC3339)
	}

	return result
}

// ── Persistence ─────────────────────────────────────────────────────

func (m *Manager) load() {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var l License
	if err := json.Unmarshal(data, &l); err != nil {
		log.Printf("[LICENSE] %s is corrupt (%v) — quarantining", m.path, err)
		_, _ = atomicfile.Quarantine(m.path)
		return
	}
	m.license = &l
}

// save persists the license atomically. NOTE: callers hold m.mu (write lock)
// when calling this, so it must NOT acquire the lock itself (RWMutex is not
// re-entrant — that would deadlock).
func (m *Manager) save() {
	data, err := json.MarshalIndent(m.license, "", "  ")
	if err != nil {
		return
	}
	if err := atomicfile.WriteFile(m.path, data, 0600); err != nil {
		log.Printf("[LICENSE] save failed: %v", err)
	}
}
