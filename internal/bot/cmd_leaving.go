package bot

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol/internal/pty"
)

// cmdLeaving runs the "ready to leave home?" self-test:
// queries the web server's healthcheck and a quick PTY input-waiting summary,
// returns one Telegram message with a checklist.
func (tb *Bot) cmdLeaving(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	if tb.webServer == nil {
		tb.reply(ctx, update, "❌ Web server недоступен.")
		return
	}

	report := tb.webServer.HealthSnapshot(ctx)

	// PTY sessions awaiting input — read directly from the manager.
	waiting := 0
	if pm := tb.webServer.PtyManager(); pm != nil {
		for _, s := range pm.List() {
			if s.Alive && pty.IsAgentKind(s.AgentKind) {
				waiting++
			}
		}
	}

	var b2 strings.Builder
	if report.OK {
		b2.WriteString("✅ <b>Готов к удалённой работе</b>\n\n")
	} else {
		b2.WriteString("⚠️ <b>Есть нерешённые проблемы</b>\n\n")
	}

	order := []string{"app", "bot", "tunnel", "public", "pty", "notify"}
	labels := map[string]string{
		"app":    "TGControl",
		"bot":    "Telegram-бот",
		"tunnel": "Публичный URL",
		"public": "Доступ из интернета",
		"pty":    "Терминалы",
		"notify": "Уведомления",
	}
	for _, k := range order {
		c, ok := report.Checks[k]
		if !ok {
			continue
		}
		icon := "🔴"
		if c.OK {
			icon = "🟢"
		} else if k == "public" {
			icon = "🟡"
		}
		line := fmt.Sprintf("%s <b>%s</b>", icon, labels[k])
		if c.Note != "" {
			line += " — " + esc(c.Note)
		}
		b2.WriteString(line + "\n")
	}

	if waiting > 0 {
		b2.WriteString(fmt.Sprintf("\n⏳ Активных AI-агентов: <b>%d</b> — проверьте, не ждут ли подтверждения.\n", waiting))
	}
	b2.WriteString(fmt.Sprintf("\n<i>Проверка заняла %d мс.</i>", report.TookMS))

	tb.reply(ctx, update, b2.String())
}
