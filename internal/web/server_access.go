package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"tgcontrol/internal/pty"
	"tgcontrol/internal/relay"
)

func (s *Server) checkServerAccess(ctx context.Context, startTrial bool) (relay.ServerAccess, error) {
	if s.serverAccessCheck != nil {
		return s.serverAccessCheck(ctx, startTrial)
	}
	return relay.CheckServerAccess(ctx, startTrial)
}

func (s *Server) apiServerAccess(w http.ResponseWriter, r *http.Request, uid int64) {
	w.Header().Set("Cache-Control", "no-store")
	access, err := s.checkServerAccess(r.Context(), false)
	if err != nil {
		jsonErrorCode(w, http.StatusServiceUnavailable, "server_access_unavailable", "Не удалось проверить подписку. Локальная работа на своём компьютере доступна.", nil)
		return
	}
	jsonResp(w, access)
}

func (s *Server) requireServerAccess(w http.ResponseWriter, r *http.Request) bool {
	access, err := s.checkServerAccess(r.Context(), true)
	if err != nil {
		jsonErrorCode(w, http.StatusServiceUnavailable, "server_access_unavailable", "Не удалось проверить подписку. Попробуйте ещё раз.", nil)
		return false
	}
	if access.Allowed {
		return true
	}
	if access.Code == "" {
		access.Code = "server_subscription_required"
		access.Message = "Для работы с серверами нужна подписка Про. Локальная работа на своём компьютере остаётся бесплатной."
	}
	jsonErrorCode(w, http.StatusPaymentRequired, access.Code, access.Message, nil)
	return false
}

// Keep metadata, credential management and stopping/cancelling accessible.
// Network operations are paid, including SFTP reads and existing SSH terminals.
func (s *Server) requiresServerAccess(r *http.Request) bool {
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/ssh/") {
		if strings.HasPrefix(p, "/api/ssh/hosts") || p == "/api/ssh/history" || p == "/api/ssh/known-hosts" || strings.HasPrefix(p, "/api/ssh/forward-specs/") {
			return false
		}
		if strings.HasPrefix(p, "/api/ssh/keys") {
			return strings.HasSuffix(p, "/install")
		}
		if p == "/api/ssh/sftp/transfers" || p == "/api/ssh/sftp/transfers/cancel" {
			return false
		}
		if strings.HasPrefix(p, "/api/ssh/forwards") {
			return r.Method == http.MethodPost
		}
		return true
	}
	if strings.HasPrefix(p, "/api/pty/") && s.ptyManager != nil {
		id := r.PathValue("id")
		if id == "" {
			id = strings.Split(strings.TrimPrefix(p, "/api/pty/"), "/")[0]
		}
		return serverTerminalAccessRequired(r.Method, s.ptyManager.Get(id))
	}
	return false
}

func (s *Server) isServerSession(id string) bool {
	if s.ptyManager == nil {
		return false
	}
	session := s.ptyManager.Get(id)
	return session != nil && session.Shell == "ssh"
}

func serverTerminalAccessRequired(method string, session *pty.Session) bool {
	return session != nil && session.Shell == "ssh" && method != http.MethodDelete && method != http.MethodPatch
}

func (s *Server) serverAccessWrap(handler func(http.ResponseWriter, *http.Request, int64)) func(http.ResponseWriter, *http.Request, int64) {
	return func(w http.ResponseWriter, r *http.Request, uid int64) {
		if s.requiresServerAccess(r) && !s.requireServerAccess(w, r) {
			return
		}
		handler(w, r, uid)
	}
}

// A long-lived socket must not keep paid access forever after one successful
// handshake. Closing the viewer never kills the terminal or its remote task.
func (s *Server) watchServerSocket(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			access, err := s.checkServerAccess(ctx, false)
			if err != nil || !access.Allowed {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "server_access_required"), time.Now().Add(time.Second))
				_ = conn.Close()
				return
			}
		}
	}
}

func (s *Server) watchServerForward(id string) {
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	for range timer.C {
		found := false
		for _, forward := range s.sshForwards.List() {
			if forward.ID == id {
				found = true
				break
			}
		}
		if !found {
			return
		}
		access, err := s.checkServerAccess(context.Background(), false)
		if err != nil || !access.Allowed {
			_ = s.sshForwards.Stop(id)
			return
		}
	}
}
