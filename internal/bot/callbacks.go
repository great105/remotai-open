package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/config"
)

// ── 1. se: Sessions ──────────────────────────────────────────────────

func (tb *Bot) onSessionCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data

	switch {
	case data == "se:new":
		kb := agentPickerKB("wz:ag:")
		tb.answerCB(ctx, update, "➕ <b>Создание сессии</b>\n\nШаг 1/3. Выберите агента:", kb)

	case data == "se:ls":
		tb.showSessions(ctx, update, uid, true)

	case strings.HasPrefix(data, "se:sw:"):
		name := data[6:]
		sess, err := tb.store.Switch(int(uid), name)
		if err != nil {
			tb.answerCB(ctx, update, "❌ "+err.Error())
			return
		}
		tb.clearUploadTarget(uid)
		tb.answerCB(ctx, update, fmt.Sprintf(
			"✅ Переключено на <b>%s</b>\n🤖 Агент: <code>%s</code>\n📂 Папка: <code>%s</code>",
			esc(name), sess.AgentType, esc(sess.Cwd)))
		// Refresh reply keyboard
		tb.sendKB(ctx, getChatID(update), uid,
			fmt.Sprintf("📌 Активна: <b>%s</b>", esc(name)))

	case strings.HasPrefix(data, "se:cl:"):
		name := data[6:]
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "✅ Да, закрыть", CallbackData: "se:cc:" + name},
				{Text: "↩️ Отмена", CallbackData: "xa:cancel"},
			}},
		}
		tb.answerCB(ctx, update, fmt.Sprintf("Закрыть сессию <b>%s</b>?", esc(name)), kb)

	case strings.HasPrefix(data, "se:cc:"):
		name := data[6:]
		// Stop if busy
		key := stopKey(uid, name)
		tb.stopMu.Lock()
		ch, wasBusy := tb.stopChans[key]
		if wasBusy {
			delete(tb.stopChans, key)
		}
		tb.stopMu.Unlock()
		if wasBusy {
			close(ch)
		}

		if err := tb.store.Close(int(uid), name); err != nil {
			tb.answerCB(ctx, update, "❌ "+err.Error())
			return
		}
		tb.history.Clear(int(uid), name)

		msg := fmt.Sprintf("✅ Сессия <b>%s</b> закрыта", esc(name))
		if wasBusy {
			msg += "\n🛑 Агент остановлен."
		}
		tb.answerCB(ctx, update, msg)

		active := tb.store.GetActive(int(uid))
		tip := "Создайте: /new"
		if active != nil {
			tip = fmt.Sprintf("Активна: <b>%s</b>", esc(active.Name))
		}
		tb.sendKB(ctx, getChatID(update), uid, tip)
	}
}

// ── 2. st: Status ────────────────────────────────────────────────────

func (tb *Bot) onStatusCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data

	switch {
	case data == "st:show":
		tb.showStatus(ctx, update, uid, true)

	case data == "st:ren":
		sess := tb.store.GetActive(int(uid))
		if sess == nil {
			tb.answerCB(ctx, update, "❌ Нет активной сессии.")
			return
		}
		tb.setWizard(uid, &wizard{Type: "rename", OldName: sess.Name})
		tb.answerCB(ctx, update,
			fmt.Sprintf("✏️ Введите новое имя для <b>%s</b>:", esc(sess.Name)))

	case data == "st:ag":
		sess := tb.store.GetActive(int(uid))
		if sess == nil {
			tb.answerCB(ctx, update, "❌ Нет активной сессии.")
			return
		}
		kb := agentPickerKB("st:sa:")
		tb.answerCB(ctx, update, fmt.Sprintf(
			"🔄 Сменить агент для <b>%s</b>\nТекущий: <code>%s</code>",
			esc(sess.Name), sess.AgentType), kb)

	case strings.HasPrefix(data, "st:sa:"):
		agentType := data[6:]
		sess := tb.store.GetActive(int(uid))
		if sess == nil {
			tb.answerCB(ctx, update, "❌ Нет активной сессии.")
			return
		}
		if !agents.IsAvailable(agentType) {
			tb.answerCB(ctx, update, "❌ Неизвестный агент: "+esc(agentType))
			return
		}
		if agentType == sess.AgentType {
			tb.answerCB(ctx, update, fmt.Sprintf("ℹ️ Агент уже <code>%s</code>.", agentType))
			return
		}
		tb.store.SetAgent(int(uid), sess.Name, agentType)
		tb.answerCB(ctx, update, fmt.Sprintf(
			"✅ Агент: <code>%s</code> для <b>%s</b>\n🔑 Контекст сброшен.",
			agentType, esc(sess.Name)))

	case data == "st:clone":
		sess := tb.store.GetActive(int(uid))
		if sess == nil {
			tb.answerCB(ctx, update, "❌ Нет активной сессии.")
			return
		}
		newName := tb.autoName(uid, sess.AgentType)
		clone, err := tb.store.Clone(int(uid), sess.Name, newName)
		if err != nil {
			tb.answerCB(ctx, update, "❌ "+err.Error())
			return
		}
		tb.answerCB(ctx, update, fmt.Sprintf(
			"✅ Клон: <b>%s</b>\n🤖 Агент: <code>%s</code>\n📂 Папка: <code>%s</code>",
			esc(newName), clone.AgentType, esc(clone.Cwd)))
		tb.sendKB(ctx, getChatID(update), uid,
			fmt.Sprintf("📌 Переключить: /use %s", esc(newName)))

	case data == "st:cwd":
		sess := tb.store.GetActive(int(uid))
		if sess == nil {
			tb.answerCB(ctx, update, "❌ Нет активной сессии.")
			return
		}
		startPath := sess.Cwd
		if _, err := os.Stat(startPath); err != nil {
			startPath, _ = os.Getwd()
		}
		text, markup := tb.buildFolderBrowser(uid, startPath, 0)
		tb.answerCB(ctx, update, text, markup)
	}
}

// ── 3. fb: Folder Browser ────────────────────────────────────────────

func (tb *Bot) onBrowserCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data

	switch {
	case strings.HasPrefix(data, "fb:g:"):
		idx, _ := strconv.Atoi(data[5:])
		path := tb.pget(uid, idx)
		if path == "" {
			tb.answerCB(ctx, update, "❌ Папка не найдена.")
			return
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			tb.answerCB(ctx, update, "❌ Папка не найдена.")
			return
		}
		text, markup := tb.buildFolderBrowser(uid, path, 0)
		tb.answerCB(ctx, update, text, markup)

	case strings.HasPrefix(data, "fb:p:"):
		parts := strings.SplitN(data[5:], ":", 2)
		if len(parts) != 2 {
			return
		}
		pidVal, _ := strconv.Atoi(parts[0])
		page, _ := strconv.Atoi(parts[1])
		path := tb.pget(uid, pidVal)
		if path == "" {
			return
		}
		text, markup := tb.buildFolderBrowser(uid, path, page)
		tb.answerCB(ctx, update, text, markup)

	case strings.HasPrefix(data, "fb:s:"):
		idx, _ := strconv.Atoi(data[5:])
		path := tb.pget(uid, idx)
		if path == "" {
			tb.answerCB(ctx, update, "❌ Папка не найдена.")
			return
		}

		wiz := tb.getWizard(uid)

		// New session wizard: CWD selected
		if wiz != nil && wiz.Type == "new_session" && wiz.Step == "cwd" {
			agentType := wiz.Agent
			name := wiz.Name
			tb.clearWizard(uid)
			ok := tb.createSessionCB(ctx, update, uid, name, agentType, path)
			if ok {
				tb.sendKB(ctx, getChatID(update), uid,
					fmt.Sprintf("💬 <b>%s</b> активна. Отправляйте текст.", esc(name)))
			}
			return
		}

		// Project wizard: parent selected
		if wiz != nil && wiz.Type == "project" && wiz.Step == "parent" {
			wiz.Parent = path
			wiz.Step = "folder_name"
			tb.setWizard(uid, wiz)
			tb.answerCB(ctx, update, fmt.Sprintf(
				"🆕 <b>Создание проекта</b>\n\nАгент: <code>%s</code>\nПапка: <code>%s</code>\n\n"+
					"Шаг 3/4. Введите <b>имя новой папки</b>:",
				wiz.Agent, esc(path)))
			return
		}

		// Default: change CWD
		sess := tb.store.GetActive(int(uid))
		if sess == nil {
			tb.answerCB(ctx, update, "❌ Нет активной сессии.")
			return
		}
		tb.store.SetCwd(int(uid), sess.Name, path)
		tb.answerCB(ctx, update, fmt.Sprintf("✅ Папка изменена: <code>%s</code>", esc(path)))
	}
}

// ── 4. fm: File Manager ──────────────────────────────────────────────

func (tb *Bot) onFileMgrCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data

	switch {
	case data == "fm:open":
		sess := tb.store.GetActive(int(uid))
		if sess == nil {
			tb.answerCB(ctx, update, "❌ Нет активной сессии.")
			return
		}
		if _, err := os.Stat(sess.Cwd); err != nil {
			tb.answerCB(ctx, update, fmt.Sprintf("❌ Папка не существует: <code>%s</code>", esc(sess.Cwd)))
			return
		}
		text, markup := tb.buildFileManager(uid, sess.Cwd, 0)
		tb.answerCB(ctx, update, text, markup)

	case strings.HasPrefix(data, "fm:g:"):
		idx, _ := strconv.Atoi(data[5:])
		path := tb.pget(uid, idx)
		if path == "" {
			tb.answerCB(ctx, update, "❌ Папка не найдена.")
			return
		}
		text, markup := tb.buildFileManager(uid, path, 0)
		tb.answerCB(ctx, update, text, markup)

	case strings.HasPrefix(data, "fm:p:"):
		parts := strings.SplitN(data[5:], ":", 2)
		if len(parts) != 2 {
			return
		}
		pidVal, _ := strconv.Atoi(parts[0])
		page, _ := strconv.Atoi(parts[1])
		path := tb.pget(uid, pidVal)
		if path == "" {
			return
		}
		text, markup := tb.buildFileManager(uid, path, page)
		tb.answerCB(ctx, update, text, markup)

	case strings.HasPrefix(data, "fm:d:"):
		idx, _ := strconv.Atoi(data[5:])
		path := tb.pget(uid, idx)
		if path == "" || !isFile(path) {
			tb.answerCB(ctx, update, "❌ Файл не найден.")
			return
		}
		f, err := os.Open(path)
		if err != nil {
			tb.answerCB(ctx, update, "❌ Ошибка открытия: "+err.Error())
			return
		}
		defer f.Close()
		chatID := getChatID(update)
		b.SendDocument(ctx, &bot.SendDocumentParams{
			ChatID:   chatID,
			Document: &models.InputFileUpload{Filename: filepath.Base(path), Data: f},
		})

	case strings.HasPrefix(data, "fm:r:"):
		// fm:r:{file_pid}:{dir_pid}
		parts := strings.SplitN(data[5:], ":", 2)
		if len(parts) != 2 {
			return
		}
		fileIdx, _ := strconv.Atoi(parts[0])
		dirPID, _ := strconv.Atoi(parts[1])
		path := tb.pget(uid, fileIdx)
		if path == "" || !isFile(path) {
			tb.answerCB(ctx, update, "❌ Файл не найден.")
			return
		}
		name := filepath.Base(path)
		size := "?"
		if info, err := os.Stat(path); err == nil {
			size = humanSize(info.Size())
		}
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "✅ Удалить", CallbackData: fmt.Sprintf("fm:cr:%d:%d", fileIdx, dirPID)},
				{Text: "↩ Назад", CallbackData: fmt.Sprintf("fm:g:%d", dirPID)},
			}},
		}
		tb.answerCB(ctx, update, fmt.Sprintf("🗑 Удалить <code>%s</code> (%s)?", esc(name), size), kb)

	case strings.HasPrefix(data, "fm:cr:"):
		parts := strings.SplitN(data[6:], ":", 2)
		if len(parts) != 2 {
			return
		}
		fileIdx, _ := strconv.Atoi(parts[0])
		dirPID, _ := strconv.Atoi(parts[1])
		path := tb.pget(uid, fileIdx)
		if path != "" {
			os.Remove(path)
		}
		dirPath := tb.pget(uid, dirPID)
		if dirPath != "" {
			if info, err := os.Stat(dirPath); err == nil && info.IsDir() {
				text, markup := tb.buildFileManager(uid, dirPath, 0)
				tb.answerCB(ctx, update, text, markup)
				return
			}
		}
		tb.answerCB(ctx, update, "✅ Файл удалён.")

	case strings.HasPrefix(data, "fm:u:"):
		dirPID, _ := strconv.Atoi(data[5:])
		dirPath := tb.pget(uid, dirPID)
		if dirPath == "" {
			tb.answerCB(ctx, update, "❌ Папка не найдена.")
			return
		}
		tb.setUploadTarget(uid, dirPath)
		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "✅ Готово", CallbackData: fmt.Sprintf("fm:ud:%d", dirPID)},
			}},
		}
		tb.answerCB(ctx, update, fmt.Sprintf(
			"📎 <b>Загрузка файлов</b>\n📂 <code>%s</code>\n\n"+
				"Отправьте файлы — они сохранятся сюда.\nКогда закончите — нажмите кнопку.",
			esc(dirPath)), kb)

	case strings.HasPrefix(data, "fm:ud:"):
		dirPID, _ := strconv.Atoi(data[6:])
		tb.clearUploadTarget(uid)
		dirPath := tb.pget(uid, dirPID)
		if dirPath != "" {
			if info, err := os.Stat(dirPath); err == nil && info.IsDir() {
				text, markup := tb.buildFileManager(uid, dirPath, 0)
				tb.answerCB(ctx, update, text, markup)
				return
			}
		}
		tb.answerCB(ctx, update, "✅ Загрузка завершена.")

	case data == "fm:x":
		tb.clearUploadTarget(uid)
		tb.answerCB(ctx, update, "📂 Файловый менеджер закрыт.")
	}
}

// ── 5. wz: Wizards ───────────────────────────────────────────────────

func (tb *Bot) onWizardCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data

	switch {
	case strings.HasPrefix(data, "wz:ag:"):
		agentType := data[6:]
		if !agents.IsAvailable(agentType) {
			tb.answerCB(ctx, update, "❌ Неизвестный агент: "+esc(agentType))
			return
		}
		tb.setWizard(uid, &wizard{Type: "new_session", Step: "name", Agent: agentType})
		tb.answerCB(ctx, update, fmt.Sprintf(
			"➕ <b>Создание сессии</b>\n\nАгент: <code>%s</code>\n\n"+
				"Шаг 2/3. Введите <b>имя</b> сессии:", agentType))

	case strings.HasPrefix(data, "wz:pa:"):
		agentType := data[6:]
		if !agents.IsAvailable(agentType) {
			tb.answerCB(ctx, update, "❌ Неизвестный агент: "+esc(agentType))
			return
		}
		tb.setWizard(uid, &wizard{Type: "project", Step: "parent", Agent: agentType})
		sess := tb.store.GetActive(int(uid))
		startPath, _ := os.Getwd()
		if sess != nil {
			if _, err := os.Stat(sess.Cwd); err == nil {
				startPath = sess.Cwd
			}
		}
		text, markup := tb.buildFolderBrowser(uid, startPath, 0)
		tb.answerCB(ctx, update, text, markup)

	case data == "wz:done":
		wiz := tb.getWizard(uid)
		if wiz == nil || wiz.Type != "project" {
			tb.answerCB(ctx, update, "❌ Нет активного визарда.")
			return
		}
		projectCwd := wiz.Cwd
		agentType := wiz.Agent
		tb.clearWizard(uid)
		if projectCwd == "" || agentType == "" {
			tb.answerCB(ctx, update, "❌ Потеряны данные проекта.")
			return
		}
		name := filepath.Base(projectCwd)
		ok := tb.createSessionCB(ctx, update, uid, name, agentType, projectCwd)
		if ok {
			tb.sendKB(ctx, getChatID(update), uid,
				fmt.Sprintf("💬 <b>%s</b> активна. Отправляйте текст.", esc(name)))
		}
	}
}

// ── 6. xa: Actions ───────────────────────────────────────────────────

func (tb *Bot) onActionCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data

	switch {
	case data == "xa:stop":
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
				tb.answerCB(ctx, update, "🛑 Запрос прерван.")
				return
			}
		}
		tb.answerCB(ctx, update, "Нет активного запроса.")

	case data == "xa:cancel":
		tb.clearWizard(uid)
		tb.answerCB(ctx, update, "↩️ Отменено")

	case data == "xa:noop":
		// do nothing
	}
}

// ── 7. qr: Quick Response Actions ────────────────────────────────────

func (tb *Bot) onQuickCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data
	chatID := int64(0)
	threadID := 0
	if query.Message.Message != nil {
		chatID = query.Message.Message.Chat.ID
		threadID = query.Message.Message.MessageThreadID
	}

	sess := tb.store.GetActive(int(uid))
	if sess == nil {
		return
	}

	// Check if this was in a topic-bound session
	sessionOverride := ""
	if threadID != 0 {
		sessionOverride = tb.resolveTopicSession(uid, chatID, threadID)
	}

	switch data {
	case "qr:cont":
		tb.sendPromptToAgent(ctx, uid, chatID, "continue", threadID, sessionOverride)

	case "qr:retry":
		targetSess := sess
		if sessionOverride != "" {
			if s := tb.store.Get(int(uid), sessionOverride); s != nil {
				targetSess = s
			}
		}
		lastMsg := tb.history.GetLastUserMessage(int(uid), targetSess.Name)
		if lastMsg == "" {
			return
		}
		tb.sendPromptToAgent(ctx, uid, chatID, lastMsg, threadID, sessionOverride)

	case "qr:compact":
		tb.store.UpdateAgentSessionID(int(uid), sess.Name, "")
		if query.Message.Message != nil {
			tb.b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: query.Message.Message.ID,
				Text:      "📦 Контекст сброшен. Следующий запрос начнёт новый диалог.",
			})
		}
	}
}

// ── 8. cf: Config ────────────────────────────────────────────────────

var claudeModels = []struct{ Key, Label string }{
	{"sonnet", "Sonnet 4.5"},
	{"opus", "Opus 4.6"},
	{"haiku", "Haiku 4.5"},
}

var claudePermModes = []struct{ Key, Label string }{
	{"bypassPermissions", "Полный автомат"},
	{"default", "Стандартный"},
	{"plan", "Plan"},
}

var codexModels = []struct{ Key, Label string }{
	{"gpt-5.3-codex", "GPT-5.3-Codex"},
	{"gpt-5.3-codex-spark", "GPT-5.3-Codex-Spark"},
	{"gpt-5.2-codex", "GPT-5.2-Codex"},
}

var codexReasoning = []struct{ Key, Label string }{
	{"xhigh", "Максимум"},
	{"high", "Высокий"},
	{"medium", "Средний"},
	{"low", "Низкий"},
	{"minimal", "Минимальный"},
}

var codexApprovalModes = []struct{ Key, Label string }{
	{"full-auto", "Автомат с песочницей"},
	{"bypass", "Полный автомат"},
	{"suggest", "Спрашивает"},
	{"auto", "Не спрашивает"},
}

func (tb *Bot) onConfigCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data
	cfg := config.Get()

	switch {
	// Claude model
	case data == "cf:model":
		var rows [][]models.InlineKeyboardButton
		for _, m := range claudeModels {
			mark := ""
			if m.Key == cfg.ClaudeModel {
				mark = "✅ "
			}
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: fmt.Sprintf("%s%s — %s", mark, m.Key, m.Label), CallbackData: "cf:sm:" + m.Key},
			})
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "↩ Назад", CallbackData: "cf:back"},
		})
		tb.answerCB(ctx, update, "📦 <b>Claude — модель:</b>",
			&models.InlineKeyboardMarkup{InlineKeyboard: rows})

	case strings.HasPrefix(data, "cf:sm:"):
		model := data[6:]
		cfg.ClaudeModel = model
		cfg.Save()
		tb.answerCB(ctx, update, fmt.Sprintf("✅ Claude модель: <code>%s</code>", model))

	// Claude permission mode
	case data == "cf:mode":
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
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "↩ Назад", CallbackData: "cf:back"},
		})
		tb.answerCB(ctx, update, "🔒 <b>Claude — режим:</b>",
			&models.InlineKeyboardMarkup{InlineKeyboard: rows})

	case strings.HasPrefix(data, "cf:sp:"):
		mode := data[6:]
		cfg.ClaudePermissionMode = mode
		cfg.Save()
		tb.answerCB(ctx, update, fmt.Sprintf("✅ Claude режим: <code>%s</code>", mode))

	// Codex model
	case data == "cf:cx_model":
		var rows [][]models.InlineKeyboardButton
		for _, m := range codexModels {
			mark := ""
			if m.Key == cfg.CodexModel {
				mark = "✅ "
			}
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: fmt.Sprintf("%s%s", mark, m.Key), CallbackData: "cf:cxm:" + m.Key},
			})
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "↩ Назад", CallbackData: "cf:back"},
		})
		tb.answerCB(ctx, update, "📦 <b>Codex — модель:</b>",
			&models.InlineKeyboardMarkup{InlineKeyboard: rows})

	case strings.HasPrefix(data, "cf:cxm:"):
		model := data[7:]
		cfg.CodexModel = model
		cfg.Save()
		tb.answerCB(ctx, update, fmt.Sprintf("✅ Codex модель: <code>%s</code>", esc(model)))

	// Codex reasoning
	case data == "cf:cx_reason":
		var rows [][]models.InlineKeyboardButton
		for _, m := range codexReasoning {
			mark := ""
			if m.Key == cfg.CodexReasoning {
				mark = "✅ "
			}
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: fmt.Sprintf("%s%s — %s", mark, m.Key, m.Label), CallbackData: "cf:cxr:" + m.Key},
			})
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "↩ Назад", CallbackData: "cf:back"},
		})
		tb.answerCB(ctx, update, "🧠 <b>Codex — reasoning effort:</b>",
			&models.InlineKeyboardMarkup{InlineKeyboard: rows})

	case strings.HasPrefix(data, "cf:cxr:"):
		level := data[7:]
		cfg.CodexReasoning = level
		cfg.Save()
		tb.answerCB(ctx, update, fmt.Sprintf("✅ Codex reasoning: <code>%s</code>", level))

	// Codex approval mode
	case data == "cf:cx_mode":
		var rows [][]models.InlineKeyboardButton
		for _, m := range codexApprovalModes {
			mark := ""
			if m.Key == cfg.CodexApprovalMode {
				mark = "✅ "
			}
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: fmt.Sprintf("%s%s — %s", mark, m.Key, m.Label), CallbackData: "cf:cxp:" + m.Key},
			})
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "↩ Назад", CallbackData: "cf:back"},
		})
		tb.answerCB(ctx, update, "🔒 <b>Codex — режим:</b>",
			&models.InlineKeyboardMarkup{InlineKeyboard: rows})

	case strings.HasPrefix(data, "cf:cxp:"):
		mode := data[7:]
		cfg.CodexApprovalMode = mode
		cfg.Save()
		tb.answerCB(ctx, update, fmt.Sprintf("✅ Codex режим: <code>%s</code>", mode))

	// Back to config main
	case data == "cf:back":
		tb.showConfig(ctx, update, true)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (tb *Bot) createSessionCB(ctx context.Context, update *models.Update, uid int64,
	name, agentType, cwd string) bool {
	if !agents.IsAvailable(agentType) {
		tb.answerCB(ctx, update, "❌ Агент недоступен: "+agentType)
		return false
	}
	if _, err := os.Stat(cwd); err != nil {
		tb.answerCB(ctx, update, fmt.Sprintf("❌ Папка не существует: <code>%s</code>", esc(cwd)))
		return false
	}
	_, err := tb.store.Create(int(uid), name, agentType, cwd)
	if err != nil {
		tb.answerCB(ctx, update, "❌ "+err.Error())
		return false
	}
	tb.answerCB(ctx, update, fmt.Sprintf(
		"✅ Сессия <b>%s</b> создана\n🤖 Агент: <code>%s</code>\n📂 Папка: <code>%s</code>\n\n"+
			"💬 Отправьте текст — он уйдёт агенту.",
		esc(name), agentType, esc(cwd)))
	return true
}
