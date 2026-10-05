package web

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/aiusage"
	"tgcontrol/internal/config"
	"tgcontrol/internal/orchestrator"
	"tgcontrol/internal/version"
)

func (s *Server) apiAgentsList(w http.ResponseWriter, r *http.Request, uid int64) {
	// Ненайденных перепроверяем сами: список агентов считался ОДИН раз при
	// старте, и ложное «не установлен» жило до перезапуска или ручного
	// «перепроверить» (agents.RefreshMissing — там весь разбор). Человек за это
	// время видит пустое место там, где стоит рабочий агент, и не догадывается
	// нажать кнопку, которой на этом экране не видно.
	agents.RefreshMissing()
	jsonResp(w, map[string]any{"agents": s.agentRegistryItemsForClient(r)})
}

func (s *Server) apiAgentsRescan(w http.ResponseWriter, r *http.Request, uid int64) {
	agents.DetectAgents()
	jsonResp(w, map[string]any{"agents": s.agentRegistryItemsForClient(r)})
}

// agentRegistryItemsForClient не даёт старому APK обойти proxy_blocked.
// 2.57.20 уже понимал список env, но не понимал новый блокирующий verdict:
// unsupported legacy proxy он бы просто отбросил и запустил CLI напрямую.
// Новый клиент объявляет contract=1 и сам показывает/блокирует конкретный
// аккаунт. Клиент без capability получает агента как недоступный, пока legacy
// настройку не очистят новым интерфейсом.
func (s *Server) agentRegistryItemsForClient(r *http.Request) []map[string]any {
	items := s.agentRegistryItems()
	if r.URL.Query().Get("account_proxy_contract") == "1" {
		return items
	}
	blocked := blockedLegacyProxyAgents()
	return applyLegacyProxyGuard(items, blocked)
}

func applyLegacyProxyGuard(items []map[string]any, blocked map[string]bool) []map[string]any {
	for _, item := range items {
		id, _ := item["id"].(string)
		if !blocked[id] {
			continue
		}
		item["detected"] = false
		item["cli"] = ""
		item["resume_cli"] = ""
		item["install"] = ""
		item["install_posix"] = ""
		item["proxy_upgrade_required"] = true
	}
	return items
}

func (s *Server) agentRegistryItems() []map[string]any {
	// Квота подписки («N% 5ч / N% 7д» на карточке агента) берётся только из
	// кэша: список агентов — частый и дешёвый эндпоинт, а живой опрос
	// вендоров уходит в фон (см. aiusage.RefreshInBackground). До первого
	// завершённого сбора фронт получает честный status=unknown, не нули.
	usageSnap, usageLoaded := aiusage.CachedSnapshot()
	aiusage.RefreshInBackground()

	items := make([]map[string]any, 0, len(agents.Registry))
	for _, d := range agents.Registry {
		if err := s.ensureAgentAllowed(d.ID); err != nil {
			continue
		}
		item := d.ToMap()
		// launch_args — хуки агента («закончил», «ждёт разрешения»): клиент
		// дописывает их к каждому локальному запуску. Здесь, а не в ToMap:
		// там их вызвали бы тесты реестра и записали бы путь тестового бинаря.
		item["launch_args"] = d.LaunchArgs()
		if quota := aiusage.QuotaForAgent(usageSnap, usageLoaded, d.ID); quota != nil {
			item["quota"] = quota
		}
		items = append(items, item)
	}
	return items
}

func (s *Server) apiConfigGet(w http.ResponseWriter, r *http.Request, uid int64) {
	cfg := config.Get()
	hostname, _ := os.Hostname()

	inboxDir := cfg.InboxDir
	if inboxDir == "" {
		inboxDir = config.DefaultInboxDir()
	}
	jsonResp(w, map[string]any{
		"claude_model":            cfg.ClaudeModel,
		"claude_permission_mode":  cfg.ClaudePermissionMode,
		"codex_model":             cfg.CodexModel,
		"codex_approval_mode":     cfg.CodexApprovalMode,
		"codex_reasoning":         cfg.CodexReasoning,
		"default_agent":           cfg.DefaultAgent,
		"default_cwd":             cfg.DefaultCwd,
		"notifications_enabled":   cfg.NotificationsEnabled,
		"max_concurrent_sessions": cfg.MaxConcurrentSessions,
		"allowed_agents":          cfg.AllowedAgents,
		"default_ttl_minutes":     cfg.DefaultTTLMinutes,
		"inbox_dir":               inboxDir,
		"inbox_dir_default":       config.DefaultInboxDir(),
		"version":                 version.Version,
		"hostname":                hostname,
		"platform":                runtime.GOOS,
		// Клиенты прячут «Отправить в Telegram», когда бот не настроен
		// (cloud-топология без Telegram-токена).
		"bot_available": s.botToken != "",
		// Распознавание вопросов агента по экрану. Выключено по умолчанию —
		// см. config.DetectAgentQuestions.
		"detect_agent_questions": cfg.DetectAgentQuestions,
		"screenshot_hotkey":      cfg.ScreenshotHotkey,
		// Тумблер и ФАКТ — разные вещи: PrtScr могла занять системная «Ножницы»
		// или сторонняя программа, и тогда «включено» без этого признака было бы
		// враньём («hotkey_taken»). На macOS и Linux — «hotkey_unsupported».
		"screenshot_hotkey_active": screenshotHotkeyActive(),
		"screenshot_hotkey_error":  screenshotHotkeyError(),
		"options": map[string]any{
			"claude_models":           []string{"sonnet", "opus", "haiku"},
			"claude_permission_modes": []string{"bypassPermissions", "default", "plan"},
			"codex_models":            []string{"gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.2-codex"},
			"codex_reasoning":         []string{"xhigh", "high", "medium", "low", "minimal"},
			"codex_approval_modes":    []string{"full-auto", "bypass", "suggest", "auto"},
			"orchestrator_models":     orchestratorModelOptions(),
		},
	})
}

func (s *Server) apiConfigUpdate(w http.ResponseWriter, r *http.Request, uid int64) {
	var body map[string]any
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "Invalid JSON", 400)
		return
	}

	cfg := config.Get()
	if v, ok := body["default_agent"].(string); ok {
		cfg.DefaultAgent = v
	}
	if v, ok := body["default_cwd"].(string); ok {
		cfg.DefaultCwd = v
	}
	if v, ok := body["notifications_enabled"].(string); ok {
		cfg.NotificationsEnabled = v
	}
	if v, ok := body["notifications_enabled"].(bool); ok {
		if v {
			cfg.NotificationsEnabled = "true"
		} else {
			cfg.NotificationsEnabled = "false"
		}
	}
	if v, ok := body["max_concurrent_sessions"].(float64); ok {
		cfg.MaxConcurrentSessions = int(v)
	}
	if v, ok := body["default_ttl_minutes"].(float64); ok {
		cfg.DefaultTTLMinutes = int(v)
	}
	if v, ok := body["allowed_agents"].([]any); ok {
		var aa []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				aa = append(aa, s)
			}
		}
		cfg.AllowedAgents = aa
	}
	if v, ok := body["inbox_dir"].(string); ok {
		v = strings.TrimSpace(v)
		// Empty string resets to default behaviour (use default location).
		cfg.InboxDir = v
	}
	if v, ok := body["detect_agent_questions"].(bool); ok {
		cfg.DetectAgentQuestions = v
		// Применяем сразу: перезапуск агента ради переключателя не нужен.
		if s.ptyManager != nil {
			s.ptyManager.SetQuestionDetection(v)
		}
	}
	if v, ok := body["screenshot_hotkey"].(bool); ok {
		cfg.ScreenshotHotkey = v
		// Тем же правилом: клавиша занимается и отпускается на ходу. Без этого
		// тумблер в приложении записал бы настройку, а PrtScr заработал бы лишь
		// после перезапуска агента — то есть «включил и не работает».
		setScreenshotHotkey(v)
	}
	cfg.Save()
	jsonResp(w, map[string]bool{"ok": true})
}

func orchestratorModelOptions() []map[string]any {
	var opts []map[string]any
	for _, m := range orchestrator.BuiltinModels {
		opts = append(opts, map[string]any{
			"id":       m.Short,
			"full_id":  m.ID,
			"name":     m.Name,
			"provider": m.Provider,
			"input_m":  m.InputM,
			"output_m": m.OutputM,
			"ctx":      m.Ctx,
		})
	}
	return opts
}

func (s *Server) apiOrchModels(w http.ResponseWriter, r *http.Request, uid int64) {
	jsonResp(w, map[string]any{"models": orchestratorModelOptions()})
}

// ── Discover API ─────────────────────────────────────────────────────

func (s *Server) apiDiscover(w http.ResponseWriter, r *http.Request, uid int64) {
	discovered := agents.DiscoverAll()

	// Check which are already imported
	existing := s.store.List(int(uid))
	existingIDs := make(map[string]bool)
	for _, sess := range existing {
		if sess.AgentSessionID != "" {
			existingIDs[sess.AgentSessionID] = true
		}
	}

	items := make([]map[string]any, 0, len(discovered))
	for _, ds := range discovered {
		items = append(items, map[string]any{
			"agent_type": ds.AgentType,
			"session_id": ds.SessionID,
			"cwd":        ds.Cwd,
			"pid":        ds.PID,
			"started_at": ds.StartedAt,
			"name":       ds.Name,
			"is_alive":   ds.IsAlive,
			"imported":   existingIDs[ds.SessionID],
		})
	}
	jsonResp(w, map[string]any{"sessions": items})
}

func (s *Server) apiDiscoverImport(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		SessionID string `json:"session_id"`
		AgentType string `json:"agent_type"`
		Cwd       string `json:"cwd"`
		Name      string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "Invalid JSON", 400)
		return
	}
	if body.SessionID == "" {
		jsonError(w, "session_id required", 400)
		return
	}

	// Check if already imported
	for _, sess := range s.store.List(int(uid)) {
		if sess.AgentSessionID == body.SessionID {
			jsonResp(w, map[string]any{"name": sess.Name, "already_imported": true})
			return
		}
	}

	sessName := body.Name
	if sessName == "" {
		sessName = body.AgentType + "-imported"
	}
	// Ensure unique name
	base := sessName
	for i := 2; s.store.Get(int(uid), sessName) != nil; i++ {
		sessName = fmt.Sprintf("%s-%d", base, i)
	}

	cwd := body.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	agentType := body.AgentType
	if agentType == "" {
		agentType = "claude"
	}
	if err := s.ensureAgentAllowed(agentType); err != nil {
		jsonErrorCode(w, http.StatusForbidden, "agent_not_allowed", err.Error(), nil)
		return
	}

	_, err := s.store.Create(int(uid), sessName, agentType, cwd)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	s.store.UpdateAgentSessionID(int(uid), sessName, body.SessionID)
	s.store.SetStatus(int(uid), sessName, "alive")
	s.Broadcast(uid, map[string]string{"type": "sessions_updated"})

	w.WriteHeader(201)
	jsonResp(w, map[string]any{"name": sessName, "imported": true})
}
