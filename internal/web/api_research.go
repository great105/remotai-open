package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/config"
	"tgcontrol/internal/orchestrator"
	"tgcontrol/internal/sessions"
)

// researchState tracks running research for a session.
type researchState struct {
	Running     bool                        `json:"running"`
	Config      orchestrator.ResearchConfig `json:"config"`
	Experiments []orchestrator.Experiment   `json:"experiments"`
	Baseline    float64                     `json:"baseline"`
	Final       float64                     `json:"final"`
	Improvement string                      `json:"improvement"`
	Branch      string                      `json:"branch"`
	FinalDiff   string                      `json:"final_diff,omitempty"`
	StartedAt   float64                     `json:"started_at"`
	FinishedAt  float64                     `json:"finished_at,omitempty"`
	Summary     string                      `json:"summary,omitempty"`
	IsError     bool                        `json:"is_error,omitempty"`
	stopCh      chan struct{}
}

var (
	researchMu     sync.RWMutex
	researchStates = map[string]*researchState{} // "uid:session" -> state
)

func researchKey(uid int64, name string) string {
	return fmt.Sprintf("%d:%s", uid, name)
}

// GET /api/sessions/{name}/research
func (s *Server) apiResearchGet(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")

	researchMu.RLock()
	state := researchStates[researchKey(uid, name)]
	researchMu.RUnlock()

	if state == nil {
		sess := s.store.Get(int(uid), name)
		cfg := researchConfigFromSession(sess)
		jsonResp(w, map[string]any{
			"running":     false,
			"config":      cfg,
			"experiments": []any{},
		})
		return
	}

	researchMu.RLock()
	defer researchMu.RUnlock()

	// Build response without stopCh
	jsonResp(w, map[string]any{
		"running":     state.Running,
		"config":      state.Config,
		"experiments": state.Experiments,
		"baseline":    state.Baseline,
		"final":       state.Final,
		"improvement": state.Improvement,
		"branch":      state.Branch,
		"started_at":  state.StartedAt,
		"finished_at": state.FinishedAt,
		"summary":     state.Summary,
		"is_error":    state.IsError,
	})
}

// POST /api/sessions/{name}/research/config — save structured research config
func (s *Server) apiResearchConfig(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	var body struct {
		EvalCommand         string   `json:"eval_command"`
		MetricPattern       string   `json:"metric_pattern"`
		MetricFile          string   `json:"metric_file"`
		MetricName          string   `json:"metric_name"`
		LowerIsBetter       bool     `json:"lower_is_better"`
		MaxExperiments      int      `json:"max_experiments"`
		MaxWallClockMinutes int      `json:"max_wall_clock_minutes"`
		MaxCostUSD          float64  `json:"max_cost_usd"`
		InvariantsCommand   string   `json:"invariants_command"`
		ProtectedPaths      []string `json:"protected_paths"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "Invalid JSON", 400)
		return
	}

	updates := map[string]string{
		"research_eval_command":       body.EvalCommand,
		"research_metric_pattern":     body.MetricPattern,
		"research_metric_file":        body.MetricFile,
		"research_metric_name":        body.MetricName,
		"research_max_experiments":    fmt.Sprintf("%d", body.MaxExperiments),
		"research_max_wall_clock":     fmt.Sprintf("%d", body.MaxWallClockMinutes),
		"research_max_cost_usd":       fmt.Sprintf("%.2f", body.MaxCostUSD),
		"research_invariants_command": body.InvariantsCommand,
		"research_protected_paths":    strings.Join(body.ProtectedPaths, ","),
	}
	if body.LowerIsBetter {
		updates["research_lower_is_better"] = "true"
	} else {
		updates["research_lower_is_better"] = "false"
	}

	s.store.UpdateConfig(int(uid), name, updates)
	jsonResp(w, map[string]any{"ok": true})
}

// POST /api/sessions/{name}/research — start research run
func (s *Server) apiResearchStart(w http.ResponseWriter, r *http.Request, uid int64) {
	// License gate: researcher requires Team plan
	if s.licenseManager != nil && !s.licenseManager.CanUseFeature("researcher") {
		jsonErrorCode(w, http.StatusForbidden, "team_required", "Research mode requires Team plan", nil)
		return
	}

	name := r.PathValue("name")
	sess := s.store.Get(int(uid), name)
	if sess == nil {
		jsonError(w, "Session not found", 404)
		return
	}

	key := researchKey(uid, name)
	researchMu.RLock()
	if state := researchStates[key]; state != nil && state.Running {
		researchMu.RUnlock()
		jsonError(w, "Research already running", 409)
		return
	}
	researchMu.RUnlock()

	var body struct {
		Task string `json:"task"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "Invalid JSON", 400)
		return
	}
	if body.Task == "" {
		body.Task = "Optimize the metric by experimenting with code changes"
	}

	rcfg := researchConfigFromSession(sess)
	if rcfg.EvalCommand == "" {
		jsonError(w, "eval_command not configured. Set research config first.", 400)
		return
	}

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		jsonError(w, "OPENROUTER_API_KEY not set", 400)
		return
	}

	cfg := config.Get()
	model := ""
	if v, ok := sess.AgentConfig["orchestrator_model"]; ok && v != "" {
		model = v
	}
	if model == "" {
		model = cfg.OrchestratorModel
	}

	stopCh := make(chan struct{})
	state := &researchState{
		Running:     true,
		Config:      rcfg,
		Experiments: []orchestrator.Experiment{},
		StartedAt:   float64(time.Now().UnixMilli()) / 1000,
		stopCh:      stopCh,
	}
	researchMu.Lock()
	researchStates[key] = state
	researchMu.Unlock()

	s.Broadcast(uid, map[string]any{
		"type": "research", "session": name, "action": "started",
	})

	go func() {
		runAgent := func(ctx context.Context, agentType, prompt, cwd string) orchestrator.AgentResult {
			if agentType == "orchestrator" || agentType == "researcher" {
				return orchestrator.AgentResult{Text: "cannot delegate to orchestrator/researcher", IsError: true}
			}
			agent, err := agents.GetAgent(agentType)
			if err != nil {
				return orchestrator.AgentResult{Text: err.Error(), IsError: true}
			}
			resp := agent.Run(ctx, agents.RunOptions{
				Prompt: prompt,
				Cwd:    cwd,
				SessionConfig: map[string]string{
					"claude_permission_mode": "bypassPermissions",
					"codex_approval_mode":    "full-auto",
				},
			})
			return orchestrator.AgentResult{
				Text: resp.Text, CostUSD: resp.CostUSD, IsError: resp.IsError,
			}
		}

		orch := orchestrator.New(apiKey, model, runAgent)

		onProgress := func(text string) {
			s.Broadcast(uid, map[string]any{
				"type": "research", "session": name, "action": "progress", "text": text,
			})
		}

		onExperiment := func(exp orchestrator.Experiment) {
			researchMu.Lock()
			state.Experiments = append(state.Experiments, exp)
			if exp.Kept {
				state.Final = exp.Metric
			}
			researchMu.Unlock()

			s.Broadcast(uid, map[string]any{
				"type": "research", "session": name, "action": "experiment", "experiment": exp,
			})
		}

		ctx := context.Background()
		result := orch.ExecuteResearch(ctx, body.Task, sess.Cwd, rcfg,
			orchestrator.ProgressFunc(onProgress), onExperiment, stopCh)

		researchMu.Lock()
		state.Running = false
		state.Summary = result.Summary
		state.IsError = result.IsError
		state.Baseline = result.BaselineMetric
		state.Final = result.FinalMetric
		state.Improvement = result.Improvement
		state.Branch = result.Branch
		state.FinalDiff = result.FinalDiff
		state.FinishedAt = float64(time.Now().UnixMilli()) / 1000
		researchMu.Unlock()

		msg := sessions.Message{
			Role:      "agent",
			Text:      fmt.Sprintf("🔬 Research complete\n\nBranch: %s\n%s", result.Branch, result.Summary),
			Timestamp: float64(time.Now().UnixMilli()) / 1000,
			CostUSD:   result.AgentCost,
		}
		s.history.Add(int(uid), name, msg)

		s.Broadcast(uid, map[string]any{
			"type": "research", "session": name, "action": "finished",
			"summary": result.Summary, "improvement": result.Improvement,
			"branch": result.Branch, "experiments": state.Experiments,
		})

		log.Printf("[Research] %s finished: %d experiments, improvement=%s, branch=%s",
			name, len(state.Experiments), result.Improvement, result.Branch)
	}()

	jsonResp(w, map[string]any{"ok": true, "status": "started"})
}

// POST /api/sessions/{name}/research/stop — clean stop
func (s *Server) apiResearchStop(w http.ResponseWriter, r *http.Request, uid int64) {
	name := r.PathValue("name")
	key := researchKey(uid, name)

	researchMu.RLock()
	state := researchStates[key]
	researchMu.RUnlock()

	if state == nil || !state.Running {
		jsonError(w, "Research not running", 404)
		return
	}

	// Signal stop — orchestrator will finish current iteration cleanly
	select {
	case <-state.stopCh:
		// already closed
	default:
		close(state.stopCh)
	}

	jsonResp(w, map[string]any{"ok": true, "status": "stopping"})
}

func researchConfigFromSession(sess *sessions.Session) orchestrator.ResearchConfig {
	if sess == nil {
		return orchestrator.ResearchConfig{}
	}
	maxExp := 10
	if v, ok := sess.AgentConfig["research_max_experiments"]; ok {
		fmt.Sscanf(v, "%d", &maxExp)
	}
	maxWall := 120
	if v, ok := sess.AgentConfig["research_max_wall_clock"]; ok {
		fmt.Sscanf(v, "%d", &maxWall)
	}
	maxCost := 5.0
	if v, ok := sess.AgentConfig["research_max_cost_usd"]; ok {
		fmt.Sscanf(v, "%f", &maxCost)
	}
	lowerIsBetter := true
	if v, ok := sess.AgentConfig["research_lower_is_better"]; ok && v == "false" {
		lowerIsBetter = false
	}
	pattern := sess.AgentConfig["research_metric_pattern"]
	if pattern == "" {
		pattern = `([\d.]+)`
	}
	metricName := sess.AgentConfig["research_metric_name"]
	if metricName == "" {
		metricName = "metric"
	}

	var protected []string
	if v, ok := sess.AgentConfig["research_protected_paths"]; ok && v != "" {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				protected = append(protected, p)
			}
		}
	}

	return orchestrator.ResearchConfig{
		EvalCommand:         sess.AgentConfig["research_eval_command"],
		MetricPattern:       pattern,
		MetricFile:          sess.AgentConfig["research_metric_file"],
		MetricName:          metricName,
		LowerIsBetter:       lowerIsBetter,
		MaxExperiments:      maxExp,
		MaxWallClockMinutes: maxWall,
		MaxCostUSD:          maxCost,
		InvariantsCommand:   sess.AgentConfig["research_invariants_command"],
		ProtectedPaths:      protected,
	}
}
