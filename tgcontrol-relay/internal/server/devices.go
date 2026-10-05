package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"tgcontrol-relay/internal/auth"
	"tgcontrol-relay/internal/db"
)

// GET /v1/me
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	u, err := db.GetUserByID(r.Context(), s.DB, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cnt, _ := db.CountDevices(r.Context(), s.DB, u.ID)
	identities, identityErr := db.ListIdentities(r.Context(), s.DB, u.ID)
	if identityErr != nil {
		writeErr(w, http.StatusInternalServerError, identityErr.Error())
		return
	}
	permanent := u.TelegramID > 0 || len(identities) > 0
	loginProvider, loginDisplay := "", ""
	if u.TelegramID > 0 {
		loginProvider = "telegram"
		loginDisplay = firstNonEmpty(u.Username, u.FirstName)
	} else if len(identities) > 0 {
		loginProvider = identities[0].Provider
		loginDisplay = identities[0].Display
		if loginDisplay == "" {
			loginDisplay = identities[0].ProviderUID
		}
	}
	effectiveTier := s.effectiveTier(u)
	resp := map[string]any{
		"id":             u.ID,
		"telegram_id":    u.TelegramID,
		"username":       u.Username,
		"first_name":     u.FirstName,
		"locale":         u.Locale,
		"tier":           u.Tier,
		"effective_tier": effectiveTier,
		// Прямой ответ на вопрос «работает ли у меня сейчас удалённый доступ».
		// Клиенту не нужно самому выводить его из тарифа, пробы и режима беты —
		// правило одно и живёт на сервере (db.CloudAccess).
		"cloud_allowed":    effectiveTier != "free",
		"founder":          u.Founder,
		"beta":             false,
		"billing_enabled":  s.billingEnabled(),
		"self_hosted":      s.selfHosted(),
		"devices_count":    cnt,
		"max_devices":      s.maxDevicesForTier(effectiveTier),
		"permanent":        permanent,
		"identities_count": len(identities) + boolInt(u.TelegramID > 0),
		"login_provider":   loginProvider,
		"login_display":    loginDisplay,
	}
	if days, ok := u.TrialDaysLeft(); ok && !s.selfHosted() {
		resp["trial_days_left"] = days
		resp["trial_end"] = u.TrialEnd.Time.Format(time.RFC3339)
	}
	if tok, exp := s.slidingUserJWT(r, claims); tok != "" {
		resp["refreshed_jwt"] = tok
		resp["refreshed_expires_at"] = exp
	}
	writeJSON(w, http.StatusOK, resp)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// GET /v1/devices
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	// Owned + granted — клиент видит и свои ПК, и те, к которым допущен грантом.
	devs, err := db.ListDevicesForUser(r.Context(), s.DB, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	requestedWorkspace := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	out := make([]map[string]any, 0, len(devs))
	workspaceCache := map[string]*db.Workspace{}
	groupCache := map[string]*db.DeviceGroup{}
	for _, d := range devs {
		if requestedWorkspace != "" && d.WorkspaceID != requestedWorkspace {
			continue
		}
		online := s.Hub.IsOnline(d.ID) // факт онлайна — из in-memory hub
		role, _ := db.DeviceRole(r.Context(), s.DB, d.ID, claims.UserID)
		workspaceName := "Доступ по приглашению"
		if workspace, ok := workspaceCache[d.WorkspaceID]; ok {
			if workspace != nil {
				workspaceName = workspace.Name
			}
		} else if workspace, werr := db.GetWorkspaceForUser(r.Context(), s.DB, d.WorkspaceID, claims.UserID); werr == nil {
			workspaceCache[d.WorkspaceID] = workspace
			workspaceName = workspace.Name
			if role == "" {
				role = workspace.Role
			}
		} else {
			workspaceCache[d.WorkspaceID] = nil
		}
		var group any
		if d.GroupID != "" {
			if cached, ok := groupCache[d.GroupID]; ok {
				if cached != nil {
					group = map[string]any{"id": cached.ID, "name": cached.Name}
				}
			} else if found, gerr := db.GroupForDevice(r.Context(), s.DB, d.GroupID); gerr == nil {
				groupCache[d.GroupID] = found
				group = map[string]any{"id": found.ID, "name": found.Name}
			} else {
				groupCache[d.GroupID] = nil
			}
		}
		tags, _ := db.TagsForDevice(r.Context(), s.DB, d.ID)
		tagOut := make([]map[string]any, 0, len(tags))
		for _, tag := range tags {
			tagOut = append(tagOut, map[string]any{"id": tag.ID, "name": tag.Name, "color": tag.Color})
		}
		favorite, _ := db.IsDeviceFavorite(r.Context(), s.DB, d.ID, claims.UserID)
		item := map[string]any{
			"id":             d.ID,
			"name":           d.Name,
			"hostname":       d.Hostname,
			"platform":       d.Platform,
			"device_type":    d.DeviceType,
			"workspace_id":   d.WorkspaceID,
			"workspace_name": workspaceName,
			"workspace_role": role,
			"zone":           group,
			"group":          group,
			"tags":           tagOut,
			"favorite":       favorite,
			"agent_version":  d.AgentVersion,
			"online":         online,
			"last_seen_at":   nullTimeString(d.LastSeenAt),
			"paired_at":      d.PairedAt,
		}
		out = append(out, item)
	}
	resp := map[string]any{"devices": out}
	if tok, exp := s.slidingUserJWT(r, claims); tok != "" {
		resp["refreshed_jwt"] = tok
		resp["refreshed_expires_at"] = exp
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /v1/devices/{id}/rename
func (s *Server) handleRenameDevice(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	id := chi.URLParam(r, "deviceID")
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<13)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.Name = trimToLen(body.Name, 64)
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	if err := db.UpdateDeviceInfrastructure(r.Context(), s.DB, id, claims.UserID, &body.Name, nil, nil, nil); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DELETE /v1/devices/{id}
func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	id := chi.URLParam(r, "deviceID")
	if err := db.RevokeDeviceAsMember(r.Context(), s.DB, id, claims.UserID); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	// Гранты — суб-доступ устройства: отзываются вместе с ним (все допущенные
	// теряют доступ).
	_ = db.RevokeDeviceGrants(r.Context(), s.DB, id)
	// Blocklist-маркер: все device-JWT этого устройства перестают приниматься
	// (agent/connect, agent/stream). Best-effort: устройство уже отозвано,
	// а agent/connect дополнительно проверяет revoked_at напрямую.
	if err := auth.RevokeDeviceTokens(r.Context(), s.DB, id, claims.UserID); err != nil {
		log.Printf("[DEVICE] revoke %s: jwt blocklist: %v", id, err)
	}
	s.closeDeviceConnections(id, "revoke")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /v1/device/revoke-self — десктоп-агент отвязывает СВОЁ устройство от облака,
// авторизуясь собственным device-JWT (его держит только сам спаренный ПК). Зовётся
// кнопкой «Удалить этот компьютер из аккаунта»: помечает устройство revoked_at,
// клиенты аккаунта теряют доступ. Идемпотентно: «уже отозвано / нет такого» →
// тоже ok (best-effort disconnect).
// Чужое устройство так не отозвать — нужен именно его device-JWT.
func (s *Server) handleDeviceRevokeSelf(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r.Header.Get("Authorization"))
	if tok == "" {
		writeErr(w, http.StatusUnauthorized, "device token required")
		return
	}
	claims, err := s.JWT.ParseDevice(tok)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid device token: "+err.Error())
		return
	}
	dev, err := db.GetDeviceForAgentAuth(r.Context(), s.DB, claims.DeviceID, claims.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		// Already physically removed: idempotent success, but do not touch any
		// live connection that may now reuse the same external device id.
		// JWT при этом ещё жив — кладём blocklist-маркер, чтобы токен перестал
		// приниматься (agent/stream не проверяет строку устройства напрямую).
		if rerr := auth.RevokeDeviceTokens(r.Context(), s.DB, claims.DeviceID, claims.UserID); rerr != nil {
			log.Printf("[DEVICE] self-revoke %s: jwt blocklist: %v", claims.DeviceID, rerr)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "device token no longer owns this PC")
		return
	}
	if err := db.RevokeDevice(r.Context(), s.DB, dev.UserID, claims.DeviceID); err != nil {
		// already revoked — idempotent for the authenticated current owner
		log.Printf("[DEVICE] self-revoke %s: %v (treated as ok)", claims.DeviceID, err)
	}
	_ = db.RevokeDeviceGrants(r.Context(), s.DB, claims.DeviceID) // допущенные тоже теряют доступ
	// Blocklist-маркер на все device-JWT устройства (см. handleRevokeDevice).
	if err := auth.RevokeDeviceTokens(r.Context(), s.DB, claims.DeviceID, dev.UserID); err != nil {
		log.Printf("[DEVICE] self-revoke %s: jwt blocklist: %v", claims.DeviceID, err)
	}
	if ac := s.Hub.Get(claims.DeviceID); ac != nil {
		ac.Close() // мгновенно рвём живую сессию агента
	}
	if n := s.Hub.CloseLiveBridges(claims.DeviceID, nil); n > 0 {
		log.Printf("[DEVICE] self-revoke %s: closed %d live streams", claims.DeviceID, n)
	}
	log.Printf("[DEVICE] self-revoked device=%s user=%d", claims.DeviceID, dev.UserID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /v1/devices/{id}/health
func (s *Server) handleDeviceHealth(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	id := chi.URLParam(r, "deviceID")
	d, err := s.deviceForUser(r, id, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"online":        s.Hub.IsOnline(id),
		"agent_version": d.AgentVersion,
		"last_seen_at":  nullTimeString(d.LastSeenAt),
	})
}

// deviceForUser — общий хелпер доступа к устройству для КЛИЕНТА (управление,
// стрим, health). Допуск: пользователь ВЛАДЕЛЕЦ (devices.user_id) ИЛИ имеет
// активный грант (device_grants). Это даёт «сколько угодно устройств управляют
// одним ПК». Управление привязкой (rename/revoke) остаётся только владельцу —
// см. db.RenameDevice/RevokeDevice (фильтр по user_id).
func (s *Server) deviceForUser(r *http.Request, deviceID string, userID int64) (*db.Device, error) {
	d, err := db.GetDevice(r.Context(), s.DB, deviceID)
	if err != nil {
		return nil, err
	}
	if d.RevokedAt.Valid {
		return nil, errors.New("device revoked")
	}
	if d.UserID != userID {
		ok, err := db.UserCanAccessDevice(r.Context(), s.DB, deviceID, userID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("device not found")
		}
	}
	return d, nil
}

func (s *Server) closeDeviceConnections(deviceID, reason string) {
	if ac := s.Hub.Get(deviceID); ac != nil {
		ac.Close()
	}
	if n := s.Hub.CloseLiveBridges(deviceID, nil); n > 0 {
		log.Printf("[DEVICE] %s %s: closed %d live streams", reason, deviceID, n)
	}
}

func nullTimeString(t sql.NullTime) any {
	if !t.Valid {
		return nil
	}
	return t.Time
}

func trimToLen(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}
