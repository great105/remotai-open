package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/config"
	"tgcontrol/internal/observability"
	"tgcontrol/internal/sessions"
)

// closeStop closes and removes the stop channel for a running agent key exactly
// once. Map presence under stopMu guarantees a single close, so /cancel, the
// callback Stop button, and the producer's own teardown can all call this
// without risking a "close of closed channel" panic.
func (tb *Bot) closeStop(key string) {
	tb.stopMu.Lock()
	defer tb.stopMu.Unlock()
	if ch, ok := tb.stopChans[key]; ok {
		close(ch)
		delete(tb.stopChans, key)
	}
}

// onDefault handles all updates not matched by specific handlers.
func (tb *Bot) onDefault(ctx context.Context, b *bot.Bot, update *models.Update) {
	// Handle callback queries from legacy buttons
	if update.CallbackQuery != nil {
		tb.onLegacyCB(ctx, b, update)
		return
	}

	// Handle attachments
	if update.Message != nil && hasAttachment(update.Message) {
		tb.onAttachment(ctx, b, update)
		return
	}

	// Handle text messages
	if update.Message != nil && update.Message.Text != "" {
		uid := getUID(update)
		if !tb.authorized(uid) {
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: getChatID(update), Text: "🚫 Нет доступа.",
			})
			return
		}

		text := strings.TrimSpace(update.Message.Text)
		log.Printf("[onDefault] text=%q", text)

		// Handle unknown commands
		if strings.HasPrefix(text, "/") {
			tb.onUnknownCommand(ctx, b, update)
			return
		}

		tb.onText(ctx, b, update)
	}
}

// onText handles plain text messages.
func (tb *Bot) onText(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	text := strings.TrimSpace(update.Message.Text)

	// Reply keyboard buttons
	switch text {
	case "📊 Статус":
		tb.clearWizard(uid)
		tb.cmdStatus(ctx, b, update)
		return
	case "➕ Новая":
		tb.clearWizard(uid)
		// Simulate no-args /new
		update.Message.Text = "/new"
		tb.cmdNew(ctx, b, update)
		return
	case "📋 Сессии":
		tb.clearWizard(uid)
		tb.cmdList(ctx, b, update)
		return
	case "📁 Файлы":
		tb.clearWizard(uid)
		tb.cmdFiles(ctx, b, update)
		return
	}

	// Session switching via prefix
	if strings.HasPrefix(text, sessionPrefix) || strings.HasPrefix(text, sessionActivePrefix) {
		tb.clearWizard(uid)
		var name string
		if strings.HasPrefix(text, sessionActivePrefix) {
			name = text[len(sessionActivePrefix):]
		} else {
			name = text[len(sessionPrefix):]
		}
		sess, err := tb.store.Switch(int(uid), name)
		if err != nil {
			tb.sendKB(ctx, getChatID(update), uid,
				fmt.Sprintf("❌ Сессия <b>%s</b> не найдена.", esc(name)))
			return
		}
		tb.clearUploadTarget(uid)
		tb.sendKB(ctx, getChatID(update), uid,
			fmt.Sprintf("✅ Переключено на <b>%s</b>\n🤖 Агент: <code>%s</code>\n📂 Папка: <code>%s</code>",
				esc(name), sess.AgentType, esc(sess.Cwd)))
		return
	}

	// Wizard text inputs
	wiz := tb.getWizard(uid)
	if wiz != nil {
		switch {
		// Rename
		case wiz.Type == "rename":
			oldName := wiz.OldName
			tb.clearWizard(uid)
			newName := firstField(text)
			if newName == "" {
				tb.reply(ctx, update, "❌ Имя не может быть пустым.")
				return
			}
			if len(newName) > maxNameLen {
				tb.reply(ctx, update, fmt.Sprintf("❌ Слишком длинное (макс. %d).", maxNameLen))
				return
			}
			if err := tb.store.Rename(int(uid), oldName, newName); err != nil {
				tb.reply(ctx, update, "❌ "+err.Error())
				return
			}
			tb.history.Rename(int(uid), oldName, newName)
			tb.sendKB(ctx, getChatID(update), uid,
				fmt.Sprintf("✅ Переименована: <b>%s</b>", esc(newName)))
			return

		// New session: waiting for name
		case wiz.Type == "new_session" && wiz.Step == "name":
			name := firstField(text)
			if name == "" {
				tb.reply(ctx, update, "❌ Имя не может быть пустым.")
				return
			}
			wiz.Name = name
			wiz.Step = "cwd"
			tb.setWizard(uid, wiz)
			sess := tb.store.GetActive(int(uid))
			startPath, _ := os.Getwd()
			if sess != nil {
				if _, err := os.Stat(sess.Cwd); err == nil {
					startPath = sess.Cwd
				}
			}
			tb.reply(ctx, update, fmt.Sprintf(
				"➕ <b>Создание сессии</b>\n\nАгент: <code>%s</code>\nИмя: <b>%s</b>\n\n"+
					"Шаг 3/3. Выберите рабочую папку:", wiz.Agent, esc(name)))
			fbText, fbMarkup := tb.buildFolderBrowser(uid, startPath, 0)
			tb.reply(ctx, update, fbText, fbMarkup)
			return

		// Project: waiting for folder name
		case wiz.Type == "project" && wiz.Step == "folder_name":
			folderName := firstField(text)
			if folderName == "" {
				tb.reply(ctx, update, "❌ Имя папки не может быть пустым.")
				return
			}
			parent := wiz.Parent
			if parent == "" {
				tb.reply(ctx, update, "❌ Родительская папка потеряна. /project")
				tb.clearWizard(uid)
				return
			}
			projectPath := filepath.Join(parent, folderName)
			if err := os.MkdirAll(projectPath, 0755); err != nil {
				tb.reply(ctx, update, "❌ Не удалось создать папку: "+err.Error())
				return
			}
			wiz.Cwd = projectPath
			wiz.Step = "upload"
			tb.setWizard(uid, wiz)
			tb.setUploadTarget(uid, projectPath)
			kb := &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{{
					{Text: "✅ Готово — создать сессию", CallbackData: "wz:done"},
				}},
			}
			tb.reply(ctx, update, fmt.Sprintf(
				"🆕 <b>Создание проекта</b>\n\n"+
					"Агент: <code>%s</code>\n📂 Папка: <code>%s</code>\n\n"+
					"Шаг 4/4. Отправьте <b>файлы</b>.\nКогда закончите — нажмите кнопку.",
				wiz.Agent, esc(projectPath)), kb)
			return
		}
	}

	// Normal text → send to agent
	tb.sendToAgent(ctx, update)
}

// firstField returns the first whitespace-delimited token, or "" if the string
// is empty/whitespace-only. Guards against strings.Fields(...)[0] panicking on
// whitespace-only messages (which would crash the whole process).
func firstField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// onUnknownCommand handles commands not registered as handlers.
func (tb *Bot) onUnknownCommand(ctx context.Context, b *bot.Bot, update *models.Update) {
	text := update.Message.Text
	cmd := firstField(text)
	if cmd == "" {
		return
	}
	cmdLower := strings.ToLower(cmd)

	// Known aliases (fallback for commands also registered as handlers)
	switch cmdLower {
	case "/cost":
		tb.cmdCost(ctx, b, update)
		return
	case "/help":
		tb.cmdHelp(ctx, b, update)
		return
	case "/model", "/config":
		tb.cmdConfig(ctx, b, update)
		return
	case "/mode":
		tb.cmdMode(ctx, b, update)
		return
	case "/clear":
		tb.cmdClear(ctx, b, update)
		return
	case "/compact":
		tb.cmdCompact(ctx, b, update)
		return
	case "/agent":
		tb.cmdAgent(ctx, b, update)
		return
	case "/continue":
		tb.cmdContinue(ctx, b, update)
		return
	case "/retry":
		tb.cmdRetry(ctx, b, update)
		return
	}

	// Unknown commands → show help instead of forwarding to agent
	// (Claude Code CLI ignores slash commands in -p mode)
	tb.reply(ctx, update, fmt.Sprintf(
		"❓ Неизвестная команда: <code>%s</code>\n\n"+
			"<b>Доступные команды:</b>\n"+
			"/model — сменить модель\n"+
			"/mode — режим разрешений\n"+
			"/agent — сменить агента\n"+
			"/clear — очистить сессию\n"+
			"/compact — сбросить контекст\n"+
			"/cost — стоимость сессии\n"+
			"/continue — продолжить ответ\n"+
			"/retry — повторить запрос\n"+
			"/help — полная справка",
		esc(cmd)))
}

// onLegacyCB handles callback queries from old inline buttons.
func (tb *Bot) onLegacyCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data

	legacyMap := map[string]string{
		"list_sessions":    "se:ls",
		"show_status":      "st:show",
		"open_files":       "fm:open",
		"new_session":      "se:new",
		"rename_session":   "st:ren",
		"change_agent":     "st:ag",
		"clone_session":    "st:clone",
		"change_cwd":       "st:cwd",
		"stop_agent":       "xa:stop",
		"cancel":           "xa:cancel",
		"noop":             "xa:noop",
		"file_close":       "fm:x",
		"file_upload_done": "fm:x",
		"file_back":        "fm:x",
		"finish_upload":    "wz:done",
	}

	if mapped, ok := legacyMap[data]; ok {
		update.CallbackQuery.Data = mapped
		prefix := mapped[:3]
		switch prefix {
		case "se:":
			tb.onSessionCB(ctx, b, update)
		case "st:":
			tb.onStatusCB(ctx, b, update)
		case "fm:":
			tb.onFileMgrCB(ctx, b, update)
		case "wz:":
			tb.onWizardCB(ctx, b, update)
		case "xa:":
			tb.onActionCB(ctx, b, update)
		}
		return
	}

	// Prefix-based legacy
	switch {
	case strings.HasPrefix(data, "switch:"):
		update.CallbackQuery.Data = "se:sw:" + data[7:]
		tb.onSessionCB(ctx, b, update)
	case strings.HasPrefix(data, "close:"):
		update.CallbackQuery.Data = "se:cl:" + data[6:]
		tb.onSessionCB(ctx, b, update)
	case strings.HasPrefix(data, "confirm_close:"):
		update.CallbackQuery.Data = "se:cc:" + data[14:]
		tb.onSessionCB(ctx, b, update)
	case strings.HasPrefix(data, "agent:"):
		update.CallbackQuery.Data = "wz:ag:" + data[6:]
		tb.onWizardCB(ctx, b, update)
	case strings.HasPrefix(data, "project_agent:"):
		update.CallbackQuery.Data = "wz:pa:" + data[14:]
		tb.onWizardCB(ctx, b, update)
	case strings.HasPrefix(data, "set_agent:"):
		update.CallbackQuery.Data = "st:sa:" + data[10:]
		tb.onStatusCB(ctx, b, update)
	default:
		tb.answerCB(ctx, update, "⚠️ Кнопка устарела. Используйте новые команды.")
	}
}

// ── Send to agent ────────────────────────────────────────────────────

func (tb *Bot) sendToAgent(ctx context.Context, update *models.Update) {
	uid := getUID(update)
	chatID := getChatID(update)
	threadID := getThreadID(update)
	prompt := update.Message.Text

	// Topic routing: if message is in a bound topic, use that session
	sessionOverride := tb.resolveTopicSession(uid, chatID, threadID)

	tb.sendPromptToAgent(ctx, uid, chatID, prompt, threadID, sessionOverride)
}

func (tb *Bot) sendPromptToAgent(ctx context.Context, uid, chatID int64, prompt string, threadID int, sessionOverride string) {
	var sess *sessions.Session
	if sessionOverride != "" {
		sess = tb.store.Get(int(uid), sessionOverride)
	} else {
		sess = tb.store.GetActive(int(uid))
	}
	if sess == nil {
		tb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text:      "📋 Нет активной сессии. Создайте: /new",
			ParseMode: models.ParseModeHTML,
		})
		return
	}

	sessName := sess.Name
	key := stopKey(uid, sessName)

	// ── Dispatch: check allowed agents ──
	cfg := config.Get()
	if !cfg.IsAgentAllowed(sess.AgentType) {
		tb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text: fmt.Sprintf("🚫 Агент <code>%s</code> не в списке разрешённых.\nРазрешённые: %s",
				esc(sess.AgentType), strings.Join(cfg.AllowedAgents, ", ")),
			ParseMode: models.ParseModeHTML,
		})
		return
	}

	// ── Dispatch: check concurrent session limit ──
	// Per-session lock
	tb.agentMu.Lock()
	lock, ok := tb.agentLocks[key]
	if !ok {
		lock = &sync.Mutex{}
		tb.agentLocks[key] = lock
	}
	tb.agentMu.Unlock()

	if !lock.TryLock() {
		tb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, MessageThreadID: threadID,
			Text:      fmt.Sprintf("⏳ Сессия <b>%s</b> занята.\nДождитесь ответа или нажмите /cancel для остановки.", esc(sessName)),
			ParseMode: models.ParseModeHTML,
		})
		return
	}

	if _, err := tb.store.StartRun(int(uid), sessName, cfg.MaxConcurrentSessions); err != nil {
		lock.Unlock()
		switch err {
		case sessions.ErrSessionBusy:
			tb.b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, MessageThreadID: threadID,
				Text:      fmt.Sprintf("⏳ Сессия <b>%s</b> занята.\nДождитесь ответа или нажмите /cancel для остановки.", esc(sessName)),
				ParseMode: models.ParseModeHTML,
			})
		case sessions.ErrConcurrentLimit:
			tb.b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, MessageThreadID: threadID,
				Text: fmt.Sprintf("⚠️ Достигнут лимит одновременных сессий (%d).\nДождитесь завершения или /cancel.",
					cfg.MaxConcurrentSessions),
				ParseMode: models.ParseModeHTML,
			})
		default:
			tb.b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, MessageThreadID: threadID,
				Text: "❌ " + err.Error(),
			})
		}
		return
	}

	// Save user message
	now := float64(time.Now().UnixMilli()) / 1000
	userMsg := sessions.Message{
		Role:      "user",
		Text:      prompt,
		Timestamp: now,
	}
	tb.history.Add(int(uid), sessName, userMsg)
	if tb.webServer != nil {
		tb.webServer.Broadcast(uid, map[string]any{
			"type": "message", "session": sessName, "message": userMsg,
		})
	}

	tb.store.SetBusy(int(uid), sessName, true)
	tb.store.SetStatus(int(uid), sessName, sessions.StatusAlive)
	if tb.webServer != nil {
		tb.webServer.Broadcast(uid, map[string]any{
			"type": "status", "session": sessName, "is_busy": true,
		})
	}

	// Send status message. May fail during a Telegram outage — then statusMsgID
	// stays 0 and every edit/delete/heartbeat is skipped instead of dereferencing
	// a nil *Message or editing MessageID=0.
	statusMsg, _ := tb.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            fmt.Sprintf("⏳ [%s] Обрабатываю...", sessName),
		ReplyMarkup:     stopKB(),
	})
	statusMsgID := 0
	if statusMsg != nil {
		statusMsgID = statusMsg.ID
	}

	// Run agent in goroutine
	go func() {
		defer observability.RecoverPanic("bot-agent-run")
		defer lock.Unlock()

		agent, err := agents.GetAgent(sess.AgentType)
		if err != nil {
			tb.store.SetBusy(int(uid), sessName, false)
			tb.store.SetStatus(int(uid), sessName, sessions.StatusDead)
			if tb.webServer != nil {
				tb.webServer.Broadcast(uid, map[string]any{
					"type": "status", "session": sessName, "is_busy": false,
				})
			}
			tb.b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, MessageThreadID: threadID,
				Text: "❌ " + err.Error(),
			})
			return
		}

		stopCh := make(chan struct{})
		tb.stopMu.Lock()
		tb.stopChans[key] = stopCh
		tb.stopMu.Unlock()

		var toolsUsed []string
		lastProgress := ""
		startTime := time.Now()

		// Heartbeat goroutine
		heartDone := make(chan struct{})
		go func() {
			defer observability.RecoverPanic("bot-heartbeat")
			defer close(heartDone)
			if statusMsgID == 0 {
				// No status message to update — just wait for completion so the
				// caller's <-heartDone doesn't block forever.
				select {
				case <-stopCh:
				case <-ctx.Done():
				}
				return
			}
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopCh:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					elapsed := int(time.Since(startTime).Seconds())
					m, s := elapsed/60, elapsed%60
					base := lastProgress
					if base == "" {
						base = fmt.Sprintf("⏳ [%s] Работает", sessName)
					}
					beat := fmt.Sprintf("%s (%d:%02d)", base, m, s)
					tb.b.EditMessageText(ctx, &bot.EditMessageTextParams{
						ChatID:      chatID,
						MessageID:   statusMsgID,
						Text:        beat,
						ReplyMarkup: stopKB(),
					})
				}
			}
		}()

		agentCtx, agentCancel := context.WithCancel(ctx)
		go func() {
			defer observability.RecoverPanic("bot-agent-cancel-watch")
			select {
			case <-stopCh:
				agentCancel()
			case <-agentCtx.Done():
			}
		}()

		sessionConfig := agents.SessionConfigWithPermissionMode(sess.AgentType, sess.AgentConfig, sess.PermissionMode)

		resp := agent.Run(agentCtx, agents.RunOptions{
			Prompt:        prompt,
			Cwd:           sess.Cwd,
			SessionID:     sess.AgentSessionID,
			SessionConfig: sessionConfig,
			OnProgress: func(text string) {
				toolsUsed = append(toolsUsed, text)
				if tb.webServer != nil {
					tb.webServer.Broadcast(uid, map[string]any{
						"type": "progress", "session": sessName, "text": text,
					})
				}
				full := fmt.Sprintf("⏳ [%s] %s", sessName, text)
				if full != lastProgress {
					if statusMsgID != 0 {
						tb.b.EditMessageText(ctx, &bot.EditMessageTextParams{
							ChatID:      chatID,
							MessageID:   statusMsgID,
							Text:        full,
							ReplyMarkup: stopKB(),
						})
					}
					lastProgress = full
				}
			},
			StopCh: stopCh,
		})

		agentCancel()

		// Check if user stopped
		userStopped := false
		select {
		case <-stopCh:
			userStopped = true
		default:
		}

		// Close + remove the stop channel exactly once. This also signals the
		// heartbeat goroutine to exit. Idempotent vs a concurrent /cancel that
		// may have already closed it.
		tb.closeStop(key)
		<-heartDone

		tb.store.SetBusy(int(uid), sessName, false)
		tb.store.Touch(int(uid), sessName)

		if resp.SessionID != "" {
			tb.store.UpdateAgentSessionID(int(uid), sessName, resp.SessionID)
		}

		// Delete status message
		if statusMsgID != 0 {
			tb.b.DeleteMessage(ctx, &bot.DeleteMessageParams{
				ChatID:    chatID,
				MessageID: statusMsgID,
			})
		}

		log.Printf("[%s] Agent returned: %d chars, error=%v, stopped=%v",
			sessName, len(resp.Text), resp.IsError, userStopped)

		if userStopped {
			text := fmt.Sprintf("🛑 [%s] Прервано.", sessName)
			if resp.Text != "" && resp.Text != "(прервано)" {
				text += "\n\nЧастичный ответ:\n" + renderHTML(resp.Text)
			}
			tb.replySplit(ctx, chatID, threadID, text)
			return
		}

		// Update status based on result
		if resp.IsError {
			tb.store.SetStatus(int(uid), sessName, sessions.StatusDead)
		} else {
			tb.store.SetStatus(int(uid), sessName, sessions.StatusAlive)
		}

		// Save agent response
		agentMsg := sessions.Message{
			Role:      "agent",
			Text:      resp.Text,
			Timestamp: float64(time.Now().UnixMilli()) / 1000,
			CostUSD:   resp.CostUSD,
			IsError:   resp.IsError,
			Tools:     toolsUsed,
		}
		tb.history.Add(int(uid), sessName, agentMsg)
		if tb.webServer != nil {
			tb.webServer.Broadcast(uid, map[string]any{
				"type": "message", "session": sessName, "message": agentMsg,
			})
			tb.webServer.Broadcast(uid, map[string]any{
				"type": "status", "session": sessName, "is_busy": false,
			})
		}

		// Build response
		var footerParts []string
		if resp.CostUSD > 0 {
			footerParts = append(footerParts, fmt.Sprintf("💰 $%.4f", resp.CostUSD))
		}
		if resp.IsError && resp.CostUSD == 0 {
			footerParts = append(footerParts, "⚠️ ошибка")
		}
		footer := ""
		if len(footerParts) > 0 {
			footer = fmt.Sprintf("\n\n<i>(%s)</i>", strings.Join(footerParts, ", "))
		}

		header := fmt.Sprintf("📌 <b>[%s]</b>\n\n", esc(sessName))
		body := renderHTML(resp.Text)
		tb.replySplitWithKB(ctx, chatID, threadID, header+body+footer, postResponseKB())

		// Oneshot mode: auto-close session after response
		if sess.Mode == sessions.ModeOneshot {
			tb.store.Close(int(uid), sessName)
			tb.history.Clear(int(uid), sessName)
			tb.b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, MessageThreadID: threadID,
				Text:      fmt.Sprintf("🔄 Oneshot-сессия <b>%s</b> автоматически закрыта.", esc(sessName)),
				ParseMode: models.ParseModeHTML,
			})
		}
	}()
}

// ── Attachment handler ───────────────────────────────────────────────

func hasAttachment(msg *models.Message) bool {
	return msg.Document != nil || len(msg.Photo) > 0 || msg.Video != nil ||
		msg.Audio != nil || msg.Voice != nil || msg.VideoNote != nil
}

func (tb *Bot) onAttachment(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	msg := update.Message

	var fileID string
	var fileName string

	switch {
	case msg.Document != nil:
		fileID = msg.Document.FileID
		fileName = msg.Document.FileName
		if fileName == "" {
			fileName = "file"
		}
	case len(msg.Photo) > 0:
		photo := msg.Photo[len(msg.Photo)-1] // largest
		fileID = photo.FileID
		fileName = fmt.Sprintf("photo_%s.jpg", photo.FileUniqueID)
	case msg.Video != nil:
		fileID = msg.Video.FileID
		fileName = msg.Video.FileName
		if fileName == "" {
			fileName = fmt.Sprintf("video_%s.mp4", msg.Video.FileUniqueID)
		}
	case msg.Audio != nil:
		fileID = msg.Audio.FileID
		fileName = msg.Audio.FileName
		if fileName == "" {
			fileName = fmt.Sprintf("audio_%s.mp3", msg.Audio.FileUniqueID)
		}
	case msg.Voice != nil:
		fileID = msg.Voice.FileID
		fileName = fmt.Sprintf("voice_%s.ogg", msg.Voice.FileUniqueID)
	case msg.VideoNote != nil:
		fileID = msg.VideoNote.FileID
		fileName = fmt.Sprintf("videonote_%s.mp4", msg.VideoNote.FileUniqueID)
	default:
		return
	}

	// Determine target directory in priority order:
	//   1. Explicit upload-target set by a wizard (e.g. /project flow)
	//   2. Topic-bound session's cwd (when the message came from a bound forum topic)
	//   3. Active session's cwd (DM with selected session)
	//   4. Configured inbox dir (config.json `inbox_dir`, with default fallback)
	chatID := getChatID(update)
	threadID := getThreadID(update)

	uploadTarget := tb.getUploadTarget(uid)
	var targetDir string
	var routedVia string // for the reply text

	if uploadTarget != "" {
		if info, err := os.Stat(uploadTarget); err == nil && info.IsDir() {
			targetDir = uploadTarget
			routedVia = "📂 загрузка"
		}
	}

	if targetDir == "" {
		if cwd, sessName := tb.resolveTopicCwd(uid, chatID, threadID); cwd != "" {
			if info, err := os.Stat(cwd); err == nil && info.IsDir() {
				targetDir = cwd
				if sessName != "" {
					routedVia = "🔗 топик → " + sessName
				} else {
					routedVia = "🔗 топик"
				}
			}
		}
	}

	if targetDir == "" {
		if sess := tb.store.GetActive(int(uid)); sess != nil {
			if info, err := os.Stat(sess.Cwd); err == nil && info.IsDir() {
				targetDir = sess.Cwd
				routedVia = "📌 активная сессия " + sess.Name
			}
		}
	}

	if targetDir == "" {
		// Fallback: inbox folder.
		cfg := config.Get()
		dir, err := cfg.EnsureInboxDir()
		if err != nil {
			tb.reply(ctx, update, "❌ Не удалось создать папку для входящих: "+esc(err.Error()))
			return
		}
		targetDir = dir
		routedVia = "📥 входящие"
	}

	// Download file
	filePath := uniqueFilename(targetDir, fileName)

	file, err := b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(strings.ToLower(errMsg), "too big") {
			tb.reply(ctx, update, fmt.Sprintf(
				"❌ Файл <code>%s</code> слишком большой (лимит Telegram Bot API ~20MB).",
				esc(fileName)))
		} else {
			tb.reply(ctx, update, "❌ Ошибка загрузки: "+esc(errMsg))
		}
		return
	}

	// Download using file link
	link := b.FileDownloadLink(file)
	if err := downloadFile(link, filePath); err != nil {
		tb.reply(ctx, update, "❌ Ошибка сохранения: "+esc(err.Error()))
		return
	}

	size := "?"
	if info, err := os.Stat(filePath); err == nil {
		size = humanSize(info.Size())
	}
	savedName := filepath.Base(filePath)
	tb.reply(ctx, update, fmt.Sprintf(
		"✅ <code>%s</code> (%s)\n📂 <code>%s</code>\n<i>%s</i>",
		esc(savedName), size, esc(targetDir), esc(routedVia)),
		tb.shareActionsKB(uid, filePath))
}

func uniqueFilename(dir, name string) string {
	target := filepath.Join(dir, name)
	if _, err := os.Stat(target); err != nil {
		return target
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s_%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
}
