package bot

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol/internal/agents"
)

// cmdDiscover scans the machine for existing AI agent sessions.
func (tb *Bot) cmdDiscover(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}

	discovered := agents.DiscoverAll()
	if len(discovered) == 0 {
		tb.reply(ctx, update, "🔍 Не найдено запущенных сессий агентов на этом компьютере.")
		return
	}

	// Check which are already imported
	existing := tb.store.List(int(uid))
	existingIDs := make(map[string]bool)
	for _, sess := range existing {
		if sess.AgentSessionID != "" {
			existingIDs[sess.AgentSessionID] = true
		}
	}

	lines := []string{fmt.Sprintf("🔍 <b>Найдено %d сессий:</b>\n", len(discovered))}
	var buttons [][]models.InlineKeyboardButton

	for i, ds := range discovered {
		desc := agents.GetDescriptor(ds.AgentType)
		icon := "🤖"
		if desc != nil {
			icon = desc.Icon
		}

		status := "🟢"
		if !ds.IsAlive {
			status = "🔴"
		}

		imported := ""
		if existingIDs[ds.SessionID] {
			imported = " ✅"
		}

		age := agents.FormatAge(ds.StartedAt)
		shortCwd := ds.Cwd
		if len(shortCwd) > 40 {
			shortCwd = "…" + shortCwd[len(shortCwd)-39:]
		}

		lines = append(lines, fmt.Sprintf(
			"%s %s <b>%s</b>%s\n   %s PID:%d | %s\n   <code>%s</code>",
			icon, ds.AgentType, esc(ds.Name), imported,
			status, ds.PID, age,
			esc(shortCwd)))

		// Add import button if not already imported
		if !existingIDs[ds.SessionID] {
			btnText := fmt.Sprintf("📥 %s — %s", ds.Name, ds.AgentType)
			if len(btnText) > 40 {
				btnText = btnText[:37] + "..."
			}
			callbackData := fmt.Sprintf("dc:imp:%d", i)
			buttons = append(buttons, []models.InlineKeyboardButton{
				{Text: btnText, CallbackData: callbackData},
			})
		}
	}

	// Store discovered sessions for callback reference
	tb.setDiscovered(uid, discovered)

	text := strings.Join(lines, "\n")
	if len(buttons) == 0 {
		text += "\n\n✅ Все сессии уже импортированы."
	}

	// Add "import all" button
	if len(buttons) > 1 {
		buttons = append(buttons, []models.InlineKeyboardButton{
			{Text: "📥 Импортировать все", CallbackData: "dc:all"},
		})
	}

	kb := &models.InlineKeyboardMarkup{InlineKeyboard: buttons}
	tb.reply(ctx, update, text, kb)
}

// onDiscoverCB handles discover callback buttons.
func (tb *Bot) onDiscoverCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data

	discovered := tb.getDiscovered(uid)
	if discovered == nil {
		tb.answerCB(ctx, update, "❌ Данные устарели. Запустите /discover снова.")
		return
	}

	switch {
	case strings.HasPrefix(data, "dc:imp:"):
		// Import single session
		idxStr := data[7:]
		idx := 0
		for _, c := range idxStr {
			idx = idx*10 + int(c-'0')
		}
		if idx < 0 || idx >= len(discovered) {
			tb.answerCB(ctx, update, "❌ Неверный индекс.")
			return
		}
		ds := discovered[idx]
		name := tb.importSession(uid, ds)
		if name == "" {
			tb.answerCB(ctx, update, "❌ Не удалось импортировать сессию.")
			return
		}
		tb.answerCB(ctx, update, fmt.Sprintf(
			"✅ Импортирована: <b>%s</b>\n🤖 %s | 📂 <code>%s</code>\n🔑 <code>%s</code>",
			esc(name), ds.AgentType, esc(ds.Cwd), esc(ds.SessionID[:8]+"…")))
		tb.sendKB(ctx, getChatID(update), uid,
			fmt.Sprintf("📌 Активна: <b>%s</b>. Отправляйте текст.", esc(name)))

	case data == "dc:all":
		// Import all sessions
		imported := 0
		var names []string
		for _, ds := range discovered {
			// Skip already imported
			if tb.isSessionImported(uid, ds.SessionID) {
				continue
			}
			name := tb.importSession(uid, ds)
			if name != "" {
				imported++
				names = append(names, name)
			}
		}
		if imported == 0 {
			tb.answerCB(ctx, update, "✅ Все сессии уже импортированы.")
			return
		}
		tb.answerCB(ctx, update, fmt.Sprintf(
			"✅ Импортировано %d сессий:\n%s",
			imported, strings.Join(names, ", ")))
		tb.sendKB(ctx, getChatID(update), uid,
			fmt.Sprintf("📥 Импортировано: %d", imported))
	}
}

// importSession creates a tgcontrol session from a discovered agent session.
func (tb *Bot) importSession(uid int64, ds agents.DiscoveredSession) string {
	// Generate unique name
	baseName := fmt.Sprintf("%s-%s", ds.AgentType, ds.Name)
	// Sanitize: replace spaces, limit length
	baseName = strings.ReplaceAll(baseName, " ", "-")
	if len(baseName) > maxNameLen {
		baseName = baseName[:maxNameLen]
	}

	name := baseName
	for i := 2; tb.store.Get(int(uid), name) != nil; i++ {
		name = fmt.Sprintf("%s-%d", baseName[:min(len(baseName), maxNameLen-3)], i)
	}

	sess, err := tb.store.Create(int(uid), name, ds.AgentType, ds.Cwd)
	if err != nil {
		return ""
	}

	// Set the agent session ID for resume
	tb.store.UpdateAgentSessionID(int(uid), name, ds.SessionID)

	// Mark as alive if process is running
	if ds.IsAlive {
		tb.store.SetStatus(int(uid), name, "alive")
	} else {
		tb.store.SetStatus(int(uid), name, "dead")
	}

	_ = sess
	return name
}

// isSessionImported checks if a session ID is already imported.
func (tb *Bot) isSessionImported(uid int64, sessionID string) bool {
	for _, sess := range tb.store.List(int(uid)) {
		if sess.AgentSessionID == sessionID {
			return true
		}
	}
	return false
}

// ── Discovered sessions cache (per-user, transient) ──────────────────

func (tb *Bot) setDiscovered(uid int64, sessions []agents.DiscoveredSession) {
	tb.discMu.Lock()
	defer tb.discMu.Unlock()
	if tb.discoveredCache == nil {
		tb.discoveredCache = make(map[int64][]agents.DiscoveredSession)
	}
	tb.discoveredCache[uid] = sessions
}

func (tb *Bot) getDiscovered(uid int64) []agents.DiscoveredSession {
	tb.discMu.RLock()
	defer tb.discMu.RUnlock()
	if tb.discoveredCache == nil {
		return nil
	}
	return tb.discoveredCache[uid]
}
