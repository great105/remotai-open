// Package agents provides CLI agent descriptors, auto-discovery, and adapters.
package agents

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/envpath"
)

// AgentDescriptor describes a known CLI agent.
type AgentDescriptor struct {
	ID string `json:"id"`
	// NativeRoute is a managed agent surface in the single Remotai client.
	// It remains available before the optional runtime has been installed.
	NativeRoute    string   `json:"-"`
	Name           string   `json:"name"`
	Icon           string   `json:"icon"`
	Description    string   `json:"description"`
	CLINames       []string `json:"-"` // binary names to search in PATH
	RunArgs        []string `json:"-"` // args template: {prompt}, {session_id}
	ResumeArgs     []string `json:"-"` // nil = no resume support
	ExtraFlags     []string `json:"-"`
	CustomClass    string   `json:"-"` // "claude", "codex", "shell" for specialized adapters
	Install        string   `json:"-"` // interactive install command (POSIX/cross-platform)
	InstallWindows string   `json:"-"` // optional Windows-specific override
	ResumeCLI      string   `json:"-"` // interactive "continue latest conversation" command
	// ResumeIDCLI — интерактивное продолжение КОНКРЕТНОЙ беседы, {session_id}
	// подставляет клиент. Нужно усыплению (internal/pty/agent_sleep.go): в одной
	// папке у человека часто открыто несколько Claude, и `--continue` поднял бы
	// не ту беседу, а последнюю по времени. Пусто = усыплять этого агента нельзя.
	// Сверено живым `--help` 29.09.2026: `claude --resume <id>`,
	// `codex resume [SESSION_ID]`; оба принимают флаги режима из LaunchFlags.
	ResumeIDCLI string `json:"-"`

	// HistoryKey / HistoryLabel — КАК У ЭТОГО АГЕНТА ОТКРЫВАЕТСЯ ЕГО СОБСТВЕННАЯ
	// ИСТОРИЯ. Байты клавиши и её человеческое имя.
	//
	// Зачем в реестре. Режим «Авто» решал это ПРОБОЙ ФАКТОМ: шлём PgUp или отчёт
	// колеса и смотрим, ответил ли экран. Признак, который должен был отличать
	// агентов (переводов строки на килобайт вывода), развалился: в августе у
	// Codex было 19 против 0,5 у Claude, 06.09 — 1,9 против 0,4–3,1 (диапазоны
	// пересеклись), а живой лог 08.09 дал у Codex НОЛЬ переводов строки на
	// 264 КБ вывода. Проба же — гонка с лагом агента, и ложное «не ответил»
	// помнится шесть часов.
	//
	// А для Codex верного канала в лестнице не было ВООБЩЕ: он держит историю
	// сам и печатает об этом на экране — «Earlier messages are available —
	// press ctrl + t to view the full transcript» (скриншот владельца 08.09).
	// Ни колесо, ни PgUp этого не открывают, поэтому «Авто» честно приходил к
	// «листать нечем» и предлагал экспорт — при живой истории внутри агента.
	//
	// Пусто = мы НЕ ЗНАЕМ, чем этот агент открывает свою историю, и не гадаем:
	// остаётся прежняя проба фактом. Заполнено — знание доезжает до интерфейса
	// без правок клиента, как у `cli` и `LaunchFlags`.
	HistoryKey   string `json:"history_key,omitempty"`
	HistoryLabel string `json:"history_label,omitempty"`
	// HistoryChannel — ЧЕМ листается история этого агента: "page" (PgUp/PgDn),
	// "wheel" (отчёт колеса) или "transcript" (отдельный вид по HistoryKey).
	//
	// Это первый слой решения режима «Авто», и он важнее замера: замер
	// (переводов строки на килобайт) перестал разделять агентов, а проба фактом
	// стоит 1,2 с и врёт, когда агент в этот момент думал. Заполняем ТОЛЬКО по
	// доказательству — пусто значит «не знаем, проверяй пробой, как раньше».
	HistoryChannel string `json:"history_channel,omitempty"`
	// HistoryRetention — что делать со стиранием истории (CSI 3 J), которое
	// присылает этот CLI (ST-04, RetentionPolicy):
	//   "preserve" — выбрасывать: стирание приходит на перерисовке и уничтожает
	//                историю, которой у самого агента нет;
	//   "honor"    — исполнять у низа и откладывать, пока человек читает;
	//   ""         — не знаем, клиент решает по умолчанию.
	//
	// Это свойство ХРАНЕНИЯ, а не навигации: из DefaultScrollMode его не
	// выводить (I-02). Заполняем ТОЛЬКО по доказательству, с датой, как
	// HistoryChannel: неверное значение даёт либо 137 копий экрана, либо
	// потерянную историю.
	HistoryRetention string `json:"history_retention,omitempty"`

	// LaunchFlags — флаги, с которыми агента осмысленно запускать с телефона.
	// Живут здесь, а не в клиенте, по тому же правилу, что и `cli`: новый агент
	// в реестре появляется в интерфейсе без правок фронта.
	LaunchFlags []LaunchFlag `json:"-"`

	// ModelFlag — флаг, которым агенту говорят, КАКОЙ МОДЕЛЬЮ работать.
	//
	// Нужен ради OpenRouter: там у человека один ключ и четыреста моделей, и
	// выбор модели — это и есть выбор «мозга» агента. Пусто = мы не проверяли,
	// чем этот CLI принимает модель, и гадать не будем (то же правило, что у
	// LaunchFlags: несуществующий флаг превращает запуск в ошибку разбора, а
	// человек с телефона видит только «агент не запустился»).
	//
	// Проверено живым `--help` (opencode 1.18.15, WSL Ubuntu, 07.08.2026):
	// `-m, --model  model to use in the format of provider/model`.
	ModelFlag string `json:"-"`

	// AccountEnv — переменная окружения, которой задаётся, ПОД КАКИМ АККАУНТОМ
	// работает CLI. Пусто = у этого агента переключать нечего (или мы ещё не
	// проверили — гадать нельзя, то же правило, что у LaunchFlags).
	//
	// Смысл в том, что у CLI-агента аккаунт — это КАТАЛОГ, а не настройка:
	// внутри лежат его OAuth-креды. Значит второй аккаунт — просто второй
	// каталог, и переключение ничего не разлогинивает: оба живут одновременно.
	// Копировать токены между каталогами не нужно и НЕЛЬЗЯ.
	AccountEnv string `json:"-"`
	// AccountEnvKind — что именно кладут в эту переменную:
	//   "config_dir" — сам каталог конфига агента (claude, codex, kimi);
	//   "home"       — домашний каталог, свою папку CLI заводит ВНУТРИ (gemini).
	// От этого зависит, где искать креды профиля (см. AccountCredentialsDir).
	AccountEnvKind string `json:"-"`
	// AccountHomeSubdir — папка агента внутри домашнего каталога; имеет смысл
	// только при AccountEnvKind == "home".
	AccountHomeSubdir string `json:"-"`
	// ProxySchemes — схемы account-level proxy, для которых transport именно
	// этого CLI подтверждён. Пусто = прокси при запуске не подставляем и
	// сохранённую legacy-настройку считаем блокирующей. Матрица живёт в реестре,
	// а не в клиенте: новый агент не становится "поддержанным" по догадке UI.
	ProxySchemes []string `json:"-"`
	// DefaultScrollMode — куда по умолчанию направлять прокрутку терминала:
	//   "agent"    — во внутреннюю историю TUI (Claude Code);
	//   "terminal" — в scrollback xterm (Codex/Kimi);
	//   "auto"/""  — оставить автоматическое определение.
	//
	// Это свойство CLI, поэтому оно живёт в реестре, а не в клиентском switch:
	// новый агент не должен получать поведение по догадке интерфейса.
	DefaultScrollMode string `json:"-"`
	// AccountShared — что у нового аккаунта общее с основным.
	//
	// ЗАЧЕМ. Аккаунт — это каталог целиком, поэтому во втором профиле пусто:
	// ни скиллов, ни плагинов, ни настроек (замер на машине владельца:
	// skills 112 МБ, plugins 84 МБ, mcp-servers 73 МБ). Человек переключается
	// «на вторую подписку», а получает чистый Claude Code — и это выглядит как
	// поломка, хотя формально всё верно.
	//
	// Поэтому знания и настройки связываем с основным профилем, а вход и
	// история остаются своими. Список белый и КОРОТКИЙ: связываем только то,
	// про что точно известно, что там нет входа. У владельца в ~/.claude лежат
	// и его собственные папки (tools, jobs, tg_inbox) — трогать их мы не
	// вправе.
	AccountShared []SharedResource `json:"-"`

	// Filled at runtime by DetectAgents()
	DetectedPath string `json:"path"`
}

// LaunchFlag — один переключатель в шторке «Запустить агента».
//
// ВАЖНО: набор проверяется по живому `<cli> --help`, а не по памяти — ровно то
// же правило, что и с npm-именами пакетов (по памяти там однажды оказался
// несуществующий пакет). Флаг, которого у агента нет, превращает запуск в
// ошибку разбора аргументов, а человек с телефона видит только «агент не
// запустился».
type LaunchFlag struct {
	// Flag — что дописывается к команде (ровно как есть, через пробел).
	Flag string `json:"flag"`
	// Title — как называется на кнопке. Словами человека, не флагом.
	Title string `json:"title"`
	// Hint — одна строка «что произойдёт».
	Hint string `json:"hint,omitempty"`
	// Danger — флаг снимает подтверждения: агент начнёт менять файлы и запускать
	// команды без вопросов. Интерфейс обязан показать это иначе, чем остальные.
	Danger bool `json:"danger,omitempty"`
	// Default — предлагать включённым (ни у одного опасного не ставить).
	Default bool `json:"default,omitempty"`
	// Env — переменные окружения, без которых флаг на СЕРВЕРЕ не работает.
	// Дописываются как `env VAR=1 <cli> …` и только в POSIX-шелле: в PowerShell
	// такого синтаксиса нет вовсе.
	//
	// Ради чего заведено (замер на живом Linux под root, 04.08.2026):
	//   claude --dangerously-skip-permissions
	//   → «--dangerously-skip-permissions cannot be used with root/sudo
	//      privileges for security reasons», выход 1 — агент НЕ запускается.
	//   IS_SANDBOX=1 claude --dangerously-skip-permissions → проходит.
	// А сервер почти всегда root: у владельца все пять SSH-серверов — root@.
	// Без этого «Без подтверждений» на сервере просто не работал бы, и человек
	// видел бы одну строку отказа вместо агента.
	Env []string `json:"env,omitempty"`
	// With — флаги, которые обязаны идти вместе с этим. Замер там же: gemini
	// --yolo на новой папке пишет «Approval mode overridden to "default"
	// because the current folder is not trusted» — то есть МОЛЧА откатывается,
	// и человек думает, что подтверждения выключены, а они включены.
	With []string `json:"with,omitempty"`
	// Only — где флаг применим: "" везде, "posix" (Linux/mac/SSH-сервер),
	// "windows".
	Only string `json:"only,omitempty"`
}

// SharedResource — один общий ресурс профиля: папка знаний или файл настроек.
//
// Папку СВЯЗЫВАЕМ (junction на Windows, симлинк на POSIX): скиллы правят в
// одном месте, а работают они во всех аккаунтах сразу. Файл КОПИРУЕМ: связать
// его надёжно нельзя — редакторы и сами CLI пишут через «временный файл +
// переименование», и связь молча рвётся, оставляя человека с копией, которая
// больше не обновляется.
type SharedResource struct {
	// Name — путь внутри каталога аккаунта (обычно одно имя).
	Name string
	// Dir — папка (связываем) или файл (копируем).
	Dir bool
	// Title — как назвать человеку в отчёте «что стало общим».
	Title string
}

// detectMu guards DetectedPath: DetectAgents() writes it (incl. at runtime via
// POST /api/agents/rescan) while launch/availability paths read it concurrently.
var detectMu sync.RWMutex

// IsDetected returns true if the agent binary was found in PATH.
func (d *AgentDescriptor) IsDetected() bool {
	detectMu.RLock()
	defer detectMu.RUnlock()
	return d.DetectedPath != ""
}

// Path returns the detected binary path under the detect lock — safe to read
// concurrently with a rescan.
func (d *AgentDescriptor) Path() string {
	detectMu.RLock()
	defer detectMu.RUnlock()
	return d.DetectedPath
}

// SupportsResume returns true if the agent supports session resumption.
func (d *AgentDescriptor) SupportsResume() bool {
	return d.ResumeArgs != nil
}

// InstallCommand returns the command that can be typed into the current PTY.
func (d *AgentDescriptor) InstallCommand() string {
	if runtime.GOOS != "windows" {
		return d.Install
	}
	command := d.Install
	if d.InstallWindows != "" {
		command = d.InstallWindows
	}
	// npm also installs npm.ps1. PowerShell prefers it to npm.cmd and a
	// default Restricted policy blocks installation before npm even starts.
	if command == "npm" || strings.HasPrefix(command, "npm ") {
		return "npm.cmd" + strings.TrimPrefix(command, "npm")
	}
	return command
}

// CLIName returns the command to type in a terminal to start the agent
// interactively ("" for built-ins like shell/orchestrator, which have no CLI).
// Prefers the name that actually matched during detection — copilot, например,
// живёт то как `copilot`, то как `github-copilot-cli`.
func (d *AgentDescriptor) CLIName() string {
	if len(d.CLINames) == 0 {
		return ""
	}
	if p := d.Path(); p != "" && p != "built-in" {
		ext := filepath.Ext(p)
		base := strings.TrimSuffix(filepath.Base(p), ext)
		for _, n := range d.CLINames {
			if strings.EqualFold(base, n) {
				// Keep the executable found by detection. Stripping .cmd made
				// PowerShell choose the npm .ps1 shim instead, so "ready to run"
				// agents failed with PSSecurityException on a default Windows PC.
				if runtime.GOOS == "windows" {
					switch strings.ToLower(ext) {
					case ".cmd", ".exe", ".bat":
						return n + ext
					}
				}
				return n
			}
		}
	}
	return d.CLINames[0]
}

// ResumeCommand uses the same executable as a fresh local launch. Only the
// leading registry CLI name changes; subcommands and arguments stay intact.
func (d *AgentDescriptor) ResumeCommand() string {
	return d.localCommand(d.ResumeCLI)
}

// ResumeIDCommand — то же для продолжения беседы по номеру (ResumeIDCLI).
func (d *AgentDescriptor) ResumeIDCommand() string {
	return d.localCommand(d.ResumeIDCLI)
}

func (d *AgentDescriptor) localCommand(template string) string {
	if runtime.GOOS != "windows" {
		return template
	}
	command := strings.TrimSpace(template)
	words := strings.Fields(command)
	if len(words) > 0 {
		for _, name := range d.CLINames {
			if strings.EqualFold(words[0], name) {
				return d.CLIName() + command[len(words[0]):]
			}
		}
	}
	return template
}

// CanSleep — агента можно усыпить и поднять ту же беседу по номеру.
func (d *AgentDescriptor) CanSleep() bool {
	return d != nil && d.ResumeIDCLI != ""
}

// SleepFlags выбирает из командной строки спящего агента флаги режима, с
// которыми его надо поднять: без них Claude, запущенный с
// --dangerously-skip-permissions, проснулся бы и начал спрашивать разрешения.
// Берём ТОЛЬКО известные флаги из LaunchFlags — не всю строку: в ней бывает
// стартовый промпт (разбудить значило бы отправить задачу второй раз), наш
// --settings хуков (его клиент добавит сам) и --continue/--resume, которые с
// продолжением по номеру несовместимы.
func (d *AgentDescriptor) SleepFlags(argv []string) []string {
	if d == nil {
		return nil
	}
	var out []string
	for _, lf := range d.LaunchFlags {
		want := strings.Fields(lf.Flag)
		if len(want) == 0 || want[0] == "--continue" || want[0] == "--resume" {
			continue
		}
		for i := range argv {
			match := i+len(want) <= len(argv)
			for j := 0; match && j < len(want); j++ {
				match = argv[i+j] == want[j]
			}
			if !match && len(want) == 2 && argv[i] == want[0]+"="+want[1] {
				match = true
			}
			if match {
				out = append(out, lf.Flag)
				break
			}
		}
	}
	return out
}

// ToMap returns a JSON-friendly map for API responses.
func (d *AgentDescriptor) ToMap() map[string]any {
	return map[string]any{
		"id":              d.ID,
		"native_route":    d.NativeRoute,
		"name":            d.Name,
		"icon":            d.Icon,
		"description":     d.Description,
		"detected":        d.IsDetected(),
		"path":            d.Path(),
		"supports_resume": d.SupportsResume(),
		"resume_cli":      d.ResumeCommand(),
		"resume_id_cli":   d.ResumeIDCommand(),
		"install":         d.InstallCommand(),
		// install_posix — команда для СЕРВЕРА. `install` резолвится по ОС
		// компьютера-бастиона, поэтому с Windows-ПК в bash Linux-сервера уехало
		// бы `winget install …` (amazon-q) и `wsl sh -lc …` (cursor-agent).
		// Платформу самого SSH-сервера агент не знает вовсе, но POSIX там
		// подавляющее большинство — то же допущение, что у ряда команд.
		"install_posix": d.Install,
		// cli — что набрать в терминале, чтобы поднять агента интерактивно
		// (кнопка «Агент» в терминале). Пусто у встроенных.
		"cli": d.CLIName(),
		// cli_names — ВСЕ известные имена бинаря. На сервере проверять надо
		// каждое: `CLIName()` отдаёт имя, найденное на ПК, а если на ПК агента
		// нет — просто первое из списка. У copilot их два
		// (`github-copilot-cli` и `copilot`), поэтому человек читал «агент не
		// установлен на сервере», хотя бинарь там есть под другим именем.
		"cli_names": d.CLINames,
		// launch_flags — с чем его осмысленно запускать (см. LaunchFlag).
		// Всегда массив, пусть и пустой: по нему клиент отличает нового агента
		// от старого и не рисует пустой раздел.
		"launch_flags": d.flagsOrEmpty(),
		// account_env — переменная, которой выбирается аккаунт (см. AccountEnv).
		// Пусто = у агента аккаунты не переключаются, и клиент не показывает
		// строку выбора. Опять же по правилу «новый агент — без правок фронта».
		"account_env": d.AccountEnv,
		// model_flag — чем этому агенту задаётся модель. Пусто = не проверяли,
		// и клиент модель не подставляет вовсе.
		"model_flag": d.ModelFlag,
		// scroll_mode_default — начальный выбор видимого переключателя
		// «Авто / Вывод / Агент». Ручной выбор человека имеет приоритет до смены
		// foreground-процесса; это только канонический старт для данного CLI.
		"scroll_mode_default": d.ScrollModeDefault(),
	}
}

const (
	ScrollModeAuto     = "auto"
	ScrollModeTerminal = "terminal"
	ScrollModeAgent    = "agent"
)

// Допустимые значения HistoryRetention (контракт — registry_scroll_test.go).
const (
	HistoryRetentionHonor    = "honor"
	HistoryRetentionPreserve = "preserve"
)

// ScrollModeDefault returns a wire-safe default even for old/new descriptors
// whose field is empty or malformed.
func (d *AgentDescriptor) ScrollModeDefault() string {
	switch d.DefaultScrollMode {
	case ScrollModeTerminal, ScrollModeAgent:
		return d.DefaultScrollMode
	default:
		return ScrollModeAuto
	}
}

// SupportsAccounts — умеет ли агент несколько аккаунтов на одной машине.
func (d *AgentDescriptor) SupportsAccounts() bool { return d.AccountEnv != "" }

// AccountCredentialsDir возвращает каталог, где у профиля лежат его креды.
//
// Для "config_dir" это сам каталог профиля, для "home" — папка агента внутри
// него: gemini кладёт всё в `<GEMINI_CLI_HOME>/.gemini`, а не в корень.
func (d *AgentDescriptor) AccountCredentialsDir(profileDir string) string {
	if profileDir == "" {
		return ""
	}
	if d.AccountEnvKind == "home" && d.AccountHomeSubdir != "" {
		return filepath.Join(profileDir, d.AccountHomeSubdir)
	}
	return profileDir
}

func (d *AgentDescriptor) flagsOrEmpty() []LaunchFlag {
	if d.LaunchFlags == nil {
		return []LaunchFlag{}
	}
	return d.LaunchFlags
}

// Registry is the list of all known agents.
var Registry = []*AgentDescriptor{
	{
		ID: "hermes", Name: "Hermes", Icon: "H",
		Description: "Помощник с памятью, навыками и параллельными задачами",
		NativeRoute: "/hermes",
		// Installation and provider login are owned by the native runtime
		// manager. No shell flags are advertised before their live acceptance.
	},
	{
		ID: "claude", Name: "Claude Code", Icon: "\U0001F7E0",
		Description: "Anthropic Claude — потоковый Agent SDK",
		CLINames:    []string{"claude"},
		RunArgs:     []string{"-p", "{prompt}", "--output-format", "stream-json"},
		ResumeArgs:  []string{"--resume", "{session_id}", "-p", "{prompt}"},
		CustomClass: "claude",
		Install:     "npm i -g @anthropic-ai/claude-code",
		// PgUp/PgDn — документированный вендором канал истории Claude Code в
		// полноэкранном режиме, и он проверен отправкой в живую сессию
		// (build/qa/probe-agent-pageup-live.mjs: экран изменился).
		HistoryChannel: "page",
		ResumeCLI:      "claude --continue",
		ResumeIDCLI:    "claude --resume {session_id}",
		// Проверено ЗАПУСКОМ 04.08.2026, а не по докам: с CLAUDE_CONFIG_DIR на
		// пустой каталог `claude config ls` отвечает «Not logged in · Please run
		// /login» и заводит там свою структуру (.claude.json, projects, sessions).
		// То есть каталог и есть аккаунт.
		AccountEnv:        "CLAUDE_CONFIG_DIR",
		AccountEnvKind:    "config_dir",
		ProxySchemes:      []string{"http", "https"},
		DefaultScrollMode: ScrollModeAgent,
		// Сверено со структурой живого ~/.claude (05.08.2026). Личное сюда не
		// попадает намеренно: `.credentials.json` — вход, `projects`/`sessions`/
		// `history.jsonl` — переписки, `.claude.json` — вход + история проектов
		// в одном файле (поэтому MCP из него переносим копией секции, см.
		// api_accounts.go).
		AccountShared: []SharedResource{
			{Name: "skills", Dir: true, Title: "скиллы"},
			{Name: "plugins", Dir: true, Title: "плагины"},
			{Name: "commands", Dir: true, Title: "свои команды"},
			{Name: "agents", Dir: true, Title: "субагенты"},
			{Name: "hooks", Dir: true, Title: "хуки"},
			{Name: "settings.json", Title: "настройки"},
			{Name: "CLAUDE.md", Title: "общая память"},
		},
		// Сверено с `claude --help` и живым запуском на Linux под root 04.08.2026.
		// Полный список режимов разрешений взят из ответа самого claude на
		// неверное значение: acceptEdits, auto, bypassPermissions, manual,
		// dontAsk, plan.
		LaunchFlags: []LaunchFlag{
			{Flag: "--dangerously-skip-permissions", Title: "Без подтверждений",
				Hint:   "Правит файлы и запускает команды, ничего не спрашивая",
				Danger: true, Env: []string{"IS_SANDBOX=1"}},
			{Flag: "--permission-mode acceptEdits", Title: "Правки без спроса",
				Hint: "Файлы правит сам, команды спрашивает", Danger: true},
			{Flag: "--permission-mode plan", Title: "Режим плана", Hint: "Сначала план, менять ничего не будет"},
			{Flag: "--continue", Title: "Продолжить", Hint: "Последний разговор в этой папке"},
			{Flag: "--verbose", Title: "Подробно", Hint: "Больше видно, что он делает"},
		},
	},
	{
		ID: "codex", Name: "Codex CLI", Icon: "\U0001F7E2",
		Description: "OpenAI Codex — кодогенерация",
		CLINames:    []string{"codex"},
		RunArgs:     []string{"exec", "{prompt}"},
		ResumeArgs:  []string{"exec", "resume", "{session_id}", "{prompt}"},
		CustomClass: "codex",
		Install:     "npm i -g @openai/codex",
		ResumeCLI:   "codex resume --last",
		ResumeIDCLI: "codex resume {session_id}",
		// Codex сам печатает это на экране: «Earlier messages are available —
		// press ctrl + t to view the full transcript». 0x14 = Ctrl+T.
		// Канал именно "transcript", а не "page": это переход в отдельный вид,
		// и молча по жесту его посылать нельзя — интерфейс обязан спросить.
		HistoryKey:     "\x14",
		HistoryLabel:   "Ctrl+T",
		HistoryChannel: "transcript",
		// Доказательство (v2.57.23, 15.08.2026, build/qa/probe-multiviewer-
		// history-live.mjs:4-8): «while a Codex terminal was at bottom in
		// explicit Output mode, a phone rotation resized the shared PTY and the
		// resulting `CSI 2J, CSI 3J, CSI H` repaint erased another viewer's rich
		// xterm history». Историю Codex держит сам (Ctrl+T), его 3J — перерисовка,
		// а не просьба человека стереть прочитанное.
		HistoryRetention: HistoryRetentionPreserve,
		// `codex --help` (04.08.2026) сам называет переменную: «-p, --profile
		// … Layer $CODEX_HOME/<name>.config.toml on top of the base user
		// config». Аккаунт (auth.json) лежит там же.
		AccountEnv:        "CODEX_HOME",
		AccountEnvKind:    "config_dir",
		ProxySchemes:      []string{"http", "https"},
		DefaultScrollMode: ScrollModeTerminal,
		// Сверено со структурой живого ~/.codex (05.08.2026). `auth.json`,
		// `sessions`, `history.jsonl` и базы sqlite остаются своими.
		// `config.toml` копируем: в нём настройки, но там же может оказаться
		// ключ API — значит это копия, которую человек правит сам, а не связь.
		AccountShared: []SharedResource{
			{Name: "skills", Dir: true, Title: "скиллы"},
			{Name: "rules", Dir: true, Title: "правила"},
			{Name: "plugins", Dir: true, Title: "плагины"},
			{Name: "AGENTS.md", Title: "общая память"},
		},
		// Сверено с `codex --help` 04.08.2026.
		LaunchFlags: []LaunchFlag{
			{Flag: "--dangerously-bypass-approvals-and-sandbox", Title: "Без подтверждений",
				Hint: "Ни подтверждений, ни песочницы", Danger: true},
			{Flag: "--ask-for-approval never", Title: "Не спрашивать",
				Hint: "Одобрения не запрашивает, песочница остаётся", Danger: true},
			{Flag: "--search", Title: "С поиском", Hint: "Разрешить поиск в интернете"},
		},
	},
	{
		ID: "aider", Name: "Aider", Icon: "\U0001F535",
		Description: "AI pair programming в терминале",
		CLINames:    []string{"aider"},
		RunArgs:     []string{"--message", "{prompt}", "--yes"},
		Install:     "pip install aider-chat",
	},
	{
		ID: "gemini", Name: "Gemini CLI", Icon: "\U0001F537",
		Description: "Google Gemini CLI агент",
		CLINames:    []string{"gemini"},
		// Сверено с `gemini --help` и живым запуском на Linux под root 04.08.2026.
		LaunchFlags: []LaunchFlag{
			{Flag: "--yolo", Title: "Без подтверждений",
				Hint: "Принимает все действия сам", Danger: true,
				With: []string{"--skip-trust"}},
			{Flag: "--skip-trust", Title: "Доверять папке",
				Hint: "Без этого на новой папке подтверждения молча включаются обратно"},
			{Flag: "--approval-mode auto_edit", Title: "Правки без спроса",
				Hint: "Файлы правит сам, остальное спрашивает", Danger: true,
				With: []string{"--skip-trust"}},
			{Flag: "--approval-mode plan", Title: "Режим плана", Hint: "Только читает и планирует"},
			{Flag: "--sandbox", Title: "В песочнице", Hint: "Запускать изолированно"},
		},
		RunArgs: []string{"-p", "{prompt}"},
		Install: "npm i -g @google/gemini-cli",
		// У gemini каталог конфига переменной НЕ задаётся — он всегда
		// `<домашний>/.gemini` (gemini-cli-core/utils/paths.js). Зато сам
		// домашний каталог переопределяется: «If GEMINI_CLI_HOME environment
		// variable is set, it returns its value» — там же, функция homedir().
		// Поэтому профиль здесь — домашний каталог, а креды внутри, в `.gemini`.
		AccountEnv:        "GEMINI_CLI_HOME",
		AccountEnvKind:    "home",
		AccountHomeSubdir: ".gemini",
		// Сверено со структурой живого ~/.gemini (05.08.2026): папок со
		// скиллами там нет вовсе, общего — только память и настройки.
		// `oauth_creds.json`, `google_accounts.json`, `projects.json` и история
		// остаются своими.
		AccountShared: []SharedResource{
			{Name: "GEMINI.md", Title: "общая память"},
			{Name: "settings.json", Title: "настройки"},
		},
	},
	{
		ID: "amazon-q", Name: "Amazon Q", Icon: "\U0001F7E1",
		Description:    "Amazon Q Developer CLI",
		CLINames:       []string{"q"},
		RunArgs:        []string{"chat", "--trust-all-tools", "{prompt}"},
		Install:        `curl --proto '=https' --tlsv1.2 -fsS https://desktop-release.q.us-east-1.amazonaws.com/latest/q-x86_64-linux.zip -o q.zip && unzip -o q.zip && ./q/install.sh`,
		InstallWindows: "winget install -e --id Amazon.AmazonQ",
	},
	{
		ID: "copilot", Name: "GitHub Copilot", Icon: "\u26AB",
		Description: "GitHub Copilot в терминале",
		CLINames:    []string{"github-copilot-cli", "copilot"},
		RunArgs:     []string{"--plan", "{prompt}"},
		Install:     "npm i -g @github/copilot",
	},
	{
		ID: "opencode", Name: "OpenCode", Icon: "\U0001F7E3",
		Description: "Open-source coding agent",
		CLINames:    []string{"opencode"},
		RunArgs:     []string{"run", "{prompt}"},
		Install:     "npm i -g opencode-ai",
		// Флаги и формат модели сверены с ЖИВЫМ `--help` версии 1.18.15 на
		// стенде (WSL Ubuntu, 07.08.2026), а не взяты из документации.
		// Там же проверено главное: одной переменной OPENROUTER_API_KEY хватает,
		// чтобы agent увидел провайдера и выполнил задачу неинтерактивно —
		// интерактивный `/connect` не нужен (см. internal/openrouter/store.go).
		ModelFlag: "-m",
		LaunchFlags: []LaunchFlag{
			{
				Flag:   "--auto",
				Title:  "Без подтверждений",
				Hint:   "Агент будет менять файлы и запускать команды, ничего не спрашивая",
				Danger: true,
			},
			{
				Flag:  "-c",
				Title: "Продолжить",
				Hint:  "Вернуться к прошлому разговору вместо нового",
			},
		},
	},
	{
		ID: "cline", Name: "Cline CLI", Icon: "\U0001F7E4",
		Description: "Cline (ex-Continue) CLI агент",
		CLINames:    []string{"cline"},
		RunArgs:     []string{"{prompt}", "-y"},
		Install:     "npm i -g cline",
	},
	{
		ID: "kilo", Name: "Kilo Code", Icon: "\U0001F534",
		Description: "Kilo Code CLI агент",
		CLINames:    []string{"kilo"},
		RunArgs:     []string{"--auto", "{prompt}"},
		ResumeArgs:  []string{"--continue", "--auto", "{prompt}"},
		Install:     "npm i -g kilocode",
		ResumeCLI:   "kilo --continue",
	},
	{
		ID: "cursor-agent", Name: "Cursor Agent", Icon: "\u2B1B",
		Description:    "Cursor IDE agent в терминале",
		CLINames:       []string{"cursor-agent"},
		RunArgs:        []string{"-p", "{prompt}"},
		ResumeArgs:     []string{"--resume={session_id}", "-p", "{prompt}"},
		Install:        "curl https://cursor.com/install -fsS | bash",
		InstallWindows: `wsl sh -lc "curl https://cursor.com/install -fsS | bash"`,
		ResumeCLI:      "cursor-agent resume",
	},
	{
		ID: "kimi", Name: "Kimi Code", Icon: "\U0001F319",
		Description: "Moonshot Kimi — кодовый агент в терминале",
		CLINames:    []string{"kimi"},
		// В -p режиме kimi авто-аппрувит инструменты сам; --auto с -p несовместим (проверено на 0.27.0).
		RunArgs:    []string{"-p", "{prompt}"},
		ResumeArgs: []string{"-S", "{session_id}", "-p", "{prompt}"},
		Install:    "npm i -g @moonshot-ai/kimi-code",
		// Из кода самого CLI (dist/main.mjs): `resolveKimiHome = homeDir ??
		// process.env["KIMI_CODE_HOME"] ?? join(homedir(), ".kimi-code")`.
		AccountEnv:        "KIMI_CODE_HOME",
		AccountEnvKind:    "config_dir",
		DefaultScrollMode: ScrollModeTerminal,
		// Общих ресурсов НЕТ намеренно: единственное, что похоже на настройки,
		// — `config.toml`, но там же живёт вход (KIMI_API_KEY и профиль
		// модели). Скопировать его значило бы перенести чужой вход в новый
		// аккаунт — ровно то, чего человек не просил. Появится отдельная папка
		// знаний — впишем сюда.
		AccountShared: nil,
		// Сверено с `kimi --help` 04.08.2026.
		LaunchFlags: []LaunchFlag{
			{Flag: "--auto", Title: "Полная автономия",
				Hint: "Не задаёт вопросов вообще", Danger: true},
			{Flag: "--yolo", Title: "Без подтверждений",
				Hint: "Инструменты запускает сам, вопросы задавать может", Danger: true},
			{Flag: "--continue", Title: "Продолжить", Hint: "Последняя сессия этой папки"},
			{Flag: "--plan", Title: "Режим плана", Hint: "Сначала план"},
		},
	},
	{
		ID: "grok", Name: "Grok", Icon: "✖️",
		Description: "xAI Grok — кодовый агент в терминале",
		CLINames:    []string{"grok"},
		// Пакет проверен в реестре npm, а не по памяти: издатель `security@x.ai`,
		// версия 1.0.4 от 13.08.2026. Community-форков с похожим именем несколько
		// (`@vibe-kit/grok-cli`, `@spikewang/grok-cli`) — берём официальный.
		Install: "npm i -g @xai-official/grok",
		// Всё ниже сверено с ЖИВЫМ `grok --help` версии 1.0.4 (Windows, 16.08.2026)
		// и живым разбором аргументов, а не с документацией.
		//
		// `-p, --single <PROMPT>  Single-turn prompt. Prints the response to
		// stdout and exits` — это и есть headless-запуск.
		RunArgs: []string{"-p", "{prompt}"},
		// `-r, --resume [<SESSION_ID_OR_TITLE>]` — значение НЕОБЯЗАТЕЛЬНОЕ.
		// Живьём разобрались обе формы, но пишем через `=`: у флага с
		// необязательным значением это единственная форма, которая не зависит от
		// того, что стоит следующим токеном. Пробел развязывает разбор только
		// пока дальше не флаг — а перестановка аргументов молча превратила бы
		// resume в «продолжить последнюю сессию» и увела человека в чужой разговор.
		ResumeArgs: []string{"--resume={session_id}", "-p", "{prompt}"},
		ResumeCLI:  "grok --continue",
		// `-m, --model <MODEL>  Model ID to use`.
		ModelFlag: "-m",
		// Каталог аккаунта назван прямо в трамплине npm-пакета (`bin/grok`):
		// «$GROK_HOME/bin … matching the Rust grok_home()», по умолчанию
		// `~/.grok`. Проверено ЗАПУСКОМ с подменённым GROK_HOME: в каталоге
		// заводятся config.toml, agent_id, sessions/ и logs/ — то есть переменная
		// указывает на сам каталог конфига, а не на домашний (в отличие от gemini).
		//
		// ⚠ ЦЕНА ПЕРЕКЛЮЧЕНИЯ, замерено там же: трамплин материализует бинарь в
		// `$GROK_HOME/bin`, и на Windows это КОПИЯ, а не симлинк — 142 МБ дважды
		// (`grok-1.0.4.exe` и `grok.exe`), то есть ~284 МБ на каждый заведённый
		// аккаунт. Функция от этого работает, но место уходит незаметно.
		AccountEnv:     "GROK_HOME",
		AccountEnvKind: "config_dir",
		// Список общих ресурсов взят из доков, которые сам CLI кладёт в
		// `$GROK_HOME/docs/user-guide/`, и намеренно КОРОТКИЙ.
		//
		// Вход у grok живёт в отдельном `auth.json` — он, `sessions/` и `logs/`
		// остаются своими. Снаружи списка оставлены ещё двое, и это решение, а не
		// недосмотр: `config.toml` — потому что там же настраиваются свои модели
		// (`11-custom-models.md`), а значит может оказаться ключ; `memory/` —
		// потому что кросс-сессионная память набирается из переписок, то есть это
		// история, а не знания.
		AccountShared: []SharedResource{
			{Name: "skills", Dir: true, Title: "скиллы"},
			{Name: "plugins", Dir: true, Title: "плагины"},
			{Name: "commands", Dir: true, Title: "свои команды"},
			{Name: "agents", Dir: true, Title: "субагенты"},
			{Name: "workflows", Dir: true, Title: "сценарии"},
		},
		// Разбор каждого флага проверен живым запуском: все четыре дошли до
		// «Not signed in», то есть аргументы разобрались, а не упали.
		// `--permission-mode` принимает default, acceptEdits, auto, dontAsk,
		// bypassPermissions, plan — список взят из самого `--help`.
		LaunchFlags: []LaunchFlag{
			{Flag: "--always-approve", Title: "Без подтверждений",
				Hint: "Запускает инструменты сам, ничего не спрашивая", Danger: true},
			{Flag: "--permission-mode acceptEdits", Title: "Правки без спроса",
				Hint: "Файлы правит сам, команды спрашивает", Danger: true},
			{Flag: "--permission-mode plan", Title: "Режим плана", Hint: "Сначала план, менять ничего не будет"},
			{Flag: "--continue", Title: "Продолжить", Hint: "Последний разговор в этой папке"},
		},
		// DefaultScrollMode намеренно ПУСТОЙ (= auto). У grok свой TUI с
		// alt-screen (`--no-alt-screen`, `--fullscreen`, `--minimal`), и по этому
		// признаку он похож на Claude Code, но замера прокрутки на живой сессии
		// не было. По правилу зоны непроверенное поведение не прописываем: агент,
		// получивший режим по догадке, листает не туда, и человек с телефона
		// видит пустоту вместо истории.
	},
	{
		ID: "shell", Name: "Shell", Icon: "\U0001F41A",
		Description: "Команды оболочки (cmd/bash)",
		CLINames:    nil, // always available
		CustomClass: "shell",
	},
	{
		ID: "orchestrator", Name: "Orchestrator", Icon: "\U0001F3AF",
		Description: "Автономный оркестратор — координирует AI-агентов",
		CLINames:    nil,
		CustomClass: "orchestrator",
	},
	{
		ID: "researcher", Name: "Researcher", Icon: "\U0001F52C",
		Description: "Автономные эксперименты — оптимизация метрики (autoresearch)",
		CLINames:    nil,
		CustomClass: "researcher",
	},
}

// registryMap provides O(1) lookup by agent ID.
var registryMap map[string]*AgentDescriptor

func init() {
	registryMap = make(map[string]*AgentDescriptor, len(Registry))
	for _, d := range Registry {
		registryMap[d.ID] = d
	}
}

// ── Detection ────────────────────────────────────────────────────────

var detectOnce sync.Once

// Где findCLI ищет МИМО PATH. Переменные, а не литералы, только ради проб:
// поведение продукта то же, но проба с подменённым PATH иначе находит агента,
// установленного на самой машине стенда (WSL Ubuntu: opencode в
// /usr/local/bin — TestAgentSurvivesNpmRetireWindow падал и на базе ae70c36).
var (
	// windowsProfilesRoot — каталог профилей, где служба под LocalSystem ищет
	// папки npm пользователей (%APPDATA%\npm).
	windowsProfilesRoot = `C:\Users`
	// unixUserBinDirs — пользовательские и системные каталоги, которых нет в
	// урезанном PATH службы systemd.
	unixUserBinDirs = envpath.UserBinDirs
)

func findCLI(name string) string {
	path, err := exec.LookPath(name)
	if err == nil {
		return path
	}
	if runtime.GOOS == "windows" {
		for _, ext := range []string{".cmd", ".exe", ".bat"} {
			if p, err := exec.LookPath(name + ext); err == nil {
				return p
			}
		}
		// Windows Service running as LocalSystem doesn't inherit user PATH,
		// so npm global binaries in %APPDATA%\npm aren't visible. Scan all
		// user profiles for the binary.
		if entries, err := os.ReadDir(windowsProfilesRoot); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				lower := entry.Name()
				if lower == "Public" || lower == "Default" || lower == "Default User" || lower == "All Users" {
					continue
				}
				for _, ext := range []string{".cmd", ".exe", ".bat"} {
					candidate := filepath.Join(windowsProfilesRoot, entry.Name(), "AppData", "Roaming", "npm", name+ext)
					if _, err := os.Stat(candidate); err == nil {
						return candidate
					}
				}
			}
		}
		return ""
	}
	// Linux/macOS: a systemd service inherits a minimal PATH that omits the
	// user-local tool dirs where CLI agents live (~/.local/bin, npm-global,
	// nvm, …). Scan them directly — same set buildLinuxPTYEnv() prepends.
	for _, dir := range unixUserBinDirs("") {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// DetectAgents scans PATH for all known agents.
func DetectAgents() {
	cfg := config.Get()
	detectMu.Lock()
	for _, desc := range Registry {
		if desc.CLINames == nil {
			desc.DetectedPath = "built-in"
			continue
		}
		desc.DetectedPath = ""
		for _, cliName := range desc.CLINames {
			if path := findCLI(cliName); path != "" {
				desc.DetectedPath = path
				log.Printf("Detected %s: %s", desc.Name, path)
				break
			}
		}
		// Fallback to explicit paths from config.json (set by setup wizard).
		if desc.DetectedPath == "" {
			var cp string
			switch desc.ID {
			case "claude":
				cp = cfg.ClaudePath
			case "codex":
				cp = cfg.CodexPath
			}
			// Skip if it's just the bare CLI name (means PATH search was intended).
			if cp != "" && filepath.IsAbs(cp) {
				if _, err := os.Stat(cp); err == nil {
					desc.DetectedPath = cp
					log.Printf("Detected %s from config: %s", desc.Name, cp)
				}
			}
		}
		// Кого нашли хоть раз — того потом перепроверяем даром (RefreshMissing).
		if desc.DetectedPath != "" {
			everFound[desc.ID] = true
		}
	}
	detectMu.Unlock()
	detected := GetDetected()
	ids := make([]string, len(detected))
	for i, d := range detected {
		ids[i] = d.ID
	}
	log.Printf("Agent detection: %d/%d available: %s",
		len(detected), len(Registry), joinStrings(ids))
}

// DetectOnce runs DetectAgents exactly once.
func DetectOnce() {
	detectOnce.Do(DetectAgents)
}

// missingRecheckTTL — сколько верить приговору «агент ПРОПАЛ».
//
// Память о находке асимметрична, как и у пробы прокрутки в терминале. «Есть» —
// факт: путь к бинарю никто не отзывает. «Пропал» — снимок одного мгновения, и
// мгновение бывает ровно то, когда агент обновляет сам себя: npm на время
// установки ПЕРЕИМЕНОВЫВАЕТ свои обёртки, а новые пишет в конце («reify mark
// retired» в его собственном журнале: `claude.cmd` → `.claude.cmd-2nQ89reZ`).
// Claude Code обновляется сам и без спроса — 09.09.2026 шесть установок за
// двадцать минут. Кто спросил список в эту секунду, получал «не установлен» и
// держал этот ответ до перезапуска Remotai или ручного «перепроверить»:
// в журнале агента владельца 07:45:26 «7/15 available» без claude при живом
// claude 2.1.266 в %APPDATA%\npm, и на телефоне он был «не установлен» ещё час.
const missingRecheckTTL = 10 * time.Second

var (
	missingRecheckMu sync.Mutex
	missingRecheckAt time.Time
	// everFound — кого этот процесс уже находил. Под detectMu, как DetectedPath.
	everFound = map[string]bool{}
)

// RefreshMissing перепроверяет тех, кто БЫЛ НАЙДЕН И ПРОПАЛ, не чаще раза в
// missingRecheckTTL.
//
// Почему не всех ненайденных: замер 09.09.2026 на 65 каталогах PATH — промах
// стоит 35 мс (полный обход × пять расширений плюс проход по профилям
// пользователей), попадание 2 мс. Полный обход реестра — 285 мс на КАЖДЫЙ
// список агентов, то есть плата за чужие непоставленные CLI. Агент, которого
// этот процесс не видел ни разу, «не установлен» честно: там кнопка «Установить»
// и ручное «перепроверить». А вот исчезнувший на секунды своего обновления —
// это ложь интерфейса, и она чинится даром.
//
// Найденных не трогаем вовсе: у них есть путь, и переискивать его — значит
// уметь потерять рабочего агента на ровном месте.
func RefreshMissing() {
	now := time.Now()
	missingRecheckMu.Lock()
	if !missingRecheckAt.IsZero() && now.Sub(missingRecheckAt) < missingRecheckTTL {
		missingRecheckMu.Unlock()
		return
	}
	missingRecheckAt = now
	missingRecheckMu.Unlock()
	for _, id := range refreshMissingIn(Registry, findCLI) {
		log.Printf("Agent detection: %s вернулся без перезапуска", id)
	}
}

// refreshMissingIn — само правило, без часов и без глобального реестра: искать
// заново там, где агента уже видели, а сейчас пусто; возвращать тех, кто нашёлся.
func refreshMissingIn(list []*AgentDescriptor, look func(string) string) []string {
	var found []string
	detectMu.Lock()
	defer detectMu.Unlock()
	for _, desc := range list {
		if desc.CLINames == nil || desc.DetectedPath != "" || !everFound[desc.ID] {
			continue
		}
		for _, cliName := range desc.CLINames {
			if path := look(cliName); path != "" {
				desc.DetectedPath = path
				found = append(found, desc.ID)
				break
			}
		}
	}
	return found
}

// GetDetected returns only agents found on the system.
func GetDetected() []*AgentDescriptor {
	var result []*AgentDescriptor
	for _, d := range Registry {
		if d.IsDetected() {
			result = append(result, d)
		}
	}
	return result
}

// GetDescriptor returns a descriptor by ID, or nil.
func GetDescriptor(id string) *AgentDescriptor {
	return registryMap[id]
}

// IsAvailable returns true if an agent is detected and ready.
func IsAvailable(id string) bool {
	d := registryMap[id]
	return d != nil && d.IsDetected()
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}
