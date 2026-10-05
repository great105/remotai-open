package web

import (
	"errors"
	"net/http"
	"strconv"

	"tgcontrol/internal/license"
	"tgcontrol/internal/orchestrator"
)

// Артефакты запусков оркестратора (бэклог «run = папка с файлами»).
// Эндпоинты — чистая проекция файлов runs/<runID>/{events.jsonl,summary.md,
// patch.diff}: память процесса не участвует, поэтому история запусков
// переживает перезапуск агента. Запись артефактов — internal/orchestrator/artifacts.go.

func (s *Server) registerOrchRuns() {
	s.mux.HandleFunc("GET /api/orch/runs", s.authWrap(s.requireTier(license.TierPro, s.apiOrchRunsList)))
	s.mux.HandleFunc("GET /api/orch/runs/{id}", s.authWrap(s.requireTier(license.TierPro, s.apiOrchRunDetail)))
}

// GET /api/orch/runs — список запусков (новые первыми), сканированием папки runs/.
func (s *Server) apiOrchRunsList(w http.ResponseWriter, r *http.Request, uid int64) {
	jsonResp(w, map[string]any{"runs": orchestrator.ListRuns(orchestrator.RunsRoot())})
}

// GET /api/orch/runs/{id}?offset=0&limit=500 — детали запуска:
// события с пагинацией + summary.md и patch.diff текстом.
func (s *Server) apiOrchRunDetail(w http.ResponseWriter, r *http.Request, uid int64) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	detail, err := orchestrator.ReadRun(orchestrator.RunsRoot(), r.PathValue("id"), offset, limit)
	if err != nil {
		switch {
		case errors.Is(err, orchestrator.ErrInvalidRunID):
			jsonError(w, "Invalid run id", 400)
		case errors.Is(err, orchestrator.ErrRunNotFound):
			jsonError(w, "Run not found", 404)
		default:
			jsonError(w, "Cannot read run: "+err.Error(), 500)
		}
		return
	}
	jsonResp(w, detail)
}
