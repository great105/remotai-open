package bot

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/sessions"
)

// ── Topic commands ───────────────────────────────────────────────────

// cmdBind binds the current topic to an agent: /bind <agent> [cwd]
func (tb *Bot) cmdBind(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}

	threadID := getThreadID(update)
	if threadID == 0 {
		tb.reply(ctx, update,
			"❌ Команда /bind работает только в <b>топиках</b> (forum topics).\n"+
				"Отправьте её в конкретном топике группы.")
		return
	}

	args := cmdArgs(update.Message.Text)
	if len(args) == 0 {
		// Show agent picker for binding
		kb := agentPickerKB("tp:bind:")
		tb.reply(ctx, update, "🔗 <b>Привязка топика</b>\n\nВыберите агента для этого топика:", kb)
		return
	}

	agentType := strings.ToLower(args[0])
	if !agents.IsAvailable(agentType) {
		tb.reply(ctx, update, fmt.Sprintf(
			"❌ Неизвестный агент <code>%s</code>. Доступные: %s",
			esc(agentType), agentIDs()))
		return
	}

	chatID := getChatID(update)
	cwd := ""
	if len(args) > 1 {
		cwd = strings.Join(args[1:], " ")
	} else {
		if sess := tb.store.GetActive(int(uid)); sess != nil {
			cwd = sess.Cwd
		} else {
			cwd, _ = os.Getwd()
		}
	}

	tb.bindTopic(ctx, update, uid, chatID, threadID, agentType, cwd)
}

// cmdUnbind removes the topic binding: /unbind
func (tb *Bot) cmdUnbind(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}

	threadID := getThreadID(update)
	if threadID == 0 {
		tb.reply(ctx, update, "❌ Команда /unbind работает только в топиках.")
		return
	}

	chatID := getChatID(update)
	key := sessions.TopicKey(chatID, threadID)
	binding := tb.topics.Get(key)
	if binding == nil {
		tb.reply(ctx, update, "ℹ️ Этот топик не привязан к агенту.")
		return
	}

	tb.topics.Unbind(key)
	tb.reply(ctx, update, fmt.Sprintf(
		"✅ Топик отвязан от <code>%s</code>\nСессия <b>%s</b> осталась, можно закрыть через /close %s",
		esc(binding.AgentType), esc(binding.SessionName), esc(binding.SessionName)))
}

// cmdTopics lists all topic bindings: /topics
func (tb *Bot) cmdTopics(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}

	bindings := tb.topics.List()
	if len(bindings) == 0 {
		tb.reply(ctx, update,
			"📋 Нет привязок топиков.\n\n"+
				"Используйте /bind <code>агент</code> в топике группы.")
		return
	}

	lines := []string{"🔗 <b>Привязки топиков:</b>\n"}
	for key, binding := range bindings {
		desc := agents.GetDescriptor(binding.AgentType)
		icon := "🤖"
		if desc != nil {
			icon = desc.Icon
		}
		status := "✅"
		sess := tb.store.Get(int(uid), binding.SessionName)
		if sess == nil {
			status = "⚪"
		} else if sess.IsBusy {
			status = "⏳"
		}
		lines = append(lines, fmt.Sprintf(
			"%s %s <code>%s</code> → <b>%s</b> (%s)",
			status, icon, esc(key), esc(binding.SessionName), esc(binding.AgentType)))
	}

	tb.reply(ctx, update, strings.Join(lines, "\n"))
}

// ── Topic callback handlers ──────────────────────────────────────────

func (tb *Bot) onTopicCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data

	switch {
	case strings.HasPrefix(data, "tp:bind:"):
		agentType := data[8:]
		if !agents.IsAvailable(agentType) {
			tb.answerCB(ctx, update, "❌ Агент недоступен: "+esc(agentType))
			return
		}

		threadID := getThreadID(update)
		if threadID == 0 {
			tb.answerCB(ctx, update, "❌ Не удалось определить топик.")
			return
		}

		chatID := getChatID(update)
		cwd := ""
		if sess := tb.store.GetActive(int(uid)); sess != nil {
			cwd = sess.Cwd
		} else {
			cwd, _ = os.Getwd()
		}

		tb.bindTopicCB(ctx, update, uid, chatID, threadID, agentType, cwd)
	}
}

// ── Topic binding logic ──────────────────────────────────────────────

func (tb *Bot) bindTopic(ctx context.Context, update *models.Update, uid, chatID int64, threadID int, agentType, cwd string) {
	key := sessions.TopicKey(chatID, threadID)
	sessionName := fmt.Sprintf("topic-%d-%s", threadID, agentType)

	binding := &sessions.TopicBinding{
		AgentType:   agentType,
		SessionName: sessionName,
		Cwd:         cwd,
		Persistent:  true,
	}
	tb.topics.Bind(key, binding)

	// Auto-create session if it doesn't exist
	sess := tb.ensureTopicSession(uid, binding, threadID)
	if sess == nil {
		tb.reply(ctx, update, "❌ Не удалось создать сессию для топика.")
		return
	}

	desc := agents.GetDescriptor(agentType)
	icon := "🤖"
	if desc != nil {
		icon = desc.Icon
	}

	tb.reply(ctx, update, fmt.Sprintf(
		"✅ <b>Топик привязан</b>\n\n"+
			"%s Агент: <code>%s</code>\n"+
			"📌 Сессия: <b>%s</b>\n"+
			"📂 Папка: <code>%s</code>\n"+
			"🔄 Режим: persistent\n\n"+
			"💬 Все сообщения в этом топике пойдут агенту <code>%s</code>.",
		icon, esc(agentType), esc(sessionName), esc(cwd), esc(agentType)))
}

func (tb *Bot) bindTopicCB(ctx context.Context, update *models.Update, uid, chatID int64, threadID int, agentType, cwd string) {
	key := sessions.TopicKey(chatID, threadID)
	sessionName := fmt.Sprintf("topic-%d-%s", threadID, agentType)

	binding := &sessions.TopicBinding{
		AgentType:   agentType,
		SessionName: sessionName,
		Cwd:         cwd,
		Persistent:  true,
	}
	tb.topics.Bind(key, binding)

	sess := tb.ensureTopicSession(uid, binding, threadID)
	if sess == nil {
		tb.answerCB(ctx, update, "❌ Не удалось создать сессию для топика.")
		return
	}

	tb.answerCB(ctx, update, fmt.Sprintf(
		"✅ Топик привязан к <code>%s</code>\n📌 Сессия: <b>%s</b>\n📂 <code>%s</code>",
		esc(agentType), esc(sessionName), esc(cwd)))
}

// ensureTopicSession finds or creates the session for a topic binding.
func (tb *Bot) ensureTopicSession(uid int64, binding *sessions.TopicBinding, threadID int) *sessions.Session {
	sess := tb.store.Get(int(uid), binding.SessionName)
	if sess != nil {
		return sess
	}

	cwd := binding.Cwd
	if cwd == "" {
		if active := tb.store.GetActive(int(uid)); active != nil {
			cwd = active.Cwd
		} else {
			cwd, _ = os.Getwd()
		}
	}

	sess, err := tb.store.Create(int(uid), binding.SessionName, binding.AgentType, cwd)
	if err != nil {
		return nil
	}

	tb.store.SetTopicID(int(uid), binding.SessionName, threadID)
	if binding.Persistent {
		tb.store.SetMode(int(uid), binding.SessionName, sessions.ModePersistent)
	}
	if binding.PermissionMode != "" {
		tb.store.SetPermissionMode(int(uid), binding.SessionName, binding.PermissionMode)
	}

	return sess
}

// resolveTopicSession resolves the session for a topic-bound message.
// Returns session name or "" if no topic binding.
func (tb *Bot) resolveTopicSession(uid, chatID int64, threadID int) string {
	if threadID == 0 {
		return ""
	}
	key := sessions.TopicKey(chatID, threadID)
	binding := tb.topics.Get(key)
	if binding == nil {
		return ""
	}

	sess := tb.ensureTopicSession(uid, binding, threadID)
	if sess == nil {
		return ""
	}
	return sess.Name
}
