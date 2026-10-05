package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/hermes"
)

// Each local backend identity gets its own Hermes home and backend. Shared
// device controllers inherit the existing transport's local owner identity.
// The browser only
// speaks to the authenticated Remotai transport; backend tokens never leave Go.
type hermesRuntime interface {
	Status() hermes.Status
	Install(context.Context) error
	Start(context.Context) error
	Stop(context.Context) error
	Close(context.Context) error
	Update(context.Context) error
	CheckUpdate(context.Context) error
	SetAutoUpdate(bool) error
	StartMaintenance(context.Context)
	Do(context.Context, string, string, io.Reader) (*http.Response, error)
	RPC(context.Context, string, any) (json.RawMessage, error)
	Reply(context.Context, string, json.RawMessage) error
	Events(uint64) hermes.EventBatch
}

func (s *Server) registerHermesRoutes() {
	s.mux.HandleFunc("GET /api/hermes/status", s.hermesAuthWrap(s.apiHermesStatus))
	s.mux.HandleFunc("POST /api/hermes/install", s.hermesAuthWrap(s.rateLimitAuth(s.apiLimiter, s.apiHermesOperation("install"))))
	s.mux.HandleFunc("POST /api/hermes/start", s.hermesAuthWrap(s.rateLimitAuth(s.apiLimiter, s.apiHermesOperation("start"))))
	s.mux.HandleFunc("POST /api/hermes/stop", s.hermesAuthWrap(s.rateLimitAuth(s.apiLimiter, s.apiHermesOperation("stop"))))
	s.mux.HandleFunc("POST /api/hermes/update", s.hermesAuthWrap(s.rateLimitAuth(s.apiLimiter, s.apiHermesOperation("update"))))
	s.mux.HandleFunc("POST /api/hermes/check-update", s.hermesAuthWrap(s.rateLimitAuth(s.apiLimiter, s.apiHermesOperation("check-update"))))
	s.mux.HandleFunc("POST /api/hermes/settings", s.hermesAuthWrap(s.apiHermesSettings))
	s.mux.HandleFunc("POST /api/hermes/rpc", s.hermesAuthWrap(s.apiHermesRPC))
	s.mux.HandleFunc("GET /api/hermes/events", s.hermesAuthWrap(s.apiHermesEvents))
	s.mux.HandleFunc("POST /api/hermes/reply", s.hermesAuthWrap(s.apiHermesReply))
	s.mux.HandleFunc("/api/hermes/backend/", s.hermesAuthWrap(s.apiHermesBackend))
}

func (s *Server) hermesAuthWrap(handler func(http.ResponseWriter, *http.Request, int64)) http.HandlerFunc {
	// Authentication and allowed-user checks use the original identity first.
	return s.authWrap(func(w http.ResponseWriter, r *http.Request, uid int64) {
		handler(w, r, hermesBackendUID(config.GetNoSetup(), uid))
	})
}

func hermesBackendUID(cfg *config.Config, uid int64) int64 {
	// A legacy local API token can predate Telegram pairing and retain UID 1.
	// Alias only the explicitly configured central-bot owner to that existing
	// profile. Other allowed users and conflicting explicit token owners stay
	// separate; shared-device relay already supplies its local token identity.
	if cfg.IsCentralBot() && (cfg.APITokenUID == 0 || cfg.APITokenUID == 1) {
		owner, err := strconv.ParseInt(cfg.TelegramUserID, 10, 64)
		if err == nil && owner > 0 && uid == owner {
			return 1
		}
	}
	return uid
}

// Do not use paths.Base here: portable Remotai can live in a synchronized folder.
// Runtime data, credentials and backups belong to this OS user's local storage.
func hermesLocalRoot(uid int64) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("не удалось определить папку пользователя для Hermes")
	}
	var base string
	switch runtime.GOOS {
	case "windows":
		base = filepath.Join(home, "AppData", "Local", "Remotai", "hermes")
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "Remotai", "hermes")
	default:
		base = filepath.Join(home, ".local", "share", "remotai", "hermes")
	}
	return filepath.Join(base, "users", strconv.FormatInt(uid, 10)), nil
}

func (s *Server) hermesForUser(uid int64) (hermesRuntime, error) {
	s.hermesMu.Lock()
	defer s.hermesMu.Unlock()
	if s.hermesClosing {
		return nil, hermes.ErrClosed
	}
	if !config.GetNoSetup().IsAgentAllowed("hermes") {
		return nil, fmt.Errorf("Hermes отключён в настройках этого компьютера")
	}
	if mgr := s.hermesManagers[uid]; mgr != nil {
		return mgr, nil
	}
	root, err := hermesLocalRoot(uid)
	if err != nil {
		return nil, err
	}
	mgr, err := hermes.New(hermes.Options{Root: root})
	if err != nil {
		return nil, err
	}
	if s.hermesManagers == nil {
		s.hermesManagers = make(map[int64]hermesRuntime)
	}
	if s.hermesCtx == nil {
		s.hermesCtx, s.hermesCancel = context.WithCancel(context.Background())
	}
	s.hermesManagers[uid] = mgr
	mgr.StartMaintenance(s.hermesCtx)
	return mgr, nil
}

func (s *Server) hermesForRequest(w http.ResponseWriter, uid int64) hermesRuntime {
	mgr, err := s.hermesForUser(uid)
	if err != nil {
		jsonErrorCode(w, http.StatusForbidden, "hermes_unavailable", err.Error(), nil)
		return nil
	}
	return mgr
}

func (s *Server) apiHermesStatus(w http.ResponseWriter, r *http.Request, uid int64) {
	if mgr := s.hermesForRequest(w, uid); mgr != nil {
		w.Header().Set("Cache-Control", "no-store")
		jsonResp(w, mgr.Status())
	}
}

func (s *Server) apiHermesOperation(operation string) func(http.ResponseWriter, *http.Request, int64) {
	return func(w http.ResponseWriter, r *http.Request, uid int64) {
		mgr := s.hermesForRequest(w, uid)
		if mgr == nil {
			return
		}
		s.enqueueHermesOperation(w, operation, uid, mgr)
	}
}

// A manager lookup can finish just before shutdown snapshots its runtimes.
// Admit the job under the same barrier so no background work starts afterwards.
func (s *Server) enqueueHermesOperation(w http.ResponseWriter, operation string, uid int64, mgr hermesRuntime) {
	s.hermesMu.Lock()
	if s.hermesClosing {
		s.hermesMu.Unlock()
		jsonErrorCode(w, http.StatusServiceUnavailable, "hermes_shutdown", "Приложение завершает работу. Запустите его снова для работы с Hermes.", nil)
		return
	}
	if s.hermesJobs == nil {
		s.hermesJobs = make(map[int64]bool)
	}
	if s.hermesJobs[uid] {
		s.hermesMu.Unlock()
		jsonErrorCode(w, http.StatusConflict, "hermes_operation_busy", "Подготовка Hermes уже выполняется. Дождитесь её окончания.", nil)
		return
	}
	s.hermesJobs[uid] = true
	if s.hermesCtx == nil {
		s.hermesCtx, s.hermesCancel = context.WithCancel(context.Background())
	}
	baseCtx := s.hermesCtx
	s.hermesMu.Unlock()
	// A relay HTTP request finishes before installation does. The device
	// owns this operation, so losing the phone connection must not cancel it.
	go func() {
		ctx, cancel := context.WithTimeout(baseCtx, 45*time.Minute)
		defer cancel()
		var err error
		switch operation {
		case "install":
			err = mgr.Install(ctx)
			if err == nil {
				err = mgr.Start(ctx)
			}
		case "start":
			err = mgr.Start(ctx)
		case "stop":
			err = mgr.Stop(ctx)
		case "update":
			err = mgr.Update(ctx)
		case "check-update":
			err = mgr.CheckUpdate(ctx)
		}
		s.hermesMu.Lock()
		delete(s.hermesJobs, uid)
		s.hermesMu.Unlock()
		// Status already contains a redacted error; never broadcast raw
		// installer output or a provider credential that an error might echo.
		s.Broadcast(uid, map[string]any{"type": "hermes.status", "status": mgr.Status(), "ok": err == nil})
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	jsonResp(w, map[string]any{"accepted": true, "operation": operation, "status": mgr.Status()})
}

func (s *Server) apiHermesSettings(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		AutoUpdate *bool `json:"auto_update"`
	}
	if err := readJSON(r, &req); err != nil || req.AutoUpdate == nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Укажите, нужно ли обновлять Hermes автоматически.", nil)
		return
	}
	mgr := s.hermesForRequest(w, uid)
	if mgr == nil {
		return
	}
	if err := mgr.SetAutoUpdate(*req.AutoUpdate); err != nil {
		jsonErrorCode(w, http.StatusConflict, "hermes_settings_failed", err.Error(), nil)
		return
	}
	jsonResp(w, mgr.Status())
}

func (s *Server) apiHermesRPC(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.Method) == "" || len(req.Method) > 128 {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Не удалось прочитать команду Hermes.", nil)
		return
	}
	if !hermesRPCAllowed(req.Method, req.Params) {
		jsonErrorCode(w, http.StatusBadRequest, "hermes_method_unavailable", "Эта команда недоступна из интерфейса Hermes.", nil)
		return
	}
	mgr := s.hermesForRequest(w, uid)
	if mgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	result, err := mgr.RPC(ctx, req.Method, req.Params)
	if err != nil {
		var rpcErr *hermes.RPCError
		if errors.As(err, &rpcErr) {
			jsonErrorCode(w, http.StatusBadGateway, "hermes_rpc_failed", rpcErr.Message, map[string]string{"rpc_code": strconv.Itoa(rpcErr.Code)})
		} else {
			jsonErrorCode(w, http.StatusBadGateway, "hermes_rpc_failed", err.Error(), nil)
		}
		return
	}
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	jsonResp(w, result)
}

// The native surface uses the public gateway contract, not an unrestricted
// CLI proxy. In particular config.get/full expands stored credentials, and
// cli.exec returns unredacted stdout from credential-bearing subprocesses.
func hermesRPCAllowed(method string, params json.RawMessage) bool {
	switch method {
	case "client.capabilities", "model.options", "model.save_key", "setup.runtime_check",
		"session.create", "session.resume", "session.activate", "session.list", "session.interrupt",
		"session.cwd.set", "session.history", "prompt.submit", "commands.catalog", "command.dispatch", "slash.exec":
		return true
	case "config.set":
		var request struct {
			Key string `json:"key"`
		}
		return json.Unmarshal(params, &request) == nil && request.Key == "model"
	default:
		return false
	}
}

func (s *Server) apiHermesEvents(w http.ResponseWriter, r *http.Request, uid int64) {
	after := uint64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		after, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			jsonErrorCode(w, http.StatusBadRequest, "bad_cursor", "Некорректная позиция событий Hermes.", nil)
			return
		}
	}
	if mgr := s.hermesForRequest(w, uid); mgr != nil {
		w.Header().Set("Cache-Control", "no-store")
		jsonResp(w, mgr.Events(after))
	}
}

func (s *Server) apiHermesReply(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := readJSON(r, &req); err != nil || req.ID == "" || len(req.ID) > 256 || len(req.Result) == 0 {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Не удалось прочитать ответ для Hermes.", nil)
		return
	}
	mgr := s.hermesForRequest(w, uid)
	if mgr == nil {
		return
	}
	if err := mgr.Reply(r.Context(), req.ID, req.Result); err != nil {
		jsonErrorCode(w, http.StatusConflict, "hermes_reply_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]bool{"ok": true})
}

func hermesBackendPath(rawPath, rawQuery string) (string, error) {
	const prefix = "/api/hermes/backend/"
	if !strings.HasPrefix(rawPath, prefix) {
		return "", fmt.Errorf("неизвестный адрес Hermes")
	}
	path := strings.TrimPrefix(rawPath, prefix)
	if strings.Contains(path, "\\") || strings.Contains(path, "..") || strings.Contains(path, "%") || strings.ContainsAny(path, "\r\n?#") {
		return "", fmt.Errorf("некорректный адрес Hermes")
	}
	// The session-token login/reveal APIs are intentionally absent. Provider
	// authentication lives in /providers/oauth and retains tokens on the host.
	if path != "providers/oauth" && !strings.HasPrefix(path, "providers/oauth/") &&
		path != "model/set" && path != "config/schema" && path != "health" && path != "health/idle" && path != "env" {
		return "", fmt.Errorf("этот адрес Hermes недоступен")
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", fmt.Errorf("некорректные параметры Hermes")
	}
	for key := range query {
		if strings.Contains(strings.ToLower(key), "token") || strings.Contains(strings.ToLower(key), "reveal") {
			return "", fmt.Errorf("секреты Hermes нельзя запрашивать из интерфейса")
		}
	}
	result := "/api/" + path
	if encoded := query.Encode(); encoded != "" {
		result += "?" + encoded
	}
	return result, nil
}

func (s *Server) apiHermesBackend(w http.ResponseWriter, r *http.Request, uid int64) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		jsonErrorCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "Недоступный способ запроса Hermes.", nil)
		return
	}
	path, err := hermesBackendPath(r.URL.Path, r.URL.RawQuery)
	if err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_backend_path", err.Error(), nil)
		return
	}
	// Retirement fencing belongs to the supervisor. The schema is public,
	// whereas config GET would expand credential references from the host.
	if (strings.HasPrefix(path, "/api/health") || strings.HasPrefix(path, "/api/config/")) && r.Method != http.MethodGet {
		jsonErrorCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "Этот адрес Hermes доступен только для чтения.", nil)
		return
	}
	mgr := s.hermesForRequest(w, uid)
	if mgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	resp, err := mgr.Do(ctx, r.Method, path, http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "hermes_backend_failed", err.Error(), nil)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// FastAPI returns {detail:...}, whereas the shared Remotai client reads
		// {error,code}. Preserve setup guidance such as device-code policy errors.
		var payload map[string]json.RawMessage
		message := "Hermes не смог выполнить действие. Проверьте его состояние и повторите."
		if json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(&payload) == nil {
			for _, key := range []string{"error", "detail", "message"} {
				var value string
				if json.Unmarshal(payload[key], &value) == nil && value != "" && len(value) <= 4000 {
					message = value
					break
				}
			}
		}
		status := resp.StatusCode
		if status == http.StatusUnauthorized {
			// An upstream token failure is not an expired Remotai login.
			status = http.StatusBadGateway
		}
		jsonErrorCode(w, status, "hermes_backend_error", message, nil)
		return
	}
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 16<<20))
}

func (s *Server) shutdownHermes(ctx context.Context) {
	s.hermesMu.Lock()
	s.hermesClosing = true
	if s.hermesCancel != nil {
		s.hermesCancel()
	}
	managers := make([]hermesRuntime, 0, len(s.hermesManagers))
	for _, mgr := range s.hermesManagers {
		managers = append(managers, mgr)
	}
	s.hermesMu.Unlock()
	for _, mgr := range managers {
		_ = mgr.Close(ctx)
	}
}
