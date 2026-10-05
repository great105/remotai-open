package web

// SSH port-forwarding (-L / -R / -D) через агента.
//
// Форварды живут только в памяти агента и НЕ персистентны: рестарт агента их
// убивает (пароль не храним, восстановить без него не можем). Клиент после
// рестарта просто пересоздаёт нужные форварды.
//
// Ошибки подключения — те же машинные code, что у /api/ssh/connect
// (host_key_unknown/auth_failed/unreachable), см. writeSSHError.

import (
	"net/http"
	"strings"

	"tgcontrol/internal/pty"
)

// GET /api/ssh/forwards — активные форварды агента.
func (s *Server) apiSSHForwardsList(w http.ResponseWriter, r *http.Request, uid int64) {
	specs := []pty.SavedForwardSpec{}
	if s.sshForwardSpecs != nil {
		specs = s.sshForwardSpecs.List()
	}
	jsonResp(w, map[string]any{"forwards": s.sshForwards.List(), "specs": specs})
}

// POST /api/ssh/forwards — поднять форвард. Ответ {"id": …}.
//
//	type: local | remote | dynamic
//	local:   агент слушает bind_addr:bind_port → target_host:target_port через сервер
//	remote:  сервер слушает bind_addr:bind_port → target_host:target_port с агента
//	dynamic: SOCKS5 на агенте bind_addr:bind_port (target не нужен)
func (s *Server) apiSSHForwardCreate(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		HostID        string `json:"host_id"`
		Host          string `json:"host"`
		Port          int    `json:"port"`
		User          string `json:"user"`
		Password      string `json:"password"` // только в память хендшейка; не логируем
		IdentityFile  string `json:"identity_file"`
		KeyPassphrase string `json:"key_passphrase"`
		TrustHost     bool   `json:"trust_host"`
		ProxyJump     string `json:"proxy_jump"`
		ProxyPassword string `json:"proxy_password"`

		Type       string `json:"type"`
		BindAddr   string `json:"bind_addr"`
		BindPort   int    `json:"bind_port"`
		TargetHost string `json:"target_host"`
		TargetPort int    `json:"target_port"`
		AllowLAN   bool   `json:"allow_lan"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	req.User = strings.TrimSpace(req.User)
	req.HostID = strings.TrimSpace(req.HostID)
	req.IdentityFile = strings.TrimSpace(req.IdentityFile)
	req.ProxyJump = strings.TrimSpace(req.ProxyJump)
	// Ключ из хранилища приложения — как в терминале и в файлах сервера.
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
	if req.BindPort <= 0 || req.BindPort > 65535 {
		jsonErrorCode(w, 400, "bad_request", "invalid bind_port", nil)
		return
	}
	req.BindAddr = strings.TrimSpace(req.BindAddr)
	if req.BindAddr == "" {
		req.BindAddr = "127.0.0.1"
	}
	if !isLoopbackBind(req.BindAddr) && !req.AllowLAN {
		jsonErrorCode(w, 400, "unsafe_bind", "non-loopback bind requires allow_lan", nil)
		return
	}
	switch req.Type {
	case pty.ForwardLocal, pty.ForwardRemote:
		if req.TargetHost == "" || req.TargetPort <= 0 || req.TargetPort > 65535 {
			jsonErrorCode(w, 400, "bad_request", "target_host/target_port are required", nil)
			return
		}
	case pty.ForwardDynamic:
	default:
		jsonErrorCode(w, 400, "bad_request", "type must be local, remote or dynamic", nil)
		return
	}

	info, err := s.sshForwards.Start(pty.ForwardSpec{
		Type:       req.Type,
		BindAddr:   req.BindAddr,
		BindPort:   req.BindPort,
		TargetHost: req.TargetHost,
		TargetPort: req.TargetPort,
		SSH: pty.SSHConfig{
			Host:          req.Host,
			Port:          req.Port,
			User:          req.User,
			Password:      req.Password,
			TrustHost:     req.TrustHost,
			ProxyJump:     req.ProxyJump,
			ProxyPassword: req.ProxyPassword,
			IdentityFile:  req.IdentityFile,
			KeyPassphrase: req.KeyPassphrase,
			PrivateKeyPEM: keyPEM,
		},
	})
	if err != nil {
		writeSSHError(w, err)
		return
	}
	var specID string
	if s.sshForwardSpecs != nil {
		spec, saveErr := s.sshForwardSpecs.Upsert(pty.SavedForwardSpec{
			HostID: req.HostID, Host: req.Host, Port: req.Port, User: req.User,
			IdentityFile: req.IdentityFile, ProxyJump: req.ProxyJump,
			Type: req.Type, BindAddr: req.BindAddr, BindPort: req.BindPort,
			TargetHost: req.TargetHost, TargetPort: req.TargetPort, AllowLAN: req.AllowLAN,
		})
		if saveErr == nil {
			specID = spec.ID
		}
	}
	go s.watchServerForward(info.ID)
	jsonResp(w, map[string]string{"id": info.ID, "spec_id": specID})
}

func (s *Server) apiSSHForwardSpecDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	if s.sshForwardSpecs == nil || s.sshForwardSpecs.Delete(r.PathValue("id")) != nil {
		jsonErrorCode(w, 404, "not_found", "forward specification not found", nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true})
}

func isLoopbackBind(addr string) bool {
	switch strings.ToLower(strings.TrimSpace(addr)) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// DELETE /api/ssh/forwards/{id} — погасить форвард.
func (s *Server) apiSSHForwardDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	if err := s.sshForwards.Stop(r.PathValue("id")); err != nil {
		jsonErrorCode(w, 404, "not_found", "forward not found", nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true})
}
