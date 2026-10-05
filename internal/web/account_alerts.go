package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/aiusage"
	"tgcontrol/internal/config"
	"tgcontrol/internal/dnsfallback"
	"tgcontrol/internal/relay"
)

// Сообщение в Telegram, когда у рабочего аккаунта кончается лимит.
//
// ЗАЧЕМ. Человек держит вторую подписку ровно затем, чтобы не встать посреди
// работы. Но узнаёт он об исчерпании последним: агент упирается в лимит где-то
// в терминале, а телефон в кармане молчит. Раз лимиты мы и так снимаем — можем
// сказать заранее и назвать аккаунт, на который есть смысл переключиться.
//
// ПРАВИЛА, которые здесь важнее кода:
//  1. Сообщение отправляется, только если ЕСТЬ ЧТО ПРЕДЛОЖИТЬ: у другого
//     аккаунта того же сервиса запас заметно больше. Иначе это не новость, а
//     расстройство — сделать человек всё равно ничего не может.
//  2. Одно сообщение на переход через порог, а не на каждый опрос. Повторяем
//     только после того, как аккаунт восстановился (лимит сбросился) — и о
//     самом восстановлении тоже говорим: это и есть «можно вернуться».
//  3. Молчим, если аккаунтов у сервиса всего один: переключаться некуда.
const (
	// alertLowPercent — «осталось мало». 15% пятичасового окна — это примерно
	// полчаса работы: успеть переключиться, а не узнать постфактум.
	alertLowPercent = 15.0
	// alertSparePercent — запас у соседа, при котором предложение осмысленно.
	alertSparePercent = 40.0
	// alertRecoverPercent — с какого остатка считаем, что окно сбросилось.
	alertRecoverPercent = 60.0
	// alertInterval — как часто смотреть. Снимок и так кэширован (5 минут),
	// поэтому опрос вендоров от этого не учащается.
	alertInterval = 10 * time.Minute
)

// alertState помнит, о чём уже говорили: ключ — «агент:аккаунт».
var alertState struct {
	sync.Mutex
	warned map[string]bool
}

// StartAccountAlerts запускает сторожа лимитов. Зовётся один раз при старте
// агента; на устройстве без облака и без аккаунтов он ничего не делает.
func StartAccountAlerts() {
	alertState.warned = map[string]bool{}
	go func() {
		// Первый проход — не сразу: на старте агент занят более важным, да и
		// снимок лимитов ещё не собран.
		time.Sleep(2 * time.Minute)
		for {
			checkAccountLimits()
			time.Sleep(alertInterval)
		}
	}()
}

// checkAccountLimits — один проход сторожа.
func checkAccountLimits() {
	// Выключенные уведомления выключают и это: сторож не имеет права быть
	// исключением из общей настройки. (Релей уважает «не беспокоить» ещё раз,
	// уже на своей стороне, — но полагаться на чужую проверку неправильно.)
	if strings.EqualFold(strings.TrimSpace(config.GetNoSetup().NotificationsEnabled), "false") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	snap := aiusage.CollectCached(ctx)

	// Группируем по сервису: предлагать можно только соседа того же вендора.
	byProvider := map[string][]aiusage.Provider{}
	for _, p := range snap.Providers {
		if p.Status == "available" || p.Status == "signed_out" {
			byProvider[p.ID] = append(byProvider[p.ID], p)
		}
	}

	for id, list := range byProvider {
		if len(list) < 2 {
			continue // переключаться некуда — новости нет
		}
		var active *aiusage.Provider
		for i := range list {
			if list[i].AccountActive {
				active = &list[i]
			}
		}
		if active == nil || active.Status != "available" {
			continue
		}
		left := leftPercent(*active)
		if left < 0 {
			continue // процента нет — врать нечем
		}
		key := id + ":" + active.AccountID

		// Восстановление: лимит сбросился — говорим и снимаем отметку.
		if left >= alertRecoverPercent {
			alertState.Lock()
			warned := alertState.warned[key]
			delete(alertState.warned, key)
			alertState.Unlock()
			if warned {
				sendOwnerText(fmt.Sprintf("♻️ %s · %s: лимит сбросился, осталось %d%%.",
					active.Name, accountName(*active), int(left)))
			}
			continue
		}
		if left > alertLowPercent {
			continue
		}

		// Есть ли куда переключиться: сосед с заметным запасом.
		best, bestLeft := "", -1.0
		for _, p := range list {
			if p.AccountID == active.AccountID || p.Status != "available" {
				continue
			}
			if l := leftPercent(p); l > bestLeft {
				best, bestLeft = accountName(p), l
			}
		}
		if bestLeft < alertSparePercent {
			continue // предложить нечего — молчим, чтобы не расстраивать зря
		}

		alertState.Lock()
		already := alertState.warned[key]
		alertState.warned[key] = true
		alertState.Unlock()
		if already {
			continue // одно сообщение на переход через порог
		}
		sendOwnerText(fmt.Sprintf(
			"⚠️ %s · %s: осталось %d%% за 5 ч.\nУ аккаунта «%s» свободно %d%% — переключить можно в разделе «Агенты».",
			active.Name, accountName(*active), int(left), best, int(bestLeft)))
	}
}

// leftPercent — сколько осталось в пятичасовом окне (или в его аналоге).
// −1 означает «неизвестно»: 0% здесь был бы прямой неправдой.
func leftPercent(p aiusage.Provider) float64 {
	if p.Stale {
		return -1
	}
	five, _ := aiusage.MainWindows(p)
	if five != nil {
		return 100 - five.UsedPercent
	}
	return -1
}

func accountName(p aiusage.Provider) string {
	if p.AccountLabel != "" {
		return p.AccountLabel
	}
	if p.Account != "" {
		return p.Account
	}
	return "основной"
}

// SendOwnerText — та же отправка для других частей агента (самодиагностика
// перерыва, сторож VPN). Канал один намеренно: он уже уважает «не беспокоить»
// и не требует ни бота на этой машине, ни знания chat_id.
//
// Настройку уведомлений проверяем здесь же: выключенные уведомления выключают
// и системные сообщения — исключений из общей настройки быть не должно, иначе
// человек, попросивший тишины, всё равно получает сообщения.
func SendOwnerText(text string) {
	if strings.EqualFold(strings.TrimSpace(config.GetNoSetup().NotificationsEnabled), "false") {
		return
	}
	sendOwnerText(text)
}

// sendOwnerText отправляет сообщение владельцу тем же путём, что и
// `remotai send`: POST /v1/agent/send с ключом устройства, дальше релей и бот.
// Отдельного канала для этого заводить не надо — этот уже есть и уважает
// «не беспокоить» на стороне релея.
func sendOwnerText(text string) {
	cfg := config.GetNoSetup()
	base := cfg.RelayHTTPBase()
	if base == "" {
		return
	}
	jwt := cfg.RelayJWT
	if jwt == "" {
		jwt, _ = relay.LoadJWT()
	}
	if jwt == "" {
		return // устройство не привязано — писать некому
	}
	raw, _ := json.Marshal(map[string]any{"text": text})
	req, err := http.NewRequest(http.MethodPost, base+"/v1/agent/send", bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	// Тот же запасной резолвер, что у `remotai send`: сообщение нужно как раз
	// тогда, когда на машине что-то не так.
	client := &http.Client{Timeout: 30 * time.Second, Transport: dnsfallback.Transport()}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[ACCOUNTS] уведомление о лимите не ушло: %v", err)
		return
	}
	_ = resp.Body.Close()
}
