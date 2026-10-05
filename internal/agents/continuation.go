package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tgcontrol/internal/paths"
	"tgcontrol/internal/sessions"
)

// Continuation packet: при смене агента в треде (задачу начал claude,
// продолжает codex) новому агенту отдаём дельту разговора ФАЙЛОМ, а в промпт
// подставляем только путь к нему — так в контекст нового агента не уезжает
// вся история переписки целиком.

const (
	// maxContinuationBytes — кап размера пакета. ~32 КБ текста хватает на
	// внятную дельту и при этом стоит заметно дешевле полной истории.
	maxContinuationBytes = 32 * 1024
	// maxContinuationMsgText — кап на текст одного сообщения внутри пакета.
	maxContinuationMsgText = 2000
)

// continuationDir — куда складываются файлы пакетов по умолчанию.
func continuationDir() string {
	return filepath.Join(paths.Base(), "continuations")
}

// BuildContinuationPacket собирает текст дельты из истории сообщений
// предыдущего агента: задача (первое сообщение пользователя), ход разговора,
// использованные инструменты и последний ответ — то, с чего продолжать.
// Результат ограничен maxContinuationBytes: при переполнении выкидывается
// середина (самые старые ходы после постановки задачи), края важнее.
func BuildContinuationPacket(sourceAgent string, msgs []sessions.Message) string {
	return buildContinuationPacket(sourceAgent, msgs, maxContinuationBytes)
}

func buildContinuationPacket(sourceAgent string, msgs []sessions.Message, capBytes int) string {
	if sourceAgent == "" {
		sourceAgent = "unknown"
	}

	var sb strings.Builder
	sb.WriteString("# Continuation packet — передача задачи между агентами\n\n")
	sb.WriteString(fmt.Sprintf("Предыдущий агент: %s\n", sourceAgent))
	sb.WriteString(fmt.Sprintf("Собран: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("Сообщений в истории: %d\n\n", len(msgs)))

	// Инструменты, которыми пользовался предыдущий агент (агрегат, без повторов).
	if tools := collectTools(msgs); len(tools) > 0 {
		sb.WriteString("Использованные инструменты: " + strings.Join(tools, ", ") + "\n\n")
	}

	sb.WriteString("## Дельта разговора\n\n")

	// Рендерим сообщения в блоки; первое сообщение пользователя (постановка
	// задачи) помечаем — его терять нельзя, при переполнении режем середину.
	blocks := make([]string, 0, len(msgs))
	for _, m := range msgs {
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		if len(text) > maxContinuationMsgText {
			text = text[:maxContinuationMsgText] + "…"
		}
		role := m.Role
		if role == "" {
			role = "agent"
		}
		blocks = append(blocks, fmt.Sprintf("**%s**: %s\n", role, text))
	}

	header := sb.String()
	body := strings.Join(blocks, "\n")
	if len(header)+len(body) > capBytes && len(blocks) > 2 {
		// Оставляем первый блок (обычно постановка задачи) и добираем хвост
		// (свежие сообщения — «что сделано и что осталось»), середину режем.
		first := blocks[0]
		tail := make([]string, 0, len(blocks))
		room := capBytes - len(header) - len(first) - 64 // запас под маркер пропуска
		size := 0
		for i := len(blocks) - 1; i >= 1; i-- {
			if size+len(blocks[i]) > room {
				break
			}
			tail = append([]string{blocks[i]}, tail...)
			size += len(blocks[i])
		}
		dropped := len(blocks) - 1 - len(tail)
		body = first + fmt.Sprintf("\n[... %d сообщений опущено — середина истории ...]\n\n", dropped) +
			strings.Join(tail, "\n")
	}
	packet := header + body

	// Жёсткий кап хвоста: если даже после схлопывания середины не влезли
	// (гигантское первое сообщение), обрезаем конец.
	if len(packet) > capBytes {
		packet = packet[:capBytes] + "\n\n[... пакет обрезан по лимиту размера ...]"
	}
	return packet
}

// WriteContinuationPacket собирает пакет и пишет его в файл. dir == "" —
// каталог по умолчанию (paths.Base()/continuations). Возвращает путь к файлу.
func WriteContinuationPacket(sourceAgent string, msgs []sessions.Message, dir string) (string, error) {
	if dir == "" {
		dir = continuationDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if sourceAgent == "" {
		sourceAgent = "unknown"
	}
	// Имя агента попадает в имя файла — отсекаем всё, кроме простых символов,
	// чтобы кривой ввод не утащил запись за пределы dir.
	var safe strings.Builder
	for _, r := range sourceAgent {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			safe.WriteRune(r)
		}
	}
	if safe.Len() == 0 {
		safe.WriteString("unknown")
	}
	name := fmt.Sprintf("continuation-%s-%s.md", safe.String(), time.Now().Format("20060102-150405.000"))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(BuildContinuationPacket(sourceAgent, msgs)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// withContinuation подставляет в промпт ссылку на файл с дельтой предыдущей
// работы. Сам текст пакета в промпт НЕ вставляется — в этом весь смысл:
// агент прочитает файл сам, своими инструментами, и потратит токены только
// на то, что ему реально нужно.
func withContinuation(prompt, continuationPath string) string {
	if continuationPath == "" {
		return prompt
	}
	return prompt + "\n\n---\n" +
		"Эта задача — продолжение работы другого агента. Дельта предыдущего разговора " +
		"(что сделано, ключевые решения, изменённые файлы, что осталось) находится в файле: " +
		continuationPath + "\n" +
		"Прочитай этот файл перед началом работы и продолжи с того места, где остановился предыдущий агент."
}

// collectTools возвращает отсортированный по первому появлению список
// уникальных инструментов из истории сообщений.
func collectTools(msgs []sessions.Message) []string {
	seen := make(map[string]bool)
	var out []string
	for _, m := range msgs {
		for _, t := range m.Tools {
			if t != "" && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}
