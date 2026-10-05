package pty

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgcontrol/internal/agents"
)

// tsProcessBadge — клиентское зеркало IsAgentKind.
const tsProcessBadge = "../../packages/shared/src/components/ProcessBadge.tsx"

// Клиентский `isAgentKind` — РУЧНОЙ список, и по нему включается ряд быстрых
// клавиш агента в терминале, счёт агентских сессий на плитках и текст
// уведомлений. Разойтись он может молча: агент запускается, работает, а
// интерфейс держит его за обычный процесс — ни кнопок ответа, ни счётчика.
//
// Так и вышло при добавлении grok: реестр (`internal/agents`) и распознавание в
// Go поправили, а этот список забыли. Отсюда тест: сверяем не глазами, а
// сборкой — каждый вид, который Go считает агентом, обязан быть и в TSX.
//
// Проверка живёт здесь, а не в vitest, намеренно: файл теста на стороне клиента
// попадает в `tsc` браузерной сборки, где `node:fs` нет вовсе.
func TestClientMirrorsAgentKinds(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(tsProcessBadge))
	if err != nil {
		t.Fatalf("не прочитать клиентское зеркало %s: %v", tsProcessBadge, err)
	}
	src := string(raw)

	body, ok := sliceBetween(src, "export function isAgentKind", "return false")
	if !ok {
		t.Fatalf("в %s не нашли тело isAgentKind — зеркало переписали, поправь тест", tsProcessBadge)
	}

	// Кандидаты — всё, что вообще может прийти видом процесса: id агентов из
	// реестра и виды из маркеров командной строки. Спрашиваем саму IsAgentKind,
	// а не копию её списка: копия — ровно та ошибка, которую тест и ловит.
	seen := map[string]bool{}
	var candidates []string
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			candidates = append(candidates, k)
		}
	}
	for _, d := range agents.Registry {
		add(d.ID)
	}
	for _, m := range agentCmdlineMarkers {
		add(m.kind)
	}

	var missing []string
	for _, kind := range candidates {
		if !IsAgentKind(kind) {
			continue
		}
		if !strings.Contains(body, `"`+kind+`"`) {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		t.Errorf("клиент не считает агентами %s — допиши их в isAgentKind, ICONS и AgentKind в %s",
			strings.Join(missing, ", "), tsProcessBadge)
	}
}

// Иконка и подпись берутся по тому же виду: агент без иконки получает «▶», то
// есть выглядит как неизвестный процесс. Проверяем там же, одним чтением.
func TestClientHasIconForEveryAgentKind(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(tsProcessBadge))
	if err != nil {
		t.Fatalf("не прочитать клиентское зеркало %s: %v", tsProcessBadge, err)
	}
	icons, ok := sliceBetween(string(raw), "const ICONS", "};")
	if !ok {
		t.Fatalf("в %s не нашли ICONS", tsProcessBadge)
	}
	for _, d := range agents.Registry {
		if !IsAgentKind(d.ID) {
			continue
		}
		// Ключ пишется и как `grok:`, и как `"cursor-agent":` — обе формы.
		if !strings.Contains(icons, d.ID+":") && !strings.Contains(icons, `"`+d.ID+`":`) {
			t.Errorf("у агента %s нет иконки в ICONS (%s) — в интерфейсе он станет «▶»", d.ID, tsProcessBadge)
		}
	}
}

func sliceBetween(src, from, to string) (string, bool) {
	i := strings.Index(src, from)
	if i < 0 {
		return "", false
	}
	rest := src[i:]
	j := strings.Index(rest, to)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}
