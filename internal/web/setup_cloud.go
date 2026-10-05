package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/relay"
)

// BuildCloudPairPayload builds the QR/deep-link payload the phone scans to pair
// over the cloud relay: the Remotai app reads relay + code and calls
// confirm-native, so no manual typing is needed. Exported so the headless
// `remotai pair` CLI renders the same payload as an ASCII QR.
func BuildCloudPairPayload(relayBase, code string) string {
	q := url.Values{}
	q.Set("relay", relayBase)
	q.Set("code", code)
	return "remotai://pair?" + q.Encode()
}

// Keep the short-lived pairing code in the fragment, including across login.
// The account page pre-fills it but still requires an explicit confirmation.
func buildCloudPairAccountURL(relayBase, code string) string {
	u, err := url.Parse(relayBase)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return ""
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/app/"
	u.RawPath, u.RawQuery = "", ""
	u.Fragment, u.RawFragment = "", ""
	q := url.Values{"add": {"1"}, "type": {"computer"}, "code": {code}}
	return u.String() + "#/cloud-login?next=" + url.QueryEscape("/infrastructure?"+q.Encode())
}

// POST /api/setup/cloud/pair-request — десктоп запрашивает у relay код для подключения
// и возвращает его в UI для отображения пользователю.
func (s *Server) apiSetupCloudPairRequest(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetNoSetup()
	base := cfg.RelayHTTPBase()
	if base == "" {
		base = config.DefaultRelayURL
	}
	deviceID := config.GetOrCreateDeviceID()
	hn, _ := os.Hostname()

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// device-JWT этого ПК (пусто, пока не привязан). Релею он нужен, чтобы выпустить
	// код на ДОБАВЛЕНИЕ устройств к уже-привязанному ПК. Как в disconnect-хендлере:
	// сперва из памяти, иначе из keystore.
	deviceJWT := cfg.RelayJWT
	if deviceJWT == "" {
		deviceJWT, _ = relay.LoadJWT()
	}
	resp, err := relay.RequestPairingCode(ctx, base, deviceID, hn, deviceJWT)
	if err != nil {
		log.Printf("[SETUP-CLOUD] pair-request failed: %v", err)
		jsonError(w, "relay unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Сохраним device_id + relay_base_url сразу, чтобы при подтверждении уже всё было готово.
	// Mode НЕ трогаем на настроенной системе: подключение телефона через облако —
	// ортогонально режиму Telegram-бота (пайринг не должен отключать own_bot).
	// Только первичный setup-wizard выбирает central_bot здесь.
	_ = config.Update(func(c *config.Config) {
		if !c.SetupComplete {
			c.Mode = config.ModeCentralBot
		}
		c.DeviceID = deviceID
		c.RelayBaseURL = base
		c.RelayURL = relayWSFromHTTP(base)
		c.PendingPairCode = resp.Code
		c.PendingPairExpiresAt = resp.ExpiresAt.Unix()
	})
	// Окно настройки больше не обязано оставаться открытым: сервер сам дождётся
	// подтверждения в Telegram и сохранит device-JWT. Pending-код в config
	// позволяет продолжить ожидание и после перезапуска приложения.
	s.startCloudPairing(base, resp.Code, resp.ExpiresAt)

	payload := BuildCloudPairPayload(base, resp.Code)
	jsonResp(w, map[string]any{
		"code":        resp.Code,
		"expires_at":  resp.ExpiresAt.Format(time.RFC3339),
		"bot_link":    resp.BotLink,
		"relay_base":  base,
		"account_url": buildCloudPairAccountURL(base, resp.Code),
		"qr_data":     payload,
		"qr_url":      "/api/setup/qr?data=" + url.QueryEscape(payload) + "&size=288",
	})
}

// GET /api/setup/cloud/pair-status?code=... — UI опрашивает.
// Если confirmed: сохраняем JWT и закрываем setup.
func (s *Server) apiSetupCloudPairStatus(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		jsonError(w, "code required", http.StatusBadRequest)
		return
	}
	cfg := config.GetNoSetup()
	base := cfg.RelayHTTPBase()
	if base == "" {
		base = config.DefaultRelayURL
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	status, err := relay.CheckPairingStatus(ctx, base, code)
	if err != nil {
		jsonError(w, "relay error: "+err.Error(), http.StatusBadGateway)
		return
	}
	if status.Confirmed && status.JWT != "" {
		if err := s.completeCloudPairing(code, status); err != nil {
			jsonError(w, "config save: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else if status.Expired {
		s.finishCloudPairing(code)
		clearPendingCloudPairing(code)
	}

	jsonResp(w, map[string]any{
		"confirmed":  status.Confirmed,
		"expired":    status.Expired,
		"device_id":  status.DeviceID,
		"expires_at": status.ExpiresAt.Format(time.RFC3339),
	})
}

const cloudPairPollInterval = 2 * time.Second

// waitCloudPairing polls until the relay confirms the code, marks it expired,
// or the bounded context ends. Transient relay failures do not discard a valid
// pairing attempt: the next tick retries it.
func waitCloudPairing(
	ctx context.Context,
	interval time.Duration,
	check func(context.Context) (*relay.PairingStatus, error),
) (*relay.PairingStatus, error) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		status, err := check(ctx)
		if err == nil {
			if status != nil && status.Confirmed && status.JWT != "" {
				return status, nil
			}
			if status != nil && status.Expired {
				return status, nil
			}
		}
		timer.Reset(interval)
	}
}

// startCloudPairing keeps the latest pairing attempt alive independently of
// the setup WebView. Closing/navigating away from the page must not strand the
// JWT at the relay after Telegram already said that the PC was connected.
func (s *Server) startCloudPairing(base, code string, expiresAt time.Time) {
	code = strings.TrimSpace(code)
	if code == "" {
		return
	}
	if base == "" {
		base = config.DefaultRelayURL
	}
	if expiresAt.IsZero() || !expiresAt.After(time.Now()) {
		clearPendingCloudPairing(code)
		return
	}

	ctx, cancel := context.WithDeadline(context.Background(), expiresAt.Add(5*time.Second))
	s.cloudPairMu.Lock()
	if s.cloudPairCancel != nil {
		s.cloudPairCancel()
	}
	s.cloudPairCode = code
	s.cloudPairCancel = cancel
	s.cloudPairMu.Unlock()

	go func() {
		status, err := waitCloudPairing(ctx, cloudPairPollInterval, func(parent context.Context) (*relay.PairingStatus, error) {
			checkCtx, checkCancel := context.WithTimeout(parent, 10*time.Second)
			defer checkCancel()
			return relay.CheckPairingStatus(checkCtx, base, code)
		})
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				log.Printf("[SETUP-CLOUD] background pair-status failed: %v", err)
			}
			s.finishCloudPairing(code)
			clearPendingCloudPairing(code)
			return
		}
		if status == nil || status.Expired {
			s.finishCloudPairing(code)
			clearPendingCloudPairing(code)
			return
		}
		if err := s.completeCloudPairing(code, status); err != nil {
			log.Printf("[SETUP-CLOUD] confirmed pairing could not be saved: %v", err)
		}
	}()
}

// completeCloudPairing is shared by the HTTP poll and the background poller.
// It is intentionally idempotent: both may observe confirmation at once.
func (s *Server) completeCloudPairing(code string, status *relay.PairingStatus) error {
	if status == nil || !status.Confirmed || status.JWT == "" {
		return errors.New("pairing is not confirmed")
	}
	// HTTP-poll страницы и фоновый poller могут увидеть один ответ одновременно.
	// Сериализуем сохранение, чтобы keystore не получал две параллельные записи.
	s.cloudPairCompleteMu.Lock()
	defer s.cloudPairCompleteMu.Unlock()
	cfg := config.GetNoSetup()
	if pending := strings.TrimSpace(cfg.PendingPairCode); pending != "" && !strings.EqualFold(pending, strings.TrimSpace(code)) {
		// A newer code replaced this watcher while its final HTTP request was
		// already in flight. Never let the stale result overwrite that attempt.
		return nil
	}
	if cfg.RelayJWT != "" && cfg.RelayJWT == status.JWT {
		clearPendingCloudPairing(code)
		s.finishCloudPairing(code)
		return nil
	}
	// device_id + relay_base were written during pair-request; only the issued
	// identity remains. Headless pairing uses PersistPairedIdentity directly.
	if err := PersistPairedIdentity("", "", status.JWT, status.ExpiresAt); err != nil {
		return err
	}
	clearPendingCloudPairing(code)
	s.finishCloudPairing(code)
	s.relayKickMu.RLock()
	kick := s.relayKick
	s.relayKickMu.RUnlock()
	if kick != nil {
		kick()
	}
	setupDoneOnce.Do(func() { close(SetupDone) })
	log.Printf("[SETUP-CLOUD] pairing confirmed in background")
	return nil
}

func (s *Server) finishCloudPairing(code string) {
	s.cloudPairMu.Lock()
	defer s.cloudPairMu.Unlock()
	if s.cloudPairCode != code {
		return
	}
	if s.cloudPairCancel != nil {
		s.cloudPairCancel()
	}
	s.cloudPairCode = ""
	s.cloudPairCancel = nil
}

func clearPendingCloudPairing(code string) {
	_ = config.Update(func(c *config.Config) {
		if code != "" && !strings.EqualFold(strings.TrimSpace(c.PendingPairCode), strings.TrimSpace(code)) {
			return
		}
		c.PendingPairCode = ""
		c.PendingPairExpiresAt = 0
	})
}

// resumeCloudPairing is called for every new web server. A code issued before
// an app restart remains claimable until its relay TTL expires.
func (s *Server) resumeCloudPairing() {
	cfg := config.GetNoSetup()
	code := strings.TrimSpace(cfg.PendingPairCode)
	if code == "" {
		return
	}
	expiresAt := time.Unix(cfg.PendingPairExpiresAt, 0)
	if cfg.PendingPairExpiresAt <= 0 || !expiresAt.After(time.Now()) {
		clearPendingCloudPairing(code)
		return
	}
	base := cfg.RelayHTTPBase()
	if base == "" {
		base = config.DefaultRelayURL
	}
	s.startCloudPairing(base, code, expiresAt)
}

// PersistPairedIdentity writes the device identity from a successful pairing to
// config + keystore. Shared by the web setup flow (apiSetupCloudPairStatus) and
// the headless `remotai pair` CLI so both persist identity identically. It does
// NOT touch a running server (relay kick / SetupDone) — callers handle that.
//
// deviceID/relayBase may be empty when the caller already persisted them (the
// web flow saves them in the pair-request step); the headless CLI passes them
// here since it has no request-phase persistence.
func PersistPairedIdentity(deviceID, relayBase, jwt string, expiresAt time.Time) error {
	if jwt == "" {
		return errors.New("empty pairing jwt")
	}
	// Мутации конфига — через Update (copy-on-write), иначе in-place запись
	// RelayJWT гонится с релей-клиентом, читающим его на каждом реконнекте.
	if err := config.Update(func(c *config.Config) {
		if deviceID != "" {
			c.DeviceID = deviceID
		}
		if relayBase != "" {
			c.RelayBaseURL = relayBase
			c.RelayURL = relayWSFromHTTP(relayBase)
		}
		c.RelayJWT = jwt
		c.RelayJWTExpiry = expiresAt.Unix()
		// Mode выбирается один раз — первичным setup. Повторный пайринг на
		// настроенной системе не должен переключать режим бота.
		if !c.SetupComplete {
			c.Mode = config.ModeCentralBot
		}
		c.SetupComplete = true
	}); err != nil {
		return err
	}
	// Save без JWT (`json:"-"`), хранилище — отдельно.
	if err := relay.SaveJWT(jwt); err != nil {
		log.Printf("[PAIR] keystore SaveJWT: %v (fallback file used)", err)
	}
	// API-токен для локального fallback (без обязательности).
	config.GetNoSetup().EnsureAPIToken(0)
	return nil
}

// POST /api/setup/cloud/cancel — пользователь нажал «Отмена», возвращаемся к выбору режима.
func (s *Server) apiSetupCloudCancel(w http.ResponseWriter, r *http.Request) {
	s.finishCloudPairing(config.GetNoSetup().PendingPairCode)
	_ = config.Update(func(c *config.Config) {
		c.RelayJWT = ""
		c.RelayJWTExpiry = 0
		c.PendingPairCode = ""
		c.PendingPairExpiresAt = 0
	})
	_ = relay.ClearJWT()
	jsonResp(w, map[string]bool{"ok": true})
}

// POST /api/setup/cloud/disconnect — «Отключить облако» из панели управления:
// сбрасывает relay-JWT и рвёт живое подключение. Телефоны теряют доступ
// (устройство для них офлайн); для повторного подключения — новый QR.
func (s *Server) apiSetupCloudDisconnect(w http.ResponseWriter, r *http.Request) {
	// Пока device-JWT ещё на руках — отзываем устройство на релее, чтобы телефоны
	// реально потеряли доступ И последующий ре-пейринг (в т.ч. с нового аккаунта)
	// прошёл штатно (reassign снимает revoked_at). Best-effort: офлайн/протухший
	// JWT не должны мешать локальному отключению.
	cfg := config.GetNoSetup()
	jwt := cfg.RelayJWT
	if jwt == "" {
		jwt, _ = relay.LoadJWT()
	}
	if jwt != "" {
		base := cfg.RelayHTTPBase()
		if base == "" {
			base = config.DefaultRelayURL
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		if err := relay.RevokeSelf(ctx, base, jwt); err != nil {
			log.Printf("[SETUP-CLOUD] self-revoke on relay failed (continuing): %v", err)
		} else {
			log.Printf("[SETUP-CLOUD] device revoked on relay")
		}
		cancel()
	}

	s.finishCloudPairing(cfg.PendingPairCode)
	if err := config.Update(func(c *config.Config) {
		c.RelayJWT = ""
		c.RelayJWTExpiry = 0
		c.PendingPairCode = ""
		c.PendingPairExpiresAt = 0
	}); err != nil {
		jsonError(w, "config save: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = relay.ClearJWT()
	// Рвём текущее соединение: клиент перечитает конфиг, не найдёт JWT и
	// встанет в standby — устройство мгновенно пропадает из облака.
	s.relayKickMu.RLock()
	kick := s.relayKick
	s.relayKickMu.RUnlock()
	if kick != nil {
		kick()
	}
	log.Printf("[SETUP-CLOUD] cloud disconnected — relay JWT cleared")
	jsonResp(w, map[string]bool{"ok": true})
}

// relayWSFromHTTP — превращает https://relay → wss://relay/v1/agent/connect.
func relayWSFromHTTP(base string) string {
	if len(base) > 8 && base[:8] == "https://" {
		return "wss://" + base[8:] + "/v1/agent/connect"
	}
	if len(base) > 7 && base[:7] == "http://" {
		return "ws://" + base[7:] + "/v1/agent/connect"
	}
	return ""
}
