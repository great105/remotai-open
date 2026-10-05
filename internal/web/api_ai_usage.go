package web

import (
	"net/http"

	"tgcontrol/internal/aiusage"
)

// GET /api/ai-usage — sanitized subscription/rate-limit snapshot from AI
// clients installed on this device. Provider credentials remain local.
// One shared collection per computer; manual refresh respects provider backoff.
func (s *Server) apiAIUsage(w http.ResponseWriter, r *http.Request, uid int64) {
	w.Header().Set("Cache-Control", "no-store")
	jsonResp(w, aiusage.CollectCachedWithRefresh(r.Context(), r.URL.Query().Get("refresh") == "1"))
}
