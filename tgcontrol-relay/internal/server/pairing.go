package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"tgcontrol-relay/internal/auth"
	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/metrics"
)

// pairCodeNotFoundMsg — текст для «такого кода нет». Отдаётся вместе с машинным
// кодом pair_code_not_found: без кода клиент разбирал только HTTP-статус и
// показывал на неверно набранный код бессмысленное «Не найдено.»
// (packages/shared/src/api-core.ts, ветка 404). Клиент предпочитает свой перевод
// кода, этот текст — фолбэк для старых сборок и curl. Срок берём из конфига
// (PAIR_CODE_TTL), а не «15 минут» строкой: расхождение с настройкой стенда
// сбивало бы с толку ровно там, где человек и так не понимает, что не так.
func (s *Server) pairCodeNotFoundMsg() string {
	return fmt.Sprintf("Код подключения не найден — скорее всего он устарел: код живёт %s, "+
		"а ещё через час исчезает совсем. Возьмите свежий в окне Remotai на том компьютере.",
		humanTTL(s.Config.PairCodeTTL))
}

// humanTTL — срок жизни кода словами, которые человек говорит вслух. «60 мин»
// формально верно, но в живой переписке («код живёт час, успеете») никто так не
// пишет, а этот текст читают именно в такой момент — когда подключение уже не
// вышло. Целые часы называем часами, остальное — минутами.
func humanTTL(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		h := int(d.Hours())
		switch {
		case h == 1:
			return "час"
		case h%10 >= 2 && h%10 <= 4 && (h < 10 || h > 20):
			return fmt.Sprintf("%d часа", h)
		default:
			return fmt.Sprintf("%d часов", h)
		}
	}
	return fmt.Sprintf("%d мин", max(int(d.Minutes()), 1))
}

// pairCodeLockedMsg — текст для кода, сгоревшего по счётчику неудачных попыток
// (анти-брутфорс, db.MaxPairCodeAttempts). Машинный код — pair_code_locked.
func pairCodeLockedMsg() string {
	return fmt.Sprintf("Код заблокирован после %d неверных попыток. Получите новый код на компьютере.",
		db.MaxPairCodeAttempts)
}

// pairAttemptFailed фиксирует неудачную попытку использовать код (анти-брутфорс,
// см. db.IncAttempts). Ошибка записи не влияет на ответ клиенту.
func (s *Server) pairAttemptFailed(r *http.Request, code string) {
	if err := db.IncAttempts(r.Context(), s.DB, code); err != nil {
		log.Printf("[PAIR] inc attempts %s: %v", code, err)
	}
}

// POST /v1/pair/request — десктоп получает pairing code.
func (s *Server) handlePairRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID     string `json:"device_id"`
		Hostname     string `json:"hostname"`
		Platform     string `json:"platform"`
		AgentVersion string `json:"agent_version"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.DeviceID == "" {
		writeErr(w, http.StatusBadRequest, "device_id required")
		return
	}
	// Имя/платформа приходят из ОТКРЫТОГО эндпоинта и раньше не ограничивались
	// ничем, кроме 16 КБ тела: гигантский hostname ломал и карточку устройства,
	// и ответ бота (Telegram режет сообщение на 4096 символах). Предел тот же,
	// что и у ручного переименования (handleRenameDevice).
	req.Hostname = trimToLen(req.Hostname, 64)
	req.Platform = trimToLen(req.Platform, 32)
	req.AgentVersion = trimToLen(req.AgentVersion, 32)
	// Если устройство УЖЕ привязано (и не отозвано), код на ДОБАВЛЕНИЕ управляющих
	// может выпустить только сам ПК — доказательство его device-JWT. Без этого
	// любой, зная device_id, выпустил бы код через этот открытый эндпоинт и
	// получил грант = захват чужого ПК (раньше от этого защищал 409 при пейринге,
	// теперь, когда чужой код даёт ГРАНТ, защиту переносим сюда). Первый пейринг
	// (устройства ещё нет) и takeover отозванного — по-прежнему без авторизации.
	if existing, gerr := db.GetDevice(r.Context(), s.DB, req.DeviceID); gerr == nil && !existing.RevokedAt.Valid {
		claims, perr := s.JWT.ParseDevice(bearer(r.Header.Get("Authorization")))
		if perr != nil || claims.DeviceID != req.DeviceID {
			// Обычно ловят СТАРЫЕ агенты (до multi-grant), не умеющие слать device-JWT.
			writeErr(w, http.StatusForbidden, "update the Remotai app on this PC to add more devices")
			return
		}
		if _, perr := db.GetDeviceForAgentAuth(r.Context(), s.DB, req.DeviceID, claims.UserID); perr != nil {
			writeErr(w, http.StatusForbidden, "device token no longer owns this PC")
			return
		}
	}
	code, err := db.GenerateCode()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to generate code")
		return
	}
	rec, err := db.InsertPairingCode(r.Context(), s.DB, code, req.DeviceID, req.Hostname, req.Platform, req.AgentVersion, s.Config.PairCodeTTL)
	if err != nil {
		log.Printf("[PAIR] insert: %v", err)
		writeErr(w, http.StatusInternalServerError, "failed to store pairing code")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":       rec.Code,
		"expires_at": rec.ExpiresAt.Format(time.RFC3339),
		"bot_link":   s.botLink(rec.Code),
	})
}

// GET /v1/pair/status?code=... — десктоп опрашивает результат.
// Returns:
//   - { "confirmed": false, "expired": false } — ждать ещё
//   - { "confirmed": false, "expired": true }  — TTL вышел
//   - { "confirmed": true, "jwt", "expires_at", "device_id" } — успех
func (s *Server) handlePairStatus(w http.ResponseWriter, r *http.Request) {
	code := db.NormalizeCode(r.URL.Query().Get("code"))
	if code == "" {
		writeErr(w, http.StatusBadRequest, "code required")
		return
	}
	rec, err := db.GetPairingCode(r.Context(), s.DB, code)
	if err != nil {
		if errors.Is(err, db.ErrCodeNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"confirmed": false, "expired": true, "error": "not_found"})
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rec.ConsumedAt.Valid && rec.IssuedJWT != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"confirmed":  true,
			"jwt":        rec.IssuedJWT,
			"device_id":  rec.DeviceID,
			"expires_at": rec.ExpiresAt.Format(time.RFC3339),
		})
		return
	}
	if time.Now().UTC().After(rec.ExpiresAt) {
		writeJSON(w, http.StatusOK, map[string]any{"confirmed": false, "expired": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"confirmed": false, "expired": false})
}

// POST /v1/pair/confirm — Mini App подтверждает код от имени пользователя.
// Auth: initData (Telegram WebApp) или Bearer JWT user-scoped.
func (s *Server) handlePairConfirm(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	// Rate-limit per user (помимо per-IP в middleware). При ошибке БД — отказ,
	// а не fail-open: иначе сбой/блокировка БД снимали бы защиту от брутфорса.
	recent, err := db.RecentAttemptsByUser(r.Context(), s.DB, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rate check failed")
		return
	}
	if recent >= s.Config.PairMaxAttempts {
		metrics.PairFail()
		writeErr(w, http.StatusTooManyRequests, "too many pairing attempts; try later")
		return
	}

	var body struct {
		Code        string `json:"code"`
		DeviceID    string `json:"device_id,omitempty"` // optional, для override "имени"
		Name        string `json:"name,omitempty"`
		WorkspaceID string `json:"workspace_id,omitempty"`
		DeviceType  string `json:"device_type,omitempty"`
		ZoneID      string `json:"zone_id,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	code := db.NormalizeCode(body.Code)
	if code == "" {
		writeErr(w, http.StatusBadRequest, "code required")
		return
	}
	rec, err := db.GetPairingCode(r.Context(), s.DB, code)
	if err != nil {
		if errors.Is(err, db.ErrCodeNotFound) {
			writeErrCode(w, http.StatusNotFound, "pair_code_not_found", s.pairCodeNotFoundMsg())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Анти-брутфорс: код сгорел после серии неудачных попыток (db.MaxPairCodeAttempts).
	if rec.Attempts >= db.MaxPairCodeAttempts {
		metrics.PairFail()
		writeErrCode(w, http.StatusGone, "pair_code_locked", pairCodeLockedMsg())
		return
	}
	// Код многоразовый в пределах TTL: НЕ отвергаем «уже использован» — повторный
	// скан того же QR с другого устройства добавляет ГРАНТ (BindDeviceForPairing).
	if time.Now().UTC().After(rec.ExpiresAt) {
		metrics.PairFail()
		s.pairAttemptFailed(r, code)
		writeErr(w, http.StatusGone, "code expired")
		return
	}
	workspace, err := workspaceForPairing(r, s.DB, claims.UserID, body.WorkspaceID)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	zoneID, err := zoneForPairing(r, s.DB, workspace.ID, body.ZoneID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	deviceType := strings.ToLower(strings.TrimSpace(body.DeviceType))
	if deviceType != "" && !db.ValidDeviceType(deviceType) {
		writeErr(w, http.StatusBadRequest, "device_type must be computer or server")
		return
	}
	workspaceOwner, err := db.GetUserByID(r.Context(), s.DB, workspace.OwnerUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "workspace owner not found")
		return
	}
	deviceLimit := s.maxDevicesForTier(s.effectiveTier(workspaceOwner))

	// Имя из тела запроса тоже режем по 64 руны — как в handleRenameDevice.
	name := trimToLen(body.Name, 64)
	if name == "" {
		if rec.Hostname != "" {
			name = rec.Hostname
		} else {
			name = "Мой ПК"
		}
	}
	dev := &db.Device{
		ID:           rec.DeviceID,
		UserID:       claims.UserID,
		WorkspaceID:  strings.TrimSpace(body.WorkspaceID),
		Name:         name,
		Hostname:     rec.Hostname,
		Platform:     rec.Platform,
		DeviceType:   deviceType,
		GroupID:      zoneID,
		AgentVersion: rec.AgentVersion,
	}
	primary, overLimit, err := db.BindDeviceForPairing(r.Context(), s.DB, dev, claims.UserID, deviceLimit)
	if overLimit {
		metrics.PairFail()
		s.pairAttemptFailed(r, code)
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"error": "Достигнут лимит устройств пространства. Откройте «Мои компьютеры» и удалите ненужную машину.",
			"code":  "device_limit",
			"limit": deviceLimit,
			"tier":  workspaceOwner.Tier,
		})
		return
	}
	if err != nil {
		metrics.PairFail()
		s.pairAttemptFailed(r, code)
		writeErr(w, http.StatusInternalServerError, "pair bind: "+err.Error())
		return
	}
	boundDevice, err := db.GetDevice(r.Context(), s.DB, rec.DeviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "paired device lookup failed")
		return
	}

	metrics.PairOK()
	resp := map[string]any{
		"ok": true, "device_id": rec.DeviceID,
		"workspace_id": boundDevice.WorkspaceID, "device_type": boundDevice.DeviceType,
		"zone_id": boundDevice.GroupID,
	}
	if primary {
		// Владелец: ПК заберёт device-JWT через /v1/pair/status. Выдаём+консьюмим
		// код только если он ещё свежий (первый пейринг этим кодом).
		if !rec.ConsumedAt.Valid {
			tok, exp, e := s.JWT.IssueDevice(rec.DeviceID, claims.UserID)
			if e != nil {
				writeErr(w, http.StatusInternalServerError, "jwt issue: "+e.Error())
				return
			}
			if _, e := db.ConsumePairingCode(r.Context(), s.DB, code, claims.UserID, tok); e != nil {
				writeErr(w, http.StatusInternalServerError, "consume: "+e.Error())
				return
			}
			resp["jwt"] = tok
			resp["expires_at"] = exp.Format(time.RFC3339)
		}
		log.Printf("[PAIR] device %s owner=user %d", rec.DeviceID, claims.UserID)
	} else {
		resp["granted"] = true
		log.Printf("[PAIR] device %s granted to user %d", rec.DeviceID, claims.UserID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /v1/pair/confirm-native — confirm a pairing code WITHOUT Telegram.
// The phone (APK) enters the code the desktop shows. We reuse the caller's
// existing user JWT if supplied (multi-device on one account), otherwise mint a
// fresh anonymous account. Returns a user JWT for the phone; the desktop agent
// collects its device JWT via the unchanged GET /v1/pair/status poll.
func (s *Server) handlePairConfirmNative(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code        string `json:"code"`
		Name        string `json:"name,omitempty"`
		WorkspaceID string `json:"workspace_id,omitempty"`
		DeviceType  string `json:"device_type,omitempty"`
		ZoneID      string `json:"zone_id,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	code := db.NormalizeCode(body.Code)
	if code == "" {
		writeErr(w, http.StatusBadRequest, "code required")
		return
	}
	rec, err := db.GetPairingCode(r.Context(), s.DB, code)
	if err != nil {
		if errors.Is(err, db.ErrCodeNotFound) {
			writeErrCode(w, http.StatusNotFound, "pair_code_not_found", s.pairCodeNotFoundMsg())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Анти-брутфорс: код сгорел после серии неудачных попыток (db.MaxPairCodeAttempts).
	if rec.Attempts >= db.MaxPairCodeAttempts {
		metrics.PairFail()
		writeErrCode(w, http.StatusGone, "pair_code_locked", pairCodeLockedMsg())
		return
	}
	// Многоразовый код в пределах TTL — повторный скан = грант (BindDeviceForPairing).
	if time.Now().UTC().After(rec.ExpiresAt) {
		metrics.PairFail()
		s.pairAttemptFailed(r, code)
		writeErr(w, http.StatusGone, "code expired")
		return
	}

	// Reuse the caller's account if they already have a user JWT; else create a
	// guest account only for a genuinely first-time client. An invalid/expired
	// Bearer token must never silently fork the infrastructure into a new guest.
	var user *db.User
	if tok := bearer(r.Header.Get("Authorization")); tok != "" {
		claims, authErr := s.requireUserAuth(r)
		if authErr != nil {
			writeErrCode(w, http.StatusUnauthorized, "session_expired", "Сессия аккаунта истекла. Войдите снова и повторите подключение.")
			return
		}
		u, userErr := db.GetUserByID(r.Context(), s.DB, claims.UserID)
		if userErr != nil {
			writeErrCode(w, http.StatusUnauthorized, "session_expired", "Аккаунт этой сессии больше недоступен. Войдите снова.")
			return
		}
		user = u
	}
	if user == nil {
		u, e := db.CreateAnonUser(r.Context(), s.DB)
		if e != nil {
			writeErr(w, http.StatusInternalServerError, "create account: "+e.Error())
			return
		}
		user = u
	}
	workspace, err := workspaceForPairing(r, s.DB, user.ID, body.WorkspaceID)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	zoneID, err := zoneForPairing(r, s.DB, workspace.ID, body.ZoneID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	deviceType := strings.ToLower(strings.TrimSpace(body.DeviceType))
	if deviceType != "" && !db.ValidDeviceType(deviceType) {
		writeErr(w, http.StatusBadRequest, "device_type must be computer or server")
		return
	}
	workspaceOwner, err := db.GetUserByID(r.Context(), s.DB, workspace.OwnerUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "workspace owner not found")
		return
	}
	deviceLimit := s.maxDevicesForTier(s.effectiveTier(workspaceOwner))

	// Имя из тела запроса тоже режем по 64 руны — как в handleRenameDevice.
	name := trimToLen(body.Name, 64)
	if name == "" {
		if rec.Hostname != "" {
			name = rec.Hostname
		} else {
			name = "Мой ПК"
		}
	}
	dev := &db.Device{
		ID:           rec.DeviceID,
		UserID:       user.ID,
		WorkspaceID:  strings.TrimSpace(body.WorkspaceID),
		Name:         name,
		Hostname:     rec.Hostname,
		Platform:     rec.Platform,
		DeviceType:   deviceType,
		GroupID:      zoneID,
		AgentVersion: rec.AgentVersion,
	}
	primary, overLimit, err := db.BindDeviceForPairing(r.Context(), s.DB, dev, user.ID, deviceLimit)
	if overLimit {
		metrics.PairFail()
		s.pairAttemptFailed(r, code)
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"error": "Достигнут лимит устройств пространства. Откройте «Мои компьютеры» и удалите ненужную машину.",
			"code":  "device_limit",
			"limit": deviceLimit,
			"tier":  workspaceOwner.Tier,
		})
		return
	}
	if err != nil {
		metrics.PairFail()
		s.pairAttemptFailed(r, code)
		writeErr(w, http.StatusInternalServerError, "pair bind: "+err.Error())
		return
	}
	boundDevice, err := db.GetDevice(r.Context(), s.DB, rec.DeviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "paired device lookup failed")
		return
	}
	metrics.PairOK()
	// Владелец (первый пейринг этим кодом) → ПК заберёт device-JWT через status-poll.
	// Грант/повторный скан → ничего не выдаём, ПК уже подключён своим JWT.
	if primary && !rec.ConsumedAt.Valid {
		deviceJWT, _, e := s.JWT.IssueDevice(rec.DeviceID, user.ID)
		if e != nil {
			writeErr(w, http.StatusInternalServerError, "device jwt: "+e.Error())
			return
		}
		if _, e := db.ConsumePairingCode(r.Context(), s.DB, code, user.ID, deviceJWT); e != nil {
			writeErr(w, http.StatusInternalServerError, "consume: "+e.Error())
			return
		}
	}

	userJWT, uexp, err := s.mintUserSessionJWT(r, user, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "user jwt: "+err.Error())
		return
	}

	log.Printf("[PAIR] native confirm: device=%s account=%d (anon=%v)", rec.DeviceID, user.ID, user.TelegramID < 0)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"device_id":    rec.DeviceID,
		"workspace_id": boundDevice.WorkspaceID,
		"device_type":  boundDevice.DeviceType,
		"zone_id":      boundDevice.GroupID,
		"user_jwt":     userJWT,
		"expires_at":   uexp.Format(time.RFC3339),
		"tier":         user.Tier,
	})
}

func (s *Server) botLink(code string) string {
	if s.Config.BotUsername == "" {
		return ""
	}
	return "https://t.me/" + s.Config.BotUsername + "?start=pair_" + code
}

func (s *Server) maxDevicesForTier(tier string) int {
	if s.selfHosted() {
		return 0 // no subscription quota on an operator's own infrastructure
	}
	switch tier {
	case "pro":
		return s.Config.ProMaxDevices
	case "team", "fleet":
		return s.Config.TeamMaxDevices
	default:
		return s.Config.FreeMaxDevices
	}
}

// dbUpsertFromInitData кладёт юзера в БД при первой авторизации.
func dbUpsertFromInitData(ctx context.Context, s *Server, init *auth.InitData) (*db.User, error) {
	return db.UpsertUser(ctx, s.DB, init.User.ID, init.User.Username, init.User.FirstName, init.User.LanguageCode)
}
