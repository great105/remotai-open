package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/agentsessions"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/pty"
)

// GET /api/agent-sessions?q=&agent=&limit=&cursor= — все прошлые беседы
// Claude Code и Codex на этом компьютере по всем аккаунтам, свежие сверху
// (экран «Беседы»). Продолжение собирает клиент — как у усыпления: аккаунт и
// прокси живут в окружении процесса и в текст команды на сервере не попадают.
func (s *Server) apiAgentSessions(w http.ResponseWriter, r *http.Request, uid int64) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	agent := strings.TrimSpace(q.Get("agent"))
	if agent != "" && agent != "claude" && agent != "codex" {
		jsonError(w, "неизвестный агент", http.StatusBadRequest)
		return
	}
	query := agentsessions.Query{
		Q:      strings.TrimSpace(q.Get("q")),
		Agent:  agent,
		Limit:  limit,
		Cursor: q.Get("cursor"),
	}
	if len(query.Q) > 200 || len(query.Cursor) > 300 {
		jsonError(w, "слишком длинный запрос", http.StatusBadRequest)
		return
	}
	open := map[string]agentsessions.Open{}
	if s.ptyManager != nil {
		for _, c := range s.ptyManager.AgentConversations() {
			open[agentsessions.Key(c.Agent, c.SessionID)] = agentsessions.Open{PtyID: c.PtyID, Sleeping: c.Sleeping}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	page, err := agentSessionsIndex().List(ctx, agentSessionRoots(), open, query)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, page)
}

// agentSessionsIndex — один индекс на процесс; кэш разбора лежит рядом с
// настройками Remotai и переживает перезапуск.
var agentSessionsIndex = sync.OnceValue(func() *agentsessions.Index {
	ix := agentsessions.NewIndex(paths.StateFile("agent-sessions.json"))
	ix.PIDAlive = pty.PIDAlive
	return ix
})

// agentSessionRoots — каталоги аккаунтов Claude и Codex: те же, что
// опрашиваются на лимиты и расход токенов (usageAccounts). Пустой каталог —
// основной аккаунт в домашней папке.
func agentSessionRoots() []agentsessions.Root {
	home, _ := os.UserHomeDir()
	var out []agentsessions.Root
	for _, a := range usageAccounts() {
		if a.Provider != "claude" && a.Provider != "codex" {
			continue
		}
		dir := a.Dir
		if dir == "" {
			if home == "" {
				continue
			}
			dir = filepath.Join(home, "."+a.Provider)
		}
		out = append(out, agentsessions.Root{Agent: a.Provider, AccountID: a.ID, AccountLabel: a.Label, Dir: dir})
	}
	return out
}
