package web

import (
	"strings"
	"testing"
	"time"

	"tgcontrol/internal/pty"
)

func TestSummaryMessageFinished(t *testing.T) {
	got := summaryMessage(pty.SummaryEvent{
		Kind:      pty.SummaryFinished,
		AgentKind: "claude",
		Name:      "Разработка",
		CWD:       `C:\Users\user\Desktop\TGControl-ALL`,
		Duration:  4*time.Minute + 20*time.Second,
	}, "Обновил README и прогнал тесты, ошибок нет.")

	for _, want := range []string{
		"Claude закончил",
		"4 мин",
		"TGControl-ALL",            // «по какому проекту» — первый вопрос человека
		"«Разработка»",             // имя терминала он задавал сам
		"Обновил README и прогнал", // ради этой строки всё и делается
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("в уведомлении нет %q:\n%s", want, got)
		}
	}
}

// Модель промолчала или квота кончилась — про завершение работы всё равно
// сообщаем: тишина вместо новости хуже короткой новости.
func TestSummaryMessageWithoutSummaryStillTellsTheNews(t *testing.T) {
	got := summaryMessage(pty.SummaryEvent{
		Kind: pty.SummaryFinished, AgentKind: "codex", CWD: "/home/user/api", Duration: 90 * time.Second,
	}, "")
	if !strings.Contains(got, "Codex закончил") || !strings.Contains(got, "api") {
		t.Fatalf("новость потерялась: %s", got)
	}
	if strings.HasSuffix(strings.TrimSpace(got), "\n") {
		t.Fatalf("пустой хвост вместо выжимки: %q", got)
	}
}

func TestSummaryMessageQuestion(t *testing.T) {
	got := summaryMessage(pty.SummaryEvent{
		Kind: pty.SummaryQuestion, AgentKind: "gemini", Name: "сервер", CWD: "/srv/app",
	}, "Разрешить перезапуск сервиса?")
	if !strings.Contains(got, "Gemini ждёт ответа") {
		t.Fatalf("не назван повод: %s", got)
	}
	if !strings.Contains(got, "Разрешить перезапуск сервиса?") {
		t.Fatalf("сам вопрос не показан: %s", got)
	}
	if strings.Contains(got, "закончил") {
		t.Fatalf("вопрос выдан за завершение работы: %s", got)
	}
}

func TestProjectName(t *testing.T) {
	cases := map[string]string{
		`C:\Users\user\Desktop\Разаработки\TGControl-ALL`: "TGControl-ALL",
		"/home/user/projects/api/":                        "api",
		"ssh:root@vps-4":                                  "ssh:root@vps-4",
		"":                                                "",
	}
	for in, want := range cases {
		if got := projectName(in); got != want {
			t.Fatalf("projectName(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		45 * time.Second:                            "45 с",
		4*time.Minute + 20*time.Second:              "4 мин",
		2 * time.Hour:                               "2 ч",
		3*time.Hour + 7*time.Minute:                 "3 ч 7 мин",
		time.Hour + 59*time.Minute + 30*time.Second: "1 ч 59 мин",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Fatalf("humanDuration(%v) = %q, ожидалось %q", in, got, want)
		}
	}
}
