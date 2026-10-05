package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/config"
	"tgcontrol/internal/orchestrator"
	"tgcontrol/internal/procutil"
	"tgcontrol/internal/sessions"
)

func (s *Server) apiSessionsList(w http.ResponseWriter, r *http.Request, uid int64) {
	sessList := s.store.List(int(uid))
	active := s.store.GetActiveName(int(uid))

	items := make([]map[string]any, 0, len(sessList))
	for _, sess := range sessList {
		desc := agents.GetDescriptor(sess.AgentType)
		name := sess.AgentType
		icon := "\U0001F916" // robot
		if desc != nil {
			name = desc.Name
			icon = desc.Icon
		}
		mode := sess.Mode
		if mode == "" {
			mode = "persistent"
		}
		status := sess.Status
		if status == "" {
			status = "alive"
		}
		items = append(items, map[string]any{
			"name":            sess.Name,
			"agent_type":      sess.AgentType,
			"agent_name":      name,
			"agent_icon":      icon,
			"cwd":             sess.Cwd,
			"is_busy":         sess.IsBusy,
			"is_active":       sess.Name == active,
			"mode":            mode,
			"status":          status,
			"permission_mode": sess.PermissionMode,
			"created_at":      sess.CreatedAt,
			"last_active_at":  sess.LastActiveAt,
			"topic_id":        sess.TopicID,
			"ttl_minutes":     sess.TTLMinutes,
		})
	}
	jsonResp(w, map[string]any{"sessions": items, "active": active})
}

func (s *Server) apiSessionCreate(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Name      string `json:"name"`
		AgentType string `json:"agent_type"`
		Cwd       string `json:"cwd"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "Invalid JSON", 400)
		return
	}
	if body.Name == "" {
		jsonError(w, "name required", 400)
		return
	}
	if body.AgentType == "" {
		body.AgentType = "claude"
	}
	if !agents.IsAvailable(body.AgentType) {
		jsonError(w, "Agent not available: "+body.AgentType, 400)
		return
	}
	if err := s.ensureAgentAllowed(body.AgentType); err != nil {
		jsonErrorCode(w, http.StatusForbidden, "agent_not_allowed", err.Error(), nil)
		return
	}
	// License-based feature gates
	if s.licenseManager != nil {
		switch body.AgentType {
		case "orchestrator":
			if !s.licenseManager.CanUseFeature("orchestrator") {
				jsonErrorCode(w, http.StatusForbidden, "team_required", "Orchestrator requires Team plan", nil)
				return
			}
		case "researcher":
			if !s.licenseManager.CanUseFeature("researcher") {
				jsonErrorCode(w, http.StatusForbidden, "team_required", "Researcher requires Team plan", nil)
				return
			}
		default:
			// Non-basic agents (anything other than claude/shell) require all_agents feature
			if body.AgentType != "claude" && body.AgentType != "shell" {
				if !s.licenseManager.CanUseFeature("all_agents") {
					jsonErrorCode(w, http.StatusForbidden, "pro_required", "Additional agents require Pro plan or higher", nil)
					return
				}
			}
		}
	}
	if body.Cwd == "" {
		body.Cwd, _ = os.Getwd()
	}

	sess, err := s.store.Create(int(uid), body.Name, body.AgentType, body.Cwd)
	if err != nil {
		jsonError(w, err.Error(), 409)
		return
	}

	s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
	w.WriteHeader(201)
	jsonResp(w, map[string]any{
		"name": sess.Name, "agent_type": sess.AgentType, "cwd": sess.Cwd,
	})
}

func (s *Server) apiSessionDetail(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	sess := s.store.Get(int(uid), name)
	if sess == nil {
		jsonError(w, "Not found", 404)
		return
	}
	desc := agents.GetDescriptor(sess.AgentType)
	agentName := sess.AgentType
	agentIcon := "\U0001F916"
	if desc != nil {
		agentName = desc.Name
		agentIcon = desc.Icon
	}
	mode := sess.Mode
	if mode == "" {
		mode = "persistent"
	}
	status := sess.Status
	if status == "" {
		status = "alive"
	}
	jsonResp(w, map[string]any{
		"name":            name,
		"agent_type":      sess.AgentType,
		"agent_name":      agentName,
		"agent_icon":      agentIcon,
		"cwd":             sess.Cwd,
		"is_busy":         sess.IsBusy,
		"is_active":       name == s.store.GetActiveName(int(uid)),
		"messages":        s.history.Get(int(uid), name),
		"agent_config":    sess.AgentConfig,
		"mode":            mode,
		"status":          status,
		"permission_mode": sess.PermissionMode,
		"created_at":      sess.CreatedAt,
		"last_active_at":  sess.LastActiveAt,
		"topic_id":        sess.TopicID,
		"ttl_minutes":     sess.TTLMinutes,
	})
}

func (s *Server) apiSessionDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	if err := s.store.Close(int(uid), name); err != nil {
		jsonError(w, err.Error(), 404)
		return
	}
	s.history.Clear(int(uid), name)
	s.cleanupSessionLocks(uid, name)
	s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
	jsonResp(w, map[string]bool{"ok": true})
}

// cleanupSessionLocks drops the per-session agent lock when a session is removed,
// so agentLocks doesn't grow for the life of the long-running process. Skipped
// while a run is in progress (the run owns the lock and serializes that session).
func (s *Server) cleanupSessionLocks(uid int64, name string) {
	key := stopKey(uid, name)
	s.stopMu.Lock()
	_, running := s.stopChans[key]
	s.stopMu.Unlock()
	if running {
		return
	}
	s.agentMu.Lock()
	delete(s.agentLocks, key)
	s.agentMu.Unlock()
}

func (s *Server) apiSessionSwitch(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	if _, err := s.store.Switch(int(uid), name); err != nil {
		jsonError(w, err.Error(), 404)
		return
	}
	s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
	jsonResp(w, map[string]bool{"ok": true})
}

func (s *Server) apiSessionRename(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	var body struct {
		NewName string `json:"new_name"`
	}
	if err := readJSON(r, &body); err != nil || body.NewName == "" {
		jsonError(w, "new_name required", 400)
		return
	}
	if err := s.store.Rename(int(uid), name, body.NewName); err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	s.history.Rename(int(uid), name, body.NewName)
	s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
	jsonResp(w, map[string]any{"ok": true, "new_name": body.NewName})
}

func (s *Server) apiSessionConfig(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	var body map[string]string
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "Invalid JSON", 400)
		return
	}
	sess, err := s.store.UpdateConfig(int(uid), name, body)
	if err != nil {
		jsonError(w, err.Error(), 404)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "agent_config": sess.AgentConfig})
}

func (s *Server) apiSessionSend(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := readJSON(r, &body); err != nil || body.Prompt == "" {
		jsonError(w, "prompt required", 400)
		return
	}

	sess := s.store.Get(int(uid), name)
	if sess == nil {
		jsonError(w, "Not found", 404)
		return
	}

	// Check for pending selection (user typed a number after a list)
	prompt := body.Prompt
	trimmed := strings.TrimSpace(prompt)
	if n, err := strconv.Atoi(trimmed); err == nil {
		key := stopKey(uid, name)
		s.pendingMu.Lock()
		ps := s.pendingSelections[key]
		if ps != nil {
			delete(s.pendingSelections, key)
		}
		s.pendingMu.Unlock()
		if ps != nil && time.Since(ps.Time) < 5*time.Minute && n >= 1 && n <= len(ps.Options) {
			if ps.Cmd == "!answer" {
				// Answer to agent question — send the selected option as prompt
				prompt = ps.Options[n-1]
			} else {
				// Convert number to slash command with the selected option
				prompt = ps.Cmd + " " + ps.Options[n-1]
			}
		}
	}

	// Handle slash commands locally (Claude Code CLI ignores them in -p mode)
	if strings.HasPrefix(prompt, "/") {
		fields := strings.Fields(prompt)
		cmd := strings.ToLower(fields[0])
		args := ""
		if len(fields) > 1 {
			args = strings.TrimSpace(prompt[len(fields[0]):])
		}
		switch cmd {
		case "/continue":
			prompt = "continue"
		case "/retry":
			lastMsg := s.history.GetLastUserMessage(int(uid), name)
			if lastMsg == "" {
				jsonError(w, "No previous message to retry", 400)
				return
			}
			prompt = lastMsg
		default:
			if result := s.handleSlashCommand(uid, name, sess, cmd, args); result != nil {
				jsonResp(w, result)
				return
			}
		}
	}
	if err := s.ensureAgentAllowed(sess.AgentType); err != nil {
		jsonErrorCode(w, http.StatusForbidden, "agent_not_allowed", err.Error(), nil)
		return
	}

	userMsg := sessions.Message{
		Role:      "user",
		Text:      prompt,
		Timestamp: float64(time.Now().UnixMilli()) / 1000,
	}
	if _, err := s.startSessionRun(uid, name); err != nil {
		switch err {
		case sessions.ErrSessionBusy:
			jsonError(w, "Session is busy", 409)
		case sessions.ErrConcurrentLimit:
			jsonError(w, fmt.Sprintf("Reached max concurrent sessions limit (%d)", config.Get().MaxConcurrentSessions), 409)
		default:
			jsonError(w, err.Error(), 409)
		}
		return
	}
	s.history.Add(int(uid), name, userMsg)
	s.Broadcast(uid, map[string]any{"type": "message", "session": name, "message": userMsg})
	s.Broadcast(uid, map[string]any{"type": "status", "session": name, "is_busy": true})

	go s.runAgent(uid, name, sess, prompt)
	jsonResp(w, map[string]any{"ok": true, "message": userMsg})
}

func (s *Server) runAgent(uid int64, name string, sess *sessions.Session, prompt string) {
	key := stopKey(uid, name)

	// Get or create per-session lock
	s.agentMu.Lock()
	lock, ok := s.agentLocks[key]
	if !ok {
		lock = &sync.Mutex{}
		s.agentLocks[key] = lock
	}
	s.agentMu.Unlock()

	if !lock.TryLock() {
		s.store.SetBusy(int(uid), name, false)
		s.Broadcast(uid, map[string]any{"type": "status", "session": name, "is_busy": false})
		return
	}
	defer lock.Unlock()

	agent, err := agents.GetAgent(sess.AgentType)
	if err != nil {
		s.sendAgentError(uid, name, err.Error())
		return
	}

	stopCh := make(chan struct{})
	s.stopMu.Lock()
	s.stopChans[key] = stopCh
	s.stopMu.Unlock()

	var tools []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Monitor stop channel
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	resp := agent.Run(ctx, agents.RunOptions{
		Prompt:        prompt,
		Cwd:           sess.Cwd,
		SessionID:     sess.AgentSessionID,
		SessionConfig: agents.SessionConfigWithPermissionMode(sess.AgentType, sess.AgentConfig, sess.PermissionMode),
		OnProgress: func(text string) {
			tools = append(tools, text)
			s.Broadcast(uid, map[string]any{
				"type": "progress", "session": name, "text": text,
			})
		},
		OnStep: func(step orchestrator.StepEvent) {
			s.Broadcast(uid, map[string]any{
				"type":    "orchestrator_step",
				"session": name,
				"step":    step,
			})
		},
		OnQuestion: func(question string, options []string) {
			// Agent is asking the user a question — show it in chat
			text := "**Agent asks:**\n\n" + question
			if len(options) > 0 {
				text += "\n\n"
				qKeys := make([]string, len(options))
				for i, o := range options {
					text += fmt.Sprintf("%d. %s\n", i+1, o)
					qKeys[i] = o
				}
				text += "\nType a number to answer."
				// Save pending so user can reply with a number
				pKey := stopKey(uid, name)
				s.pendingMu.Lock()
				s.pendingSelections[pKey] = &pendingSelection{
					Cmd:     "!answer", // special: not a slash command, just stores options
					Options: qKeys,
					Time:    time.Now(),
				}
				s.pendingMu.Unlock()
			}
			qMsg := sessions.Message{
				Role:      "agent",
				Text:      text,
				Timestamp: float64(time.Now().UnixMilli()) / 1000,
			}
			s.history.Add(int(uid), name, qMsg)
			s.Broadcast(uid, map[string]any{"type": "message", "session": name, "message": qMsg})
		},
		StopCh: stopCh,
	})

	// Cleanup
	s.stopMu.Lock()
	delete(s.stopChans, key)
	s.stopMu.Unlock()
	s.store.SetBusy(int(uid), name, false)

	if resp.SessionID != "" {
		s.store.UpdateAgentSessionID(int(uid), name, resp.SessionID)
	}

	agentMsg := sessions.Message{
		Role:      "agent",
		Text:      resp.Text,
		Timestamp: float64(time.Now().UnixMilli()) / 1000,
		CostUSD:   resp.CostUSD,
		IsError:   resp.IsError,
		Tools:     tools,
	}
	s.history.Add(int(uid), name, agentMsg)
	s.Broadcast(uid, map[string]any{"type": "message", "session": name, "message": agentMsg})
	s.Broadcast(uid, map[string]any{"type": "status", "session": name, "is_busy": false})

	// Push-style notify event so APK / Mini App can surface a system notification.
	// Skip if user manually stopped the session (StopCh closed before completion).
	cancelled := false
	select {
	case <-stopCh:
		cancelled = true
	default:
	}
	if !cancelled {
		event := "session.completed"
		if resp.IsError {
			event = "session.failed"
		}
		s.Broadcast(uid, map[string]any{
			"type":    "notify",
			"event":   event,
			"session": name,
			"agent":   sess.AgentType,
			"summary": summarize(resp.Text, 200),
			"error":   resp.IsError,
		})
	}
}

// summarize trims and shortens text for notification body.
func summarize(text string, max int) string {
	t := strings.TrimSpace(text)
	if len(t) <= max {
		return t
	}
	// Cut at a rune boundary.
	cut := max
	for cut > 0 && t[cut]&0xC0 == 0x80 {
		cut--
	}
	return t[:cut] + "…"
}

func (s *Server) sendAgentError(uid int64, name, errText string) {
	s.store.SetBusy(int(uid), name, false)
	agentMsg := sessions.Message{
		Role:      "agent",
		Text:      "Error: " + errText,
		Timestamp: float64(time.Now().UnixMilli()) / 1000,
		IsError:   true,
	}
	s.history.Add(int(uid), name, agentMsg)
	s.Broadcast(uid, map[string]any{"type": "message", "session": name, "message": agentMsg})
	s.Broadcast(uid, map[string]any{"type": "status", "session": name, "is_busy": false})
	s.Broadcast(uid, map[string]any{
		"type":    "notify",
		"event":   "session.failed",
		"session": name,
		"summary": summarize(errText, 200),
		"error":   true,
	})
}

// handleSlashCommand handles slash commands from the web UI.
// Returns a JSON response map if handled, nil if the command should be forwarded.
func (s *Server) handleSlashCommand(uid int64, sessName string, sess *sessions.Session, cmd, args string) map[string]any {
	cfg := config.Get()
	now := float64(time.Now().UnixMilli()) / 1000
	isCodex := sess.AgentType == "codex"
	isOrch := sess.AgentType == "orchestrator"

	makeResponse := func(text string) map[string]any {
		msg := sessions.Message{Role: "agent", Text: text, Timestamp: now}
		s.history.Add(int(uid), sessName, msg)
		s.Broadcast(uid, map[string]any{"type": "message", "session": sessName, "message": msg})
		return map[string]any{"ok": true, "handled": true, "message": msg}
	}

	type option struct {
		key, label string
	}

	// Helper: get effective session config value with global fallback
	effective := func(key string) string {
		if v, ok := sess.AgentConfig[key]; ok && v != "" {
			return v
		}
		switch key {
		case "claude_model":
			return cfg.ClaudeModel
		case "claude_permission_mode":
			return cfg.ClaudePermissionMode
		case "codex_model":
			if cfg.CodexModel != "" {
				return cfg.CodexModel
			}
			return "gpt-5.3-codex"
		case "codex_reasoning":
			if cfg.CodexReasoning != "" {
				return cfg.CodexReasoning
			}
			return "high"
		case "codex_approval_mode":
			if cfg.CodexApprovalMode != "" {
				return cfg.CodexApprovalMode
			}
			return "full-auto"
		case "orchestrator_model":
			if cfg.OrchestratorModel != "" {
				return cfg.OrchestratorModel
			}
			return "sonnet"
		}
		return ""
	}

	// Helper: set per-session config value
	setConfig := func(key, value string) {
		s.store.UpdateConfig(int(uid), sessName, map[string]string{key: value})
	}

	// Helper: show list with pending selection
	showList := func(title, current, cmdName string, opts []option) map[string]any {
		text := "**" + title + ":** `" + current + "`\n\n"
		keys := make([]string, len(opts))
		for i, o := range opts {
			keys[i] = o.key
			mark := "  "
			if o.key == current {
				mark = "→ "
			}
			text += fmt.Sprintf("%s%d. `%s` — %s\n", mark, i+1, o.key, o.label)
		}
		text += "\nType number or `" + cmdName + " <name>` to switch."
		key := stopKey(uid, sessName)
		s.pendingMu.Lock()
		s.pendingSelections[key] = &pendingSelection{Cmd: cmdName, Options: keys, Time: time.Now()}
		s.pendingMu.Unlock()
		return makeResponse(text)
	}

	// Helper: validate and set from list
	setFromList := func(opts []option, configKey, arg string) map[string]any {
		for _, o := range opts {
			if o.key == arg {
				setConfig(configKey, arg)
				return makeResponse("Set to **" + arg + "** for this session.")
			}
		}
		keys := make([]string, len(opts))
		for i, o := range opts {
			keys[i] = o.key
		}
		return makeResponse("Unknown value: `" + arg + "`\nAvailable: " + strings.Join(keys, ", "))
	}

	switch cmd {

	// ── Model ────────────────────────────────────────────────────────

	case "/model":
		if isCodex {
			models := []option{
				{"gpt-5.3-codex", "GPT-5.3 Codex — most capable"},
				{"gpt-5.3-codex-spark", "GPT-5.3 Codex Spark — fast, 1000+ tok/s"},
				{"gpt-5.2-codex", "GPT-5.2 Codex — previous gen, cheaper"},
			}
			current := effective("codex_model")
			if args != "" {
				return setFromList(models, "codex_model", strings.TrimSpace(args))
			}
			return showList("model", current, "/model", models)
		}
		if isOrch {
			models := []option{
				{"sonnet", "Claude Sonnet 4.6 — fast planning"},
				{"opus", "Claude Opus 4.6 — deep reasoning"},
				{"haiku", "Claude Haiku 4.5 — cheapest"},
			}
			current := effective("orchestrator_model")
			if args != "" {
				return setFromList(models, "orchestrator_model", strings.ToLower(strings.TrimSpace(args)))
			}
			return showList("orchestrator model", current, "/model", models)
		}
		// Claude models
		models := []option{
			{"sonnet", "Claude Sonnet 4.6 — fast & smart"},
			{"opus", "Claude Opus 4.6 — most capable"},
			{"haiku", "Claude Haiku 4.5 — fastest & cheapest"},
		}
		current := effective("claude_model")
		if args != "" {
			return setFromList(models, "claude_model", strings.ToLower(strings.TrimSpace(args)))
		}
		return showList("model", current, "/model", models)

	// ── Permission mode (Claude) / Approval mode (Codex) ─────────

	case "/mode", "/permissions":
		if isCodex {
			modes := []option{
				{"full-auto", "Auto-approve + workspace sandbox"},
				{"bypass", "Full auto, no sandbox (dangerous)"},
				{"suggest", "Ask before untrusted actions"},
				{"auto", "Auto-approve, errors go to model"},
			}
			current := effective("codex_approval_mode")
			if args != "" {
				return setFromList(modes, "codex_approval_mode", strings.TrimSpace(args))
			}
			return showList("approval mode", current, "/mode", modes)
		}
		modes := []option{
			{"bypassPermissions", "No confirmations (full auto)"},
			{"default", "Asks for confirmation"},
			{"acceptEdits", "Auto-accept edits, confirm commands"},
			{"dontAsk", "Don't ask, deny risky actions"},
			{"plan", "Planning only, no execution"},
			{"auto", "Automatic based on context"},
		}
		current := effective("claude_permission_mode")
		if args != "" {
			return setFromList(modes, "claude_permission_mode", strings.TrimSpace(args))
		}
		return showList("permission mode", current, "/mode", modes)

	// ── Effort (Claude) / Reasoning (Codex) ──────────────────────

	case "/effort", "/reasoning":
		if isCodex {
			levels := []option{
				{"xhigh", "Maximum — slowest, smartest"},
				{"high", "High — complex tasks"},
				{"medium", "Balanced speed and depth"},
				{"low", "Faster, simpler"},
				{"minimal", "Fastest, least reasoning"},
			}
			current := effective("codex_reasoning")
			if args != "" {
				return setFromList(levels, "codex_reasoning", strings.ToLower(strings.TrimSpace(args)))
			}
			return showList("reasoning effort", current, "/effort", levels)
		}
		levels := []option{
			{"low", "Quick answers, minimal reasoning"},
			{"medium", "Balanced speed and depth"},
			{"high", "Thorough, detailed reasoning"},
			{"max", "Maximum reasoning depth"},
		}
		current := effective("claude_effort")
		if current == "" {
			current = "(default)"
		}
		if args != "" {
			return setFromList(levels, "claude_effort", strings.ToLower(strings.TrimSpace(args)))
		}
		return showList("effort", current, "/effort", levels)

	// ── Agent ────────────────────────────────────────────────────────

	case "/agent":
		detected := agents.GetDetected()
		if args != "" {
			arg := strings.TrimSpace(args)
			if !agents.IsAvailable(arg) {
				avail := make([]string, len(detected))
				for i, d := range detected {
					avail[i] = d.ID
				}
				return makeResponse("Agent not available: `" + arg + "`\nAvailable: " + strings.Join(avail, ", "))
			}
			if err := s.ensureAgentAllowed(arg); err != nil {
				return makeResponse("Agent is not allowed: `" + arg + "`")
			}
			if err := s.store.SetAgent(int(uid), sessName, arg); err != nil {
				return makeResponse("Error: " + err.Error())
			}
			desc := agents.GetDescriptor(arg)
			icon := ""
			if desc != nil {
				icon = desc.Icon + " "
			}
			s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
			return makeResponse(icon + "Agent switched to **" + arg + "**. Context reset.")
		}
		agentOpts := make([]option, len(detected))
		for i, d := range detected {
			agentOpts[i] = option{d.ID, d.Icon + " " + d.Description}
		}
		return showList("agent", sess.AgentType, "/agent", agentOpts)

	// ── Fast mode (Claude: toggle sonnet) ────────────────────────

	case "/fast":
		if isCodex {
			return makeResponse("Fast mode not available for Codex. Use `/model` to switch models.")
		}
		current := effective("claude_model")
		switch strings.ToLower(strings.TrimSpace(args)) {
		case "on", "":
			if current == "sonnet" {
				return makeResponse("Fast mode already **on** (sonnet).")
			}
			setConfig("claude_model", "sonnet")
			return makeResponse("Fast mode **on** — switched to sonnet.")
		case "off":
			if current != "sonnet" {
				return makeResponse("Fast mode already **off** (`" + current + "`).")
			}
			setConfig("claude_model", "opus")
			return makeResponse("Fast mode **off** — switched to opus.")
		}
		return makeResponse("Usage: `/fast` or `/fast on|off`")

	// ── Config (show all settings) ───────────────────────────────

	case "/config", "/settings":
		if isCodex {
			text := "**Session config (Codex):**\n"
			text += "- model: `" + effective("codex_model") + "`\n"
			text += "- reasoning: `" + effective("codex_reasoning") + "`\n"
			text += "- approval: `" + effective("codex_approval_mode") + "`\n"
			text += "- cwd: `" + sess.Cwd + "`\n"
			text += "\nUse `/model`, `/mode`, `/effort` to change."
			return makeResponse(text)
		}
		if isOrch {
			text := "**Session config (Orchestrator):**\n"
			text += "- planning model: `" + effective("orchestrator_model") + "`\n"
			text += "- cwd: `" + sess.Cwd + "`\n"
			text += "- max iterations: 25\n"
			text += "\nUse `/model` to change planning model."
			return makeResponse(text)
		}
		text := "**Session config (Claude):**\n"
		text += "- model: `" + effective("claude_model") + "`\n"
		text += "- permission mode: `" + effective("claude_permission_mode") + "`\n"
		eff := effective("claude_effort")
		if eff != "" {
			text += "- effort: `" + eff + "`\n"
		}
		text += "- agent: `" + sess.AgentType + "`\n"
		text += "- cwd: `" + sess.Cwd + "`\n"
		text += "\nUse `/model`, `/mode`, `/effort`, `/agent` to change."
		return makeResponse(text)

	// ── Status ───────────────────────────────────────────────────────

	case "/status":
		desc := agents.GetDescriptor(sess.AgentType)
		agentLabel := sess.AgentType
		if desc != nil {
			agentLabel = desc.Icon + " " + desc.Name
		}
		cost := s.history.GetSessionCost(int(uid), sessName)
		count := s.history.MessageCount(int(uid), sessName)
		text := "**Session:** `" + sessName + "`\n"
		text += "**Agent:** " + agentLabel + "\n"
		if isCodex {
			text += "**Model:** `" + effective("codex_model") + "`\n"
			text += "**Reasoning:** `" + effective("codex_reasoning") + "`\n"
			text += "**Approval:** `" + effective("codex_approval_mode") + "`\n"
		} else if isOrch {
			text += "**Planning model:** `" + effective("orchestrator_model") + "`\n"
		} else {
			text += "**Model:** `" + effective("claude_model") + "`\n"
			text += "**Mode:** `" + effective("claude_permission_mode") + "`\n"
		}
		text += "**CWD:** `" + sess.Cwd + "`\n"
		text += fmt.Sprintf("**Messages:** %d\n", count)
		text += fmt.Sprintf("**Cost:** $%.4f\n", cost)
		if sess.IsBusy {
			text += "**Status:** running..."
		} else {
			text += "**Status:** idle"
		}
		return makeResponse(text)

	// ── Diff (git diff in session cwd) ───────────────────────────

	case "/diff":
		gitArgs := []string{"diff"}
		if args != "" {
			gitArgs = append(gitArgs, strings.Fields(args)...)
		}
		// Без окна: diff читаем сами и отправляем текстом в чат. Окно git тут
		// было бы чистой вспышкой на ПК — вывод в него никто не смотрит.
		gitCmd := procutil.Hidden(exec.Command("git", gitArgs...))
		gitCmd.Dir = sess.Cwd
		out, err := gitCmd.CombinedOutput()
		text := strings.TrimSpace(string(out))
		if err != nil && text == "" {
			return makeResponse("Error: " + err.Error())
		}
		if text == "" {
			return makeResponse("No changes.")
		}
		if len(text) > 4000 {
			text = text[:4000] + "\n... (truncated)"
		}
		return makeResponse("```diff\n" + text + "\n```")

	// ── CWD (show/change working directory) ──────────────────────

	case "/cwd", "/cd":
		if args == "" {
			return makeResponse("**Working directory:** `" + sess.Cwd + "`")
		}
		newCwd := strings.TrimSpace(args)
		// Verify directory exists
		if info, err := os.Stat(newCwd); err != nil || !info.IsDir() {
			return makeResponse("Directory not found: `" + newCwd + "`")
		}
		if err := s.store.SetCwd(int(uid), sessName, newCwd); err != nil {
			return makeResponse("Error: " + err.Error())
		}
		s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
		return makeResponse("Working directory changed to `" + newCwd + "`")

	// ── Rename ───────────────────────────────────────────────────────

	case "/rename":
		if args == "" {
			return makeResponse("Usage: `/rename <new_name>`")
		}
		newName := strings.TrimSpace(args)
		if err := s.store.Rename(int(uid), sessName, newName); err != nil {
			return makeResponse("Error: " + err.Error())
		}
		s.history.Rename(int(uid), sessName, newName)
		s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
		return makeResponse("Session renamed to **" + newName + "**")

	// ── Export ────────────────────────────────────────────────────────

	case "/export":
		messages := s.history.Get(int(uid), sessName)
		if len(messages) == 0 {
			return makeResponse("No messages to export.")
		}
		var sb strings.Builder
		sb.WriteString("## Session: " + sessName + "\n\n")
		for _, m := range messages {
			t := time.Unix(int64(m.Timestamp), 0).Format("2006-01-02 15:04:05")
			role := "User"
			if m.Role == "agent" {
				role = "Agent"
			}
			sb.WriteString("### " + role + " [" + t + "]\n\n")
			sb.WriteString(m.Text + "\n\n---\n\n")
		}
		text := sb.String()
		if len(text) > 8000 {
			text = text[:8000] + "\n... (truncated)"
		}
		return makeResponse(text)

	// ── Plan mode shortcut ───────────────────────────────────────────

	case "/plan":
		if isCodex {
			return makeResponse("Plan mode is not available for Codex.")
		}
		setConfig("claude_permission_mode", "plan")
		return makeResponse("Switched to **plan** mode. Claude will analyze but not execute.\nUse `/mode bypassPermissions` to return to full auto.")

	// ── Session management ───────────────────────────────────────────

	case "/clear", "/reset", "/new":
		s.store.UpdateAgentSessionID(int(uid), sessName, "")
		s.history.Clear(int(uid), sessName)
		return makeResponse("Session cleared. Next message starts a new conversation.")

	case "/compact":
		s.store.UpdateAgentSessionID(int(uid), sessName, "")
		if args != "" {
			return makeResponse("Context reset with focus: _" + args + "_\nNext message starts a new conversation.")
		}
		return makeResponse("Context reset. Next message starts a new conversation.")

	case "/cost":
		cost := s.history.GetSessionCost(int(uid), sessName)
		count := s.history.MessageCount(int(uid), sessName)
		text := fmt.Sprintf("**Session cost:** $%.4f\n", cost)
		text += fmt.Sprintf("**Messages:** %d", count)
		return makeResponse(text)

	// ── Help ─────────────────────────────────────────────────────────

	case "/help":
		text := "**Available commands:**\n\n"
		text += "**Model & config:**\n"
		text += "- `/model` — show/switch models\n"
		text += "- `/mode` — permission mode (Claude) / approval mode (Codex)\n"
		text += "- `/effort` — reasoning effort\n"
		text += "- `/fast` — toggle fast mode (Claude)\n"
		text += "- `/agent` — switch agent\n"
		text += "- `/config` — show all settings\n"
		text += "- `/status` — session details\n\n"
		text += "**Session:**\n"
		text += "- `/clear` — clear session & history\n"
		text += "- `/compact` — reset context\n"
		text += "- `/cost` — session cost\n"
		text += "- `/continue` — continue response\n"
		text += "- `/retry` — retry last message\n"
		text += "- `/rename <name>` — rename session\n"
		text += "- `/export` — export conversation\n\n"
		text += "**Tools:**\n"
		text += "- `/diff` — git diff in session cwd\n"
		text += "- `/cwd [path]` — show/change working directory\n"
		text += "- `/plan` — switch to plan mode\n"
		return makeResponse(text)

	case "/continue":
		return nil // forward "continue" as prompt

	case "/retry":
		return nil // handled by caller
	}

	// Unknown slash command — forward to agent as regular prompt
	return nil
}

func (s *Server) apiSessionStop(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	key := stopKey(uid, name)

	s.stopMu.Lock()
	ch, ok := s.stopChans[key]
	if ok {
		delete(s.stopChans, key) // Remove before unlock to prevent double close
	}
	s.stopMu.Unlock()

	if !ok {
		jsonError(w, "Not running", 400)
		return
	}
	close(ch)
	jsonResp(w, map[string]bool{"ok": true})
}
