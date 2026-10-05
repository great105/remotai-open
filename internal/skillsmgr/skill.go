// Package skillsmgr — скиллы CLI-агентов на этом компьютере: что стоит,
// установка из GitHub или ZIP, копирование между агентами, удаление в
// резервную копию и возврат.
//
// ЗАЧЕМ. Скилл — это папка `<каталог агента>/skills/<имя>/SKILL.md`. Поставить
// его с телефона раньше было нечем: только терминал, `git clone` и ручное
// копирование в нужный каталог каждого агента.
//
// ГЛАВНОЕ ПРАВИЛО ПАКЕТА: ничего не удаляем. Удаление и замена переносят
// старую папку в резервную копию (`skills-backup/<время>/<место>/<имя>`),
// откуда её возвращает Restore. Каталоги скиллов у аккаунтов часто СВЯЗАНЫ с
// основным профилем (junction/симлинк, см. internal/web/account_share.go) —
// такие места сворачиваются в основное, чтобы одно и то же не показывалось
// дважды и не удалялось «через связь».
package skillsmgr

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// SkillFile — имя файла, по которому папка считается скиллом.
const SkillFile = "SKILL.md"

// Skill — один скилл в одном месте.
type Skill struct {
	// Name — имя папки: по нему скилл адресуется во всех операциях.
	Name string `json:"name"`
	// Title — `name:` из шапки SKILL.md (может отличаться от папки).
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path"`
	// Link — сама папка скилла — ссылка (его поставили куда-то ещё, например
	// в ~/.agents/skills, а сюда связали). Удаление переносит только ссылку.
	Link       bool   `json:"link,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`
	// ReadOnly — встроенный или из плагина: не удаляем и не заменяем.
	ReadOnly bool `json:"readonly,omitempty"`
	// Source — "", "system" (встроенный в CLI) или имя плагина.
	Source string `json:"source,omitempty"`
}

// Meta — то, что удалось прочитать из шапки SKILL.md.
type Meta struct {
	Name        string
	Description string
}

const maxDescription = 600

// ParseFrontmatter разбирает YAML-шапку между строками `---`.
//
// Полный YAML тут не нужен и опасен (лишняя зависимость ради двух полей):
// берём ключи верхнего уровня `name`/`description` в тех формах, что
// встречаются в живых скиллах — простое значение, в кавычках, блочное
// (`>`/`|`) и простое с переносом на отступе.
func ParseFrontmatter(data []byte) Meta {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var lines []string
	first := true
	closed := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if first {
			first = false
			if strings.TrimSpace(line) != "---" {
				return Meta{}
			}
			continue
		}
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		lines = append(lines, line)
	}
	if !closed {
		return Meta{}
	}
	fields := map[string]string{}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		colon := strings.Index(line, ":")
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		// Продолжение значения — строки с отступом ниже.
		var cont []string
		for i+1 < len(lines) && (lines[i+1] == "" || lines[i+1][0] == ' ' || lines[i+1][0] == '\t') {
			i++
			cont = append(cont, strings.TrimSpace(lines[i]))
		}
		switch {
		case strings.HasPrefix(val, "|"):
			val = strings.TrimSpace(strings.Join(cont, "\n"))
		case strings.HasPrefix(val, ">"):
			val = joinFolded(cont)
		case strings.HasPrefix(val, `"`):
			full := strings.TrimSpace(val + " " + joinFolded(cont))
			if u, err := strconv.Unquote(full); err == nil {
				val = u
			} else {
				val = strings.Trim(full, `"`)
			}
		case strings.HasPrefix(val, "'"):
			full := strings.TrimSpace(val + " " + joinFolded(cont))
			full = strings.TrimSuffix(strings.TrimPrefix(full, "'"), "'")
			val = strings.ReplaceAll(full, "''", "'")
		default:
			if len(cont) > 0 {
				val = strings.TrimSpace(val + " " + joinFolded(cont))
			}
		}
		if _, seen := fields[key]; !seen {
			fields[key] = val
		}
	}
	meta := Meta{Name: fields["name"], Description: fields["description"]}
	if r := []rune(meta.Description); len(r) > maxDescription {
		meta.Description = string(r[:maxDescription-1]) + "…"
	}
	return meta
}

func joinFolded(lines []string) string {
	var parts []string
	for _, l := range lines {
		if l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " ")
}

// ReadMeta читает шапку SKILL.md в папке (не больше 256 КБ).
func ReadMeta(dir string) (Meta, bool) {
	f, err := os.Open(filepath.Join(dir, SkillFile))
	if err != nil {
		return Meta{}, false
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, 256*1024))
	return ParseFrontmatter(data), true
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// ValidNewName — годится ли имя для НОВОЙ папки скилла: латиница, цифры,
// `.`, `_`, `-`, без служебных имён Windows и точки в конце. Строго, потому
// что имя приходит из чужого архива.
func ValidNewName(name string) bool {
	if !safeName.MatchString(name) || strings.HasSuffix(name, ".") {
		return false
	}
	base := strings.ToUpper(name)
	if i := strings.Index(base, "."); i >= 0 {
		base = base[:i]
	}
	return !windowsReserved[base]
}

// validExistingName — имя УЖЕ стоящего скилла: мягче (человек мог назвать
// папку по-русски), но ровно один компонент пути и не скрытая папка.
func validExistingName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return false
	}
	return !strings.ContainsAny(name, `/\:`+"\x00")
}

// isLink — запись каталога является ссылкой. На Windows junction с Go 1.23
// приходит как ModeIrregular, а не ModeSymlink, поэтому смотрим оба.
func isLink(mode os.FileMode) bool {
	return mode&(os.ModeSymlink|os.ModeIrregular) != 0
}

// listDir читает скиллы одной папки. Скрытая `.system` (встроенные скиллы
// Codex) читается как «только чтение».
func listDir(dir string, readOnly bool, source string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(dir, name)
		if strings.HasPrefix(name, ".") {
			if name == ".system" && source == "" {
				out = append(out, listDir(full, true, "system")...)
			}
			continue
		}
		st, err := os.Stat(full) // идём по ссылке: скилл может лежать где угодно
		if err != nil || !st.IsDir() {
			continue
		}
		meta, ok := ReadMeta(full)
		if !ok {
			continue // папка без SKILL.md — не скилл
		}
		sk := Skill{Name: name, Title: meta.Name, Description: meta.Description, Path: full, ReadOnly: readOnly, Source: source}
		if li, err := os.Lstat(full); err == nil && isLink(li.Mode()) {
			sk.Link = true
			if target, err := os.Readlink(full); err == nil {
				sk.LinkTarget = target
			}
		}
		out = append(out, sk)
	}
	return out
}
