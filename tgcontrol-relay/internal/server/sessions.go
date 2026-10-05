package server

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"tgcontrol-relay/internal/db"
)

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	sessions, err := db.ListUserSessions(r.Context(), s.DB, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		item := map[string]any{
			"id":           session.SessionID,
			"client_kind":  session.ClientKind,
			"client_name":  session.ClientName,
			"ip_address":   session.IPAddress,
			"created_at":   session.CreatedAt,
			"last_seen_at": session.LastSeenAt,
			"current":      session.SessionID == claims.SessionID,
		}
		if session.ExpiresAt.Valid {
			item["expires_at"] = session.ExpiresAt.Time
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out, "current_session_id": claims.SessionID})
}

func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	sessionID := strings.TrimSpace(chi.URLParam(r, "sessionID"))
	if sessionID == "" {
		writeErr(w, http.StatusBadRequest, "session id required")
		return
	}
	// Текущий сеанс закрывают выходом из аккаунта, а не отзывом. Код нужен,
	// чтобы клиент не выдал этот отказ за конфликт пейринга (находка N131).
	if sessionID == claims.SessionID {
		writeErrCode(w, http.StatusConflict, "current_session",
			"Это текущий сеанс — закройте его выходом из аккаунта.")
		return
	}
	if err := db.RevokeUserSession(r.Context(), s.DB, claims.UserID, sessionID); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	// Bound the body even though the endpoint currently has no options. This
	// keeps accidental giant requests out and leaves room for future filters.
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 1<<12))
	count, err := db.RevokeOtherUserSessions(r.Context(), s.DB, claims.UserID, claims.SessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": count})
}
