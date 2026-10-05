package web

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"tgcontrol/internal/pty"
)

// POST /api/ssh/connect — подключить SSH-сервер ЧЕРЕЗ АГЕНТ (бастион) и открыть
// его как обычный PTY-терминал. Ответ идентичен POST /api/pty: {"id": …};
// дальше клиент идёт на тот же /ws/pty/{id} — нового протокола нет.
//
// Ошибки — 4xx с машинным полем code (клиент ветвится по нему, а не по тексту):
//
//	host_key_unknown (409, +fingerprint) — сервер неизвестен; клиент спрашивает
//	                  доверие и повторяет с trust_host:true (TOFU);
//	auth_failed (400) — пароль/ключи не подошли;
//	unreachable (400) — хост недоступен с ПК-агента (dial/таймаут);
//	bad_request (400) — не хватает host/user или битое тело.
//
// Изменившийся хост-ключ (MITM) машинного кода не имеет — всегда 500, без обхода.
func (s *Server) apiSSHConnect(w http.ResponseWriter, r *http.Request, uid int64) {
	// Тот же лимит терминалов, что у обычных PTY (см. apiPtyCreate): SSH-сессия
	// живёт в том же списке /api/pty.
	if s.licenseManager != nil {
		limits := s.licenseManager.GetLimits()
		if limits.MaxPTYTerminals > 0 {
			active := len(s.ptyManager.List())
			if active >= limits.MaxPTYTerminals {
				log.Printf("[LICENSE] PTY limit reached (%d/%d, tier: %s)", active, limits.MaxPTYTerminals, s.licenseManager.GetTier())
				jsonErrorCodeAny(w, 403, "pty_limit", "Достигнут лимит терминалов. Закройте ненужный терминал и повторите.", map[string]any{
					"count": active, "limit": limits.MaxPTYTerminals,
				})
				return
			}
		}
	}

	var req struct {
		HostID        string `json:"host_id"`
		Host          string `json:"host"`
		Port          int    `json:"port"`
		User          string `json:"user"`
		Password      string `json:"password"`
		IdentityFile  string `json:"identity_file"`
		KeyPassphrase string `json:"key_passphrase"`
		Cols          int    `json:"cols"`
		Rows          int    `json:"rows"`
		TrustHost     bool   `json:"trust_host"`
		ProxyJump     string `json:"proxy_jump"`     // "user@host:port", опционально
		ProxyPassword string `json:"proxy_password"` // пароль бастиона, только в память хендшейка
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	req.User = strings.TrimSpace(req.User)
	req.ProxyJump = strings.TrimSpace(req.ProxyJump)
	req.HostID = strings.TrimSpace(req.HostID)
	req.IdentityFile = strings.TrimSpace(req.IdentityFile)
	// Ключ из хранилища приложения: приходит содержимым, наружу не уходит.
	var keyPEM string
	if req.HostID != "" {
		h, ok := s.resolveSSHHost(req.HostID)
		if !ok {
			jsonErrorCode(w, 404, "not_found", "host not found", nil)
			return
		}
		pemData, keyPass, keyErr := s.sshHostKeyMaterial(h.KeyID)
		if keyErr != nil {
			writeSSHKeyError(w, keyErr)
			return
		}
		keyPEM = pemData
		if req.KeyPassphrase == "" {
			req.KeyPassphrase = keyPass
		}
		if req.Host == "" {
			req.Host = h.Host
		}
		if req.Port == 0 {
			req.Port = h.Port
		}
		if req.User == "" {
			req.User = h.User
		}
		if req.IdentityFile == "" {
			req.IdentityFile = h.IdentityFile
		}
		if req.ProxyJump == "" {
			req.ProxyJump = h.ProxyJump
		}
		if secret, ok := s.sshSecretFor(req.HostID); ok {
			if req.Password == "" {
				req.Password = secret.password
			}
			if req.KeyPassphrase == "" {
				req.KeyPassphrase = secret.keyPassphrase
			}
			if req.ProxyPassword == "" {
				req.ProxyPassword = secret.proxyPassword
			}
		}
	}
	if req.Host == "" || req.User == "" {
		jsonErrorCode(w, 400, "bad_request", "host and user are required", nil)
		return
	}
	if req.Port < 0 || req.Port > 65535 {
		jsonErrorCode(w, 400, "bad_request", "invalid port", nil)
		return
	}
	if req.Port == 0 {
		req.Port = 22
	}
	if req.Cols <= 0 {
		req.Cols = 80
	}
	if req.Rows <= 0 {
		req.Rows = 24
	}

	sess, err := s.ptyManager.CreateSSH(uid, pty.SSHConfig{
		HostID:        req.HostID,
		Host:          req.Host,
		Port:          req.Port,
		User:          req.User,
		Password:      req.Password, // только в память хендшейка; не логируем
		Cols:          req.Cols,
		Rows:          req.Rows,
		TrustHost:     req.TrustHost,
		ProxyJump:     req.ProxyJump,
		ProxyPassword: req.ProxyPassword,
		IdentityFile:  req.IdentityFile,
		KeyPassphrase: req.KeyPassphrase,
		PrivateKeyPEM: keyPEM,
	})
	if err != nil {
		writeSSHError(w, err)
		return
	}
	// История «недавних» хостов (last_at/count для сортировки в /api/ssh/hosts).
	// Ошибка записи не фатальна — подключение уже живёт.
	if s.sshHistory != nil {
		_ = s.sshHistory.Record(req.Host, req.Port, req.User, req.ProxyJump)
	}
	jsonResp(w, map[string]string{"id": sess.ID})
}

// writeSSHError маппит ошибки SSH-подключения (pty) в машинные code-ответы:
// клиент ветвится по коду, а не по тексту. Общий для connect/forwards/sftp.
//
//	host_key_unknown (409, +fingerprint) — TOFU: клиент спрашивает доверие и
//	                  повторяет с trust_host:true;
//	auth_failed (400), unreachable (400);
//	прочее (включая KeyMismatch/MITM) — 500 без обхода. Пароля в err нет.
func writeSSHError(w http.ResponseWriter, err error) {
	var hkErr *pty.ErrHostKeyUnknown
	var mismatch *pty.ErrHostKeyMismatch
	var encrypted *pty.ErrSSHKeyEncrypted
	var authDetails *pty.ErrSSHAuthDetails
	switch {
	case errors.As(err, &hkErr):
		jsonErrorCode(w, 409, "host_key_unknown", "unknown host key",
			map[string]string{"fingerprint": hkErr.Fingerprint})
	case errors.As(err, &mismatch):
		jsonErrorCodeAny(w, 409, "host_key_mismatch", "host key changed", map[string]any{
			"host": mismatch.Host, "fingerprint": mismatch.Got, "expected": mismatch.Fingerprints,
		})
	case errors.As(err, &encrypted):
		jsonErrorCode(w, 400, "key_encrypted", "private key requires a passphrase",
			map[string]string{"identity_file": encrypted.Path})
	case errors.As(err, &authDetails):
		jsonErrorCodeAny(w, 400, "auth_failed", "authentication failed",
			map[string]any{"tried": authDetails.Tried})
	case errors.Is(err, pty.ErrSSHAuth):
		jsonErrorCode(w, 400, "auth_failed", "authentication failed", nil)
	case errors.Is(err, pty.ErrSSHUnreachable):
		jsonErrorCode(w, 400, "unreachable", "host unreachable from agent", nil)
	default:
		log.Printf("[SSH-API] error: %v", err)
		jsonError(w, err.Error(), 500)
	}
}

func jsonErrorCodeAny(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"error": msg, "code": code}
	for k, v := range extra {
		body[k] = v
	}
	_ = json.NewEncoder(w).Encode(body)
}

// jsonErrorCode — jsonError с машинным полем code и опциональными extra-полями
// (fingerprint и т.п.), чтобы клиент ветвился по коду, а не по тексту ошибки.
func jsonErrorCode(w http.ResponseWriter, status int, code, msg string, extra map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]string{"error": msg, "code": code}
	for k, v := range extra {
		body[k] = v
	}
	json.NewEncoder(w).Encode(body)
}
