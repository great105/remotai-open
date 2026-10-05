package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/aiusage"
)

// Аккаунты нейросетей на этой машине.
//
// ЗАЧЕМ. У человека бывает несколько подписок на одного провайдера (две Claude,
// две ChatGPT) — их держат ровно затем, чтобы не упираться в лимит. Но CLI-агент
// знает только один вход, и «переключиться» значило разлогиниться и войти
// заново: в терминале, с телефона, каждый раз.
//
// КАК ЭТО РАБОТАЕТ. У всех четырёх CLI аккаунт — это КАТАЛОГ, а не настройка
// внутри программы, и каталог задаётся переменной окружения (см. AccountEnv в
// internal/agents/registry.go — там для каждого агента записано, чем именно
// проверено). Поэтому:
//   - второй аккаунт = второй каталог, оба живут одновременно;
//   - переключение ничего не разлогинивает и ничего не перезаписывает;
//   - токены НЕ копируются между каталогами — их выдаёт сам провайдер при
//     входе, и лежат они только там, куда он их положил.
//
// ПРАВИЛО ЭТОГО ФАЙЛА: основной аккаунт (тот, которым человек пользуется
// сейчас, в домашнем каталоге) не трогаем НИКОГДА — у него пустой Dir, и
// переменная окружения при запуске просто не подставляется. Иначе первое же
// переключение сломало бы работающий вход.
//
// Список живёт на КОМПЬЮТЕРЕ, а не в пульте, — по образцу своих команд
// (api_commands.go) и пресетов: пультов у человека несколько, а машина, где
// стоят агенты, одна.
type AgentAccount struct {
	ID      string `json:"id"`
	AgentID string `json:"agent_id"`
	// Label — как человек назвал: «рабочий», «личный». Пусто у основного.
	Label string `json:"label"`
	// Dir — каталог профиля. ПУСТО = основной: запускаем как раньше, без
	// подстановки переменной.
	Dir string `json:"dir,omitempty"`
	// Proxy — через какой прокси этому аккаунту ходить в сеть.
	//
	// ЗАЧЕМ (задача владельца от 05.08.2026). Аккаунты у человека разные — и
	// сеть под ними бывает нужна разная: рабочая подписка через рабочий канал,
	// личная напрямую. Одного системного прокси на всю машину для этого мало:
	// он общий для всех агентов сразу.
	//
	// Хранится строкой URL. В launch env попадает только схема, подтверждённая
	// для конкретного CLI: сейчас HTTP/HTTPS у Claude Code и Codex. Legacy
	// SOCKS/Gemini/Kimi остаётся видимым для исправления, но помечается
	// proxy_blocked: новый клиент не запускает CLI, пока настройку не очистят
	// или не заменят. Простое исключение env было бы fail-open (прямой выход).
	//
	// Userinfo (логин/пароль в URL) запрещён до отдельного backend env-at-spawn:
	// сейчас env превращается клиентом в shell-команду и попал бы в preview,
	// scrollback и историю. Старое значение редактируется и блокирует launch до
	// очистки/замены; просто убрать его из env означало бы прямой выход.
	Proxy     string `json:"proxy,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
}

// accountEnvPairs — переменные окружения, с которыми запускается этот аккаунт.
//
// Пар стало НЕСКОЛЬКО, и это главное отличие от прежней схемы: раньше клиент
// получал одну (`env_name`/`env_value`) — каталог профиля. Прокси одной парой
// не задать: CLI и библиотеки под ними смотрят на разные имена, поэтому
// ставятся все три сразу.
func accountEnvPairs(a AgentAccount, d *agents.AgentDescriptor) []map[string]string {
	pairs := make([]map[string]string, 0, 10)
	if d != nil && d.AccountEnv != "" && a.Dir != "" {
		pairs = append(pairs, map[string]string{"name": d.AccountEnv, "value": a.Dir})
	}
	if proxy := strings.TrimSpace(a.Proxy); proxy != "" && validateProxyURLForAgent(a.AgentID, proxy) == nil {
		// Обе раскладки обязательны: разные runtime-ы выбирают разный регистр,
		// а у части POSIX-библиотек унаследованный lowercase сильнее uppercase.
		// Если оставить только верхний, чужой системный proxy может молча
		// победить выбранный аккаунтом.
		for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"} {
			pairs = append(pairs, map[string]string{"name": name, "value": proxy})
		}
		// NO_PROXY поддерживают актуальные Claude Code и Codex. Но на одну эту
		// переменную как на гарантию не полагаемся: transport allowlist и
		// client-side contract отдельно запрещают неподтверждённую схему.
		// Не наследуем host NO_PROXY: там может быть `*` или домен провайдера,
		// и тогда аккаунт с выбранным proxy молча уйдёт напрямую. Для этого
		// контракта исключения только loopback, одинаково в обоих регистрах.
		const noProxy = "localhost,127.0.0.1,::1"
		pairs = append(pairs,
			map[string]string{"name": "NO_PROXY", "value": noProxy},
			map[string]string{"name": "no_proxy", "value": noProxy},
		)
	}
	return pairs
}

// probeProxy — ЖИВАЯ проверка: запрос ЧЕРЕЗ прокси на эхо-адрес, ответ — это
// выходной IP, который увидит провайдер агента.
//
// Почему не TCP-стук в порт (так было до 14.08.2026): порт может слушать, а
// наружу не пускать — и человек узнает об этом только по «агент не отвечает».
// А главное — задача прокси у аккаунта в том, чтобы выйти ОТДЕЛЬНЫМ адресом,
// а не общим VPN машины. Выходной IP доказывает этот путь для проверочного
// Go-запроса; путь CLI отдельно ограничен transport allowlist-ом.
//
// Проверка доказывает доступность САМОГО прокси. Поддержку transport-а агента
// доказывает отдельный allowlist в validateProxyURLForAgent: смешивать эти два
// факта нельзя — Go-клиент умеет больше схем, чем некоторые CLI.
func probeProxy(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("адрес не разобрать")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return "", fmt.Errorf("системный HTTP transport недоступен")
	}
	transport := base.Clone()
	// Это разовая проверка при сохранении, а не пул запросов. Не оставляем
	// CONNECT/readLoop жить после ответа или отмены клиента.
	transport.DisableKeepAlives = true
	defer transport.CloseIdleConnections()
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(u)
	default:
		return "", fmt.Errorf("вид прокси не поддерживается: %s", u.Scheme)
	}
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("эхо-адрес ответил %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("эхо-адрес вернул не IP: %q", ip)
	}
	return ip, nil
}

// proxyVerdict — итог проверки для ответа ручки сохранения.
//
// Предупреждение — когда агент через такой прокси НЕ выйдет (замер того же
// стенда: недоступный прокси Node долбит бесконечно — ловушка приняла 75 930
// попыток за полминуты, а человек видит только «агент не отвечает»). Это
// предупреждение, а не запрет: адрес может быть верным, а прокси — ещё не
// поднятым. Выходной адрес — когда всё сошлось: человек СРАЗУ видит, что
// отдельный путь получился, и с каким IP.
func proxyVerdict(ctx context.Context, raw string) (warning, exitIP string) {
	exitIP, err := probeProxy(ctx, raw)
	if err != nil {
		return "Прокси " + proxyLabel(raw) + " не удалось подтвердить. Настройка сохранена, но новые запуски через него сейчас не заработают.", ""
	}
	return "", exitIP
}

// proxyLabel не возвращает никакую часть userinfo legacy URL в API/DOM.
// Username тоже может быть токеном, поэтому безопасная метка содержит только
// схему, нейтральный маркер и host — без path/query/fragment.
func proxyLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "некорректный URL"
	}
	if u.User != nil {
		return u.Scheme + "://***@" + u.Host
	}
	// Даже URL без userinfo может содержать token в path/query/fragment.
	// Для интерфейса достаточно transport + authority.
	return u.Scheme + "://" + u.Host
}

type accountsFile struct {
	Accounts []AgentAccount `json:"accounts"`
	// Active — какой аккаунт выбран у каждого агента (agent_id → account id).
	// Отсутствие записи = основной.
	Active map[string]string `json:"active,omitempty"`
	// Folders — аккаунт, закреплённый за папкой: "<agent_id>\n<путь>" → id.
	//
	// Рабочий код всегда открывают рабочей подпиской, свой — личной, и решать
	// это каждый раз руками человек не должен. Закрепление сильнее общего
	// выбора: оно точнее — сказано про конкретную папку.
	Folders map[string]string `json:"folders,omitempty"`
	// DefaultProxy — прокси ОСНОВНОГО аккаунта, по агентам (agent_id → url).
	//
	// Отдельной картой, потому что основной аккаунт в файле не хранится вовсе —
	// он синтетический (см. DefaultAccountID). А прокси ему нужен в первую
	// очередь: у большинства людей аккаунт ровно один, и это как раз основной.
	DefaultProxy map[string]string `json:"default_proxy,omitempty"`
}

// validateProxyURL — адрес прокси должен быть понятен CLI, а не «почти верен».
//
// Проверяем не только схему, но и границу доставки секрета: userinfo пока
// запрещён, потому что env клиента превращается в видимую shell-команду.
func validateProxyURLForAgent(agentID, raw string) error {
	if raw == "" {
		return nil // пусто = прокси нет, это нормальное состояние
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("адрес прокси не разобрать: нужно http://хост:порт")
	}
	if u.User != nil {
		return fmt.Errorf("прокси с логином или паролем пока не поддерживается: секрет попал бы в текст shell-команды")
	}
	if u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return fmt.Errorf("адрес прокси должен содержать только схему, хост и порт — без пути, query и fragment")
	}
	scheme := strings.ToLower(u.Scheme)
	d := agents.GetDescriptor(agentID)
	if d != nil {
		for _, allowed := range d.ProxySchemes {
			if scheme == allowed {
				return nil
			}
		}
	}
	switch scheme {
	case "socks5", "socks5h":
		return fmt.Errorf("SOCKS-прокси для этого агента не подтверждён; используйте HTTP-прокси, например http://127.0.0.1:12334")
	case "":
		return fmt.Errorf("укажите вид прокси: http://%s", raw)
	case "http", "https":
		name := agentID
		if d != nil {
			name = d.Name
		}
		return fmt.Errorf("прокси для «%s» пока не подтверждён; запуск заблокирован, чтобы агент не ушёл напрямую", name)
	default:
		return fmt.Errorf("такой вид прокси не поддерживается: %s (нужно http или https)", u.Scheme)
	}
}

// defaultAccount — основной аккаунт агента: пустой каталог, но, возможно, свой
// прокси. Конструктор один на все места, где раньше писали структуру руками:
// их три, и «а тут забыли прокси» — вопрос времени.
func defaultAccount(f accountsFile, agentID string) AgentAccount {
	return AgentAccount{
		ID:      DefaultAccountID,
		AgentID: agentID,
		Proxy:   f.DefaultProxy[agentID],
	}
}

// folderKey — ключ закрепления. Путь приводим к одному виду: разделители и
// регистр диска на Windows пишут как придётся, а «C:/Work» и «c:\work» — это
// одна и та же папка.
func folderKey(agentID, path string) string {
	return agentID + "\n" + normalizeFolder(path)
}

func normalizeFolder(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(strings.ReplaceAll(p, "/", `\`))
	}
	return strings.TrimRight(p, `\/`)
}

// accountForFolder ищет аккаунт, закреплённый за папкой или любой папкой ВЫШЕ
// неё: закрепив аккаунт за проектом, человек ожидает его и во вложенных
// каталогах — агента запускают из любого места дерева.
func accountForFolder(f accountsFile, agentID, cwd string) string {
	dir := normalizeFolder(cwd)
	if dir == "" || len(f.Folders) == 0 {
		return ""
	}
	for {
		if id, ok := f.Folders[folderKey(agentID, dir)]; ok {
			return id
		}
		parent := filepath.Dir(dir)
		if parent == dir || parent == "." {
			return ""
		}
		dir = normalizeFolder(parent)
	}
}

// DefaultAccountID — идентификатор основного аккаунта. Он не хранится в файле:
// это то, что уже настроено на машине, и существует всегда.
const DefaultAccountID = "default"

var accountsMu sync.Mutex

// blockedLegacyProxyAgents — агенты с ЛЮБЫМ настроенным proxy. Старый клиент
// не умеет process-scoped env и после запуска оставляет proxy/profile в
// интерактивном PowerShell, поэтому даже валидный HTTP transport для него
// небезопасен. Новый client явно объявляет account_proxy_contract=1.
func blockedLegacyProxyAgents() map[string]bool {
	accountsMu.Lock()
	f := loadAccountsFile()
	accountsMu.Unlock()
	blocked := map[string]bool{}
	for agentID, proxy := range f.DefaultProxy {
		if strings.TrimSpace(proxy) != "" {
			blocked[agentID] = true
		}
	}
	for _, account := range f.Accounts {
		if strings.TrimSpace(account.Proxy) != "" {
			blocked[account.AgentID] = true
		}
	}
	return blocked
}

func accountsPath() string {
	return filepath.Join(realUserHome(), ".tgcontrol-accounts.json")
}

// accountsRoot — где заводятся каталоги дополнительных аккаунтов.
//
// Рядом с остальными служебными файлами агента, а НЕ в видимой папке
// `~/Remotai`: внутри лежат OAuth-токены, а ту папку человек открывает,
// показывает на экране и кидает в неё файлы.
func accountsRoot() string {
	return filepath.Join(realUserHome(), ".tgcontrol-accounts")
}

func loadAccountsFile() accountsFile {
	var f accountsFile
	data, err := os.ReadFile(accountsPath())
	if err != nil {
		return f
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return accountsFile{}
	}
	return f
}

func saveAccountsFile(f accountsFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(accountsPath(), data, 0o600)
}

// accountsForAgent отдаёт список аккаунтов агента с основным ПЕРВЫМ.
func accountsForAgent(f accountsFile, agentID string) []AgentAccount {
	list := []AgentAccount{defaultAccount(f, agentID)}
	for _, a := range f.Accounts {
		if a.AgentID == agentID {
			list = append(list, a)
		}
	}
	return list
}

// activeAccount возвращает выбранный аккаунт агента. Если выбранный удалён или
// его каталог исчез, молча откатываемся на основной: висящий выбор означал бы
// запуск агента в каталог, которого нет, — то есть «войдите заново» на пустом
// месте.
func activeAccount(f accountsFile, agentID string) AgentAccount {
	return activeAccountIn(f, agentID, "")
}

// activeAccountIn — то же, но с учётом папки: закрепление за папкой сильнее
// общего выбора, потому что сказано точнее. Пустой cwd = общий выбор.
func activeAccountIn(f accountsFile, agentID, cwd string) AgentAccount {
	id := f.Active[agentID]
	if pinned := accountForFolder(f, agentID, cwd); pinned != "" {
		id = pinned
	}
	if id == "" || id == DefaultAccountID {
		return defaultAccount(f, agentID)
	}
	for _, a := range f.Accounts {
		if a.ID == id && a.AgentID == agentID {
			if a.Dir != "" {
				if st, err := os.Stat(a.Dir); err != nil || !st.IsDir() {
					return defaultAccount(f, agentID)
				}
			}
			return a
		}
	}
	return defaultAccount(f, agentID)
}

// accountPayload — как аккаунт выглядит для клиента.
func accountPayload(a AgentAccount, d *agents.AgentDescriptor, active bool) map[string]any {
	out := map[string]any{
		"id":         a.ID,
		"agent_id":   a.AgentID,
		"label":      a.Label,
		"dir":        a.Dir,
		"is_default": a.ID == DefaultAccountID,
		"active":     active,
	}
	if a.CreatedAt > 0 {
		out["created_at"] = a.CreatedAt
	}
	// env_name/env_value — из чего клиент соберёт команду запуска. Собирает он
	// сам, потому что синтаксис зависит от шелла: в POSIX `env VAR=… claude`,
	// в PowerShell `$env:VAR='…'; claude`. У основного значения нет вовсе —
	// переменную подставлять не надо.
	if d != nil && d.AccountEnv != "" && a.Dir != "" {
		out["env_name"] = d.AccountEnv
		out["env_value"] = a.Dir
	}
	// env — полный список пар (каталог + прокси). Старые `env_name`/`env_value`
	// оставлены рядом намеренно: клиент обновляется отдельно от агента, и на
	// старом пульте аккаунт обязан продолжать работать как раньше.
	if pairs := accountEnvPairs(a, d); len(pairs) > 0 {
		out["env"] = pairs
	}
	if a.Proxy != "" {
		if err := validateProxyURLForAgent(a.AgentID, a.Proxy); err != nil {
			// В старое поле `proxy` invalid legacy не кладём: старый APK не знает
			// warning и иначе назовёт отключённый канал активным «через ...».
			// Новый клиент показывает безопасную метку и запрещает запуск до
			// очистки/замены настройки.
			out["proxy_legacy"] = proxyLabel(a.Proxy)
			out["proxy_warning"] = err.Error()
			out["proxy_blocked"] = true
		} else {
			// Userinfo для нового значения запрещён validation-ом, поэтому здесь
			// нет секрета, который мог бы попасть в DOM или shell preview.
			out["proxy"] = a.Proxy
		}
	}
	return out
}

// GET /api/accounts[?agent_id=…] — аккаунты по агентам.
//
// Ключ `accounts` есть всегда: по нему клиент отличает агента, который умеет
// аккаунты, от старого, который про них не знает.
func (s *Server) apiAccountsList(w http.ResponseWriter, r *http.Request, uid int64) {
	only := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	// cwd — папка терминала, из которого спрашивают. С ней ответ учитывает
	// закрепление за папкой, и клиенту не нужно знать про эти правила вовсе:
	// «активный» уже посчитан правильно для ЭТОГО места.
	cwd := strings.TrimSpace(r.URL.Query().Get("cwd"))

	accountsMu.Lock()
	f := loadAccountsFile()
	accountsMu.Unlock()

	// Заодно чиним каталоги, в которые уже вошли: Claude Code гоняет мастер
	// первого запуска (вплоть до экрана входа) до тех пор, пока в его конфиге
	// нет отметки о пройденном мастере — см. account_onboarding.go. Место
	// выбрано не случайно: список смотрят ровно перед тем, как запускать агента.
	healAccountsOnboarding(f)

	out := make([]map[string]any, 0, 8)
	for _, d := range agents.Registry {
		if !d.SupportsAccounts() {
			continue
		}
		if only != "" && d.ID != only {
			continue
		}
		active := activeAccountIn(f, d.ID, cwd)
		pinnedID := accountForFolder(f, d.ID, cwd)
		for _, a := range accountsForAgent(f, d.ID) {
			item := accountPayload(a, d, a.ID == active.ID)
			// pinned — этот аккаунт закреплён за папкой терминала. Интерфейс
			// обязан отличать «выбран сейчас» от «закреплён за проектом»:
			// второе переживёт любой общий выбор.
			if pinnedID != "" && a.ID == pinnedID {
				item["pinned"] = true
			}
			out = append(out, item)
		}
	}
	jsonResp(w, map[string]any{"accounts": out, "proxy_contract_version": 1})
}

// POST /api/accounts — завести аккаунт {agent_id, label} или переименовать
// существующий {id, label}.
//
// Заводится ПУСТОЙ каталог: войти в него всё равно должен сам человек — токен
// выдаёт провайдер, и обойти это нечем. Первый запуск агента в новом каталоге
// сам приводит человека к экрану входа (мастер первого запуска Claude Code), а
// когда вход состоялся, мастер снимается отметкой — см. account_onboarding.go:
// без неё он крутится по кругу и выглядит как «второй аккаунт не запоминает
// вход».
func (s *Server) apiAccountsUpsert(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		ID      string `json:"id"`
		AgentID string `json:"agent_id"`
		Label   string `json:"label"`
		// Proxy — прокси этого аккаунта. Указатель, чтобы отличить «не трогай»
		// (поле не прислали — переименование) от «сотри» (прислали пустую
		// строку). Без этого различия снять прокси было бы нечем.
		Proxy *string `json:"proxy"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	req.Label = strings.TrimSpace(req.Label)

	accountsMu.Lock()
	accountsLocked := true
	unlockAccounts := func() {
		if accountsLocked {
			accountsLocked = false
			accountsMu.Unlock()
		}
	}
	defer unlockAccounts()
	f := loadAccountsFile()

	// Прокси ОСНОВНОГО аккаунта: он в файле не хранится, поэтому у него нет ни
	// имени, ни каталога — менять тут можно ровно прокси и ничего больше.
	if req.ID == DefaultAccountID {
		d := agents.GetDescriptor(req.AgentID)
		if d == nil {
			jsonErrorCode(w, 400, "unknown_agent", "неизвестный агент", nil)
			return
		}
		if req.Proxy == nil {
			jsonErrorCode(w, 400, "bad_request", "у основного аккаунта меняется только прокси", nil)
			return
		}
		proxy := strings.TrimSpace(*req.Proxy)
		if err := validateProxyURLForAgent(req.AgentID, proxy); err != nil {
			jsonErrorCode(w, 400, "bad_proxy", err.Error(), nil)
			return
		}
		if f.DefaultProxy == nil {
			f.DefaultProxy = map[string]string{}
		}
		if proxy == "" {
			delete(f.DefaultProxy, req.AgentID)
		} else {
			f.DefaultProxy[req.AgentID] = proxy
		}
		if err := saveAccountsFile(f); err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		active := activeAccount(f, req.AgentID)
		out := map[string]any{"account": accountPayload(defaultAccount(f, req.AgentID), d, active.ID == DefaultAccountID)}
		// Сетевой probe может ждать до шести секунд. Файл уже атомарно сохранён,
		// поэтому глобальный accountsMu перед сетью освобождаем: иначе один
		// мёртвый прокси блокирует список/выбор/удаление всех аккаунтов.
		unlockAccounts()
		if proxy != "" {
			warning, exitIP := proxyVerdict(r.Context(), proxy)
			if warning != "" {
				out["warning"] = warning
			}
			if exitIP != "" {
				out["proxy_exit"] = exitIP
			}
		}
		jsonResp(w, out)
		return
	}

	// Переименование.
	if req.ID != "" && req.ID != DefaultAccountID {
		for i := range f.Accounts {
			if f.Accounts[i].ID != req.ID {
				continue
			}
			// Прокси меняется и отдельно от имени: карточка правит его своим
			// полем, не трогая название.
			if req.Proxy != nil {
				proxy := strings.TrimSpace(*req.Proxy)
				if err := validateProxyURLForAgent(f.Accounts[i].AgentID, proxy); err != nil {
					jsonErrorCode(w, 400, "bad_proxy", err.Error(), nil)
					return
				}
				f.Accounts[i].Proxy = proxy
				if req.Label == "" {
					if err := saveAccountsFile(f); err != nil {
						jsonError(w, err.Error(), 500)
						return
					}
					d := agents.GetDescriptor(f.Accounts[i].AgentID)
					active := activeAccount(f, f.Accounts[i].AgentID)
					out := map[string]any{"account": accountPayload(f.Accounts[i], d, active.ID == req.ID)}
					unlockAccounts()
					if proxy != "" {
						warning, exitIP := proxyVerdict(r.Context(), proxy)
						if warning != "" {
							out["warning"] = warning
						}
						if exitIP != "" {
							out["proxy_exit"] = exitIP
						}
					}
					jsonResp(w, out)
					return
				}
			}
			if req.Label == "" {
				jsonErrorCode(w, 400, "empty_label", "у аккаунта должно быть имя", nil)
				return
			}
			f.Accounts[i].Label = req.Label
			if err := saveAccountsFile(f); err != nil {
				jsonError(w, err.Error(), 500)
				return
			}
			// Имя аккаунта едет в карточку лимитов — снимок обязан его узнать.
			aiusage.InvalidateCache()
			d := agents.GetDescriptor(f.Accounts[i].AgentID)
			active := activeAccount(f, f.Accounts[i].AgentID)
			jsonResp(w, map[string]any{"account": accountPayload(f.Accounts[i], d, active.ID == req.ID)})
			return
		}
		jsonErrorCode(w, 404, "not_found", "аккаунт не найден", nil)
		return
	}

	d := agents.GetDescriptor(req.AgentID)
	if d == nil {
		jsonErrorCode(w, 400, "unknown_agent", "неизвестный агент", nil)
		return
	}
	if !d.SupportsAccounts() {
		// Честный отказ вместо пустого каталога: у этого CLI аккаунт
		// переменной не выбирается, и профиль ничего бы не изменил.
		jsonErrorCode(w, 400, "accounts_unsupported",
			"У «"+d.Name+"» переключение аккаунтов не поддерживается", nil)
		return
	}
	if req.Label == "" {
		jsonErrorCode(w, 400, "empty_label", "у аккаунта должно быть имя", nil)
		return
	}
	for _, a := range f.Accounts {
		if a.AgentID == req.AgentID && strings.EqualFold(a.Label, req.Label) {
			jsonErrorCode(w, 409, "label_exists", "аккаунт с таким именем уже есть", nil)
			return
		}
	}

	account := AgentAccount{
		ID:        newAccountID(),
		AgentID:   req.AgentID,
		Label:     req.Label,
		CreatedAt: time.Now().Unix(),
	}
	account.Dir = filepath.Join(accountsRoot(), req.AgentID, account.ID)
	// 0700: внутри окажутся OAuth-токены. На Windows права носят справочный
	// характер, но каталог всё равно лежит в профиле пользователя.
	if err := os.MkdirAll(account.Dir, 0o700); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// Знания и настройки делаем общими с основным аккаунтом — иначе человек
	// переключится и обнаружит агента без единого скилла (см. account_share.go).
	// Вход и переписки при этом остаются раздельными, ради чего всё и затевалось.
	shared := shareAccountResources(d, mainAccountDir(d), d.AccountCredentialsDir(account.Dir))
	mcpCount := 0
	if d.ID == "claude" {
		// Источник — `~/.claude.json` (в КОРНЕ домашней папки, не в ~/.claude):
		// у Claude Code этот файл исторически лежит именно там, и попытка взять
		// его из каталога конфига молча переносила ноль серверов.
		if n, err := copyClaudeMCP(filepath.Join(realUserHome(), ".claude.json"), account.Dir); err == nil {
			mcpCount = n
		}
	}
	f.Accounts = append(f.Accounts, account)
	if err := saveAccountsFile(f); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	// Список аккаунтов изменился — снимок лимитов устарел РОВНО СЕЙЧАС, когда
	// человек на него и смотрит (найдено живым прогоном: заведённый аккаунт не
	// появлялся в лимитах пять минут, а «сейчас» стояло у прежнего).
	aiusage.InvalidateCache()
	jsonResp(w, map[string]any{
		"account": accountPayload(account, d, false),
		// shared/mcp_servers — чтобы интерфейс сказал ПРАВДУ о том, что стало
		// общим, а не обещал «всё перенесено».
		"shared":      shared,
		"mcp_servers": mcpCount,
	})
}

// POST /api/accounts/activate {agent_id, id} — каким аккаунтом запускать агента.
//
// Выбор влияет ТОЛЬКО на новые запуски: уже работающий терминал живёт со своим
// окружением, и менять его на лету было бы неправдой.
func (s *Server) apiAccountsActivate(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		AgentID string `json:"agent_id"`
		ID      string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	d := agents.GetDescriptor(req.AgentID)
	if d == nil || !d.SupportsAccounts() {
		jsonErrorCode(w, 400, "unknown_agent", "неизвестный агент", nil)
		return
	}

	accountsMu.Lock()
	defer accountsMu.Unlock()
	f := loadAccountsFile()
	if req.ID != DefaultAccountID {
		found := false
		for _, a := range f.Accounts {
			if a.ID == req.ID && a.AgentID == req.AgentID {
				found = true
				break
			}
		}
		if !found {
			jsonErrorCode(w, 404, "not_found", "аккаунт не найден", nil)
			return
		}
	}
	if f.Active == nil {
		f.Active = map[string]string{}
	}
	f.Active[req.AgentID] = req.ID
	if err := saveAccountsFile(f); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	// Выбранный аккаунт вот-вот запустят — самое время убрать из него мастер
	// первого запуска, если вход в нём уже есть (account_onboarding.go).
	healAccountsOnboarding(f)
	// Список аккаунтов изменился — снимок лимитов устарел РОВНО СЕЙЧАС, когда
	// человек на него и смотрит (найдено живым прогоном: заведённый аккаунт не
	// появлялся в лимитах пять минут, а «сейчас» стояло у прежнего).
	aiusage.InvalidateCache()
	active := activeAccount(f, req.AgentID)
	jsonResp(w, map[string]any{"account": accountPayload(active, d, true)})
}

// POST /api/accounts/pin {agent_id, id, path} — закрепить аккаунт за папкой.
//
// Пустой id снимает закрепление. Закрепление действует и на вложенные папки:
// агента запускают из любого места дерева проекта, а подписка у проекта одна.
func (s *Server) apiAccountsPin(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		AgentID string `json:"agent_id"`
		ID      string `json:"id"`
		Path    string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	d := agents.GetDescriptor(req.AgentID)
	if d == nil || !d.SupportsAccounts() {
		jsonErrorCode(w, 400, "unknown_agent", "неизвестный агент", nil)
		return
	}
	path := normalizeFolder(req.Path)
	if path == "" {
		jsonErrorCode(w, 400, "bad_request", "нужна папка", nil)
		return
	}

	accountsMu.Lock()
	defer accountsMu.Unlock()
	f := loadAccountsFile()
	if f.Folders == nil {
		f.Folders = map[string]string{}
	}
	key := folderKey(req.AgentID, path)
	if req.ID == "" {
		delete(f.Folders, key)
	} else {
		f.Folders[key] = req.ID
	}
	if err := saveAccountsFile(f); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	aiusage.InvalidateCache()
	jsonResp(w, map[string]any{
		"pinned":  req.ID != "",
		"path":    path,
		"account": accountPayload(activeAccountIn(f, req.AgentID, path), d, true),
	})
}

// DELETE /api/accounts?id=… — забыть аккаунт.
//
// Каталог удаляется вместе с записью — иначе на диске остался бы живой токен от
// аккаунта, который человек считает удалённым. Но только НАШ каталог (внутри
// accountsRoot): чужой путь мы не заводили и стирать не вправе.
func (s *Server) apiAccountsDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" || id == DefaultAccountID {
		jsonErrorCode(w, 400, "bad_request", "нужен id дополнительного аккаунта", nil)
		return
	}

	accountsMu.Lock()
	defer accountsMu.Unlock()
	f := loadAccountsFile()
	out := make([]AgentAccount, 0, len(f.Accounts))
	var removed *AgentAccount
	for i := range f.Accounts {
		if f.Accounts[i].ID == id {
			removed = &f.Accounts[i]
			continue
		}
		out = append(out, f.Accounts[i])
	}
	if removed == nil {
		jsonErrorCode(w, 404, "not_found", "аккаунт не найден", nil)
		return
	}
	f.Accounts = out
	if f.Active[removed.AgentID] == id {
		delete(f.Active, removed.AgentID)
	}
	if err := saveAccountsFile(f); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	// Список аккаунтов изменился — снимок лимитов устарел РОВНО СЕЙЧАС, когда
	// человек на него и смотрит (найдено живым прогоном: заведённый аккаунт не
	// появлялся в лимитах пять минут, а «сейчас» стояло у прежнего).
	aiusage.InvalidateCache()
	if dir := removed.Dir; dir != "" && withinAccountsRoot(dir) {
		_ = os.RemoveAll(dir)
	}
	jsonResp(w, map[string]any{"ok": true})
}

// POST /api/pty/{id}/account {account_id, label} — запомнить, каким аккаунтом
// запущен агент этого терминала.
//
// Зовётся клиентом в момент отправки команды запуска: только он знает, что
// именно отправляет. Пустой account_id стирает отметку (запустили основным).
func (s *Server) apiPtySetAccount(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	if id == "" {
		jsonErrorCode(w, 400, "bad_request", "нужен id терминала", nil)
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
		Label     string `json:"label"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	if err := s.ptyManager.SetMetaAccount(id, strings.TrimSpace(req.AccountID), strings.TrimSpace(req.Label)); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResp(w, map[string]any{"ok": true})
}

// withinAccountsRoot — лежит ли путь внутри каталога, который заводили мы.
func withinAccountsRoot(dir string) bool {
	root := accountsRoot()
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func newAccountID() string { return fmt.Sprintf("acc-%d", time.Now().UnixNano()) }

// mainAccountDir — каталог ОСНОВНОГО аккаунта агента: тот, которым человек уже
// пользуется. Именно из него берутся общие скиллы и настройки.
//
// Для "config_dir" это стандартный каталог агента в домашней папке, для "home"
// — папка агента внутри домашней (у gemini всё лежит в `~/.gemini`).
func mainAccountDir(d *agents.AgentDescriptor) string {
	if d == nil {
		return ""
	}
	home := realUserHome()
	if home == "" {
		return ""
	}
	switch d.ID {
	case "claude":
		return filepath.Join(home, ".claude")
	case "codex":
		return filepath.Join(home, ".codex")
	case "gemini":
		return filepath.Join(home, ".gemini")
	case "kimi":
		return filepath.Join(home, ".kimi-code")
	}
	return ""
}

// usageAccounts — список аккаунтов для сбора лимитов (aiusage.SetAccountsSource).
//
// Отдаём каталог КРЕДОВ, а не каталог профиля: у gemini профиль — это домашний
// каталог, а креды внутри, в `.gemini` (см. AccountCredentialsDir). Основной
// аккаунт едет с пустым Dir — читать его надо там, где он всегда и лежал.
func usageAccounts() []aiusage.Account {
	accountsMu.Lock()
	f := loadAccountsFile()
	accountsMu.Unlock()

	out := make([]aiusage.Account, 0, len(f.Accounts)+2)
	for _, d := range agents.Registry {
		if !d.SupportsAccounts() {
			continue
		}
		active := activeAccount(f, d.ID)
		for _, a := range accountsForAgent(f, d.ID) {
			label := a.Label
			if a.ID == DefaultAccountID {
				label = "основной"
			}
			out = append(out, aiusage.Account{
				ID:       a.ID,
				Provider: d.ID,
				Label:    label,
				Dir:      d.AccountCredentialsDir(a.Dir),
				Active:   a.ID == active.ID,
			})
		}
	}
	return out
}
