package web

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/mcpmgr"
	"tgcontrol/internal/paths"
)

// MCP-серверы агентов без правки JSON (раздел «Агенты» → «MCP-серверы»).
// Вся логика и все проверки — в internal/mcpmgr; здесь только аккаунты и HTTP.
//
// Значения env и заголовков наружу НЕ уходят никогда: в ответах только имена
// ключей, а секреты в аргументах и адресе замаскированы (mcpmgr.MaskArgs/URL).

var (
	mcpOnce sync.Once
	mcpMgr  *mcpmgr.Manager
)

func mcpManager() *mcpmgr.Manager {
	mcpOnce.Do(func() {
		mcpMgr = mcpmgr.New(&mcpmgr.Store{Path: paths.StateFile("mcp-disabled.json")})
	})
	return mcpMgr
}

// mcpAccount — выбранный аккаунт агента для операции.
//
// requested — id аккаунтов, которые прислал клиент (у каждого агента свои
// id, «default» — основной у любого). Не прислал ничего подходящего — берём
// тот, что выбран в «Аккаунтах»: именно им агент и запустится.
func mcpAccount(f accountsFile, agentID string, requested []string) (AgentAccount, bool) {
	list := accountsForAgent(f, agentID)
	for _, id := range requested {
		for _, a := range list {
			if a.ID == id {
				if a.Dir != "" {
					if st, err := os.Stat(a.Dir); err != nil || !st.IsDir() {
						return a, false
					}
				}
				return a, true
			}
		}
	}
	return activeAccount(f, agentID), true
}

func mcpTarget(agentID string, acc AgentAccount) mcpmgr.Target {
	exe := ""
	if d := agents.GetDescriptor(agentID); d != nil {
		exe = mcpmgr.NativeExe(agentID, d.Path())
	}
	return mcpmgr.Target{
		Agent:     agentID,
		AccountID: acc.ID,
		Exe:       exe,
		ConfigDir: acc.Dir,
		Home:      realUserHome(),
	}
}

func mcpErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, mcpmgr.ErrInvalid):
		return 400, "invalid"
	case errors.Is(err, mcpmgr.ErrExists):
		return 409, "exists"
	case errors.Is(err, mcpmgr.ErrNotFound):
		return 404, "not_found"
	case errors.Is(err, mcpmgr.ErrUnsupported):
		return 422, "unsupported"
	case errors.Is(err, mcpmgr.ErrReadOnly):
		return 409, "read_only"
	case errors.Is(err, mcpmgr.ErrStoreForeign):
		return 409, "store_foreign"
	}
	// НЕ 502: клиент читает 502 как «компьютер не в сети» (isPcOffline) и
	// показал бы баннер обрыва вместо ответа агента.
	return 424, "agent_error"
}

func mcpWriteError(w http.ResponseWriter, err error) {
	status, code := mcpErrorStatus(err)
	msg := err.Error()
	if errors.Is(err, mcpmgr.ErrStoreForeign) {
		msg = "Список выключенных серверов записан другой учётной записью Windows — прочитать его отсюда нельзя"
	}
	jsonErrorCode(w, status, code, msg, nil)
}

// GET /api/mcp[?account=<id>…] — серверы по агентам.
func (s *Server) apiMCPList(w http.ResponseWriter, r *http.Request, uid int64) {
	requested := r.URL.Query()["account"]
	accountsMu.Lock()
	f := loadAccountsFile()
	accountsMu.Unlock()

	out := make([]map[string]any, 0, len(mcpmgr.SupportedIDs))
	for _, id := range mcpmgr.SupportedIDs {
		d := agents.GetDescriptor(id)
		if d == nil {
			continue
		}
		acc, ok := mcpAccount(f, id, requested)
		accounts := []map[string]any{}
		for _, a := range accountsForAgent(f, id) {
			accounts = append(accounts, map[string]any{
				"id":         a.ID,
				"label":      a.Label,
				"is_default": a.ID == DefaultAccountID,
				"selected":   a.ID == acc.ID,
			})
		}
		item := map[string]any{
			"id":        id,
			"name":      d.Name,
			"installed": d.IsDetected(),
			"caps":      mcpmgr.Supported[id],
			"account":   acc.ID,
			"accounts":  accounts,
			"servers":   []mcpmgr.Server{},
		}
		switch {
		case !d.IsDetected():
		case !ok:
			item["error"] = "Каталог этого аккаунта не найден"
		default:
			tg := mcpTarget(id, acc)
			if tg.Exe == "" {
				item["error"] = d.Name + " найден, но запустить его напрямую не удалось — переустановите агента"
				break
			}
			list, err := mcpManager().List(r.Context(), tg)
			if err != nil {
				_, code := mcpErrorStatus(err)
				item["error"] = err.Error()
				item["error_code"] = code
			} else {
				item["servers"] = list
			}
		}
		out = append(out, item)
	}
	jsonResp(w, map[string]any{"agents": out})
}

type mcpTargetRef struct {
	Agent   string `json:"agent"`
	Account string `json:"account"`
}

func (s *Server) mcpResolve(ref mcpTargetRef) (mcpmgr.Target, error) {
	if _, ok := mcpmgr.Supported[ref.Agent]; !ok {
		return mcpmgr.Target{}, &mcpmgr.UserError{Kind: mcpmgr.ErrUnsupported, Msg: "Этот агент пока не умеет MCP отсюда"}
	}
	d := agents.GetDescriptor(ref.Agent)
	if d == nil || !d.IsDetected() {
		return mcpmgr.Target{}, &mcpmgr.UserError{Kind: mcpmgr.ErrUnsupported, Msg: "Агент не установлен на компьютере"}
	}
	accountsMu.Lock()
	f := loadAccountsFile()
	accountsMu.Unlock()
	var requested []string
	if ref.Account != "" {
		requested = []string{ref.Account}
	}
	acc, ok := mcpAccount(f, ref.Agent, requested)
	if !ok || (ref.Account != "" && acc.ID != ref.Account) {
		return mcpmgr.Target{}, &mcpmgr.UserError{Kind: mcpmgr.ErrNotFound, Msg: "Аккаунт не найден"}
	}
	tg := mcpTarget(ref.Agent, acc)
	if tg.Exe == "" {
		return mcpmgr.Target{}, &mcpmgr.UserError{Kind: mcpmgr.ErrUnsupported, Msg: d.Name + ": не удалось найти исполняемый файл агента"}
	}
	return tg, nil
}

// POST /api/mcp — добавить сервер в одного или нескольких агентов.
//
// {targets:[{agent, account}], server:{name, type, command, args, env, url, headers}}
// Ответ — по агенту: добавить в Claude могло получиться, а в Codex нет
// (например, SSE), и это человек должен увидеть по каждому отдельно.
func (s *Server) apiMCPAdd(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Targets []mcpTargetRef `json:"targets"`
		Server  mcpmgr.Spec    `json:"server"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "неверный JSON", nil)
		return
	}
	if len(body.Targets) == 0 || len(body.Targets) > len(mcpmgr.SupportedIDs) {
		jsonErrorCode(w, 400, "bad_request", "Выберите агента", nil)
		return
	}
	// Ввод проверяем ДО первого запуска CLI: иначе в одного агента запишется,
	// а во второго нет — из-за опечатки, которую видно сразу.
	if _, err := body.Server.Normalize(); err != nil {
		mcpWriteError(w, err)
		return
	}
	results := make([]map[string]any, 0, len(body.Targets))
	okCount := 0
	var lastErr error
	for _, ref := range body.Targets {
		res := map[string]any{"agent": ref.Agent, "ok": false}
		tg, err := s.mcpResolve(ref)
		if err == nil {
			err = mcpManager().Add(r.Context(), tg, body.Server)
		}
		if err != nil {
			_, code := mcpErrorStatus(err)
			res["error"] = err.Error()
			res["code"] = code
			lastErr = err
		} else {
			res["ok"] = true
			okCount++
		}
		results = append(results, res)
	}
	if okCount == 0 && lastErr != nil && len(results) == 1 {
		// Один агент и отказ — обычная ошибка с кодом: клиенту так проще.
		status, code := mcpErrorStatus(lastErr)
		jsonErrorCodeAny(w, status, code, lastErr.Error(), map[string]any{"results": results})
		return
	}
	jsonResp(w, map[string]any{"ok": okCount == len(results), "results": results})
}

// DELETE /api/mcp?agent=&account=&name= — удалить сервер (и выключенный тоже).
func (s *Server) apiMCPDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	q := r.URL.Query()
	tg, err := s.mcpResolve(mcpTargetRef{Agent: q.Get("agent"), Account: q.Get("account")})
	if err != nil {
		mcpWriteError(w, err)
		return
	}
	if err := mcpManager().Remove(r.Context(), tg, strings.TrimSpace(q.Get("name"))); err != nil {
		mcpWriteError(w, err)
		return
	}
	jsonResp(w, map[string]any{"ok": true})
}

// POST /api/mcp/toggle {agent, account, name, enabled}
func (s *Server) apiMCPToggle(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		mcpTargetRef
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "неверный JSON", nil)
		return
	}
	tg, err := s.mcpResolve(body.mcpTargetRef)
	if err != nil {
		mcpWriteError(w, err)
		return
	}
	if err := mcpManager().SetEnabled(r.Context(), tg, strings.TrimSpace(body.Name), body.Enabled); err != nil {
		mcpWriteError(w, err)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "enabled": body.Enabled})
}
