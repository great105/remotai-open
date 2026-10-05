// Package agenthooks — структурные сигналы ИИ-агентов поверх нашего PTY.
//
// Зачем. «Агент закончил», «ждёт разрешения» и «задал вопрос» мы угадывали по
// экрану: конец работы — по тишине (у агентов без файла статуса 120 с), а
// вопросы пришлось выключить вовсе (2.49.4) — по тексту экрана рассказ о меню
// от самого меню не отличить. Сами агенты при этом ЗНАЮТ ответ и умеют его
// сообщить: у Claude Code есть хуки, у Codex — программа `notify`. Их и берём,
// а терминал остаётся тем же PTY: экран агента человек видит как раньше.
//
// Как это устроено (проверено запуском 11.09.2026, claude 2.1.268 и
// codex-cli 0.154.0):
//
//   - Claude: клиент дописывает к запуску `--settings <файл>`; файл пишет агент
//     (EnsureClaudeSettings). `--settings` ДОБАВЛЯЕТ хуки к настройкам
//     человека, а не заменяет их — хук плагина из ~/.claude отработал рядом с
//     нашим.
//   - Codex: хуки per-invocation не передаются (документация прямо это
//     говорит, живой `-c hooks.Stop=…` молчал), а `-c notify=[…]` работает:
//     приходит `agent-turn-complete`. Но `-c` ПЕРЕЗАПИСЫВАЕТ notify человека
//     (у владельца им пользуется плагин computer-use), поэтому наш хук обязан
//     вызвать прежний notify сам (OriginalCodexNotify).
//
// Канал хук → агент — файл-очередь на терминал (SpoolPath). Путь хук берёт из
// окружения PTY (EnvFile), его ставит pty-host. Не HTTP: не нужны ни порт, ни
// токен, и события не теряются, пока агент перезапускается, — хост терминала
// живёт отдельно от него.
package agenthooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// EnvFile — переменная окружения PTY с путём файла-очереди этого терминала.
	EnvFile = "REMOTAI_HOOK_FILE"
	// EnvPTY — id терминала, в котором работает агент (для диагностики).
	EnvPTY = "REMOTAI_PTY_ID"

	// maxSpool — после этого размера очередь начинается заново. Строка события
	// — сотни байт, так что это тысячи событий; читатель забирает их раз в
	// секунду, и дорасти до потолка очередь может только без читателя.
	maxSpool = 256 << 10
	// MaxInput — сколько читаем из stdin хука. Вход Claude несёт tool_input
	// целиком (у Write это всё содержимое файла), а нам нужны только поля
	// разметки.
	MaxInput = 4 << 20

	detailLimit  = 120
	messageLimit = 200
)

// Нормализованные виды событий. Имена — как у хуков Claude Code: у Codex
// единственное событие notify (`agent-turn-complete`) приводится к Stop.
const (
	KindSessionStart = "SessionStart"
	KindPrompt       = "UserPromptSubmit"
	KindPermission   = "PermissionRequest"
	KindNotification = "Notification"
	KindPreTool      = "PreToolUse"
	KindPostTool     = "PostToolUse"
	KindStop         = "Stop"
	KindSessionEnd   = "SessionEnd"
)

// Event — одна строка очереди.
type Event struct {
	At               int64  `json:"at"` // unix ms, время хука
	Agent            string `json:"agent"`
	Kind             string `json:"event"`
	SessionID        string `json:"session_id,omitempty"`
	SessionScope     string `json:"session_scope,omitempty"` // Codex: root/subagent, verified from session_meta
	Transcript       string `json:"transcript,omitempty"`
	ConfigHome       string `json:"config_home,omitempty"` // source environment, captured by the local hook
	NotificationType string `json:"notification_type,omitempty"`
	Message          string `json:"message,omitempty"`
	Tool             string `json:"tool,omitempty"`
	// Detail — суть вызова инструмента одной строкой: команда Bash, путь файла,
	// у AskUserQuestion — сам вопрос.
	Detail string `json:"detail,omitempty"`
	// Options — подписи пунктов AskUserQuestion в порядке цифр меню. С экрана
	// их не прочитать: под каждым пунктом строка описания, а за разделителем
	// стоит ещё «Chat about this» (скриншот владельца 13.09.2026).
	Options []string `json:"options,omitempty"`
	TurnID  string   `json:"turn_id,omitempty"`
}

// Time — время события.
func (e Event) Time() time.Time { return time.UnixMilli(e.At) }

// SpoolPath — файл-очередь терминала. Рядом с журналом pty-host (тот же
// TempDir): оба процесса — хост и агент — работают под одним пользователем.
func SpoolPath(ptyID string) string {
	return filepath.Join(os.TempDir(), "remotai-hooks-"+safeID(ptyID)+".jsonl")
}

func safeID(id string) string {
	return strings.Map(func(r rune) rune {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_') {
			return r
		}
		return '_'
	}, id)
}

// ParseClaude разбирает вход хука Claude Code (JSON в stdin).
func ParseClaude(raw []byte, now time.Time) (Event, bool) {
	var in struct {
		HookEventName    string         `json:"hook_event_name"`
		SessionID        string         `json:"session_id"`
		TranscriptPath   string         `json:"transcript_path"`
		NotificationType string         `json:"notification_type"`
		Message          string         `json:"message"`
		ToolName         string         `json:"tool_name"`
		ToolInput        map[string]any `json:"tool_input"`
	}
	if json.Unmarshal(raw, &in) != nil || in.HookEventName == "" {
		return Event{}, false
	}
	ev := Event{
		At:               now.UnixMilli(),
		Agent:            "claude",
		Kind:             in.HookEventName,
		SessionID:        in.SessionID,
		Transcript:       in.TranscriptPath,
		NotificationType: in.NotificationType,
		Message:          oneLine(in.Message, messageLimit),
		Tool:             in.ToolName,
		Detail:           toolDetail(in.ToolInput),
	}
	if in.ToolName == "AskUserQuestion" {
		ev.Detail, ev.Options = askQuestion(in.ToolInput)
	}
	return ev, true
}

// optionLimit — подпись пункта на кнопке ответа (как choiceOptionLen в pty).
const optionLimit = 40

// askQuestion — вопрос AskUserQuestion и подписи его пунктов. Вход Claude:
//
//	{"questions":[{"question":"…","header":"…","multiSelect":false,
//	  "options":[{"label":"…","description":"…"}]}]}
//
// Пункты отдаём, только когда вопрос один и выбор одиночный: несколько
// вопросов идут вкладками, а при multiSelect цифра лишь ставит галочку —
// кнопка «2» тогда не ответила бы, а соврала.
func askQuestion(input map[string]any) (string, []string) {
	qs, _ := input["questions"].([]any)
	if len(qs) == 0 {
		return "", nil
	}
	q, _ := qs[0].(map[string]any)
	text, _ := q["question"].(string)
	question := oneLine(text, detailLimit)
	if len(qs) != 1 {
		return question, nil
	}
	if multi, _ := q["multiSelect"].(bool); multi {
		return question, nil
	}
	opts, _ := q["options"].([]any)
	labels := make([]string, 0, len(opts))
	for _, o := range opts {
		m, _ := o.(map[string]any)
		label, _ := m["label"].(string)
		if label = oneLine(label, optionLimit); label == "" {
			return question, nil // пустой пункт — цифры разъедутся с меню
		}
		labels = append(labels, label)
	}
	if len(labels) < 2 {
		return question, nil
	}
	return question, labels
}

// ParseCodexNotify разбирает аргумент программы notify у Codex:
//
//	{"type":"agent-turn-complete","thread-id":"…","turn-id":"…","cwd":"…",
//	 "client":"codex_exec","input-messages":[…],"last-assistant-message":"…"}
func ParseCodexNotify(arg string, now time.Time) (Event, bool) {
	var in struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread-id"`
		TurnID   string `json:"turn-id"`
		Last     string `json:"last-assistant-message"`
	}
	if json.Unmarshal([]byte(arg), &in) != nil || in.Type != "agent-turn-complete" {
		return Event{}, false
	}
	return Event{
		At:        now.UnixMilli(),
		Agent:     "codex",
		Kind:      KindStop,
		SessionID: in.ThreadID,
		TurnID:    in.TurnID,
		Message:   oneLine(in.Last, messageLimit),
	}, true
}

// toolDetail — самое говорящее поле вызова инструмента.
func toolDetail(input map[string]any) string {
	for _, key := range []string{"command", "file_path", "notebook_path", "url", "pattern", "description"} {
		if v, ok := input[key].(string); ok && strings.TrimSpace(v) != "" {
			return oneLine(v, detailLimit)
		}
	}
	return ""
}

// oneLine — строка без переводов и управляющих символов, не длиннее limit рун.
func oneLine(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > limit {
		s = strings.TrimRight(string(runes[:limit-1]), " ") + "…"
	}
	return s
}

// Append дописывает событие в очередь. Переполненную очередь сдвигает в .old:
// читатель увидит, что файл стал короче, и начнёт его сначала.
func Append(path string, ev Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if st, err := os.Stat(path); err == nil && st.Size() > maxSpool {
		_ = os.Remove(path + ".old")
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// Tail читает очередь с места, где остановился в прошлый раз.
//
// Первое чтение уже СУЩЕСТВУЮЩЕГО файла встаёт в его конец: это рестарт
// агента при живом терминале, и события в файле — прошлые. Проиграть их
// заново значило бы разослать «агент закончил» про работу, которая кончилась
// час назад. Файла нет — терминал новый, читаем с нуля, когда он появится.
type Tail struct {
	Path    string
	off     int64
	started bool
	partial []byte
}

// Read возвращает новые события (nil — ничего нового или файла нет).
func (t *Tail) Read() []Event {
	f, err := os.Open(t.Path)
	if err != nil {
		t.started = true // файла ещё нет: когда появится — с начала
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	size := st.Size()
	if !t.started {
		t.started, t.off = true, size
		return nil
	}
	if size < t.off { // очередь начата заново (Append сдвинул её в .old)
		t.off, t.partial = 0, nil
	}
	if size == t.off {
		return nil
	}
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSpool))
	if err != nil {
		return nil
	}
	t.off += int64(len(data))
	data = append(t.partial, data...)
	lines := bytes.Split(data, []byte{'\n'})
	// Последний кусок без перевода строки — запись, которую хук ещё дописывает.
	t.partial = append([]byte(nil), lines[len(lines)-1]...)
	var out []Event
	for _, l := range lines[:len(lines)-1] {
		var ev Event
		if json.Unmarshal(bytes.TrimSpace(l), &ev) == nil && ev.Kind != "" {
			out = append(out, ev)
		}
	}
	return out
}

// ── Claude: файл настроек с хуками ──────────────────────────────

// claudeHookEvents — на что подписываемся.
//
// Хук стоит процесса: замер 11.09.2026 — 70–80 мс на вызов remotai.exe (первый,
// холодный — 1,1 с). Поэтому на инструменты вешаем его только там, где он
// нужен: PreToolUse — ради AskUserQuestion (у вопроса агента нет своего
// события), PostToolUse — только у инструментов, которые спрашивают
// разрешение: он закрывает вопрос после ответа. Самые частые Read/Grep/Glob
// хук не трогает вовсе.
var claudeHookEvents = []struct{ event, matcher string }{
	{KindSessionStart, ""},
	{KindPrompt, ""},
	{KindPermission, ""},
	{KindNotification, ""},
	{KindPreTool, "AskUserQuestion"},
	{KindPostTool, PostToolMatcher},
	{KindStop, ""},
	{KindSessionEnd, ""},
}

// PostToolMatcher — инструменты, на которые Claude спрашивает разрешение.
const PostToolMatcher = "Bash|PowerShell|Write|Edit|MultiEdit|NotebookEdit|WebFetch|WebSearch|AskUserQuestion|ExitPlanMode|Task|Agent|mcp__.*"

// HookCommand — команда, которую агент вызовет из хука. Путь с прямыми
// слешами и в двойных кавычках: так его одинаково понимают bash (Git Bash на
// Windows), cmd и sh.
func HookCommand(exe, agent string) string {
	return `"` + filepath.ToSlash(exe) + `" hook ` + agent
}

// ClaudeSettings — содержимое файла для `claude --settings`.
func ClaudeSettings(exe string) []byte {
	hooks := map[string]any{}
	for _, h := range claudeHookEvents {
		entry := map[string]any{
			"hooks": []map[string]any{{
				"type":    "command",
				"command": HookCommand(exe, "claude"),
				"timeout": 10,
			}},
		}
		if h.matcher != "" {
			entry["matcher"] = h.matcher
		}
		hooks[h.event] = []map[string]any{entry}
	}
	b, _ := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	return append(b, '\n')
}

var settingsMu sync.Mutex

// EnsureClaudeSettings пишет файл настроек в dir (только если содержимое
// изменилось — список агентов спрашивают часто) и возвращает его путь.
func EnsureClaudeSettings(dir, exe string) (string, error) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	path := filepath.Join(dir, "claude-hooks.json")
	want := ClaudeSettings(exe)
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// ── Codex: notify ───────────────────────────────────────────────

// CodexNotifyArg — значение для `codex -c …`. На Windows строки TOML в
// одинарных кавычках: PowerShell 5.1 калечит двойные кавычки внутри аргумента
// нативной программы. Путь с кавычками и метасимволами cmd не подставляем
// вовсе — лучше эвристика, чем сломанный запуск.
func CodexNotifyArg(exe string, windows bool) (string, bool) {
	p := filepath.ToSlash(exe)
	if p == "" || strings.ContainsAny(p, "'\"%!^&\r\n") {
		return "", false
	}
	if windows {
		return fmt.Sprintf("notify=['%s','hook','codex']", p), true
	}
	return fmt.Sprintf(`notify=["%s","hook","codex"]`, p), true
}

// IsOurNotify — notify указывает на нас самих (иначе цепочка зациклится).
func IsOurNotify(argv []string) bool {
	return len(argv) >= 3 && argv[1] == "hook" && argv[2] == "codex"
}

// CodexConfigPath — config.toml этого профиля Codex.
func CodexConfigPath(codexHome string) string {
	return filepath.Join(codexHome, "config.toml")
}

// CodexNotifyState — что в config.toml человека с notify.
type CodexNotifyState int

const (
	// CodexNotifyNone — своего notify нет: подставлять наш можно.
	CodexNotifyNone CodexNotifyState = iota
	// CodexNotifyChainable — свой notify есть и разобран: наш хук вызовет его.
	CodexNotifyChainable
	// CodexNotifyUnknown — notify есть, но разобрать его мы не смогли
	// (многострочный массив и т. п.). Подставлять наш НЕЛЬЗЯ: `-c` заменит
	// чужой notify, а вызвать его вместо Codex нам будет нечем.
	CodexNotifyUnknown
)

// OriginalCodexNotify — notify человека из config.toml (верхний уровень, до
// первой таблицы) и его состояние.
func OriginalCodexNotify(codexHome string) ([]string, CodexNotifyState) {
	data, err := os.ReadFile(CodexConfigPath(codexHome))
	if err != nil {
		return nil, CodexNotifyNone
	}
	return parseCodexNotify(string(data))
}

func parseCodexNotify(config string) ([]string, CodexNotifyState) {
	for _, raw := range strings.Split(config, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if strings.HasPrefix(line, "[") {
			break // дальше — таблицы, notify там не верхнего уровня
		}
		if !strings.HasPrefix(line, "notify") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "notify"))
		if !strings.HasPrefix(rest, "=") {
			continue // notify_что-то — другой ключ
		}
		argv, ok := parseTOMLStringArray(strings.TrimSpace(rest[1:]))
		if !ok || len(argv) == 0 {
			return nil, CodexNotifyUnknown
		}
		if IsOurNotify(argv) {
			return nil, CodexNotifyNone
		}
		return argv, CodexNotifyChainable
	}
	return nil, CodexNotifyNone
}

// parseTOMLStringArray — однострочный массив строк TOML: базовые "…" и
// буквальные '…', хвостовой комментарий допустим. Всё прочее — не наш случай.
func parseTOMLStringArray(s string) ([]string, bool) {
	if !strings.HasPrefix(s, "[") {
		return nil, false
	}
	var out []string
	i := 1
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			return nil, false
		}
		switch s[i] {
		case ']':
			rest := strings.TrimSpace(s[i+1:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return nil, false
			}
			return out, true
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			out = append(out, s[i+1:i+1+j])
			i += j + 2
		case '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, false
			}
			v, err := strconv.Unquote(s[i : j+1])
			if err != nil {
				return nil, false
			}
			out = append(out, v)
			i = j + 1
		default:
			return nil, false
		}
	}
}
