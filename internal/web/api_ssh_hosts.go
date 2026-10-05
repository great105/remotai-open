package web

// Сохранённые SSH-хосты и история подключений.
//
// Источники хостов два, и они сливаются в один список:
//   - config: блоки Host из ~/.ssh/config пользователя (только чтение;
//     id вида "cfg:<name>") — редактировать/удалять их через API нельзя,
//     это чужой файл;
//   - saved:  хосты, добавленные из приложения (ssh_hosts.json, CRUD).
//
// В saved-записях есть всё, кроме секретов: пароли и приватные ключи лежат в
// зашифрованных хранилищах рядом (ssh_secrets.enc, ssh_keys.enc) и наружу не
// отдаются — клиент видит лишь флаги unlocked/auth_ready и имя ключа.

import (
	"errors"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tgcontrol/internal/pty"
)

// sshHostJSON — элемент ответа /api/ssh/hosts.
type sshHostJSON struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	User           string   `json:"user"`
	IdentityFile   string   `json:"identity_file,omitempty"`
	ProxyJump      string   `json:"proxy_jump,omitempty"`
	Tags           []string `json:"tags"`
	Source         string   `json:"source"` // config | saved
	LastAt         string   `json:"last_at,omitempty"`
	Count          int      `json:"count,omitempty"`
	UserAssumed    bool     `json:"user_assumed,omitempty"`
	Unlocked       bool     `json:"unlocked"`
	AuthReady      bool     `json:"auth_ready"`
	LinkedDeviceID string   `json:"linked_device_id,omitempty"`
	// KeyID/KeyName — ключ из хранилища приложения. Имя приходит вместе с
	// хостом, чтобы список серверов не делал второй запрос ради подписи
	// «вход по ключу „Прод“».
	KeyID   string `json:"key_id,omitempty"`
	KeyName string `json:"key_name,omitempty"`
	// SecretPersisted — пароль сохранён на диск и переживёт перезапуск ПК.
	// Отличается от Unlocked: тот значит «есть прямо сейчас в памяти».
	SecretPersisted bool `json:"secret_persisted,omitempty"`
}

// GET /api/ssh/hosts — объединённый список: config-хосты + saved-хосты,
// обогащённые историей (last_at/count по ключу user@host:port).
// Сортировка: недавние сверху (last_at desc), затем по имени.
func (s *Server) apiSSHHostsList(w http.ResponseWriter, r *http.Request, uid int64) {
	var hist map[string]pty.SSHHistoryEntry
	if s.sshHistory != nil {
		hist = s.sshHistory.Snapshot()
	} else {
		hist = map[string]pty.SSHHistoryEntry{}
	}

	hosts := make([]sshHostJSON, 0, 16)

	for _, h := range pty.LoadSSHConfigHosts() {
		userName := h.User
		assumed := false
		if userName == "" {
			userName = currentSSHUser()
			assumed = userName != ""
		}
		host := sshHostJSON{
			ID:           "cfg:" + h.Name,
			Name:         h.Name,
			Host:         h.Host,
			Port:         h.Port,
			User:         userName,
			IdentityFile: h.IdentityFile,
			ProxyJump:    h.ProxyJump,
			Tags:         nonNilTags(h.Tags),
			Source:       "config",
			UserAssumed:  assumed,
		}
		s.enrichSSHHost(&host)
		hosts = append(hosts, host)
	}
	if s.sshHosts != nil {
		for _, h := range s.sshHosts.List() {
			host := sshHostJSON{
				ID:             h.ID,
				Name:           h.Name,
				Host:           h.Host,
				Port:           h.Port,
				User:           h.User,
				IdentityFile:   h.IdentityFile,
				ProxyJump:      h.ProxyJump,
				Tags:           nonNilTags(h.Tags),
				Source:         "saved",
				LinkedDeviceID: h.LinkedDeviceID,
				KeyID:          h.KeyID,
			}
			s.enrichSSHHost(&host)
			hosts = append(hosts, host)
		}
	}

	// Мерж истории: ключ user@host:port, порт нормализуем (0 → 22).
	for i := range hosts {
		port := hosts[i].Port
		if port <= 0 {
			port = 22
		}
		if e, ok := hist[pty.SSHHistoryKey(hosts[i].User, hosts[i].Host, port)]; ok {
			hosts[i].LastAt = e.LastAt.UTC().Format(time.RFC3339)
			hosts[i].Count = e.Count
		}
	}

	sort.SliceStable(hosts, func(i, j int) bool {
		if hosts[i].LastAt != hosts[j].LastAt {
			// Пустой last_at (никогда не подключались) — ниже любой даты.
			if hosts[i].LastAt == "" {
				return false
			}
			if hosts[j].LastAt == "" {
				return true
			}
			return hosts[i].LastAt > hosts[j].LastAt
		}
		return hosts[i].Name < hosts[j].Name
	})

	jsonResp(w, map[string]any{"hosts": hosts})
}

// POST /api/ssh/hosts — сохранить новый хост (source=saved). Пароля в теле
// нет: он приходит отдельной ручкой /unlock, которая кладёт его в
// зашифрованное хранилище, а не в этот открытый JSON.
func (s *Server) apiSSHHostCreate(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Name           string   `json:"name"`
		Host           string   `json:"host"`
		Port           int      `json:"port"`
		User           string   `json:"user"`
		IdentityFile   string   `json:"identity_file"`
		ProxyJump      string   `json:"proxy_jump"`
		Tags           []string `json:"tags"`
		LinkedDeviceID string   `json:"linked_device_id"`
		KeyID          string   `json:"key_id"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Host = strings.TrimSpace(body.Host)
	body.User = strings.TrimSpace(body.User)
	if body.Name == "" || body.Host == "" || body.User == "" {
		jsonErrorCode(w, 400, "bad_request", "name, host and user are required", nil)
		return
	}
	if body.Port < 0 || body.Port > 65535 {
		jsonErrorCode(w, 400, "bad_request", "invalid port", nil)
		return
	}

	h, err := s.sshHosts.Add(pty.SavedSSHHost{
		Name:           body.Name,
		Host:           body.Host,
		Port:           body.Port,
		User:           body.User,
		IdentityFile:   body.IdentityFile,
		ProxyJump:      body.ProxyJump,
		Tags:           body.Tags,
		LinkedDeviceID: strings.TrimSpace(body.LinkedDeviceID),
		KeyID:          strings.TrimSpace(body.KeyID),
	})
	if err != nil {
		jsonError(w, "failed to save host", 500)
		return
	}
	host := savedHostJSON(h)
	s.enrichSSHHost(&host)
	jsonResp(w, map[string]any{"host": host})
}

// PATCH /api/ssh/hosts/{id} — точечное обновление saved-хоста.
// Config-хосты (id "cfg:…") не редактируются: это ~/.ssh/config пользователя.
func (s *Server) apiSSHHostUpdate(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	if strings.HasPrefix(id, "cfg:") {
		jsonErrorCode(w, 400, "config_host", "hosts from ~/.ssh/config are read-only", nil)
		return
	}
	var patch pty.SSHHostPatch
	if err := readJSON(r, &patch); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	h, err := s.sshHosts.Update(id, patch)
	if err != nil {
		if err == pty.ErrSSHHostNotFound {
			jsonErrorCode(w, 404, "not_found", "host not found", nil)
			return
		}
		jsonError(w, "failed to save host", 500)
		return
	}
	host := savedHostJSON(h)
	s.enrichSSHHost(&host)
	jsonResp(w, map[string]any{"host": host})
}

// DELETE /api/ssh/hosts/{id} — удалить saved-хост (config-хосты — 400).
func (s *Server) apiSSHHostDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	if strings.HasPrefix(id, "cfg:") {
		jsonErrorCode(w, 400, "config_host", "hosts from ~/.ssh/config are read-only", nil)
		return
	}
	if err := s.sshHosts.Delete(id); err != nil {
		if err == pty.ErrSSHHostNotFound {
			jsonErrorCode(w, 404, "not_found", "host not found", nil)
			return
		}
		jsonError(w, "failed to delete host", 500)
		return
	}
	if s.sshSecrets != nil {
		s.sshSecrets.forget(id)
	}
	jsonResp(w, map[string]any{"ok": true})
}

// POST /api/ssh/hosts/{id}/unlock запоминает пароли этого сервера. По
// умолчанию — на диск, в зашифрованный ssh_secrets.enc (persist:false оставляет
// их в памяти до перезапуска, как было до 2.41.0). В ssh_hosts.json они не
// попадают и клиенту не возвращаются ни в каком виде.
//
// Запрос без секретов — не кривой вызов, а вопрос «пароль этого сервера ещё у
// тебя в памяти?». Раньше на него отвечал общий bad_request, и человек читал
// «Не удалось выполнить операцию» вместо понятного «введите пароль заново»;
// теперь ответ зависит от состояния памяти: помним — unlocked:true, не
// помним — код secret_not_remembered, для которого в словаре клиента есть
// человеческий текст.
//
// Переданные поля ДОПОЛНЯЮТ запомненное, а не затирают его целиком: иначе
// «запомнить новый пароль сервера» стирало бы пароль ключа и бастиона,
// введённые раньше, и следующая операция падала бы без видимой причины
// (так же дополняет память листинг с remember=1 — rememberSFTPSecret).
func (s *Server) apiSSHHostUnlock(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	if _, ok := s.resolveSSHHost(id); !ok {
		jsonErrorCode(w, 404, "not_found", "host not found", nil)
		return
	}
	var body struct {
		Password      string `json:"password"`
		KeyPassphrase string `json:"key_passphrase"`
		ProxyPassword string `json:"proxy_password"`
		// Persist — сохранить пароль на диск (по умолчанию да). Ложь оставляет
		// его в памяти до перезапуска агента: способ подключиться, не оставляя
		// на компьютере ничего.
		Persist *bool `json:"persist"`
	}
	// Пустое тело (io.EOF) — тот же вопрос «помнишь?», что и `{}`.
	if err := readJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	if s.sshSecrets == nil {
		// Памяти секретов нет вовсе — запоминать некуда. Тот же честный код:
		// «пароль не сохранён», а не 500 «ошибка сервера».
		jsonErrorCode(w, 409, "secret_not_remembered", "credential memory is unavailable", nil)
		return
	}
	current, remembered := s.sshSecretFor(id)
	if body.Password == "" && body.KeyPassphrase == "" && body.ProxyPassword == "" {
		if remembered {
			jsonResp(w, map[string]any{"ok": true, "unlocked": true})
			return
		}
		jsonErrorCode(w, 409, "secret_not_remembered", "no credentials for this host in agent memory", nil)
		return
	}
	if body.Password != "" {
		current.password = body.Password
	}
	if body.KeyPassphrase != "" {
		current.keyPassphrase = body.KeyPassphrase
	}
	if body.ProxyPassword != "" {
		current.proxyPassword = body.ProxyPassword
	}
	// Умолчание — сохранить: человек, нажавший «Запомнить», ждёт, что завтра
	// после перезагрузки сервер откроется без вопросов.
	current.persist = body.Persist == nil || *body.Persist
	s.sshSecrets.set(id, current)
	jsonResp(w, map[string]any{"ok": true, "unlocked": true, "persist": current.persist})
}

// DELETE /api/ssh/hosts/{id}/unlock immediately forgets in-memory credentials.
func (s *Server) apiSSHHostForgetSecret(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	if _, ok := s.resolveSSHHost(id); !ok {
		jsonErrorCode(w, 404, "not_found", "host not found", nil)
		return
	}
	s.sshSecrets.forget(id)
	jsonResp(w, map[string]any{"ok": true, "unlocked": false})
}

// DELETE /api/ssh/known-hosts removes an entry only from Remotai's private
// TOFU file. The user's ~/.ssh/known_hosts remains untouched.
func (s *Server) apiSSHKnownHostDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	body.Host = strings.TrimSpace(body.Host)
	if body.Host == "" || body.Port < 0 || body.Port > 65535 {
		jsonErrorCode(w, 400, "bad_request", "host and valid port are required", nil)
		return
	}
	removed, err := pty.RemoveKnownHost(body.Host, body.Port)
	if err != nil {
		jsonError(w, "failed to update known hosts", 500)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "removed": removed})
}

// GET /api/ssh/history — top 20 успешных подключений, свежие сверху.
func (s *Server) apiSSHHistory(w http.ResponseWriter, r *http.Request, uid int64) {
	entries := s.sshHistory.List(20)
	type entryJSON struct {
		Host      string `json:"host"`
		Port      int    `json:"port"`
		User      string `json:"user"`
		ProxyJump string `json:"proxy_jump,omitempty"`
		LastAt    string `json:"last_at"`
		Count     int    `json:"count"`
	}
	out := make([]entryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryJSON{
			Host:      e.Host,
			Port:      e.Port,
			User:      e.User,
			ProxyJump: e.ProxyJump,
			LastAt:    e.LastAt.UTC().Format(time.RFC3339),
			Count:     e.Count,
		})
	}
	jsonResp(w, map[string]any{"entries": out})
}

func savedHostJSON(h pty.SavedSSHHost) sshHostJSON {
	return sshHostJSON{
		ID:             h.ID,
		Name:           h.Name,
		Host:           h.Host,
		Port:           h.Port,
		User:           h.User,
		IdentityFile:   h.IdentityFile,
		ProxyJump:      h.ProxyJump,
		Tags:           nonNilTags(h.Tags),
		Source:         "saved",
		LinkedDeviceID: h.LinkedDeviceID,
		KeyID:          h.KeyID,
	}
}

func (s *Server) enrichSSHHost(h *sshHostJSON) {
	if h.Port <= 0 {
		h.Port = 22
	}
	if s.sshSecrets != nil {
		secret, ok := s.sshSecrets.get(h.ID)
		h.Unlocked = ok
		h.SecretPersisted = ok && secret.persist
	}
	// Ключ хранилища называем по имени: «вход по ключу» без имени в списке из
	// трёх ключей не отвечает на вопрос «по какому».
	if h.KeyID != "" && s.sshKeys != nil {
		if key, err := s.sshKeys.Get(h.KeyID); err == nil {
			h.KeyName = key.Name
		}
	}
	// Explicit keys and an unlocked secret are deterministic authentication
	// sources. Default keys/ssh-agent may still work when auth_ready is false.
	h.AuthReady = h.Unlocked || h.KeyName != "" || sshAuthSourceAvailable(h.IdentityFile)
}

func sshAuthSourceAvailable(identityFile string) bool {
	if identityFile != "" {
		path := identityFile
		if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
			if home, err := os.UserHomeDir(); err == nil {
				path = filepath.Join(home, path[2:])
			}
		}
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		if _, err := os.Stat(filepath.Join(home, ".ssh", name)); err == nil {
			return true
		}
	}
	return false
}

func (s *Server) resolveSSHHost(id string) (sshHostJSON, bool) {
	if strings.HasPrefix(id, "cfg:") {
		name := strings.TrimPrefix(id, "cfg:")
		for _, h := range pty.LoadSSHConfigHosts() {
			if h.Name != name {
				continue
			}
			userName := h.User
			assumed := false
			if userName == "" {
				userName = currentSSHUser()
				assumed = userName != ""
			}
			out := sshHostJSON{
				ID: id, Name: h.Name, Host: h.Host, Port: h.Port, User: userName,
				IdentityFile: h.IdentityFile, ProxyJump: h.ProxyJump,
				Tags: nonNilTags(h.Tags), Source: "config", UserAssumed: assumed,
			}
			s.enrichSSHHost(&out)
			return out, true
		}
		return sshHostJSON{}, false
	}
	if s.sshHosts == nil {
		return sshHostJSON{}, false
	}
	h, err := s.sshHosts.Get(id)
	if err != nil {
		return sshHostJSON{}, false
	}
	out := savedHostJSON(h)
	s.enrichSSHHost(&out)
	return out, true
}

func currentSSHUser() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(u.Username)
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// nonNilTags — tags в ответе всегда массив (null ломает клиентские списки).
func nonNilTags(t []string) []string {
	if t == nil {
		return []string{}
	}
	return t
}
