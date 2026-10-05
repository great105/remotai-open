// Package mcpmgr — MCP-серверы агентов без правки JSON руками.
//
// ЗАЧЕМ. Подключить агенту MCP-сервер (браузер, базу, трекер задач) сегодня
// значит найти `~/.claude.json` или `~/.codex/config.toml` и дописать туда
// кусок в формате, который у каждого CLI свой. Для владельца без технического
// фона это стена. Здесь то же самое делается формой: тип, имя, команда или
// адрес, переменные.
//
// ГЛАВНОЕ ПРАВИЛО: ЗАПИСЬ — ТОЛЬКО ЧЕРЕЗ CLI САМОГО АГЕНТА. Формат конфига
// принадлежит агенту и меняется от версии к версии; `claude mcp add-json` и
// `codex mcp add` знают его, а мы — нет. Читаем же тем способом, который
// надёжнее и не имеет побочных эффектов:
//   - Claude — разбором `.claude.json` (только чтение): `claude mcp list`
//     проверяет здоровье КАЖДОГО сервера, то есть запускает их все, и это
//     секунды плюс чужие процессы на каждый заход в экран;
//   - Codex — `codex mcp list --json`: у него есть машинный вывод, а TOML мы
//     разбирать не умеем и тащить парсер ради чтения не стоит.
//
// Аккаунт — это КАТАЛОГ (CLAUDE_CONFIG_DIR, CODEX_HOME; см. agents.AccountEnv).
// Операция для выбранного аккаунта запускает CLI с его переменной, и сервер
// ложится в конфиг именно этого аккаунта.
//
// Флаги сверены ЗАПУСКОМ (29.09.2026, claude 2.1.284, codex-cli 0.158.0), см.
// Контекст/Журнал/2026-09-29_mcp-bez-json.md.
package mcpmgr

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Типы подключения, как их называет интерфейс.
const (
	TypeStdio = "stdio"
	TypeHTTP  = "http"
	TypeSSE   = "sse"
)

// Масочная замена значения секрета. Одна на весь пакет, чтобы тесты и клиент
// узнавали её одинаково.
const Mask = "••••"

// Caps — что умеет CLI конкретного агента. Отдаётся клиенту: форма показывает
// только то, что агент примет, а не то, что мы угадали.
type Caps struct {
	Types   []string `json:"types"`
	Headers bool     `json:"headers"`
}

// Supported — агенты, у которых команды MCP проверены живым `--help`.
//
// Codex: `codex mcp add` умеет только `-- <команда>` и `--url` (streamable
// HTTP) — SSE у него нет вовсе, а заголовки CLI не задаёт (есть лишь
// `--bearer-token-env-var`). Предлагать их в форме — значит обещать то, что
// запишется молча не так.
var Supported = map[string]Caps{
	"claude": {Types: []string{TypeStdio, TypeHTTP, TypeSSE}, Headers: true},
	"codex":  {Types: []string{TypeStdio, TypeHTTP}, Headers: false},
}

// SupportedIDs — порядок показа.
var SupportedIDs = []string{"claude", "codex"}

// Server — сервер, как его видит клиент. Значений env и заголовков здесь нет
// НИКОГДА: только имена. Аргументы и адрес — с замаскированными секретами.
type Server struct {
	Name       string   `json:"name"`
	Agent      string   `json:"agent"`
	Scope      string   `json:"scope"` // "user" — общий для аккаунта, "project" — одной папки
	Project    string   `json:"project,omitempty"`
	Type       string   `json:"type"`
	Command    string   `json:"command,omitempty"`
	Args       []string `json:"args,omitempty"`
	URL        string   `json:"url,omitempty"`
	EnvKeys    []string `json:"env_keys,omitempty"`
	HeaderKeys []string `json:"header_keys,omitempty"`
	Enabled    bool     `json:"enabled"`
	// ReadOnly — менять отсюда нельзя (проектный сервер, выключенный самим
	// агентом). CanToggle — можно ли выключить без потери настроек.
	ReadOnly  bool   `json:"read_only,omitempty"`
	CanToggle bool   `json:"can_toggle"`
	Note      string `json:"note,omitempty"`
}

// Spec — то, что человек ввёл в форму.
type Spec struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Ошибки, по которым веб-слой выбирает код ответа.
var (
	ErrInvalid     = errors.New("invalid")
	ErrExists      = errors.New("exists")
	ErrNotFound    = errors.New("not found")
	ErrUnsupported = errors.New("unsupported")
	ErrReadOnly    = errors.New("read only")
)

// UserError — ошибка с текстом для человека. Kind — один из Err* выше.
type UserError struct {
	Kind error
	Msg  string
}

func (e *UserError) Error() string { return e.Msg }
func (e *UserError) Unwrap() error { return e.Kind }

func userErr(kind error, format string, a ...any) error {
	return &UserError{Kind: kind, Msg: fmt.Sprintf(format, a...)}
}

// Имя сервера. Claude принимает только буквы, цифры, «-» и «_» (живой ответ:
// «Names can only contain letters, numbers, hyphens, and underscores»), Codex —
// ещё `: @ / .`. Берём пересечение: одно имя должно подойти обоим агентам,
// иначе «добавить в оба» ломалось бы на втором.
// Первый символ — не «-»: имя `-h` ушло бы в CLI флагом, тот напечатал бы
// справку с кодом 0, и сервер ответил бы «добавлено» (скептик 29.09).
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$`)

// Имя переменной окружения — как у POSIX-шелла: иначе агент либо откажет,
// либо положит ключ, который никто не прочтёт.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Имя заголовка — token из RFC 9110 без экзотики.
var headerKeyRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,128}$`)

const (
	maxValueLen = 4096
	maxItems    = 64
)

// ValidName проверяет имя сервера.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return userErr(ErrInvalid, "Имя — латинские буквы, цифры, «-» и «_», без пробелов, до 64 знаков")
	}
	return nil
}

func badValue(s string) bool {
	return len(s) > maxValueLen || strings.ContainsAny(s, "\x00\r\n")
}

// Normalize приводит ввод к виду, который пойдёт в CLI, и проверяет его.
// Пустые строки аргументов выкидываются (в форме это просто лишняя строка).
func (s Spec) Normalize() (Spec, error) {
	s.Name = strings.TrimSpace(s.Name)
	s.Type = strings.ToLower(strings.TrimSpace(s.Type))
	if s.Type == "" {
		s.Type = TypeStdio
	}
	if err := ValidName(s.Name); err != nil {
		return s, err
	}
	switch s.Type {
	case TypeStdio:
		s.Command = strings.TrimSpace(s.Command)
		if s.Command == "" {
			return s, userErr(ErrInvalid, "Укажите команду запуска, например npx")
		}
		if badValue(s.Command) {
			return s, userErr(ErrInvalid, "Команда не должна содержать переводов строки")
		}
		args := make([]string, 0, len(s.Args))
		for _, a := range s.Args {
			if strings.TrimSpace(a) == "" {
				continue
			}
			if badValue(a) {
				return s, userErr(ErrInvalid, "Аргумент не должен содержать переводов строки")
			}
			args = append(args, a)
		}
		if len(args) > maxItems {
			return s, userErr(ErrInvalid, "Слишком много аргументов")
		}
		s.Args = args
		s.URL = ""
		if len(s.Headers) > 0 {
			return s, userErr(ErrInvalid, "Заголовки бывают только у серверов по адресу")
		}
		s.Headers = nil
	case TypeHTTP, TypeSSE:
		s.URL = strings.TrimSpace(s.URL)
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || badValue(s.URL) {
			return s, userErr(ErrInvalid, "Адрес должен начинаться с https:// или http://")
		}
		if u.User != nil {
			return s, userErr(ErrInvalid, "Логин и пароль в адресе не поддерживаются — передайте их заголовком")
		}
		s.Command = ""
		s.Args = nil
		if len(s.Env) > 0 {
			return s, userErr(ErrInvalid, "Переменные окружения бывают только у серверов-программ")
		}
		s.Env = nil
		if len(s.Headers) > maxItems {
			return s, userErr(ErrInvalid, "Слишком много заголовков")
		}
		for k, v := range s.Headers {
			if !headerKeyRe.MatchString(k) {
				return s, userErr(ErrInvalid, "Недопустимое имя заголовка «%s»", k)
			}
			if badValue(v) {
				return s, userErr(ErrInvalid, "Значение заголовка «%s» не должно содержать переводов строки", k)
			}
		}
	default:
		return s, userErr(ErrInvalid, "Неизвестный тип подключения")
	}
	if len(s.Env) > maxItems {
		return s, userErr(ErrInvalid, "Слишком много переменных")
	}
	for k, v := range s.Env {
		if !envKeyRe.MatchString(k) {
			return s, userErr(ErrInvalid, "Недопустимое имя переменной «%s»: латиница, цифры и «_»", k)
		}
		if badValue(v) {
			return s, userErr(ErrInvalid, "Значение «%s» не должно содержать переводов строки", k)
		}
	}
	return s, nil
}

// CheckCaps — примет ли этот агент такой сервер.
func (s Spec) CheckCaps(agent string) error {
	caps, ok := Supported[agent]
	if !ok {
		return userErr(ErrUnsupported, "Этот агент пока не умеет MCP отсюда")
	}
	typeOK := false
	for _, t := range caps.Types {
		if t == s.Type {
			typeOK = true
		}
	}
	if !typeOK {
		return userErr(ErrUnsupported, "%s не поддерживает подключение «%s»", agentTitle(agent), s.Type)
	}
	if len(s.Headers) > 0 && !caps.Headers {
		return userErr(ErrUnsupported, "%s не принимает заголовки для MCP-сервера", agentTitle(agent))
	}
	return nil
}

func agentTitle(agent string) string {
	switch agent {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	}
	return agent
}

// secrets — все значения, которые нельзя показывать (для вычистки из текста
// ошибок CLI: он иногда цитирует переданное).
func (s Spec) secrets() []string {
	out := []string{}
	for _, v := range s.Env {
		out = append(out, v)
	}
	for _, v := range s.Headers {
		out = append(out, v)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── Маскирование ─────────────────────────────────────────────────────────────

// secretWordRe — по имени флага или параметра видно, что дальше секрет.
var secretWordRe = regexp.MustCompile(`(?i)(key|token|secret|passw|auth|bearer|credential|cookie)`)

// looksLikeToken — длинная строка без пробелов и разделителей пути, где есть и
// буквы, и цифры: так выглядят ключи API (`sk-…`, `ghp_…`, `xoxb-…`).
func looksLikeToken(s string) bool {
	// Точка и двоеточие допустимы: JWT (`xxx.yyy.zzz`) и пары `user:token`
	// тоже ключи. Лишняя маска на длинном имени хоста безопаснее утечки.
	if len(s) < 24 || strings.ContainsAny(s, " /\\@") {
		return false
	}
	hasDigit, hasLetter := false, false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasLetter = true
		case r == '-' || r == '_' || r == '=' || r == '+' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return hasDigit && hasLetter
}

// MaskArgs прячет секреты в аргументах команды.
//
// Ключ API часто передают прямо аргументом (`--api-key sk-…`, `TOKEN=…`), и
// показать команду как есть значило бы показать ключ на экране телефона.
// Правила: значение после флага с «секретным» именем; `имя=значение` с таким
// именем; и просто длинная строка, похожая на ключ.
func MaskArgs(args []string) []string {
	out := make([]string, len(args))
	prevSecret := false
	for i, a := range args {
		switch {
		case prevSecret && !strings.HasPrefix(a, "-"):
			out[i] = Mask
			prevSecret = false
			continue
		case strings.Contains(a, "="):
			k, _, _ := strings.Cut(a, "=")
			if secretWordRe.MatchString(k) {
				out[i] = k + "=" + Mask
				prevSecret = false
				continue
			}
		}
		prevSecret = strings.HasPrefix(a, "-") && secretWordRe.MatchString(a)
		if looksLikeToken(a) {
			out[i] = Mask
			continue
		}
		out[i] = a
	}
	return out
}

// MaskURL прячет логин/пароль и значения параметров запроса: ключ нередко
// передают прямо в адресе (`?api_key=…`).
func MaskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return Mask
	}
	if u.User != nil {
		u.User = url.User(Mask)
	}
	if u.RawQuery != "" {
		q := u.Query()
		parts := make([]string, 0, len(q))
		for _, k := range sortedKeys(q) {
			parts = append(parts, url.QueryEscape(k)+"="+Mask)
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	u.Fragment = ""
	// Ключ бывает прямо в пути (`…/s/<ключ>/mcp` у Zapier и Composio).
	if u.Path != "" {
		segs := strings.Split(u.Path, "/")
		masked := false
		for i, seg := range segs {
			if looksLikeToken(seg) {
				segs[i] = Mask
				masked = true
			}
		}
		if masked {
			u.Path = strings.Join(segs, "/")
			u.RawPath = ""
		}
	}
	// Query уже собран руками: url.String экранировал бы маску.
	return strings.Replace(u.String(), url.QueryEscape(Mask), Mask, -1)
}

// scrub вычищает известные секреты из текста (ошибки CLI) и обрезает его.
func scrub(text string, secrets []string) string {
	for _, s := range secrets {
		if len(s) >= 3 {
			text = strings.ReplaceAll(text, s, Mask)
		}
	}
	text = strings.TrimSpace(text)
	// Первая содержательная строка: остальное — стек и советы CLI.
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "WARNING:") {
			continue
		}
		text = line
		break
	}
	if len([]rune(text)) > 300 {
		text = string([]rune(text)[:300]) + "…"
	}
	return text
}
