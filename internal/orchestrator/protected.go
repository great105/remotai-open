package orchestrator

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// ── Protected paths policy ────────────────────────────────────────────
//
// Оркестратор исполняет write_file / run_command / run_agent молча и быстро.
// Некоторые пути слишком дороги для ошибки модели: секреты (.env, *.key,
// *.pem), миграции БД, служебный .git. Любое действие, затрагивающее такой
// путь, эскалирует в подтверждение человеком (ConfirmFunc), а не выполняется
// молча. Подтверждения нет (nil) — действие отклоняется.

// DefaultProtectedGlobs — встроенная политика. `**` матчит любое число
// сегментов пути (включая ноль), остальные сегменты — filepath.Match.
var DefaultProtectedGlobs = []string{
	"**/.env*",
	"**/*.key",
	"**/*.pem",
	"migrations/**",
	"**/migrations/**",
	".git/**",
}

// ConfirmFunc спрашивает человека, можно ли выполнить действие `what`
// (например "write_file: migrations/001.sql"). true — разрешить один раз.
type ConfirmFunc func(what string) bool

// defaultConfirm — пакетный мост для вызывающего кода, который не достаёт до
// экземпляра Orchestrator (он создаётся внутри internal/agents). Бот ставит
// сюда свой TG-approve один раз на старте; экземплярный SetConfirmFunc
// имеет приоритет.
var defaultConfirm atomic.Value // хранит ConfirmFunc

// SetDefaultConfirmFunc задаёт ConfirmFunc для всех оркестраторов без
// экземплярного хука. nil (или отсутствие вызова) = запрещать.
func SetDefaultConfirmFunc(f ConfirmFunc) {
	defaultConfirm.Store(f)
}

// SetConfirmFunc задаёт экземплярный ConfirmFunc. nil = fallback на
// пакетный дефолт, а если и его нет — запрещать.
func (o *Orchestrator) SetConfirmFunc(f ConfirmFunc) {
	o.confirmFn = f
}

// SetProtectedGlobs заменяет дефолтную политику защищённых путей.
// Пустой срез отключает защиту; nil (значение по умолчанию) = DefaultProtectedGlobs.
func (o *Orchestrator) SetProtectedGlobs(globs []string) {
	o.protectedGlobs = globs
}

func (o *Orchestrator) globs() []string {
	if o.protectedGlobs == nil {
		return DefaultProtectedGlobs
	}
	return o.protectedGlobs
}

func (o *Orchestrator) confirm(what string) bool {
	if o.confirmFn != nil {
		return o.confirmFn(what)
	}
	if f, ok := defaultConfirm.Load().(ConfirmFunc); ok && f != nil {
		return f(what)
	}
	return false
}

// gateProtected — точка гейта для execTool. Возвращает "" когда действие
// разрешено (не защищено или человек подтвердил), иначе — текст отказа модели.
func (o *Orchestrator) gateProtected(tool string, input map[string]any, cwd string) string {
	what, hit := o.protectedTarget(tool, input, cwd)
	if !hit {
		return ""
	}
	if o.confirm(what) {
		return ""
	}
	return fmt.Sprintf("PROTECTED: %s — путь входит в protected paths. Действие НЕ выполнено: "+
		"нужно подтверждение человека (отклонено или недоступно). Не повторяй этот вызов; "+
		"если путь критичен для задачи — опиши в finish, какой доступ нужен.", what)
}

// protectedTarget определяет, задевает ли вызов тула защищённый путь, и
// возвращает человекочитаемое описание для подтверждения.
func (o *Orchestrator) protectedTarget(tool string, input map[string]any, cwd string) (string, bool) {
	globs := o.globs()
	switch tool {
	case "write_file":
		p, _ := input["path"].(string)
		if matchProtectedPath(p, cwd, globs) {
			return "write_file: " + p, true
		}
	case "run_command":
		cmd, _ := input["command"].(string)
		if tok, hit := mentionsProtected(cmd, cwd, globs); hit {
			return fmt.Sprintf("run_command: %s (защищённый путь: %s)", truncate(cmd, 120), tok), true
		}
	case "run_agent":
		prompt, _ := input["prompt"].(string)
		if tok, hit := mentionsProtected(prompt, cwd, globs); hit {
			return fmt.Sprintf("run_agent: в промпте защищённый путь %s", tok), true
		}
	case "race":
		// Гонка применяет победивший diff к cwd — та же проверка текста задачи,
		// что у run_agent по промпту.
		task, _ := input["task"].(string)
		if tok, hit := mentionsProtected(task, cwd, globs); hit {
			return fmt.Sprintf("race: в задаче защищённый путь %s", tok), true
		}
	}
	return "", false
}

// ── Сегментный матчинг ────────────────────────────────────────────────

// matchProtectedPath матчит путь по глобам. Путь нормализуется (Join+Clean,
// резолв `..`) и приводится к виду относительно cwd; матчинг сегментный —
// никакого over-match по префиксу строки (vendor ≠ vendor2).
func matchProtectedPath(path, cwd string, globs []string) bool {
	if path == "" {
		return false
	}
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	segs := relSegments(p, cwd)
	if len(segs) == 0 {
		return false
	}
	for _, g := range globs {
		if segmentsMatch(globSegments(g, cwd), segs) {
			return true
		}
	}
	return false
}

// relSegments — путь в виде сегментов относительно base (слеши, без Clean-ошибок).
// Путь вне base даёт сегменты вида ["..", ...] — их тоже матчим (`**` съест "..").
func relSegments(path, base string) []string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		rel = path
	}
	return strings.Split(filepath.ToSlash(rel), "/")
}

// globSegments нормализует глоб к сегментам относительно base. Глоб вне base
// (abs-паттерн другого дерева) не участвует — возвращает nil.
func globSegments(pattern, base string) []string {
	g := strings.TrimSpace(pattern)
	if g == "" {
		return nil
	}
	g = filepath.Clean(filepath.FromSlash(g))
	if filepath.IsAbs(g) {
		rel, err := filepath.Rel(base, g)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
		g = rel
	}
	return strings.Split(filepath.ToSlash(g), "/")
}

// segmentsMatch: `**` — любое число сегментов (включая 0), прочие сегменты —
// filepath.Match. Исчерпание паттерна = путь совпал или лежит ВНУТРИ
// защищённой директории. Исчерпание пути при остатке паттерна — совпадение,
// только если остаток весь из `**` (глоб директории матчит и её саму).
func segmentsMatch(pat, segs []string) bool {
	if len(pat) == 0 {
		return false // пустой паттерн не защищает ничего
	}
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if segmentsMatchTail(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := filepath.Match(pat[0], segs[0])
		if err != nil {
			ok = pat[0] == segs[0] // битый глоб — литеральное сравнение
		}
		if !ok {
			return false
		}
		pat = pat[1:]
		segs = segs[1:]
	}
	return true
}

// segmentsMatchTail — как segmentsMatch, но пустой паттерн = совпадение
// (хвост после `**` может отсутствовать).
func segmentsMatchTail(pat, segs []string) bool {
	if len(pat) == 0 {
		return true
	}
	return segmentsMatch(pat, segs)
}

// ── Эвристика для команд и промптов ───────────────────────────────────

// mentionsProtected ищет в тексте (shell-команда, промпт агента) упоминание
// защищённого пути. Консервативно: любой токен, матчящий политику, — гейт.
// Возвращает первый такой токен для сообщения человеку/модели.
func mentionsProtected(text, cwd string, globs []string) (string, bool) {
	for _, tok := range strings.FieldsFunc(text, isTokenSep) {
		if tok == "" || strings.HasPrefix(tok, "-") {
			continue // флаги не пути
		}
		if matchProtectedPath(tok, cwd, globs) {
			return tok, true
		}
	}
	return "", false
}

func isTokenSep(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', ';', '&', '|', '<', '>', '(', ')', '"', '\'', '`', ',', '=':
		return true
	}
	return false
}
