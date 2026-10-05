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
	s.mux.HandleFunc("GET /api/hermes/control/{action}", s.hermesAuthWrap(s.apiHermesControl))
	s.mux.HandleFunc("POST /api/hermes/control/{action}", s.hermesAuthWrap(s.apiHermesControl))
	s.mux.HandleFunc("GET /api/hermes/artifact", s.hermesAuthWrap(s.apiHermesArtifact))
	s.mux.HandleFunc("GET /api/hermes/status", s.hermesAuthWrap(s.apiHermesStatus))
	s.mux.HandleFunc("POST /api/hermes/install", s.hermesAuthWrap(s.rateLimitAuth(s.apiLimiter, s.apiHermesOperation("install"))))
	s.mux.HandleFunc("POST /api/hermes/start", s.hermesAuthWrap(s.rateLimitAuth(s.apiLimiter, s.apiHermesOperation("start"))))
	s.mux.HandleFunc("POST /api/hermes/stop", s.hermesAuthWrap(s.rateLimitAuth(s.apiLimiter, s.apiHermesOperation("stop"))))
	s.mux.HandleFunc("POST /api/hermes/update", s.hermesAuthWrap(s.rateLimitAuth(s.apiLimiter, s.apiHermesOperation("update"))))
	s.mux.HandleFunc("POST /api/hermes/check-update", s.hermesAuthWrap(s.rateLimitAuth(s.apiLimiter, s.apiHermesOperation("check-update"))))
	s.mux.HandleFunc("POST /api/hermes/settings", s.hermesAuthWrap(s.apiHermesSettings))
	s.mux.HandleFunc("POST /api/hermes/rpc", s.hermesAuthWrap(s.apiHermesRPC))
	s.mux.HandleFunc("GET /api/hermes/events", s.hermesAuthWrap(s.apiHermesEvents))
	s.mux.HandleFunc("GET /api/hermes/subagents/activity", s.hermesAuthWrap(s.apiHermesSubagentActivity))
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
	s.configureHermesDelivery(mgr, uid)
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
		AutoStart  *bool `json:"auto_start"`
		Delivery   *bool `json:"delivery_enabled"`
	}
	if err := readHermesControlJSON(w, r, &req); err != nil || (req.AutoUpdate == nil && req.AutoStart == nil && req.Delivery == nil) {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Укажите, нужно ли обновлять Hermes автоматически.", nil)
		return
	}
	mgr := s.hermesForRequest(w, uid)
	if mgr == nil {
		return
	}
	var err error
	if req.AutoStart != nil {
		setter, ok := mgr.(interface{ SetAutoStart(bool) error })
		if !ok {
			err = fmt.Errorf("Обновите Remotai для автоматического запуска")
		} else {
			err = setter.SetAutoStart(*req.AutoStart)
		}
	}
	if err == nil && req.Delivery != nil {
		setter, ok := mgr.(interface{ SetDelivery(bool) error })
		if !ok {
			err = fmt.Errorf("Обновите Remotai для доставки уведомлений")
		} else {
			err = setter.SetDelivery(*req.Delivery)
		}
	}
	if err == nil && req.AutoUpdate != nil {
		err = mgr.SetAutoUpdate(*req.AutoUpdate)
	}
	if err != nil {
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
	if req.Method == "file.attach" {
		var scope map[string]json.RawMessage
		_ = json.Unmarshal(req.Params, &scope)
		var path string
		_ = json.Unmarshal(scope["path"], &path)
		// Match apiPtyUpload's actual root; reject traversal before cleaning.
		traversal := false
		for _, part := range strings.Split(filepath.ToSlash(path), "/") {
			if part == ".." {
				traversal = true
			}
		}
		source, err := browserUploadSource(path)
		if !filepath.IsAbs(path) || traversal || err != nil {
			jsonErrorCode(w, http.StatusBadRequest, "hermes_attachment_unavailable", "Выберите файл, загруженный через интерфейс Remotai.", nil)
			return
		}
		scope["path"], _ = json.Marshal(source)
		req.Params, _ = json.Marshal(scope)
	}
	mgr := s.hermesForRequest(w, uid)
	if mgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	method, params := req.Method, req.Params
	if method == "command.dispatch" {
		var scope map[string]json.RawMessage
		_ = json.Unmarshal(params, &scope)
		command, _ := hermesReadCommand(method, scope)
		method = "slash.exec"
		delete(scope, "name")
		delete(scope, "arg")
		scope["command"], _ = json.Marshal("/" + command)
		params, _ = json.Marshal(scope)
	}
	result, err := mgr.RPC(ctx, method, params)
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
	if req.Method == "commands.catalog" {
		var catalog map[string]json.RawMessage
		if json.Unmarshal(result, &catalog) == nil && catalog != nil {
			// Server policy replaces any upstream-supplied capability marker.
			names := []string{}
			for _, name := range []string{"status", "usage", "history", "clear", "models", "rename", "effort", "model"} {
				command, _ := json.Marshal("/" + name)
				if _, ok := hermesReadCommand("slash.exec", map[string]json.RawMessage{"command": command}); ok {
					names = append(names, name)
				}
			}
			catalog["remotai_commands"], _ = json.Marshal(map[string]any{"version": 1, "without_arguments": names})
			result, _ = json.Marshal(catalog)
		}
	}
	jsonResp(w, result)
}

// The native surface uses the public gateway contract, not an unrestricted
// CLI proxy. In particular config.get/full expands stored credentials, and
// cli.exec returns unredacted stdout from credential-bearing subprocesses.
func hermesRPCAllowed(method string, params json.RawMessage) bool {
	// A native compatibility endpoint must not escape this owner's profile.
	// Reject routing overrides globally, not just for tools.show.
	var scope map[string]json.RawMessage
	if len(params) > 0 && json.Unmarshal(params, &scope) != nil {
		return false
	}
	for key := range scope {
		switch strings.ToLower(key) {
		case "profile":
			var profile string
			if json.Unmarshal(scope[key], &profile) != nil || profile != "default" {
				return false
			}
		case "owner", "uid", "surface", "hermes_home":
			return false
		}
	}
	switch method {
	case "client.capabilities", "model.options", "model.save_key", "setup.runtime_check",
		"session.create", "session.resume", "session.activate", "session.list", "session.interrupt",
		"session.cwd.set", "session.history", "commands.catalog":
		return true
	case "command.dispatch", "slash.exec":
		// Native command.dispatch prioritizes quick/plugin handlers before builtins.
		// Only fixed read-only commands are permitted, through slash.exec's live
		// handlers below; arbitrary skills/loops/review/prompt remain fail closed.
		_, ok := hermesReadCommand(method, scope)
		return ok
	case "file.attach":
		// Stage an already-uploaded host file through the native contract. No
		// inline payloads or routing overrides on this authenticated HTTP lane.
		var req struct {
			Profile   string `json:"profile"`
			SessionID string `json:"session_id"`
			Path      string `json:"path"`
			Name      string `json:"name"`
		}
		return hermes.DecodeControlRequest(params, &req) == nil &&
			(req.Profile == "" || req.Profile == "default") &&
			req.SessionID != "" && req.SessionID == strings.TrimSpace(req.SessionID) && len(req.SessionID) <= 256 &&
			req.Path != "" && len(req.Path) <= 32768 && !strings.ContainsAny(req.Path, "\x00\r\n") && len(req.Name) <= 1024
	case "subagent.list":
		// The runtime remains scoped to the authenticated backend UID; upstream
		// additionally checks this live session's transport/owner authority.
		// Admit only the read-only default-profile roster, not control siblings.
		var request map[string]json.RawMessage
		if json.Unmarshal(params, &request) != nil || request == nil {
			return false
		}
		for key := range request {
			if key != "session_id" && key != "profile" {
				return false
			}
		}
		var sessionID string
		if json.Unmarshal(request["session_id"], &sessionID) != nil || sessionID == "" ||
			sessionID != strings.TrimSpace(sessionID) || len(sessionID) > 256 {
			return false
		}
		if raw, exists := request["profile"]; exists {
			var profile string
			if json.Unmarshal(raw, &profile) != nil || profile != "default" {
				return false
			}
		}
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
	wait := time.Duration(0)
	if values, supplied := r.URL.Query()["wait_ms"]; supplied {
		if len(values) != 1 || values[0] == "" {
			jsonErrorCode(w, http.StatusBadRequest, "bad_wait", "Некорректное время ожидания событий Hermes.", nil)
			return
		}
		ms, err := strconv.ParseUint(values[0], 10, 64)
		if err != nil {
			jsonErrorCode(w, http.StatusBadRequest, "bad_wait", "Некорректное время ожидания событий Hermes.", nil)
			return
		}
		if ms > 20000 {
			ms = 20000
		}
		wait = time.Duration(ms) * time.Millisecond
	}
	if mgr := s.hermesForRequest(w, uid); mgr != nil {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Query().Get("cursor") == "1" {
			if cursor, ok := mgr.(interface{ EventCursor() hermes.EventBatch }); ok {
				jsonResp(w, cursor.EventCursor())
			} else {
				// Legacy runtimes have no cheap snapshot capability.
				batch := mgr.Events(after)
				jsonResp(w, hermes.EventBatch{Events: []hermes.Event{}, LatestSeq: batch.LatestSeq})
			}
			return
		}
		if waiter, ok := mgr.(interface {
			WaitEvents(context.Context, uint64, time.Duration) (hermes.EventBatch, error)
		}); ok && wait > 0 {
			batch, err := waiter.WaitEvents(r.Context(), after, wait)
			if err != nil {
				if r.Context().Err() != nil {
					return
				}
				jsonErrorCode(w, http.StatusServiceUnavailable, "hermes_events_unavailable", "Hermes завершает работу. Запустите приложение снова.", nil)
				return
			}
			jsonResp(w, batch)
			return
		}
		jsonResp(w, mgr.Events(after))
	}
}

func (s *Server) apiHermesReply(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := readHermesControlJSON(w, r, &req); err != nil || req.ID == "" || len(req.ID) > 256 || len(req.Result) == 0 {
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

type hermesBackendPolicy struct {
	methods   []string
	queryKeys []string
}

// Keep automation routes finite: a future upstream sibling must not become
// reachable merely because it shares a prefix with an accepted route.
func hermesWorkBackendPolicy(path string) (hermesBackendPolicy, bool) {
	read := []string{http.MethodGet}
	profile := []string{"profile"}
	switch path {
	case "cron/jobs":
		return hermesBackendPolicy{[]string{http.MethodGet, http.MethodPost}, profile}, true
	case "memory", "skills":
		return hermesBackendPolicy{read, profile}, true
	case "skills/content":
		return hermesBackendPolicy{read, []string{"profile", "name"}}, true
	case "skills/toggle":
		return hermesBackendPolicy{[]string{http.MethodPut}, profile}, true
	case "profiles/default/soul":
		return hermesBackendPolicy{[]string{http.MethodGet, http.MethodPut}, nil}, true
	}
	parts := strings.Split(path, "/")
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "cron" || parts[1] != "jobs" || !hermesBackendID(parts[2]) {
		return hermesBackendPolicy{}, false
	}
	if len(parts) == 3 {
		return hermesBackendPolicy{[]string{http.MethodGet, http.MethodPut, http.MethodDelete}, profile}, true
	}
	switch parts[3] {
	case "pause", "resume":
		return hermesBackendPolicy{[]string{http.MethodPost}, profile}, true
	case "runs":
		return hermesBackendPolicy{read, []string{"profile", "limit"}}, true
	}
	return hermesBackendPolicy{}, false
}

func hermesBackendID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func hermesSkillName(value string) bool {
	if value == "" || len(value) > 256 || strings.Contains(value, "..") || strings.ContainsAny(value, "\\%:\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." {
			return false
		}
	}
	return true
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
	policy, workRoute := hermesWorkBackendPolicy(path)
	if !workRoute && path != "providers/oauth" && !strings.HasPrefix(path, "providers/oauth/") &&
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
	if workRoute {
		for key, values := range query {
			allowed := false
			for _, candidate := range policy.queryKeys {
				allowed = allowed || key == candidate
			}
			if !allowed || len(values) != 1 {
				return "", fmt.Errorf("недоступные параметры Hermes")
			}
		}
		if path != "profiles/default/soul" {
			if values, supplied := query["profile"]; supplied && values[0] != "default" {
				return "", fmt.Errorf("этот профиль Hermes недоступен")
			}
			// Upstream's cron list otherwise defaults to every profile. The native
			// surface owns one explicit profile even when the caller omits the query.
			query.Set("profile", "default")
		}
		if path == "skills/content" && !hermesSkillName(query.Get("name")) {
			return "", fmt.Errorf("некорректное имя навыка Hermes")
		}
		if rawLimit, supplied := query["limit"]; supplied {
			limit, err := strconv.Atoi(rawLimit[0])
			if err != nil || limit < 1 || limit > 100 {
				return "", fmt.Errorf("некорректное число запусков Hermes")
			}
		}
	}
	result := "/api/" + path
	if encoded := query.Encode(); encoded != "" {
		result += "?" + encoded
	}
	return result, nil
}

func hermesBackendMethodAllowed(method, path string) bool {
	path, _, _ = strings.Cut(path, "?")
	if policy, ok := hermesWorkBackendPolicy(strings.TrimPrefix(path, "/api/")); ok {
		for _, allowed := range policy.methods {
			if method == allowed {
				return true
			}
		}
		return false
	}
	// Preserve the existing setup surface. Retirement fencing belongs only to
	// the supervisor, and full configuration is never admitted by the path gate.
	return !strings.HasPrefix(path, "/api/health") && !strings.HasPrefix(path, "/api/config/") || method == http.MethodGet
}

// Upstream treats an item's profile query as a hint and otherwise searches
// other profiles. Admit only an exact ID returned by this surface's default
// catalog, never a name or an arbitrary client-supplied ID. This is a preflight
// guard; upstream does not provide an atomic scoped lookup/mutation contract.
func hermesDefaultJobExists(ctx context.Context, mgr hermesRuntime, id string) (bool, error) {
	resp, err := mgr.Do(ctx, http.MethodGet, "/api/cron/jobs?profile=default", nil)
	if err != nil {
		return false, fmt.Errorf("не удалось проверить список задач Hermes")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("не удалось проверить список задач Hermes")
	}
	const maxCatalog = 8 << 20
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalog+1))
	if err != nil || len(content) > maxCatalog {
		return false, fmt.Errorf("не удалось прочитать список задач Hermes")
	}
	var jobs []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(content, &jobs) != nil || len(jobs) > 4096 {
		return false, fmt.Errorf("Hermes вернул неизвестный список задач")
	}
	for _, job := range jobs {
		if job.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// Transport metadata has already served local authentication/access checks.
// Do not send it to Hermes or expand the business-query allowlist. Keep the
// original request intact for Remotai transport policy; reject ambiguous keys.
// Shared post-auth metadata normalization; business-key validation remains in
// each endpoint. Never strip identity overrides or arbitrary keys here.
func hermesAuthenticatedQuery(r *http.Request) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("некорректные параметры Hermes")
	}
	for _, key := range []string{"initData", "transport"} {
		if values, supplied := query[key]; supplied && len(values) != 1 {
			return nil, fmt.Errorf("недоступные параметры Hermes")
		}
		query.Del(key)
	}
	return query, nil
}

func hermesAuthenticatedBackendPath(r *http.Request) (string, error) {
	query, err := hermesAuthenticatedQuery(r)
	if err != nil {
		return "", err
	}
	return hermesBackendPath(r.URL.EscapedPath(), query.Encode())
}

func (s *Server) apiHermesBackend(w http.ResponseWriter, r *http.Request, uid int64) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		jsonErrorCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "Недоступный способ запроса Hermes.", nil)
		return
	}
	path, err := hermesAuthenticatedBackendPath(r)
	if err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_backend_path", err.Error(), nil)
		return
	}
	if !hermesBackendMethodAllowed(r.Method, path) {
		jsonErrorCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "Этот способ запроса Hermes недоступен.", nil)
		return
	}
	body := io.Reader(http.MaxBytesReader(w, r.Body, 1<<20))
	if backendPath, _, _ := strings.Cut(path, "?"); backendPath == "/api/skills/toggle" {
		var toggle struct {
			Name    string  `json:"name"`
			Enabled *bool   `json:"enabled"`
			Profile *string `json:"profile,omitempty"`
		}
		decoder := json.NewDecoder(body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&toggle); err != nil || !hermesSkillName(toggle.Name) || toggle.Enabled == nil || (toggle.Profile != nil && *toggle.Profile != "default") {
			jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Некорректные параметры навыка Hermes.", nil)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Некорректные параметры навыка Hermes.", nil)
			return
		}
		profile := "default"
		toggle.Profile = &profile
		encoded, _ := json.Marshal(toggle)
		body = strings.NewReader(string(encoded))
	} else if backendPath == "/api/profiles/default/soul" && r.Method == http.MethodPut {
		var soul struct {
			Content *string `json:"content"`
		}
		decoder := json.NewDecoder(body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&soul); err != nil || soul.Content == nil {
			jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Некорректный текст инструкции Hermes.", nil)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Некорректный текст инструкции Hermes.", nil)
			return
		}
		encoded, _ := json.Marshal(soul)
		body = strings.NewReader(string(encoded))
	}
	mgr := s.hermesForRequest(w, uid)
	if mgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	backendPath, _, _ := strings.Cut(path, "?")
	if suffix, item := strings.CutPrefix(backendPath, "/api/cron/jobs/"); item {
		id, _, _ := strings.Cut(suffix, "/")
		exists, err := hermesDefaultJobExists(ctx, mgr, id)
		if err != nil {
			jsonErrorCode(w, http.StatusBadGateway, "hermes_backend_error", err.Error(), nil)
			return
		}
		if !exists {
			jsonErrorCode(w, http.StatusNotFound, "hermes_job_not_found", "Задача не найдена в этом профиле Hermes. Обновите список.", nil)
			return
		}
	}
	resp, err := mgr.Do(ctx, r.Method, path, body)
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
