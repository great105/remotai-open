package server

import (
	"net/http"
	"time"

	"tgcontrol-relay/internal/db"
)

// The device authenticates the machine, never a locally editable licence file.
// GET only describes access; POST starts the SAME trial used by cloudGate.
func (s *Server) handleServerAccess(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.Config == nil {
		writeErrCode(w, http.StatusServiceUnavailable, "server_access_unavailable", "Не удалось проверить подписку. Попробуйте ещё раз.")
		return
	}
	dev, err := s.peerAuth(r)
	if err != nil {
		writeErrCode(w, http.StatusUnauthorized, "server_account_required", "Подключите компьютер к аккаунту Remotai для работы с серверами.")
		return
	}
	u, err := db.GetUserByID(r.Context(), s.DB, dev.UserID)
	if err != nil {
		writeErrCode(w, http.StatusServiceUnavailable, "server_access_unavailable", "Не удалось проверить подписку. Попробуйте ещё раз.")
		return
	}
	if !s.selfHosted() && r.Method == http.MethodPost && u.EffectiveTier(false) == "free" && !u.TrialEnd.Valid {
		if err := db.StartTrialIfNeeded(r.Context(), s.DB, dev.UserID, s.Config.TrialDays); err != nil {
			writeErrCode(w, http.StatusServiceUnavailable, "server_access_unavailable", "Не удалось начать пробный период. Попробуйте ещё раз.")
			return
		}
		u, err = db.GetUserByID(r.Context(), s.DB, dev.UserID)
		if err != nil {
			writeErrCode(w, http.StatusServiceUnavailable, "server_access_unavailable", "Не удалось проверить подписку. Попробуйте ещё раз.")
			return
		}
	}
	// BETA_FREE is not an entitlement. Only a trial, paid access or an existing
	// founder/admin grant authorizes server operations, including over LAN.
	tier := s.effectiveTier(u)
	out := map[string]any{
		"device_id": dev.ID, "allowed": tier != "free", "tier": tier,
		"trial_available": tier == "free" && !u.TrialEnd.Valid && s.Config.TrialDays > 0,
		"trial_days":      s.Config.TrialDays,
		"feature":         "servers",
		"self_hosted":     s.selfHosted(),
	}
	if s.selfHosted() {
		out["trial_days"] = 0
	}
	if !s.selfHosted() && u.TrialEnd.Valid {
		out["trial_end"] = u.TrialEnd.Time.UTC().Format(time.RFC3339)
	}
	if tier == "free" {
		out["code"] = "server_subscription_required"
		out["message"] = "Для работы с серверами нужна подписка Про. Локальная работа на своём компьютере остаётся бесплатной."
	}
	writeJSON(w, http.StatusOK, out)
}
