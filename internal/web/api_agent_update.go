package web

import (
	"context"
	"net/http"
	"sync"
	"time"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/agentupdate"
	"tgcontrol/internal/paths"
)

// Обновление CLI-агентов тем же менеджером, которым он поставлен
// (internal/agentupdate — там весь разбор). Сервер только ЗНАЕТ: версию,
// владельца установки, последнюю версию и команду. Само обновление идёт
// командой в НОВЫЙ терминал — как установка: человек видит процесс и то, чем
// он кончился, а не крутилку поверх невидимого npm.

var (
	agentUpdateOnce    sync.Once
	agentUpdateChecker *agentupdate.Checker
)

func agentUpdates() *agentupdate.Checker {
	agentUpdateOnce.Do(func() {
		agentUpdateChecker = agentupdate.NewLive(paths.StateFile("agent-versions.json"))
	})
	return agentUpdateChecker
}

// updateCandidates — обнаруженные агенты с бинарём (без встроенных shell и
// оркестратора) и разрешённые этому компьютеру.
func (s *Server) updateCandidates() []agentupdate.Agent {
	var out []agentupdate.Agent
	for _, d := range agents.GetDetected() {
		p := d.Path()
		if len(d.CLINames) == 0 || p == "" || p == "built-in" {
			continue
		}
		if err := s.ensureAgentAllowed(d.ID); err != nil {
			continue
		}
		out = append(out, agentupdate.Agent{ID: d.ID, Name: d.Name, Path: p, Install: d.Install})
	}
	return out
}

// runningAgentSessions — сколько живых терминалов этого компьютера сейчас
// держат каждого агента. Обновление поверх работающего агента на Windows
// упирается в занятый файл, а на любой ОС оставляет открытые сессии на старой
// версии до перезапуска — интерфейс предупреждает об этом до нажатия.
func (s *Server) runningAgentSessions() map[string]int {
	counts := map[string]int{}
	if s.ptyManager == nil {
		return counts
	}
	for _, info := range s.ptyManager.List() {
		if !info.Alive || info.Shell == "ssh" || info.AgentKind == "" {
			continue
		}
		counts[info.AgentKind]++
	}
	return counts
}

// GET /api/agents/updates[?refresh=1] — версия, владелец, последняя версия и
// команда обновления для каждого обнаруженного агента.
func (s *Server) apiAgentUpdates(w http.ResponseWriter, r *http.Request, uid int64) {
	agents.RefreshMissing()
	refresh := r.URL.Query().Get("refresh") == "1"
	// Потолок на весь замер: у каждого запуска свой таймаут, но пусть и
	// медленный реестр не держит экран дольше этого.
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	items := agentUpdates().Check(ctx, s.updateCandidates(), refresh)
	running := s.runningAgentSessions()
	for i := range items {
		items[i].RunningSessions = running[items[i].ID]
	}
	jsonResp(w, map[string]any{"agents": items})
}

// GET /api/agents/{id}/versions — когда какая версия была замечена.
func (s *Server) apiAgentVersions(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	if agents.GetDescriptor(id) == nil {
		jsonErrorCode(w, http.StatusNotFound, "agent_not_found", "agent not found", nil)
		return
	}
	jsonResp(w, map[string]any{"id": id, "events": agentUpdates().History.Events(id)})
}
