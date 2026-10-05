package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgcontrol/internal/sessions"
)

func fakeHistory() []sessions.Message {
	return []sessions.Message{
		{Role: "user", Text: "Почини авторизацию в internal/auth"},
		{Role: "agent", Text: "Посмотрел код, проблема в проверке токена.", Tools: []string{"Read", "Grep"}},
		{Role: "user", Text: "Давай чинить"},
		{Role: "agent", Text: "Поправил validateToken, добавил тест. Осталось прогнать go test.", Tools: []string{"Edit", "Read"}},
	}
}

func TestBuildContinuationPacket_ContainsDelta(t *testing.T) {
	pkt := BuildContinuationPacket("claude", fakeHistory())

	for _, want := range []string{
		"Предыдущий агент: claude",
		"Сообщений в истории: 4",
		"Использованные инструменты: Read, Grep, Edit",
		"**user**: Почини авторизацию в internal/auth",
		"Поправил validateToken",
	} {
		if !strings.Contains(pkt, want) {
			t.Errorf("packet missing %q\n--- packet ---\n%s", want, pkt)
		}
	}
}

func TestBuildContinuationPacket_UnknownAgent(t *testing.T) {
	pkt := BuildContinuationPacket("", fakeHistory())
	if !strings.Contains(pkt, "Предыдущий агент: unknown") {
		t.Error("empty agent name should fall back to unknown")
	}
}

func TestBuildContinuationPacket_TruncatesLongMessage(t *testing.T) {
	long := strings.Repeat("x", maxContinuationMsgText+500)
	pkt := BuildContinuationPacket("codex", []sessions.Message{{Role: "agent", Text: long}})
	if strings.Contains(pkt, long) {
		t.Error("long message should be truncated")
	}
	if !strings.Contains(pkt, "…") {
		t.Error("truncated message should carry an ellipsis")
	}
}

func TestBuildContinuationPacket_CapDropsMiddle(t *testing.T) {
	var msgs []sessions.Message
	msgs = append(msgs, sessions.Message{Role: "user", Text: "ЗАДАЧА: сделать фичу"})
	for i := 0; i < 50; i++ {
		msgs = append(msgs, sessions.Message{Role: "agent", Text: strings.Repeat("a", 300)})
	}
	msgs = append(msgs, sessions.Message{Role: "agent", Text: "ПОСЛЕДНЕЕ: осталось дописать тест"})

	const cap = 4000
	pkt := buildContinuationPacket("claude", msgs, cap)

	if len(pkt) > cap+128 { // +маркер обрезки за пределами капа
		t.Errorf("packet over cap: %d > %d", len(pkt), cap)
	}
	if !strings.Contains(pkt, "ЗАДАЧА: сделать фичу") {
		t.Error("first user message (task) must survive truncation")
	}
	if !strings.Contains(pkt, "ПОСЛЕДНЕЕ: осталось дописать тест") {
		t.Error("latest messages (what remains) must survive truncation")
	}
	if !strings.Contains(pkt, "сообщений опущено") && !strings.Contains(pkt, "пакет обрезан") {
		t.Error("truncation marker expected")
	}
}

func TestWriteContinuationPacket(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteContinuationPacket("claude", fakeHistory(), dir)
	if err != nil {
		t.Fatalf("WriteContinuationPacket: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("packet written to unexpected dir: %s", path)
	}
	if !strings.HasPrefix(filepath.Base(path), "continuation-claude-") {
		t.Errorf("unexpected file name: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read packet: %v", err)
	}
	if !strings.Contains(string(data), "Почини авторизацию") {
		t.Error("written packet does not contain history delta")
	}
}

func TestWithContinuation_EmptyPath(t *testing.T) {
	if got := withContinuation("сделай X", ""); got != "сделай X" {
		t.Errorf("empty path must leave prompt unchanged, got %q", got)
	}
}

func TestWithContinuation_PathInPromptNotContent(t *testing.T) {
	got := withContinuation("сделай X", `C:\tmp\continuation-claude.md`)
	if !strings.Contains(got, "сделай X") {
		t.Error("original prompt must be preserved")
	}
	if !strings.Contains(got, `C:\tmp\continuation-claude.md`) {
		t.Error("prompt must contain the packet file path")
	}
	if !strings.Contains(got, "продолжение работы другого агента") {
		t.Error("prompt must explain what the file is")
	}
}

func TestCLIAgentBuildCmd_WithContinuation(t *testing.T) {
	desc := &AgentDescriptor{
		ID:         "kimi",
		RunArgs:    []string{"-p", "{prompt}"},
		ExtraFlags: nil,
	}
	a := &CLIAgent{Descriptor: desc}
	prompt := withContinuation("исправь баг", "/tmp/packet.md")
	args := a.buildCmd(prompt, "")

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/tmp/packet.md") {
		t.Errorf("built command must carry the packet path, args: %v", args)
	}
	if !strings.Contains(joined, "исправь баг") {
		t.Errorf("built command must carry the original prompt, args: %v", args)
	}
}
