package web

// SSH-ключи как объект продукта: /api/ssh/keys.
//
// Наружу уходит только открытая часть — имя, тип, отпечаток, публичный ключ.
// Приватного материала нет ни в одном ответе: он читается лишь внутри агента,
// в момент подключения (см. ssh_keys.go).

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"tgcontrol/internal/pty"
)

// GET /api/ssh/keys — список ключей. foreign=true означает «ключи есть, но их
// приватные части зашифрованы другой учётной записью Windows»: интерфейс
// обязан сказать это словами, а не показывать список, который не работает.
func (s *Server) apiSSHKeysList(w http.ResponseWriter, r *http.Request, uid int64) {
	keys := s.sshKeys.List()
	jsonResp(w, map[string]any{
		"keys":    keys,
		"foreign": s.sshKeys.Foreign(),
	})
}

// POST /api/ssh/keys — принести готовый ключ: содержимым (private_key) или
// путём к файлу на этом ПК (path). Второе — для окна exe, где файл под рукой;
// первое — единственный способ добавить ключ с телефона.
func (s *Server) apiSSHKeyImport(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Name       string `json:"name"`
		PrivateKey string `json:"private_key"`
		Path       string `json:"path"`
		Passphrase string `json:"passphrase"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	data := []byte(body.PrivateKey)
	if len(strings.TrimSpace(body.PrivateKey)) == 0 {
		path := expandUserPath(strings.TrimSpace(body.Path))
		if path == "" {
			jsonErrorCode(w, 400, "bad_request", "private_key or path is required", nil)
			return
		}
		fileData, err := os.ReadFile(path)
		if err != nil {
			// Путь набирает человек, и промах по нему — самая частая причина
			// отказа. Общий bad_request заставил бы гадать, что не так.
			jsonErrorCode(w, 400, "key_file_missing", "cannot read key file", nil)
			return
		}
		data = fileData
	}

	key, err := s.sshKeys.Import(body.Name, data, body.Passphrase)
	if err != nil {
		writeSSHKeyImportError(w, err)
		return
	}
	jsonResp(w, map[string]any{"key": key})
}

// POST /api/ssh/keys/generate — создать пару прямо здесь. Приватная часть
// сразу ложится в зашифрованное хранилище и наружу не показывается: экспорт
// приватного ключа осознанно не сделан, чтобы он не всплыл в переписке.
func (s *Server) apiSSHKeyGenerate(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		Passphrase string `json:"passphrase"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	key, err := s.sshKeys.Generate(body.Name, body.Type, body.Passphrase)
	if err != nil {
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"key": key})
}

// PATCH /api/ssh/keys/{id} — переименовать.
func (s *Server) apiSSHKeyRename(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	key, err := s.sshKeys.Rename(r.PathValue("id"), body.Name)
	if err != nil {
		if errors.Is(err, pty.ErrSSHKeyNotFound) {
			jsonErrorCode(w, 404, "not_found", "key not found", nil)
			return
		}
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"key": key})
}

// DELETE /api/ssh/keys/{id} — удалить ключ и отвязать его от серверов.
//
// Оставить у сервера ссылку на удалённый ключ значило бы поменять способ входа
// молча: «Подключиться» падало бы с невнятным отказом, а в карточке сервера
// по-прежнему значился бы вход по ключу.
func (s *Server) apiSSHKeyDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	if err := s.sshKeys.Delete(id); err != nil {
		if errors.Is(err, pty.ErrSSHKeyNotFound) {
			jsonErrorCode(w, 404, "not_found", "key not found", nil)
			return
		}
		jsonError(w, "failed to delete key", 500)
		return
	}
	detached := 0
	if s.sshHosts != nil {
		empty := ""
		for _, h := range s.sshHosts.List() {
			if h.KeyID != id {
				continue
			}
			if _, err := s.sshHosts.Update(h.ID, pty.SSHHostPatch{KeyID: &empty}); err == nil {
				detached++
			}
		}
	}
	jsonResp(w, map[string]any{"ok": true, "detached_hosts": detached})
}

// POST /api/ssh/keys/{id}/install — установить публичный ключ на сервер
// (аналог ssh-copy-id). Пароль нужен ровно один раз: дальше вход идёт ключом.
//
// По умолчанию ключ тут же назначается серверу — иначе успешная установка
// ничего бы не изменила в поведении «Подключиться», и человек второй раз
// проходил бы тот же путь руками.
func (s *Server) apiSSHKeyInstall(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	key, err := s.sshKeys.Get(id)
	if err != nil {
		jsonErrorCode(w, 404, "not_found", "key not found", nil)
		return
	}
	var body struct {
		HostID        string `json:"host_id"`
		Host          string `json:"host"`
		Port          int    `json:"port"`
		User          string `json:"user"`
		Password      string `json:"password"`
		TrustHost     bool   `json:"trust_host"`
		ProxyJump     string `json:"proxy_jump"`
		ProxyPassword string `json:"proxy_password"`
		// Assign — назначить ключ серверу после установки (по умолчанию да).
		Assign *bool `json:"assign"`
		// Remember — заодно запомнить пароль сервера. По умолчанию НЕТ: смысл
		// установки ключа в том, чтобы пароль больше не понадобился.
		Remember bool `json:"remember"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	body.Host = strings.TrimSpace(body.Host)
	body.User = strings.TrimSpace(body.User)
	body.HostID = strings.TrimSpace(body.HostID)
	body.ProxyJump = strings.TrimSpace(body.ProxyJump)

	identityFile := ""
	if body.HostID != "" {
		h, ok := s.resolveSSHHost(body.HostID)
		if !ok {
			jsonErrorCode(w, 404, "not_found", "host not found", nil)
			return
		}
		if body.Host == "" {
			body.Host = h.Host
		}
		if body.Port == 0 {
			body.Port = h.Port
		}
		if body.User == "" {
			body.User = h.User
		}
		if body.ProxyJump == "" {
			body.ProxyJump = h.ProxyJump
		}
		identityFile = h.IdentityFile
		if secret, ok := s.sshSecretFor(body.HostID); ok {
			if body.Password == "" {
				body.Password = secret.password
			}
			if body.ProxyPassword == "" {
				body.ProxyPassword = secret.proxyPassword
			}
		}
	}
	if body.Host == "" || body.User == "" {
		jsonErrorCode(w, 400, "bad_request", "host and user are required", nil)
		return
	}
	if body.Port <= 0 {
		body.Port = 22
	}

	already, err := pty.InstallAuthorizedKey(pty.SSHConfig{
		Host:          body.Host,
		Port:          body.Port,
		User:          body.User,
		Password:      body.Password,
		TrustHost:     body.TrustHost,
		ProxyJump:     body.ProxyJump,
		ProxyPassword: body.ProxyPassword,
		IdentityFile:  identityFile,
	}, key.PublicKey)
	if err != nil {
		if errors.Is(err, pty.ErrAuthorizedKeyShell) {
			jsonErrorCode(w, 400, "key_install_failed", err.Error(), nil)
			return
		}
		writeSSHError(w, err)
		return
	}

	assigned := false
	if body.HostID != "" && !strings.HasPrefix(body.HostID, "cfg:") &&
		(body.Assign == nil || *body.Assign) {
		if _, err := s.sshHosts.Update(body.HostID, pty.SSHHostPatch{KeyID: &id}); err == nil {
			assigned = true
		}
	}
	if body.Remember && body.HostID != "" && body.Password != "" && s.sshSecrets != nil {
		current, _ := s.sshSecrets.get(body.HostID)
		current.password = body.Password
		current.persist = true
		s.sshSecrets.set(body.HostID, current)
	}
	jsonResp(w, map[string]any{"ok": true, "already": already, "assigned": assigned})
}

// writeSSHKeyImportError разводит три причины отказа, за которыми стоят три
// разных действия человека: ввести пароль ключа, вспомнить другой пароль,
// принести другой файл.
func writeSSHKeyImportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pty.ErrSSHKeyNeedsPassphrase):
		jsonErrorCode(w, 400, "key_encrypted", "private key requires a passphrase", nil)
	case errors.Is(err, pty.ErrSSHKeyBadPassphrase):
		jsonErrorCode(w, 400, "key_bad_passphrase", "passphrase does not match", nil)
	case errors.Is(err, pty.ErrSSHKeyInvalid):
		jsonErrorCode(w, 400, "key_invalid", "not a valid ssh private key", nil)
	default:
		jsonError(w, "failed to store key", 500)
	}
}

// expandUserPath раскрывает «~/» — путь к ключу человек пишет именно так.
func expandUserPath(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
