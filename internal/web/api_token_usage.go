package web

import (
	"net/http"
	"strconv"
	"time"

	"tgcontrol/internal/tokenusage"
)

// GET /api/token-usage?days=1|7|30 — сколько токенов сожгли агенты этой машины:
// по дням, папкам, моделям, аккаунтам и самые дорогие сессии. Считается по
// файлам сессий на диске; наружу уходят только цифры и пути папок.
func (s *Server) apiTokenUsage(w http.ResponseWriter, r *http.Request, uid int64) {
	w.Header().Set("Cache-Control", "no-store")
	tr := tokenusage.Default()
	if tr == nil {
		http.Error(w, "token usage is not running", http.StatusServiceUnavailable)
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	jsonResp(w, tr.Report(days, time.Now()))
}

// tokenUsageSources — те же аккаунты, что опрашиваются на лимиты
// (usageAccounts), но только агенты, чьи файлы сессий мы умеем читать.
func tokenUsageSources() []tokenusage.Source {
	var out []tokenusage.Source
	for _, a := range usageAccounts() {
		if a.Provider != "claude" && a.Provider != "codex" {
			continue
		}
		out = append(out, tokenusage.Source{AccountID: a.ID, Provider: a.Provider, Label: a.Label, Dir: a.Dir})
	}
	return out
}
