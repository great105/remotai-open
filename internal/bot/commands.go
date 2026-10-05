package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/config"
)

func (tb *Bot) cmdStart(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: getChatID(update), Text: "🚫 Нет доступа.",
		})
		return
	}
	tb.sendKB(ctx, getChatID(update), uid,
		"👋 Добро пожаловать в <b>TGControl</b>!\n\n"+
			"Управляйте CLI-агентами (Claude, Codex, Shell) прямо из Telegram.\n\n"+
			"Используйте кнопки внизу или /help для справки.")
}

func (tb *Bot) cmdHelp(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	tb.reply(ctx, update, helpText())
}

func (tb *Bot) cmdNew(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	args := cmdArgs(update.Message.Text)

	if len(args) > 0 {
		agentType := strings.ToLower(args[0])
		if !agents.IsAvailable(agentType) {
			tb.reply(ctx, update, fmt.Sprintf(
				"❌ Неизвестный агент <code>%s</code>. Доступные: %s",
				esc(agentType), agentIDs()))
			return
		}
		name := tb.autoName(uid, agentType)
		if len(args) > 1 {
			name = args[1]
		}
		cwd := ""
		if len(args) > 2 {
			cwd = strings.Join(args[2:], " ")
		} else {
			if sess := tb.store.GetActive(int(uid)); sess != nil {
				cwd = sess.Cwd
			} else {
				cwd, _ = os.Getwd()
			}
		}
		tb.createSession(ctx, update, uid, name, agentType, cwd)
		return
	}

	// Interactive wizard
	tb.clearWizard(uid)
	kb := agentPickerKB("wz:ag:")
	tb.reply(ctx, update, "➕ <b>Создание сессии</b>\n\nШаг 1/3. Выберите агента:", kb)
}

func (tb *Bot) cmdProject(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	tb.clearWizard(uid)
	kb := agentPickerKB("wz:pa:")
	tb.reply(ctx, update, "🆕 <b>Создание проекта</b>\n\nШаг 1/4. Выберите агента:", kb)
}

func (tb *Bot) cmdList(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	tb.showSessions(ctx, update, uid, false)
}

func (tb *Bot) cmdUse(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	args := cmdArgs(update.Message.Text)
	if len(args) == 0 {
		tb.showSessions(ctx, update, uid, false)
		return
	}
	name := args[0]
	sess, err := tb.store.Switch(int(uid), name)
	if err != nil {
		tb.reply(ctx, update, "❌ "+err.Error())
		return
	}
	tb.clearUploadTarget(uid)
	tb.sendKB(ctx, getChatID(update), uid,
		fmt.Sprintf("✅ Переключено на <b>%s</b>\n🤖 Агент: <code>%s</code>\n📂 Папка: <code>%s</code>",
			esc(name), sess.AgentType, esc(sess.Cwd)))
}

func (tb *Bot) cmdClose(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	args := cmdArgs(update.Message.Text)
	var name string
	if len(args) > 0 {
		name = args[0]
	} else {
		sess := tb.store.GetActive(int(uid))
		if sess == nil {
			tb.reply(ctx, update, "❌ Нет активной сессии.")
			return
		}
		name = sess.Name
	}
	// Check exists
	if tb.store.Get(int(uid), name) == nil {
		tb.reply(ctx, update, fmt.Sprintf("❌ Сессия <b>%s</b> не найдена", esc(name)))
		return
	}
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "✅ Да, закрыть", CallbackData: "se:cc:" + name},
			{Text: "↩️ Отмена", CallbackData: "xa:cancel"},
		}},
	}
	tb.reply(ctx, update, fmt.Sprintf("Закрыть сессию <b>%s</b>?", esc(name)), kb)
}

func (tb *Bot) cmdStatus(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	tb.showStatus(ctx, update, uid, false)
}

func (tb *Bot) cmdCwd(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		tb.reply(ctx, update, "❌ Нет активной сессии.")
		return
	}
	args := cmdArgs(update.Message.Text)
	if len(args) == 0 {
		startPath := sess.Cwd
		if _, err := os.Stat(startPath); err != nil {
			startPath, _ = os.Getwd()
		}
		text, markup := tb.buildFolderBrowser(uid, startPath, 0)
		tb.reply(ctx, update, text, markup)
		return
	}
	newCwd := strings.Join(args, " ")
	if !filepath.IsAbs(newCwd) {
		newCwd = filepath.Join(sess.Cwd, newCwd)
	}
	newCwd = filepath.Clean(newCwd)
	if _, err := os.Stat(newCwd); err != nil {
		tb.reply(ctx, update, fmt.Sprintf("❌ Директория не существует: <code>%s</code>", esc(newCwd)))
		return
	}
	tb.store.SetCwd(int(uid), sess.Name, newCwd)
	tb.reply(ctx, update, fmt.Sprintf("✅ Папка изменена: <code>%s</code>", esc(newCwd)))
}

func (tb *Bot) cmdRename(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		tb.reply(ctx, update, "❌ Нет активной сессии.")
		return
	}
	args := cmdArgs(update.Message.Text)
	if len(args) == 0 {
		tb.setWizard(uid, &wizard{Type: "rename", OldName: sess.Name})
		tb.reply(ctx, update, fmt.Sprintf(
			"✏️ Введите новое имя для <b>%s</b> (макс. %d символов):",
			esc(sess.Name), maxNameLen))
		return
	}
	newName := args[0]
	if len(newName) > maxNameLen {
		tb.reply(ctx, update, fmt.Sprintf("❌ Имя слишком длинное (макс. %d символов).", maxNameLen))
		return
	}
	if err := tb.store.Rename(int(uid), sess.Name, newName); err != nil {
		tb.reply(ctx, update, "❌ "+err.Error())
		return
	}
	tb.history.Rename(int(uid), sess.Name, newName)
	tb.sendKB(ctx, getChatID(update), uid,
		fmt.Sprintf("✅ Переименована: <b>%s</b>", esc(newName)))
}

func (tb *Bot) cmdLast(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	prev := tb.store.GetPreviousName(int(uid))
	if prev == "" {
		tb.reply(ctx, update, "❌ Нет предыдущей сессии.")
		return
	}
	sess, err := tb.store.Switch(int(uid), prev)
	if err != nil {
		tb.reply(ctx, update, "❌ "+err.Error())
		return
	}
	tb.clearUploadTarget(uid)
	tb.sendKB(ctx, getChatID(update), uid,
		fmt.Sprintf("✅ Переключено на <b>%s</b>\n🤖 Агент: <code>%s</code>\n📂 Папка: <code>%s</code>",
			esc(prev), sess.AgentType, esc(sess.Cwd)))
}

func (tb *Bot) cmdClone(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		tb.reply(ctx, update, "❌ Нет активной сессии.")
		return
	}
	args := cmdArgs(update.Message.Text)
	newName := tb.autoName(uid, sess.AgentType)
	if len(args) > 0 {
		newName = args[0]
	}
	clone, err := tb.store.Clone(int(uid), sess.Name, newName)
	if err != nil {
		tb.reply(ctx, update, "❌ "+err.Error())
		return
	}
	tb.sendKB(ctx, getChatID(update), uid,
		fmt.Sprintf("✅ Клон: <b>%s</b>\n🤖 Агент: <code>%s</code>\n📂 Папка: <code>%s</code>",
			esc(newName), clone.AgentType, esc(clone.Cwd)))
}

func (tb *Bot) cmdFiles(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		tb.reply(ctx, update, "❌ Нет активной сессии.")
		return
	}
	if _, err := os.Stat(sess.Cwd); err != nil {
		tb.reply(ctx, update, fmt.Sprintf("❌ Папка не существует: <code>%s</code>", esc(sess.Cwd)))
		return
	}
	text, markup := tb.buildFileManager(uid, sess.Cwd, 0)
	tb.reply(ctx, update, text, markup)
}

func (tb *Bot) cmdConfig(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	log.Printf("[cmdConfig] called, uid=%d, text=%q", uid, update.Message.Text)
	if !tb.authorized(uid) {
		return
	}
	tb.showConfig(ctx, update, false)
}

func (tb *Bot) cmdClear(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		tb.reply(ctx, update, "📋 Нет активной сессии.")
		return
	}
	tb.store.UpdateAgentSessionID(int(uid), sess.Name, "")
	tb.history.Clear(int(uid), sess.Name)
	tb.sendKB(ctx, getChatID(update), uid,
		fmt.Sprintf("🗑 Сессия <b>%s</b> очищена. Следующий запрос начнёт новый диалог.", esc(sess.Name)))
}

func (tb *Bot) cmdCompact(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		tb.reply(ctx, update, "📋 Нет активной сессии.")
		return
	}
	tb.store.UpdateAgentSessionID(int(uid), sess.Name, "")
	tb.sendKB(ctx, getChatID(update), uid,
		fmt.Sprintf("📦 Контекст сессии <b>%s</b> сброшен. Следующий запрос начнёт новый диалог.", esc(sess.Name)))
}

func (tb *Bot) cmdCost(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		tb.reply(ctx, update, "📋 Нет активной сессии.")
		return
	}
	cost := tb.history.GetSessionCost(int(uid), sess.Name)
	msgCount := tb.history.MessageCount(int(uid), sess.Name)
	tb.reply(ctx, update, fmt.Sprintf(
		"💰 <b>Стоимость: %s</b>\n\n"+
			"Итого: <code>$%.4f</code>\n"+
			"Сообщений: %d",
		esc(sess.Name), cost, msgCount))
}

func (tb *Bot) cmdMode(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	cfg := config.Get()
	var rows [][]models.InlineKeyboardButton
	for _, m := range claudePermModes {
		mark := ""
		if m.Key == cfg.ClaudePermissionMode {
			mark = "✅ "
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("%s%s — %s", mark, m.Key, m.Label), CallbackData: "cf:sp:" + m.Key},
		})
	}
	tb.reply(ctx, update, "🔒 <b>Режим разрешений Claude:</b>",
		&models.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (tb *Bot) cmdAgent(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		tb.reply(ctx, update, "📋 Нет активной сессии.")
		return
	}
	kb := agentPickerKB("st:sa:")
	tb.reply(ctx, update, fmt.Sprintf(
		"🤖 <b>Сменить агента</b>\nТекущий: <code>%s</code>", sess.AgentType), kb)
}

func (tb *Bot) cmdContinue(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	update.Message.Text = "continue"
	tb.sendToAgent(ctx, update)
}

func (tb *Bot) cmdRetry(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		tb.reply(ctx, update, "📋 Нет активной сессии.")
		return
	}
	lastMsg := tb.history.GetLastUserMessage(int(uid), sess.Name)
	if lastMsg == "" {
		tb.reply(ctx, update, "❌ Нет предыдущего сообщения для повтора.")
		return
	}
	update.Message.Text = lastMsg
	tb.sendToAgent(ctx, update)
}

func (tb *Bot) cmdCancel(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	hadWizard := tb.getWizard(uid) != nil
	tb.clearWizard(uid)

	// Try to stop active agent for any session
	sess := tb.store.GetActive(int(uid))
	if sess != nil {
		key := stopKey(uid, sess.Name)
		tb.stopMu.Lock()
		ch, ok := tb.stopChans[key]
		if ok {
			delete(tb.stopChans, key)
		}
		tb.stopMu.Unlock()
		if ok {
			close(ch)
			tb.reply(ctx, update, "🛑 Запрос прерван и состояние сброшено.")
			return
		}
	}

	if hadWizard {
		tb.reply(ctx, update, "↩️ Отменено.")
	} else {
		tb.reply(ctx, update, "Нечего отменять.")
	}
}

// ── Shared views ─────────────────────────────────────────────────────

func (tb *Bot) showSessions(ctx context.Context, update *models.Update, uid int64, isCallback bool) {
	sessList := tb.store.List(int(uid))
	if len(sessList) == 0 {
		text := "📋 Нет сессий. Создайте с помощью /new"
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "➕ Создать сессию", CallbackData: "se:new"},
			}},
		}
		if isCallback {
			tb.answerCB(ctx, update, text, kb)
		} else {
			tb.reply(ctx, update, text, kb)
		}
		return
	}

	activeName := tb.store.GetActiveName(int(uid))
	lines := []string{"📋 <b>Сессии:</b>\n"}
	var buttons [][]models.InlineKeyboardButton
	for _, sess := range sessList {
		mark := "▫️"
		if sess.Name == activeName {
			mark = "✅"
		}
		busy := ""
		if sess.IsBusy {
			busy = " ⏳"
		}
		lines = append(lines, fmt.Sprintf("%s <b>%s</b> (%s) — <code>%s</code>%s",
			mark, esc(sess.Name), sess.AgentType, esc(sess.Cwd), busy))
		buttons = append(buttons, []models.InlineKeyboardButton{
			{Text: "▶ " + sess.Name, CallbackData: "se:sw:" + sess.Name},
			{Text: "❌", CallbackData: "se:cl:" + sess.Name},
		})
	}

	text := strings.Join(lines, "\n")
	kb := &models.InlineKeyboardMarkup{InlineKeyboard: buttons}
	if isCallback {
		tb.answerCB(ctx, update, text, kb)
	} else {
		tb.reply(ctx, update, text, kb)
	}
}

func (tb *Bot) showStatus(ctx context.Context, update *models.Update, uid int64, isCallback bool) {
	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		text := "📋 Нет активной сессии. Создайте с помощью /new"
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "➕ Создать сессию", CallbackData: "se:new"},
			}},
		}
		if isCallback {
			tb.answerCB(ctx, update, text, kb)
		} else {
			tb.reply(ctx, update, text, kb)
		}
		return
	}

	busyText := "нет"
	if sess.IsBusy {
		busyText = "⏳ да"
	}
	sessionID := sess.AgentSessionID
	if sessionID == "" {
		sessionID = "нет"
	}

	cost := tb.history.GetSessionCost(int(uid), sess.Name)
	msgCount := tb.history.MessageCount(int(uid), sess.Name)

	// Session mode & status
	mode := sess.Mode
	if mode == "" {
		mode = "persistent"
	}
	status := sess.Status
	if status == "" {
		status = "alive"
	}
	statusIcon := "🟢"
	switch status {
	case "dead":
		statusIcon = "🔴"
	case "not-ready":
		statusIcon = "🟡"
	}

	text := fmt.Sprintf(
		"📊 <b>Текущая сессия</b>\n\n"+
			"📌 Имя: <b>%s</b>\n"+
			"🤖 Агент: <code>%s</code>\n"+
			"📂 Папка: <code>%s</code>\n"+
			"🔑 ID: <code>%s</code>\n"+
			"%s Статус: <code>%s</code> | Режим: <code>%s</code>\n"+
			"⏳ Занята: %s\n"+
			"💰 Стоимость: <code>$%.4f</code> (%d сообщ.)",
		esc(sess.Name), sess.AgentType, esc(sess.Cwd), esc(sessionID),
		statusIcon, status, mode, busyText, cost, msgCount)

	// Show permission mode if set
	if sess.PermissionMode != "" {
		text += fmt.Sprintf("\n🔒 Права: <code>%s</code>", esc(sess.PermissionMode))
	}
	// Show topic binding
	if sess.TopicID != 0 {
		text += fmt.Sprintf("\n🔗 Топик: <code>%d</code>", sess.TopicID)
	}
	// Show TTL
	if sess.TTLMinutes > 0 {
		text += fmt.Sprintf("\n⏱ TTL: %d мин.", sess.TTLMinutes)
	}

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "✏️ Имя", CallbackData: "st:ren"},
				{Text: "🔄 Агент", CallbackData: "st:ag"},
				{Text: "📋 Клон", CallbackData: "st:clone"},
			},
			{
				{Text: "📁 Файлы", CallbackData: "fm:open"},
				{Text: "📂 Папка", CallbackData: "st:cwd"},
				{Text: "❌ Закрыть", CallbackData: "se:cl:" + sess.Name},
			},
		},
	}

	if isCallback {
		tb.answerCB(ctx, update, text, kb)
	} else {
		tb.reply(ctx, update, text, kb)
	}
}

func (tb *Bot) showConfig(ctx context.Context, update *models.Update, isCallback bool) {
	cfg := config.Get()
	rows := [][]models.InlineKeyboardButton{
		{
			{Text: "📦 Claude модель", CallbackData: "cf:model"},
			{Text: "🔒 Claude режим", CallbackData: "cf:mode"},
		},
	}
	text := fmt.Sprintf(
		"⚙️ <b>Настройки</b>\n\n"+
			"<b>Claude</b>\n"+
			"📦 Модель: <code>%s</code>\n"+
			"🔒 Режим: <code>%s</code>",
		esc(cfg.ClaudeModel), esc(cfg.ClaudePermissionMode))

	if agents.IsAvailable("codex") {
		text += fmt.Sprintf(
			"\n\n<b>Codex</b>\n"+
				"📦 Модель: <code>%s</code>\n"+
				"🧠 Reasoning: <code>%s</code>\n"+
				"🔒 Режим: <code>%s</code>",
			esc(cfg.CodexModel), esc(cfg.CodexReasoning), esc(cfg.CodexApprovalMode))
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "📦 Codex модель", CallbackData: "cf:cx_model"},
			{Text: "🧠 Reasoning", CallbackData: "cf:cx_reason"},
		})
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "🔒 Codex режим", CallbackData: "cf:cx_mode"},
		})
	}

	kb := &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	if isCallback {
		tb.answerCB(ctx, update, text, kb)
	} else {
		tb.reply(ctx, update, text, kb)
	}
}

// ── Session creation ─────────────────────────────────────────────────

func (tb *Bot) createSession(ctx context.Context, update *models.Update, uid int64,
	name, agentType, cwd string) bool {
	if len(name) > maxNameLen {
		tb.reply(ctx, update, fmt.Sprintf("❌ Имя слишком длинное (макс. %d символов).", maxNameLen))
		return false
	}
	if !agents.IsAvailable(agentType) {
		tb.reply(ctx, update, fmt.Sprintf(
			"❌ Неизвестный агент <code>%s</code>. Доступные: %s",
			esc(agentType), agentIDs()))
		return false
	}
	if !filepath.IsAbs(cwd) {
		base, _ := os.Getwd()
		cwd = filepath.Join(base, cwd)
	}
	cwd = filepath.Clean(cwd)
	if _, err := os.Stat(cwd); err != nil {
		tb.reply(ctx, update, fmt.Sprintf("❌ Директория не существует: <code>%s</code>", esc(cwd)))
		return false
	}

	_, err := tb.store.Create(int(uid), name, agentType, cwd)
	if err != nil {
		tb.reply(ctx, update, fmt.Sprintf("❌ Сессия <b>%s</b> уже существует.", esc(name)))
		return false
	}

	kb := tb.buildMainKeyboard(uid)
	tb.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: getChatID(update),
		Text: fmt.Sprintf(
			"✅ Сессия <b>%s</b> создана\n"+
				"🤖 Агент: <code>%s</code>\n"+
				"📂 Папка: <code>%s</code>\n\n"+
				"💬 Отправьте текст — он уйдёт агенту.",
			esc(name), agentType, esc(cwd)),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: kb,
	})
	return true
}
