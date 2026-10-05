package agents

import (
	"encoding/json"
	"testing"
)

// Канал собственной истории агента обязан ДОЕЗЖАТЬ ДО КЛИЕНТА и быть тем, что
// агент печатает про себя сам.
//
// Codex на экране пишет: «Earlier messages are available — press ctrl + t to
// view the full transcript» (скриншот владельца 08.09). Ни колесо, ни PgUp
// этого не открывают, поэтому режим «Авто» приходил к «листать нечем» при живой
// истории внутри агента. Знание живёт в реестре — клиент его не хардкодит.
func TestCodexDeclaresTranscriptKey(t *testing.T) {
	var codex *AgentDescriptor
	for _, a := range Registry {
		if a.ID == "codex" {
			codex = a
			break
		}
	}
	if codex == nil {
		t.Fatal("в реестре нет агента codex")
	}
	if codex.HistoryKey != "\x14" {
		t.Fatalf("Codex открывает историю по Ctrl+T (0x14), в реестре %q", codex.HistoryKey)
	}
	if codex.HistoryLabel != "Ctrl+T" {
		t.Fatalf("подпись канала должна быть человеческой («Ctrl+T»), получено %q", codex.HistoryLabel)
	}

	// ⚠ Поле обязано быть в JSON: клиент берёт его из /api/agents, и `json:"-"`
	// здесь означал бы «знание есть, но интерфейс о нём не узнает».
	raw, err := json.Marshal(codex)
	if err != nil {
		t.Fatalf("реестр не сериализуется: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("ответ не разобрался: %v", err)
	}
	if out["history_key"] != "\x14" {
		t.Fatalf("history_key не доехал до клиента: %#v", out["history_key"])
	}
	if out["history_label"] != "Ctrl+T" {
		t.Fatalf("history_label не доехал до клиента: %#v", out["history_label"])
	}
}

// Молчание — законный ответ. Пусто значит «не знаем, чем этот агент открывает
// свою историю», и тогда остаётся прежняя проба фактом. Выдуманная клавиша
// хуже отсутствующей: она уйдёт в чужой агент и что-нибудь там нажмёт.
func TestUnknownAgentsDeclareNoHistoryKey(t *testing.T) {
	for _, a := range Registry {
		if a.HistoryKey == "" {
			continue
		}
		if a.HistoryLabel == "" {
			t.Fatalf("агент %s объявил клавишу %q без человеческой подписи", a.ID, a.HistoryKey)
		}
		// Управляющий байт, а не текст: иначе это попадёт в ввод агента буквами.
		if len(a.HistoryKey) > 8 {
			t.Fatalf("агент %s объявил подозрительно длинный канал %q", a.ID, a.HistoryKey)
		}
	}
}
