package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"tgcontrol-relay/internal/db"
)

// cloudGate checks paid cloud operations. Local SSH/SFTP has a separate entry
// point in server_access.go backed by the same account entitlement and trial.
//
// Канон: «Локальная работа бесплатна. Серверы и облако — по подписке.» Локальные
// подключения до релея не доходят вовсе, поэтому проверять «а не дома ли он»
// здесь не нужно: сам факт, что запрос пришёл сюда, означает «через облако».
//
// Возвращает false, если запрос уже обработан (клиенту отправлен отказ).
// Побочно запускает пробный период при первом облачном подключении.
func (s *Server) cloudGate(w http.ResponseWriter, r *http.Request, userID int64) bool {
	if s.Config == nil {
		writeErrCode(w, http.StatusServiceUnavailable, "server_access_unavailable", "Не удалось проверить подписку. Попробуйте ещё раз.")
		return false
	}
	// Проба стартует ровно один раз и именно здесь — в момент, когда человек
	// впервые реально воспользовался облаком.
	if !s.selfHosted() {
		if err := db.StartTrialIfNeeded(r.Context(), s.DB, userID, s.Config.TrialDays); err != nil {
			log.Printf("[BILLING] trial start failed user=%d: %v", userID, err)
			writeErrCode(w, http.StatusServiceUnavailable, "server_access_unavailable", "Не удалось проверить пробный период. Попробуйте ещё раз.")
			return false
		}
	}

	dec, err := s.cloudAccess(r.Context(), userID)
	if err != nil {
		log.Printf("[BILLING] cloud access check failed user=%d: %v", userID, err)
		writeErrCode(w, http.StatusServiceUnavailable, "server_access_unavailable", "Не удалось проверить подписку. Попробуйте ещё раз.")
		return false
	}
	if dec.Allowed {
		return true
	}

	log.Printf("[BILLING] cloud denied user=%d tier=%s", userID, dec.Tier)
	writeErrCode(w, http.StatusPaymentRequired, dec.Code, dec.Reason)
	return false
}

// Recheck long-lived viewers, without disconnecting the agent or killing work.
func (s *Server) watchCloudAccess(ctx context.Context, conn *websocket.Conn, userID int64, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			access, err := s.cloudAccess(checkCtx, userID)
			cancel()
			if err != nil || !access.Allowed {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "subscription_required"), time.Now().Add(time.Second))
				_ = conn.Close()
				return
			}
		}
	}
}
