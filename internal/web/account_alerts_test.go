package web

import (
	"testing"

	"tgcontrol/internal/aiusage"
)

func window(id string, used float64) aiusage.Window {
	return aiusage.Window{ID: id, UsedPercent: used}
}

// Остаток считается по пятичасовому окну; «процента нет» — это −1, а не 0:
// нулём мы бы сказали «всё израсходовано», чего не знаем.
func TestLeftPercentHonestAboutUnknown(t *testing.T) {
	full := aiusage.Provider{Windows: []aiusage.Window{window("seven_day", 10), window("five_hour", 82)}}
	if got := leftPercent(full); got != 18 {
		t.Fatalf("осталось %v, ждали 18", got)
	}
	minutes := int64(300)
	byDuration := aiusage.Provider{Windows: []aiusage.Window{{ID: "codex:primary", UsedPercent: 44, DurationMinutes: &minutes}}}
	if got := leftPercent(byDuration); got != 56 {
		t.Fatalf("осталось %v, ждали 56", got)
	}
	if got := leftPercent(aiusage.Provider{}); got != -1 {
		t.Fatalf("без окон ждали -1 (неизвестно), получили %v", got)
	}
}

// Имя аккаунта в сообщении: сначала данное человеком, потом почта вендора, и
// только потом «основной». Безымянное «осталось 9%» не отвечает на вопрос «у
// кого именно».
func TestAccountNamePrefersHumanLabel(t *testing.T) {
	if got := accountName(aiusage.Provider{AccountLabel: "рабочий", Account: "a@b.c"}); got != "рабочий" {
		t.Fatalf("имя %q", got)
	}
	if got := accountName(aiusage.Provider{Account: "a@b.c"}); got != "a@b.c" {
		t.Fatalf("имя %q", got)
	}
	if got := accountName(aiusage.Provider{}); got != "основной" {
		t.Fatalf("имя %q", got)
	}
}
