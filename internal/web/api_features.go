package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/sessions"
)

// ── Pinned Prompts ───────────────────────────────────────────────────

type pinnedPromptsData struct {
	Prompts map[string][]string `json:"prompts"` // session_name -> list
}

var (
	_promptsMu   sync.RWMutex
	_promptsData pinnedPromptsData
	_promptsPath string
)

func promptsInit() {
	if _promptsPath != "" {
		return
	}
	_promptsPath = paths.StateFile("prompts.json")
	data, err := os.ReadFile(_promptsPath)
	if err != nil {
		_promptsData = pinnedPromptsData{Prompts: make(map[string][]string)}
		return
	}
	if err := json.Unmarshal(data, &_promptsData); err != nil {
		_promptsData = pinnedPromptsData{Prompts: make(map[string][]string)}
	}
	if _promptsData.Prompts == nil {
		_promptsData.Prompts = make(map[string][]string)
	}
}

func promptsSave() {
	data, _ := json.MarshalIndent(_promptsData, "", "  ")
	os.WriteFile(_promptsPath, data, 0644)
}

func (s *Server) apiPromptsGet(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	_promptsMu.RLock()
	promptsInit()
	prompts := _promptsData.Prompts[name]
	_promptsMu.RUnlock()
	if prompts == nil {
		prompts = []string{}
	}
	jsonResp(w, map[string]any{"prompts": prompts})
}

func (s *Server) apiPromptsAdd(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := readJSON(r, &body); err != nil || body.Prompt == "" {
		jsonError(w, "prompt required", 400)
		return
	}
	_promptsMu.Lock()
	promptsInit()
	// Avoid duplicates
	for _, p := range _promptsData.Prompts[name] {
		if p == body.Prompt {
			_promptsMu.Unlock()
			jsonResp(w, map[string]bool{"ok": true})
			return
		}
	}
	_promptsData.Prompts[name] = append(_promptsData.Prompts[name], body.Prompt)
	promptsSave()
	_promptsMu.Unlock()
	jsonResp(w, map[string]bool{"ok": true})
}

func (s *Server) apiPromptsRemove(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "prompt required", 400)
		return
	}
	_promptsMu.Lock()
	promptsInit()
	var filtered []string
	for _, p := range _promptsData.Prompts[name] {
		if p != body.Prompt {
			filtered = append(filtered, p)
		}
	}
	_promptsData.Prompts[name] = filtered
	promptsSave()
	_promptsMu.Unlock()
	jsonResp(w, map[string]bool{"ok": true})
}

// ── Session Templates ────────────────────────────────────────────────

type SessionTemplate struct {
	Name           string `json:"name"`
	AgentType      string `json:"agent_type"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode,omitempty"`
	Mode           string `json:"mode,omitempty"`
}

type templatesData struct {
	Templates []SessionTemplate `json:"templates"`
}

var (
	_templatesMu   sync.RWMutex
	_templatesData templatesData
	_templatesPath string
)

func templatesInit() {
	if _templatesPath != "" {
		return
	}
	_templatesPath = paths.StateFile("templates.json")
	data, err := os.ReadFile(_templatesPath)
	if err != nil {
		_templatesData = templatesData{}
		return
	}
	json.Unmarshal(data, &_templatesData)
}

func templatesSave() {
	data, _ := json.MarshalIndent(_templatesData, "", "  ")
	os.WriteFile(_templatesPath, data, 0644)
}

func (s *Server) apiTemplatesList(w http.ResponseWriter, r *http.Request, uid int64) {
	_templatesMu.RLock()
	templatesInit()
	t := _templatesData.Templates
	_templatesMu.RUnlock()
	if t == nil {
		t = []SessionTemplate{}
	}
	jsonResp(w, map[string]any{"templates": t})
}

func (s *Server) apiTemplateCreate(w http.ResponseWriter, r *http.Request, uid int64) {
	var tmpl SessionTemplate
	if err := readJSON(r, &tmpl); err != nil || tmpl.Name == "" || tmpl.AgentType == "" {
		jsonError(w, "name and agent_type required", 400)
		return
	}
	_templatesMu.Lock()
	templatesInit()
	// Replace if same name
	var filtered []SessionTemplate
	for _, t := range _templatesData.Templates {
		if t.Name != tmpl.Name {
			filtered = append(filtered, t)
		}
	}
	_templatesData.Templates = append(filtered, tmpl)
	templatesSave()
	_templatesMu.Unlock()
	w.WriteHeader(201)
	jsonResp(w, map[string]bool{"ok": true})
}

func (s *Server) apiTemplateDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "name required", 400)
		return
	}
	_templatesMu.Lock()
	templatesInit()
	var filtered []SessionTemplate
	for _, t := range _templatesData.Templates {
		if t.Name != body.Name {
			filtered = append(filtered, t)
		}
	}
	_templatesData.Templates = filtered
	templatesSave()
	_templatesMu.Unlock()
	jsonResp(w, map[string]bool{"ok": true})
}

// Apply template: create session from template
func (s *Server) apiTemplateApply(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		TemplateName string `json:"template_name"`
		SessionName  string `json:"session_name"`
	}
	if err := readJSON(r, &body); err != nil || body.TemplateName == "" {
		jsonError(w, "template_name required", 400)
		return
	}

	_templatesMu.RLock()
	templatesInit()
	var tmpl *SessionTemplate
	for _, t := range _templatesData.Templates {
		if t.Name == body.TemplateName {
			tmpl = &t
			break
		}
	}
	_templatesMu.RUnlock()

	if tmpl == nil {
		jsonError(w, "template not found", 404)
		return
	}
	if err := s.ensureAgentAllowed(tmpl.AgentType); err != nil {
		jsonErrorCode(w, http.StatusForbidden, "agent_not_allowed", err.Error(), nil)
		return
	}

	sessName := body.SessionName
	if sessName == "" {
		sessName = tmpl.Name
	}
	// Ensure unique
	base := sessName
	for i := 2; s.store.Get(int(uid), sessName) != nil; i++ {
		sessName = fmt.Sprintf("%s-%d", base, i)
	}

	cwd := tmpl.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	_, err := s.store.Create(int(uid), sessName, tmpl.AgentType, cwd)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if tmpl.PermissionMode != "" {
		s.store.SetPermissionMode(int(uid), sessName, tmpl.PermissionMode)
	}
	if tmpl.Mode != "" {
		s.store.SetMode(int(uid), sessName, tmpl.Mode)
	}
	s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
	w.WriteHeader(201)
	jsonResp(w, map[string]any{"name": sessName})
}

// ── Multi-Send ───────────────────────────────────────────────────────

func (s *Server) apiMultiSend(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Sessions []string `json:"sessions"`
		Prompt   string   `json:"prompt"`
	}
	if err := readJSON(r, &body); err != nil || body.Prompt == "" || len(body.Sessions) == 0 {
		jsonError(w, "sessions and prompt required", 400)
		return
	}

	sent := 0
	for _, name := range body.Sessions {
		sess := s.store.Get(int(uid), name)
		if sess == nil {
			continue
		}
		if err := s.ensureAgentAllowed(sess.AgentType); err != nil {
			continue
		}
		if _, err := s.startSessionRun(uid, name); err != nil {
			continue
		}
		// Save user message
		userMsg := sessions.Message{
			Role:      "user",
			Text:      body.Prompt,
			Timestamp: float64(time.Now().UnixMilli()) / 1000,
		}
		s.history.Add(int(uid), name, userMsg)
		s.Broadcast(uid, map[string]any{
			"type": "message", "session": name, "message": userMsg,
		})
		s.Broadcast(uid, map[string]any{
			"type": "status", "session": name, "is_busy": true,
		})
		go s.runAgent(uid, name, sess, body.Prompt)
		sent++
	}

	jsonResp(w, map[string]any{"sent": sent, "total": len(body.Sessions)})
}

// ── Claude History Sync ──────────────────────────────────────────────

type claudeHistoryEntry struct {
	Display   string `json:"display"`
	Timestamp int64  `json:"timestamp"`
	SessionID string `json:"sessionId"`
	Project   string `json:"project"`
}

func (s *Server) apiClaudeHistory(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	sess := s.store.Get(int(uid), name)
	if sess == nil {
		jsonError(w, "session not found", 404)
		return
	}
	if sess.AgentSessionID == "" {
		jsonResp(w, map[string]any{"messages": []any{}})
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		jsonResp(w, map[string]any{"messages": []any{}})
		return
	}

	histPath := filepath.Join(home, ".claude", "history.jsonl")
	file, err := os.Open(histPath)
	if err != nil {
		jsonResp(w, map[string]any{"messages": []any{}})
		return
	}
	defer file.Close()

	var messages []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var entry claudeHistoryEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.SessionID != sess.AgentSessionID {
			continue
		}
		if entry.Display == "" {
			continue
		}
		messages = append(messages, map[string]any{
			"role":      "user",
			"text":      entry.Display,
			"timestamp": float64(entry.Timestamp) / 1000,
			"source":    "claude-code",
		})
	}

	jsonResp(w, map[string]any{"messages": messages})
}

// ── Session Clone (web API) ──────────────────────────────────────────

func (s *Server) apiSessionClone(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	sess := s.store.Get(int(uid), name)
	if sess == nil {
		jsonError(w, "session not found", 404)
		return
	}

	// Auto-generate name
	newName := name + "-clone"
	base := newName
	for i := 2; s.store.Get(int(uid), newName) != nil; i++ {
		newName = fmt.Sprintf("%s-%d", base, i)
	}

	clone, err := s.store.Clone(int(uid), name, newName)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
	w.WriteHeader(201)
	jsonResp(w, map[string]any{"name": clone.Name, "agent_type": clone.AgentType, "cwd": clone.Cwd})
}

// ── Session Clear Context (web API) ──────────────────────────────────

func (s *Server) apiSessionClear(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	if s.store.Get(int(uid), name) == nil {
		jsonError(w, "session not found", 404)
		return
	}
	s.store.UpdateAgentSessionID(int(uid), name, "")
	s.history.Clear(int(uid), name)
	s.store.SetStatus(int(uid), name, sessions.StatusNotReady)
	s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
	jsonResp(w, map[string]bool{"ok": true})
}

// ── Session Change Agent (web API) ───────────────────────────────────

func (s *Server) apiSessionChangeAgent(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	var body struct {
		AgentType string `json:"agent_type"`
	}
	if err := readJSON(r, &body); err != nil || body.AgentType == "" {
		jsonError(w, "agent_type required", 400)
		return
	}
	if !agents.IsAvailable(body.AgentType) {
		jsonError(w, "agent not available", 400)
		return
	}
	if err := s.ensureAgentAllowed(body.AgentType); err != nil {
		jsonErrorCode(w, http.StatusForbidden, "agent_not_allowed", err.Error(), nil)
		return
	}
	if err := s.store.SetAgent(int(uid), name, body.AgentType); err != nil {
		jsonError(w, err.Error(), 404)
		return
	}
	s.Broadcast(uid, map[string]string{"type": "sessions_updated"})
	jsonResp(w, map[string]bool{"ok": true})
}
