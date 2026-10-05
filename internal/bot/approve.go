package bot

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// ── Подтверждение protected paths оркестратора (ap:) ──────────────────
//
// Оркестратор создаётся внутри internal/agents — бот до экземпляра не
// достаёт, поэтому подтверждение подключается через пакетный хук
// orchestrator.SetDefaultConfirmFunc (ставится в Start). Гейт блокирует
// горутину агента, пока пользователь не нажмёт кнопку (или таймаут → отказ).

const approvalTimeout = 5 * time.Minute

var (
	apprMu      sync.Mutex
	apprSeq     int
	apprPending = map[string]chan bool{}
)

// orchestratorConfirm — orchestrator.ConfirmFunc: спрашивает пользователя в
// Telegram кнопками «✅ Разрешить / ↩️ Запретить». Любая неясность (нет чата,
// не отправилось, таймаут) трактуется как отказ — fail-closed.
func (tb *Bot) orchestratorConfirm(what string) bool {
	target, ok := tb.anyChatTarget()
	if !ok {
		return false
	}

	apprMu.Lock()
	apprSeq++
	id := fmt.Sprintf("%d", apprSeq)
	ch := make(chan bool, 1)
	apprPending[id] = ch
	apprMu.Unlock()
	defer func() {
		apprMu.Lock()
		delete(apprPending, id)
		apprMu.Unlock()
	}()

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "✅ Разрешить", CallbackData: "ap:y:" + id},
			{Text: "↩️ Запретить", CallbackData: "ap:n:" + id},
		}},
	}
	_, err := tb.b.SendMessage(context.Background(), &bot.SendMessageParams{
		ChatID:          target.ChatID,
		MessageThreadID: target.ThreadID,
		Text: fmt.Sprintf(
			"🛡 <b>Оркестратор просит доступ к защищённому пути</b>\n\n<code>%s</code>\n\n"+
				"Разрешить один раз? Без ответа за %d мин — отказ.", esc(what), int(approvalTimeout.Minutes())),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: kb,
	})
	if err != nil {
		return false
	}

	select {
	case v := <-ch:
		return v
	case <-time.After(approvalTimeout):
		return false
	}
}

// anyChatTarget выбирает чат для эскалации. Бот однопользовательский
// (allowed), так что берём последний известный чат первого разрешённого
// пользователя; если список пуст — единственный запомненный чат.
func (tb *Bot) anyChatTarget() (chatTarget, bool) {
	tb.chatMu.RLock()
	defer tb.chatMu.RUnlock()
	if len(tb.allowed) > 0 {
		uids := make([]int64, 0, len(tb.allowed))
		for uid := range tb.allowed {
			uids = append(uids, uid)
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		for _, uid := range uids {
			if t, ok := tb.chatTargets[uid]; ok {
				return t, true
			}
		}
		return chatTarget{}, false
	}
	if len(tb.chatTargets) == 1 {
		for _, t := range tb.chatTargets {
			return t, true
		}
	}
	return chatTarget{}, false
}

func (tb *Bot) onApproveCB(ctx context.Context, b *bot.Bot, update *models.Update) {
	query := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	uid := query.From.ID
	if !tb.authorized(uid) {
		return
	}
	data := query.Data // "ap:y:<id>" / "ap:n:<id>"

	var allow bool
	var id string
	switch {
	case len(data) > 5 && data[:5] == "ap:y:":
		allow, id = true, data[5:]
	case len(data) > 5 && data[:5] == "ap:n:":
		allow, id = false, data[5:]
	default:
		return
	}

	apprMu.Lock()
	ch, ok := apprPending[id]
	if ok {
		delete(apprPending, id)
	}
	apprMu.Unlock()
	if !ok {
		tb.answerCB(ctx, update, "⌛ Запрос уже обработан или истёк.")
		return
	}
	ch <- allow

	if allow {
		tb.answerCB(ctx, update, "✅ Разрешено — оркестратор продолжает.")
	} else {
		tb.answerCB(ctx, update, "↩️ Запрещено — действие отклонено.")
	}
}
