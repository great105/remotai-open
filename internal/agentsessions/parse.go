package agentsessions

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// headCap — сколько головы файла читаем ради заголовка. Замер 29.09.2026:
	// первое сообщение человека у Claude на p90 = 31 КБ, у Codex на p99 =
	// 430 КБ, максимум 730 КБ. Реже (1 % Claude: беседы, начатые с огромной
	// вставки) оно глубже — тогда заголовок берём из ai-title в хвосте.
	headCap = 1 << 20
	// maxLine — строки длиннее не разбираем: это вывод инструментов и
	// картинки в base64, человеческого сообщения там нет.
	maxLine = 512 << 10
	// tailCap — хвост, в котором ищем ai-title Claude.
	tailCap = 256 << 10
	// titleRunes — длина заголовка.
	titleRunes = 120
	// countChunk — порция фонового подсчёта.
	countChunk = 4 << 20
)

type head struct {
	sessionID string
	cwd       string
	title     string
	titleAI   bool
	full      bool
	scanned   int64
	hidden    bool
}

// forEachLine читает r построчно, не больше limit байт. Строки длиннее
// maxLine пропускаются целиком (fn их не видит). Возвращает, сколько байт
// прочитано, и дошли ли до конца файла. fn возвращает false — стоп.
func forEachLine(r io.Reader, limit int64, fn func(line []byte) bool) (int64, bool, error) {
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 64<<10)
	var read int64
	skipping := false
	for read < limit {
		want := int64(len(chunk))
		if limit-read < want {
			want = limit - read
		}
		n, err := r.Read(chunk[:want])
		read += int64(n)
		data := chunk[:n]
		for len(data) > 0 {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				if !skipping {
					if len(buf)+len(data) > maxLine {
						skipping, buf = true, buf[:0]
					} else {
						buf = append(buf, data...)
					}
				}
				break
			}
			if skipping {
				skipping = false
			} else if len(buf)+i <= maxLine {
				line := append(buf, data[:i]...)
				if !fn(line) {
					return read, false, nil
				}
			}
			buf = buf[:0]
			data = data[i+1:]
		}
		if err == io.EOF {
			if len(buf) > 0 && !skipping {
				fn(buf)
			}
			return read, true, nil
		}
		if err != nil {
			return read, false, err
		}
	}
	return read, false, nil
}

// readHead разбирает голову файла беседы.
func readHead(path, agent string) (head, error) {
	f, err := os.Open(path)
	if err != nil {
		return head{}, err
	}
	defer f.Close()
	var h head
	var fallback string // «/команда» без аргументов — если ничего лучше нет
	conversation := false
	fn := func(line []byte) bool {
		switch agent {
		case "claude":
			// Разбираем только то, что может дать папку или сообщение: полный
			// JSON-разбор каждой строки головы делал холодный список втрое дольше.
			if h.cwd != "" && !bytes.Contains(line, claudeUserMark) && !bytes.Contains(line, claudeAssistantMark) {
				return true
			}
			var rec claudeRecord
			if json.Unmarshal(line, &rec) != nil {
				return true
			}
			if h.cwd == "" && rec.CWD != "" {
				h.cwd = rec.CWD
			}
			if rec.Type != "user" && rec.Type != "assistant" {
				return true
			}
			conversation = true
			if rec.Type != "user" || rec.IsMeta || rec.IsSidechain {
				return true
			}
			text, cmd := humanText(claudeUserText(rec.Message.Content))
			if text != "" {
				h.title = text
				return false
			}
			if cmd != "" && fallback == "" {
				fallback = cmd
			}
		case "codex":
			if h.sessionID != "" && !bytes.Contains(line, codexUserRoleMark) {
				if bytes.Contains(line, codexItemMark) {
					conversation = true
				}
				return true
			}
			var rec codexRecord
			if json.Unmarshal(line, &rec) != nil {
				return true
			}
			switch rec.Type {
			case "session_meta":
				if h.sessionID == "" {
					var meta codexMeta
					if json.Unmarshal(rec.Payload, &meta) == nil {
						h.sessionID, h.cwd = meta.ID, meta.CWD
						// Субагенты — ветки чужой беседы, человек их не начинал
						// и продолжать по отдельности не будет (замер: 493 из 889).
						if meta.ParentThreadID != "" || meta.ThreadSource == "subagent" || bytes.HasPrefix(bytes.TrimSpace(meta.Source), []byte("{")) {
							h.hidden = true
							return false
						}
					}
				}
			case "response_item":
				text, cmd := humanText(codexUserText(rec.Payload))
				if text != "" {
					h.title = text
					return false
				}
				if cmd != "" && fallback == "" {
					fallback = cmd
				}
			}
			conversation = true
		}
		return true
	}
	scanned, eof, err := forEachLine(f, headCap, fn)
	if err != nil {
		return head{}, err
	}
	h.scanned = scanned
	h.full = eof || scanned >= headCap
	if h.hidden {
		return h, nil
	}
	if h.title == "" && fallback != "" {
		h.title = fallback
	}
	if h.title == "" && agent == "claude" {
		if t := claudeTailTitle(f); t != "" {
			h.title, h.titleAI = t, true
		}
	}
	if h.title == "" && !conversation && eof {
		h.hidden = true // ни одного сообщения: служебный файл, продолжать нечего
	}
	return h, nil
}

type claudeRecord struct {
	Type        string `json:"type"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	CWD         string `json:"cwd"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	AITitle string `json:"aiTitle"`
}

type codexRecord struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type codexMeta struct {
	ID             string          `json:"id"`
	CWD            string          `json:"cwd"`
	Source         json.RawMessage `json:"source"`
	ThreadSource   string          `json:"thread_source"`
	ParentThreadID string          `json:"parent_thread_id"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// claudeUserText — текст сообщения человека. Ответ инструмента (tool_result)
// приходит тоже как «user», но человеком не написан — пусто.
func claudeUserText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			return ""
		case "text":
			parts = append(parts, b.Text)
		}
	}
	// Служебные вставки идут отдельными блоками рядом с настоящим текстом:
	// берём первый блок, в котором после чистки что-то осталось.
	for _, p := range parts {
		if t, _ := humanText(p); t != "" {
			return p
		}
	}
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// codexUserText — текст сообщения человека в response_item Codex.
func codexUserText(payload json.RawMessage) string {
	var msg struct {
		Type    string         `json:"type"`
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	if json.Unmarshal(payload, &msg) != nil || msg.Type != "message" || msg.Role != "user" {
		return ""
	}
	first := ""
	for _, b := range msg.Content {
		if b.Type != "input_text" {
			continue
		}
		if t, _ := humanText(b.Text); t != "" {
			return b.Text
		}
		if first == "" {
			first = b.Text
		}
	}
	return first
}

var (
	leadingTag  = regexp.MustCompile(`^<([A-Za-z][A-Za-z0-9_-]*)[^>]*>`)
	spaces      = regexp.MustCompile(`\s+`)
	inlineImage = regexp.MustCompile(`</?image\b[^>]*>`)
)

// humanText чистит сообщение от служебных вставок CLI и возвращает заголовок.
// Второе значение — «/команда» (слеш-команда без текста): годится в заголовок,
// только если настоящего сообщения в голове нет.
//
// Что вырезаем — сверено с живыми файлами 29.09.2026 по префиксам первых
// сообщений (без чтения содержимого): <command-name>, <local-command-stdout>,
// <local-command-caveat>, <system-reminder>, <pasted_content>,
// <task-notification> у Claude; <environment_context>, <recommended_plugins>,
// <codex_internal_context> и «# AGENTS.md instructions» (202 беседы из 396) у
// Codex.
func humanText(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "# AGENTS.md instructions") ||
		strings.HasPrefix(s, "[Request interrupted") || strings.HasPrefix(s, "Caveat: The messages below") {
		return "", ""
	}
	cmd := ""
	if name := tagBody(s, "command-name"); name != "" {
		args := strings.TrimSpace(tagBody(s, "command-args"))
		if args != "" {
			return clip(name + " " + args), ""
		}
		cmd = clip(name)
	}
	for i := 0; i < 16; i++ {
		m := leadingTag.FindStringSubmatch(s)
		if m == nil {
			break
		}
		closing := "</" + m[1] + ">"
		end := strings.Index(s, closing)
		if end < 0 {
			// Незакрытый тег в начале — служебная вставка целиком.
			return "", cmd
		}
		s = strings.TrimSpace(s[end+len(closing):])
	}
	// Вложенная картинка Codex посреди текста: `<image name=[Image #1]>`.
	s = strings.TrimSpace(inlineImage.ReplaceAllString(s, " "))
	if s == "" {
		return "", cmd
	}
	return clip(s), cmd
}

func tagBody(s, tag string) string {
	open := "<" + tag + ">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, "</"+tag+">")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// clip — одна строка до titleRunes символов.
func clip(s string) string {
	s = strings.TrimSpace(spaces.ReplaceAllString(s, " "))
	if utf8.RuneCountInString(s) <= titleRunes {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:titleRunes-1])) + "…"
}

// claudeTailTitle — последний ai-title в хвосте файла: Claude сам пишет
// короткое название беседы и обновляет его по ходу (замер: есть у 298 из 322).
func claudeTailTitle(f *os.File) string {
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	from := info.Size() - tailCap
	if from < 0 {
		from = 0
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return ""
	}
	title := ""
	_, _, _ = forEachLine(f, tailCap, func(line []byte) bool {
		if !bytes.Contains(line, []byte(`"ai-title"`)) {
			return true
		}
		var rec claudeRecord
		if json.Unmarshal(line, &rec) == nil && rec.Type == "ai-title" && strings.TrimSpace(rec.AITitle) != "" {
			title = clip(rec.AITitle)
		}
		return true
	})
	return title
}

// countMessages считает сообщения человека и текстовые ответы агента в
// [from, size) — только целые строки. Возвращает число и новое смещение
// (сразу после последнего перевода строки).
func countMessages(ctx context.Context, path, agent string, from, size int64, pause time.Duration) (int, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, from, err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, from, err
	}
	count := 0
	off := from // сразу после последнего разобранного перевода строки
	pos := from
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 1<<20)
	skipping := false
	var sinceRest int64
	for pos < size {
		if err := ctx.Err(); err != nil {
			return count, off, err
		}
		// Отдых между порциями: первый проход на ПК владельца — 13 ГБ, и
		// ноутбук не должен захлебнуться диском ради счётчика.
		if sinceRest >= countChunk {
			sinceRest = 0
			if pause > 0 {
				select {
				case <-ctx.Done():
					return count, off, ctx.Err()
				case <-time.After(pause):
				}
			}
		}
		want := int64(len(chunk))
		if size-pos < want {
			want = size - pos
		}
		n, err := f.Read(chunk[:want])
		data := chunk[:n]
		for len(data) > 0 {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				if !skipping {
					if len(buf)+len(data) > maxLine {
						skipping, buf = true, buf[:0]
					} else {
						buf = append(buf, data...)
					}
				}
				break
			}
			if !skipping && len(buf)+i <= maxLine {
				line := append(buf, data[:i]...)
				if isMessageLine(agent, line) {
					count++
				}
			}
			skipping, buf = false, buf[:0]
			data = data[i+1:]
			off = pos + int64(n-len(data))
		}
		pos += int64(n)
		sinceRest += int64(n)
		if err == io.EOF || n == 0 {
			break
		}
		if err != nil {
			return count, off, err
		}
	}
	return count, off, nil
}

var (
	claudeUserMark      = []byte(`"type":"user"`)
	claudeAssistantMark = []byte(`"type":"assistant"`)
	claudeTextMark      = []byte(`"type":"text"`)
	toolResultMark      = []byte(`"tool_use_id"`)
	codexItemMark       = []byte(`"type":"response_item"`)
	codexMessageMark    = []byte(`"type":"message"`)
	codexUserRoleMark   = []byte(`"role":"user"`)
)

// isMessageLine — строка это сообщение человека или текстовый ответ агента.
// Сначала дешёвая проверка байтов: большинство строк — вызовы и вывод
// инструментов, их разбирать незачем.
func isMessageLine(agent string, line []byte) bool {
	switch agent {
	case "claude":
		if bytes.Contains(line, claudeAssistantMark) {
			if !bytes.Contains(line, claudeTextMark) {
				return false
			}
			var rec claudeRecord
			if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.IsSidechain {
				return false
			}
			var blocks []contentBlock
			if json.Unmarshal(rec.Message.Content, &blocks) != nil {
				return false
			}
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					return true
				}
			}
			return false
		}
		if !bytes.Contains(line, claudeUserMark) || bytes.Contains(line, toolResultMark) {
			return false
		}
		var rec claudeRecord
		if json.Unmarshal(line, &rec) != nil || rec.Type != "user" || rec.IsMeta || rec.IsSidechain {
			return false
		}
		text, cmd := humanText(claudeUserText(rec.Message.Content))
		return text != "" || cmd != ""
	case "codex":
		if !bytes.Contains(line, codexItemMark) || !bytes.Contains(line, codexMessageMark) {
			return false
		}
		var rec codexRecord
		if json.Unmarshal(line, &rec) != nil || rec.Type != "response_item" {
			return false
		}
		var msg struct {
			Type    string         `json:"type"`
			Role    string         `json:"role"`
			Content []contentBlock `json:"content"`
		}
		if json.Unmarshal(rec.Payload, &msg) != nil || msg.Type != "message" {
			return false
		}
		switch msg.Role {
		case "assistant":
			for _, b := range msg.Content {
				if b.Type == "output_text" && strings.TrimSpace(b.Text) != "" {
					return true
				}
			}
		case "user":
			text, cmd := humanText(codexUserText(rec.Payload))
			return text != "" || cmd != ""
		}
	}
	return false
}
