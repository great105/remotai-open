package web

// «Проверить подключение» агента: что он РЕАЛЬНО использует и отвечает ли
// модель прямо сейчас. Правила складывания источников и сама проверка — в
// internal/agentcheck; здесь только то, что знает web-слой: аккаунты, ключ
// OpenRouter, .env Remotai и окружение, с которым агент будет запущен.
//
// КЛЮЧ НАРУЖУ НЕ ЕДЕТ. Connection сериализует только маску и источник, а
// Result — вычищенный текст ошибки (тест TestAgentCheckNeverReturnsKey).
//
// ПОЧЕМУ ПРОВЕРКА ЖДЁТ НЕ ДОЛЬШЕ 20 СЕКУНД. Разовый запуск CLI по подписке
// идёт до минуты, а облачный клиент обрывает запрос через 30 с (cloudApi в
// apk/src/api.ts), релей — через 60. Поэтому POST запускает проверку и ждёт её
// agentCheckWait; не успела — отвечает `state: running` с номером, и клиент
// повторяет POST с этим номером: ждёт ТУ ЖЕ проверку, второй запрос к модели
// не уходит.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"

	"tgcontrol/internal/agentcheck"
	"tgcontrol/internal/agents"
	"tgcontrol/internal/paths"
)

// Подменяются в тестах: настоящий CLI на стенде не запускаем.
var (
	agentCheckRunner agentcheck.Runner
	agentCheckProber agentcheck.Prober = agentcheck.ExecProber(nil)
	agentCheckWait                     = 20 * time.Second
	// agentCheckKeep — сколько помнить законченную проверку для повторного
	// POST с номером (клиент мог оборвать ожидание на плохой сети).
	agentCheckKeep = 5 * time.Minute
)

type agentCheckJob struct {
	id       string
	key      string
	done     chan struct{}
	result   agentcheck.Result
	conn     *agentcheck.Connection
	finished time.Time
}

var agentCheckModelRe = regexp.MustCompile(`^[A-Za-z0-9._:/@-]+$`)

var agentChecks = struct {
	sync.Mutex
	byID  map[string]*agentCheckJob
	byKey map[string]*agentCheckJob
	seq   int64
}{byID: map[string]*agentCheckJob{}, byKey: map[string]*agentCheckJob{}}

// agentCheckEnvFile — .env Remotai (оба места, как в loadEnvFromExeDir).
// Нужен ТОЛЬКО чтобы назвать источник переменной; значения наружу не идут.
func agentCheckEnvFile() map[string]string {
	out := map[string]string{}
	files := []string{filepath.Join(paths.Base(), ".env")}
	if exe, err := os.Executable(); err == nil {
		files = append(files, filepath.Join(filepath.Dir(exe), ".env"))
	}
	for _, f := range files {
		m, err := godotenv.Read(f)
		if err != nil {
			continue
		}
		for k, v := range m {
			if _, seen := out[k]; !seen {
				out[k] = v
			}
		}
	}
	return out
}

// agentCheckInput собирает всё, из чего складывается подключение агента под
// аккаунтом (пусто = активный). Возвращает ещё прокси аккаунта — им пойдёт
// прямой HTTP-запрос проверки.
func (s *Server) agentCheckInput(d *agents.AgentDescriptor, accountID string) (agentcheck.Input, string, error) {
	accountsMu.Lock()
	f := loadAccountsFile()
	accountsMu.Unlock()

	acc := defaultAccount(f, d.ID)
	if d.SupportsAccounts() {
		if accountID == "" {
			acc = activeAccount(f, d.ID)
		} else {
			found := false
			for _, a := range accountsForAgent(f, d.ID) {
				if a.ID == accountID {
					acc, found = a, true
					break
				}
			}
			if !found {
				return agentcheck.Input{}, "", fmt.Errorf("account_not_found")
			}
		}
	}

	var pairs []agentcheck.EnvPair
	for _, p := range accountEnvPairs(acc, d) {
		pairs = append(pairs, agentcheck.EnvPair{Name: p["name"], Value: p["value"]})
	}
	proxyURL, proxyLbl := "", ""
	if acc.Proxy != "" && validateProxyURLForAgent(d.ID, acc.Proxy) == nil {
		proxyURL, proxyLbl = acc.Proxy, proxyLabel(acc.Proxy)
	}
	configDir := mainAccountDir(d)
	if acc.Dir != "" {
		configDir = d.AccountCredentialsDir(acc.Dir)
	}
	remove := append([]string{d.AccountEnv}, agentcheck.ProxyEnvNames...)
	cli := d.Path()
	if cli == "built-in" {
		cli = ""
	}
	var orKey, orModel string
	if s.openrouter != nil {
		orKey, orModel = s.openrouter.Key(), s.openrouter.Model()
	}
	label := acc.Label
	in := agentcheck.Input{
		AgentID:   d.ID,
		AgentName: d.Name,
		ConfigDir: configDir,
		Account: agentcheck.Account{
			ID: acc.ID, Label: label, IsDefault: acc.ID == DefaultAccountID,
			Pairs: pairs, ProxyLabel: proxyLbl,
		},
		Getenv:                  os.LookupEnv,
		EnvFile:                 agentCheckEnvFile(),
		OpenRouterKey:           orKey,
		OpenRouterModel:         orModel,
		CLI:                     cli,
		LaunchEnv:               agentcheck.LaunchEnv(os.Environ(), remove, pairs),
		Probe:                   agentCheckProber,
		SupportsOpenRouterModel: d.ModelFlag != "",
	}
	return in, proxyURL, nil
}

func agentFromPath(w http.ResponseWriter, r *http.Request) *agents.AgentDescriptor {
	d := agents.GetDescriptor(r.PathValue("id"))
	if d == nil || len(d.CLINames) == 0 {
		jsonErrorCode(w, 404, "unknown_agent", "такого агента нет", nil)
		return nil
	}
	return d
}

// GET /api/agents/{id}/connection?account= — действующее подключение.
func (s *Server) apiAgentConnection(w http.ResponseWriter, r *http.Request, uid int64) {
	d := agentFromPath(w, r)
	if d == nil {
		return
	}
	in, _, err := s.agentCheckInput(d, strings.TrimSpace(r.URL.Query().Get("account")))
	if err != nil {
		jsonErrorCode(w, 404, "account_not_found", "такого аккаунта нет", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	jsonResp(w, agentcheck.Resolve(ctx, in))
}

// POST /api/agents/{id}/check {account, model?, id?} — один настоящий запрос.
func (s *Server) apiAgentCheck(w http.ResponseWriter, r *http.Request, uid int64) {
	d := agentFromPath(w, r)
	if d == nil {
		return
	}
	var req struct {
		Account string `json:"account"`
		Model   string `json:"model"`
		ID      string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	// Белый список, а не чёрный: через шим claude.cmd cmd.exe раскрыл бы
	// `%…%` и `^` в имени модели (скептик 29.09).
	if req.Model != "" && (len(req.Model) > 200 || !agentCheckModelRe.MatchString(req.Model)) {
		jsonErrorCode(w, 400, "bad_model", "имя модели не похоже на модель", nil)
		return
	}

	var job *agentCheckJob
	if req.ID != "" {
		agentChecks.Lock()
		job = agentChecks.byID[req.ID]
		agentChecks.Unlock()
		if job == nil || !strings.HasPrefix(job.key, d.ID+"\n") {
			jsonErrorCode(w, 404, "check_not_found", "проверка не найдена — запустите заново", nil)
			return
		}
	} else {
		in, proxyURL, err := s.agentCheckInput(d, strings.TrimSpace(req.Account))
		if err != nil {
			jsonErrorCode(w, 404, "account_not_found", "такого аккаунта нет", nil)
			return
		}
		job = startAgentCheck(in, proxyURL, req.Model)
	}

	timer := time.NewTimer(agentCheckWait)
	defer timer.Stop()
	select {
	case <-job.done:
		jsonResp(w, map[string]any{"state": "done", "id": job.id, "result": job.result, "connection": job.conn})
	case <-timer.C:
		jsonResp(w, map[string]any{"state": "running", "id": job.id})
	case <-r.Context().Done():
	}
}

// startAgentCheck запускает проверку или присоединяется к уже идущей с теми же
// агентом, аккаунтом и моделью: двойное нажатие не тратит второй запрос.
func startAgentCheck(in agentcheck.Input, proxyURL, model string) *agentCheckJob {
	key := in.AgentID + "\n" + in.Account.ID + "\n" + model
	agentChecks.Lock()
	defer agentChecks.Unlock()
	now := time.Now()
	for id, j := range agentChecks.byID {
		if !j.finished.IsZero() && now.Sub(j.finished) > agentCheckKeep {
			delete(agentChecks.byID, id)
			if agentChecks.byKey[j.key] == j {
				delete(agentChecks.byKey, j.key)
			}
		}
	}
	if j := agentChecks.byKey[key]; j != nil && j.finished.IsZero() {
		return j
	}
	agentChecks.seq++
	job := &agentCheckJob{
		id:   fmt.Sprintf("chk-%d-%d", now.UnixNano(), agentChecks.seq),
		key:  key,
		done: make(chan struct{}),
	}
	agentChecks.byID[job.id] = job
	agentChecks.byKey[key] = job

	go func() {
		// Не r.Context(): запрос может вернуться раньше, а проверка обязана
		// дойти до конца — иначе повторный POST с номером ждал бы вечно.
		ctx, cancel := context.WithTimeout(context.Background(), agentcheck.CLITimeout+30*time.Second)
		defer cancel()
		conn := agentcheck.Resolve(ctx, in)
		dir := filepath.Join(os.TempDir(), "remotai-agent-check")
		_ = os.MkdirAll(dir, 0o700)
		res := agentcheck.Run(ctx, conn, agentcheck.Options{
			Model:    model,
			ProxyURL: proxyURL,
			CLI:      in.CLI,
			Env:      in.LaunchEnv,
			Dir:      dir,
			Run:      agentCheckRunner,
		})
		agentChecks.Lock()
		job.result, job.conn, job.finished = res, conn, time.Now()
		agentChecks.Unlock()
		close(job.done)
	}()
	return job
}
