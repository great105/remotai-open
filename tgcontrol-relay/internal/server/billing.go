package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"tgcontrol-relay/internal/billing"
	"tgcontrol-relay/internal/db"
)

// Приём денег через ЮKassa. Контракт выяснен живыми запросами к боевому
// магазину 01.09.2026 — подробности в internal/billing/yookassa.go.
//
// Границы, которые здесь держатся:
//   • деньги берём только за облако; локальное управление бесплатно навсегда,
//     поэтому оплату вообще не предлагаем тем, кому она не нужна;
//   • founder не платит НИКОГДА (71 аккаунт до 05.08.2026, канон);
//   • webhook открыт наружу, поэтому его словам мы НЕ ВЕРИМ: статус платежа
//     перепроверяем запросом к самой ЮKassa.

// Оплаченный период — ровно 30 дней, как указано перед оплатой.
const paidPeriod = 30 * 24 * time.Hour

// Проверочный платёж даёт СУТКИ, а не месяц: он нужен, чтобы убедиться, что
// деньги доходят, чек приходит и доступ открывается, — а не чтобы получить
// Про за десять рублей. Даже себе.
const testPeriod = 24 * time.Hour

func periodForTier(tier string) time.Duration {
	if tier == "test" {
		return testPeriod
	}
	return paidPeriod
}

type checkoutRequest struct {
	Tier   string `json:"tier"`   // pro | fleet
	Method string `json:"method"` // sbp | bank_card | "" — человек выберет на форме
	Email  string `json:"email"`  // для чека
}

// POST /v1/billing/checkout — создать платёж и вернуть ссылку на оплату.
func (s *Server) handleBillingCheckout(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if !s.billingEnabled() {
		writeErr(w, http.StatusNotImplemented, "Оплата пока не подключена")
		return
	}
	client := s.yooKassa()
	if client == nil {
		// Честный отказ вместо кнопки в никуда.
		writeErr(w, http.StatusNotImplemented, "Оплата пока не подключена")
		return
	}
	var req checkoutRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	plan, ok := planByID(req.Tier)
	if !ok || plan.Monthly <= 0 {
		writeErr(w, http.StatusBadRequest, "Неизвестная полка")
		return
	}
	u, err := db.GetUserByID(r.Context(), s.DB, claims.UserID)
	if err != nil || u == nil {
		writeErr(w, http.StatusInternalServerError, "user not found")
		return
	}
	// Скрытая полка («Проверка оплаты», 10 ₽) — только администратору. Без этой
	// проверки любой, кто узнал её идентификатор, покупал бы доступ за десятку:
	// то, что полки нет на витрине, само по себе не защита.
	if plan.hidden && !s.isAdminTelegramID(u.TelegramID) {
		writeErr(w, http.StatusForbidden, "Эта полка недоступна")
		return
	}
	// Основателям платить не за что: у них Про навсегда (канон, п. 5).
	//
	// ⚠ Кроме служебной полки. Владелец — основатель, и до 01.09.2026 этот
	// запрет делал проверочный платёж невозможным для того единственного
	// человека, которому он и нужен: кнопка «10 ₽» есть, нажатие — 409. А
	// проверочный платёж не покупка доступа, а проба живыми деньгами того
	// пути, по которому пойдут чужие: касса → чек → карта → продление.
	if u.Founder && !plan.hidden {
		writeErr(w, http.StatusConflict, "У вас Про навсегда — оплата не нужна")
		return
	}
	// Почта для чека (54-ФЗ) — из трёх источников по убыванию свежести:
	// что человек ввёл сейчас → что он вводил в прошлый раз → его email-вход.
	//
	// ⚠ 01.09.2026: раньше источник был ОДИН — email-вход, и заплатить не мог
	// никто. В продукт входят через Telegram и по коду с устройства: в
	// user_identities не было ни одной почты ни у одного из 72 пользователей.
	// Каждый checkout упирался в «Нужна почта для чека», а спросить её было
	// негде — поля в приложении не существовало.
	email := strings.TrimSpace(req.Email)
	if email != "" && !strings.Contains(email, "@") {
		writeErrCode(w, http.StatusBadRequest, "email_invalid", "Проверьте адрес почты")
		return
	}
	if email == "" {
		email = strings.TrimSpace(u.BillingEmail)
	}
	if email == "" {
		if ids, err := db.ListIdentities(r.Context(), s.DB, u.ID); err == nil {
			for _, id := range ids {
				if id.Provider == "email" && strings.Contains(id.ProviderUID, "@") {
					email = id.ProviderUID
					break
				}
			}
		}
	}
	if email == "" {
		// Отдельный КОД, а не только текст: по нему приложение показывает поле
		// почты и повторяет оплату, вместо тупика «что-то пошло не так».
		//
		// ⚠ Текст видят ТОЛЬКО старые приложения: те, что понимают код, вместо
		// него рисуют поле и ошибку не показывают вовсе. Владелец 02.09.2026
		// упёрся именно в это — кнопка нажимается, красная надпись есть, а
		// ввести адрес негде. Поэтому текст здесь — инструкция для того, у кого
		// приложение ещё не обновилось, а не описание причины.
		writeErrCode(w, http.StatusBadRequest, "email_required",
			"Нужна почта для чека. Обновите приложение — в личном кабинете появится поле для адреса")
		return
	}
	// Запоминаем, чтобы спросить один раз за всё время, а не на каждом платеже.
	if email != u.BillingEmail {
		if err := db.SetBillingEmail(r.Context(), s.DB, u.ID, email); err != nil {
			log.Printf("[BILLING] почта для чека user=%d: %v", u.ID, err)
		}
	}

	pay, err := client.Create(r.Context(), billing.CreateRequest{
		Amount: billing.Money(plan.Monthly),
		// Описание человек увидит в банке и в чеке — срок должен быть настоящим:
		// у служебной полки это сутки, а не месяц.
		Description: plan.Name + ", " + planPeriodName(plan.ID) + " — Remotai",
		ReturnURL:   s.Config.YooKassaReturnURL,
		Email:       email,
		Method:      req.Method,
		// Fixed-period purchase: never opt a buyer into unverified renewals.
		SaveCard: false,
		Metadata: map[string]string{
			"user_id": itoa(u.ID),
			"tier":    plan.ID,
		},
	})
	if err != nil {
		log.Printf("[BILLING] создание платежа для user=%d: %v", u.ID, err)
		writeErr(w, http.StatusBadGateway, "Касса не ответила, попробуйте ещё раз")
		return
	}
	if err := db.RecordPayment(r.Context(), s.DB, db.Payment{
		PaymentID:   pay.ID,
		UserID:      u.ID,
		Tier:        plan.ID,
		AmountMinor: int64(plan.Monthly),
		Status:      "pending",
		Method:      pay.PaymentMethod.Type,
	}); err != nil {
		log.Printf("[BILLING] журнал платежа %s: %v", pay.ID, err)
		writeErr(w, http.StatusServiceUnavailable, "Не удалось сохранить заказ. Если деньги списаны, обратитесь в поддержку; иначе попробуйте позже")
		return
	}
	if pay.Status == "succeeded" || pay.Status == "canceled" {
		if _, err := s.applyPayment(r.Context(), pay); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "Проверяем оплату. Обновите личный кабинет через минуту")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"payment_id":       pay.ID,
		"status":           pay.Status,
		"confirmation_url": pay.Confirmation.ConfirmationURL,
		"amount_minor":     plan.Monthly,
		"tier":             plan.ID,
	})
}

// GET /v1/billing/subscription — что показывать на экране подписки.
func (s *Server) handleBillingSubscription(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	u, err := db.GetUserByID(r.Context(), s.DB, claims.UserID)
	if err != nil || u == nil {
		writeErr(w, http.StatusInternalServerError, "user not found")
		return
	}
	if s.selfHosted() {
		writeJSON(w, http.StatusOK, map[string]any{
			"tier": "team", "founder": u.Founder, "self_hosted": true,
			"billing_enabled": false, "card": nil, "paid_until": nil,
			"auto_renew": false, "payment_mode": "none", "period_days": 0,
			"can_test_pay": false, "billing_email": "",
		})
		return
	}
	// Досматриваем незакрытые платежи ДО чтения подписки: человек возвращается
	// из кассы прямо на этот экран, и увидеть он должен уже оплаченный доступ.
	//
	// ⚠ Перечитываем пользователя: тариф считается из его строки вместе с
	// подпиской (LEFT JOIN), а прочитан он был до зачисления — иначе кабинет
	// показал бы «бесплатно» человеку, которому только что открыл доступ.
	if s.settlePending(r.Context(), u.ID) {
		if fresh, err := db.GetUserByID(r.Context(), s.DB, u.ID); err == nil && fresh != nil {
			u = fresh
		}
	}

	sub, err := db.GetSubscription(r.Context(), s.DB, u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{
		"tier":            s.effectiveTier(u),
		"founder":         u.Founder,
		"billing_enabled": s.billingEnabled(),
		"self_hosted":     s.selfHosted(),
		"card":            nil,
		"paid_until":      nil,
		"auto_renew":      false,
		"payment_mode":    "one_time",
		"period_days":     30,
		// Проверочный платёж показываем только администратору — решает сервер,
		// клиент лишь рисует по флагу. Иначе кнопку «10 ₽» было бы видно всем,
		// кто откроет исходники приложения.
		"can_test_pay": s.isAdminTelegramID(u.TelegramID),
		// Почта для чека. Владелец 02.09.2026: «почту надо сохранять, чтобы не
		// вводить 2 раза». Она и сохраняется — но человеку это ниоткуда не было
		// видно, а невидимое сохранение равно отсутствующему: в следующий раз он
		// опять ждёт, что спросят.
		"billing_email": u.BillingEmail,
	}
	if sub != nil {
		if sub.PaidUntil.Valid {
			out["paid_until"] = sub.PaidUntil.Time.UTC().Format(time.RFC3339)
		}
		// Карта показывается ради одного — чтобы человек её узнал и мог
		// отвязать. Полного номера у нас нет и не бывает.
		if sub.HasCard() {
			out["card"] = map[string]any{"last4": sub.CardLast4, "type": sub.CardType}
		}
		if sub.UnboundAt.Valid {
			out["unbound_at"] = sub.UnboundAt.Time.UTC().Format(time.RFC3339)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /v1/billing/unbind — человек отвязывает карту сам.
//
// ⚠ Требование ЮKassa (менеджер, 01.09.2026): отвязка должна быть доступна
// человеку в любой момент и БЕЗ запроса к ним, но токен повторов мы обязаны
// удалить у себя. Оплаченные дни при этом не сгорают — за них уже заплачено.
func (s *Server) handleBillingUnbind(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := db.UnbindCard(r.Context(), s.DB, claims.UserID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("[BILLING] карта отвязана пользователем user=%d", claims.UserID)
	sub, _ := db.GetSubscription(r.Context(), s.DB, claims.UserID)
	resp := map[string]any{"ok": true, "auto_renew": false, "card": nil}
	if sub != nil && sub.PaidUntil.Valid {
		resp["paid_until"] = sub.PaidUntil.Time.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /v1/billing/webhook — уведомление от ЮKassa.
//
// ⚠ Ручка открыта наружу, значит её словам верить нельзя: кто угодно может
// прислать «платёж успешен». Поэтому из уведомления берём ТОЛЬКО идентификатор
// платежа, а состояние спрашиваем у самой ЮKassa по ключу магазина.
func (s *Server) handleBillingWebhook(w http.ResponseWriter, r *http.Request) {
	client := s.yooKassa()
	if client == nil {
		writeErr(w, http.StatusNotImplemented, "billing off")
		return
	}
	var event struct {
		Event  string `json:"event"`
		Object struct {
			ID string `json:"id"`
		} `json:"object"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&event); err != nil || event.Object.ID == "" {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}

	pay, err := client.Get(r.Context(), event.Object.ID)
	if err != nil {
		var apiErr *billing.APIError
		if errors.As(err, &apiErr) && apiErr.HTTPStatus == http.StatusNotFound {
			// Чужой или выдуманный платёж — молча закрываем, не 500.
			log.Printf("[BILLING] webhook: платёж %s не наш", event.Object.ID)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		log.Printf("[BILLING] webhook: не спросить статус %s: %v", event.Object.ID, err)
		// 500 — чтобы ЮKassa повторила: терять оплату нельзя.
		writeErr(w, http.StatusInternalServerError, "retry later")
		return
	}

	dup, err := s.applyPayment(r.Context(), pay)
	if err != nil {
		// 500 — чтобы ЮKassa повторила: терять оплату нельзя.
		writeErr(w, http.StatusInternalServerError, "retry later")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": dup})
}

// applyPayment атомарно записывает оплату и продлевает доступ. Общая для
// уведомления ЮKassa и досмотра из кабинета; повтор не продлевает срок дважды.
func (s *Server) applyPayment(ctx context.Context, pay *billing.Payment) (duplicate bool, err error) {
	rec, err := db.GetPayment(ctx, s.DB, pay.ID)
	if err != nil {
		return false, err
	}
	if rec == nil {
		return true, nil
	}
	if pay.Test && rec.Tier != "test" {
		return false, errors.New("test payment cannot grant a commercial plan")
	}
	return db.ApplyPaymentResult(ctx, s.DB, pay.ID, pay.Status, pay.PaymentMethod.Type,
		pay.Amount.Value, pay.Amount.Currency, pay.Paid, periodForTier(rec.Tier))
}

// settlePending — досмотр незакрытых платежей человека при открытии кабинета.
//
// ⚠ Страховка на случай, когда уведомление от ЮKassa не дошло: адрес для
// уведомлений задаётся в личном кабинете магазина, и по ключу магазина его не
// прочитать (API /v3/webhooks требует OAuth) — то есть проверить его настройку
// из кода нельзя в принципе. Без досмотра единственный сбой доставки означал бы
// «деньги списаны, доступа нет» — худшее, что может случиться на этом экране.
//
// Тихая: любая ошибка здесь не должна мешать показать кабинет. Возвращает
// true, если что-то зачла — тогда сведения о человеке надо перечитать, иначе
// экран покажет доступ, который был ДО оплаты.
func (s *Server) settlePending(ctx context.Context, userID int64) bool {
	client := s.yooKassa()
	if client == nil {
		return false
	}
	// A missed webhook must still be reconciled after a long absence.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	list, err := db.PendingPayments(ctx, s.DB, userID, time.Time{})
	if err != nil || len(list) == 0 {
		return false
	}
	settled := false
	log.Printf("[BILLING] досмотр user=%d: незакрытых платежей %d", userID, len(list))
	for _, p := range list {
		pay, err := client.Get(ctx, p.PaymentID)
		if err != nil {
			// ⚠ Молчать здесь нельзя. 02.09.2026 первый живой платёж остался
			// незакрытым, и разобрать почему было НЕЧЕМ: цикл проглатывал
			// ошибку без следа, а в журнале не появлялось ни строки.
			log.Printf("[BILLING] досмотр: не спросить статус %s: %v", p.PaymentID, err)
			continue
		}
		if pay.Status != "succeeded" && pay.Status != "canceled" {
			log.Printf("[BILLING] досмотр: платёж %s ещё в работе у кассы", p.PaymentID)
			continue // человек ещё не закончил оплату — не наш случай
		}
		if _, err := s.applyPayment(ctx, pay); err != nil {
			log.Printf("[BILLING] досмотр платежа %s: %v", p.PaymentID, err)
			continue
		}
		log.Printf("[BILLING] досмотр: платёж %s закрыт статусом %s без уведомления", p.PaymentID, pay.Status)
		settled = true
	}
	return settled
}

// yooKassa — клиент магазина; nil, если ключей нет (тогда касса выключена).
func (s *Server) yooKassa() *billing.Client {
	if s.BillingClient != nil {
		return s.BillingClient
	}
	if s.Config == nil {
		return nil
	}
	c := billing.New(s.Config.YooKassaShopID, s.Config.YooKassaSecretKey)
	if !c.Configured() {
		return nil
	}
	return c
}

// planPeriodName — срок полки словами, для описания платежа и чека.
func planPeriodName(tier string) string {
	if periodForTier(tier) == testPeriod {
		return "сутки"
	}
	return "30 дней"
}

// POST /v1/billing/email — сменить почту для чека.
//
// Отдельная ручка, а не поле в checkout: адрес меняют и тогда, когда платить
// прямо сейчас не собираются («чек уходит не туда»), и заставлять ради этого
// начинать оплату было бы издевательством.
func (s *Server) handleBillingEmail(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	email := strings.TrimSpace(req.Email)
	if !strings.Contains(email, "@") || len(email) > 254 {
		writeErrCode(w, http.StatusBadRequest, "email_invalid", "Проверьте адрес почты")
		return
	}
	if err := db.SetBillingEmail(r.Context(), s.DB, claims.UserID, email); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "billing_email": email})
}
