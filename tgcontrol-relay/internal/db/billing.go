package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type billingStore interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Подписка и платежи ЮKassa. Таблица subscriptions живёт с 0002 (её писали под
// Stripe, который так и не подключили), поля ЮKassa добавлены в 0021 — чтобы у
// одного человека не оказалось двух подписок в двух местах.

// Subscription — что известно о платной подписке человека.
type Subscription struct {
	UserID      int64
	Tier        string
	PaidUntil   sql.NullTime
	CancelledAt sql.NullTime
	// PaymentMethodID — токен способа оплаты в ЮKassa, по нему идут автосписания.
	// Пусто = карта не привязана или человек её отвязал.
	PaymentMethodID string
	CardLast4       string
	CardType        string
	UnboundAt       sql.NullTime
}

// HasCard — есть ли чем списывать в следующем месяце.
func (s *Subscription) HasCard() bool { return s != nil && s.PaymentMethodID != "" }

// Active — оплачен ли доступ прямо сейчас.
func (s *Subscription) Active(now time.Time) bool {
	return s != nil && s.PaidUntil.Valid && s.PaidUntil.Time.After(now)
}

// GetSubscription возвращает подписку пользователя. Её отсутствие — не ошибка:
// у большинства людей подписки нет и не должно быть (локальное всё бесплатно).
func GetSubscription(ctx context.Context, d billingStore, userID int64) (*Subscription, error) {
	row := d.QueryRowContext(ctx, `
		SELECT user_id, COALESCE(tier,'free'), current_period_end, cancelled_at,
		       COALESCE(payment_method_id,''), COALESCE(card_last4,''),
		       COALESCE(card_type,''), unbound_at
		  FROM subscriptions WHERE user_id = ?`, userID)
	var s Subscription
	err := row.Scan(&s.UserID, &s.Tier, &s.PaidUntil, &s.CancelledAt,
		&s.PaymentMethodID, &s.CardLast4, &s.CardType, &s.UnboundAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ExtendSubscription продлевает оплаченный срок и запоминает способ оплаты.
//
// ⚠ Продление считается от БОЛЬШЕЙ из двух дат: конца оплаченного периода и
// «сейчас». Иначе человек, заплативший за неделю до конца месяца, терял
// остаток — а он за него уже отдал деньги.
func ExtendSubscription(ctx context.Context, d billingStore, userID int64, tier string, period time.Duration, methodID, last4, cardType string) (time.Time, error) {
	now := time.Now().UTC()
	cur, err := GetSubscription(ctx, d, userID)
	if err != nil {
		return time.Time{}, err
	}
	from := now
	if cur != nil && cur.PaidUntil.Valid && cur.PaidUntil.Time.After(now) {
		from = cur.PaidUntil.Time
	}
	until := from.Add(period)

	// Способ оплаты перезаписываем ТОЛЬКО когда он пришёл: у автосписания его в
	// ответе может не быть, и пустая строка стёрла бы привязку молча.
	if methodID != "" {
		_, err = d.ExecContext(ctx, `
			INSERT INTO subscriptions (user_id, tier, current_period_end, cancelled_at,
			                           provider, payment_method_id, card_last4, card_type, unbound_at, updated_at)
			VALUES (?, ?, ?, NULL, 'yookassa', ?, ?, ?, NULL, CURRENT_TIMESTAMP)
			ON CONFLICT(user_id) DO UPDATE SET
			  tier = excluded.tier,
			  current_period_end = excluded.current_period_end,
			  cancelled_at = NULL,
			  provider = 'yookassa',
			  payment_method_id = excluded.payment_method_id,
			  card_last4 = excluded.card_last4,
			  card_type = excluded.card_type,
			  unbound_at = NULL,
			  updated_at = CURRENT_TIMESTAMP`,
			userID, tier, until, methodID, last4, cardType)
	} else {
		_, err = d.ExecContext(ctx, `
			INSERT INTO subscriptions (user_id, tier, current_period_end, cancelled_at, provider, updated_at)
			VALUES (?, ?, ?, NULL, 'yookassa', CURRENT_TIMESTAMP)
			ON CONFLICT(user_id) DO UPDATE SET
			  tier = excluded.tier,
			  current_period_end = excluded.current_period_end,
			  cancelled_at = NULL,
			  updated_at = CURRENT_TIMESTAMP`,
			userID, tier, until)
	}
	if err != nil {
		return time.Time{}, err
	}
	return until, nil
}

// UnbindCard убирает привязку карты.
//
// ⚠ Требование ЮKassa (менеджер, 01.09.2026): при отвязке мы ОБЯЗАНЫ удалить
// токен у себя, сообщать им не нужно. Поэтому здесь именно NULL, а не флаг
// «отвязано»: помеченная строка — это всё ещё хранимый токен.
//
// Оплаченный срок при этом НЕ трогаем: человек заплатил за месяц и обязан
// доработать его до конца. Отвязка означает «больше не списывать», а не
// «выключить сейчас».
func UnbindCard(ctx context.Context, d *sql.DB, userID int64) error {
	_, err := d.ExecContext(ctx, `
		UPDATE subscriptions
		   SET payment_method_id = NULL,
		       card_last4 = NULL,
		       card_type = NULL,
		       unbound_at = CURRENT_TIMESTAMP,
		       cancelled_at = CURRENT_TIMESTAMP,
		       updated_at = CURRENT_TIMESTAMP
		 WHERE user_id = ?`, userID)
	return err
}

// Payment — строка журнала платежей.
type Payment struct {
	PaymentID   string
	UserID      int64
	Tier        string
	AmountMinor int64
	Status      string
	Method      string
	Recurring   bool
	CreatedAt   time.Time
	SettledAt   sql.NullTime
}

// RecordPayment заводит платёж в журнал. Повтор с тем же payment_id не создаёт
// вторую строку — на этом же держится защита от двойной обработки уведомления.
func RecordPayment(ctx context.Context, d *sql.DB, p Payment) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO payments (payment_id, user_id, tier, amount_minor, currency, status, method, recurring)
		VALUES (?, ?, ?, ?, 'RUB', ?, ?, ?)
		ON CONFLICT(payment_id) DO NOTHING`,
		p.PaymentID, p.UserID, p.Tier, p.AmountMinor, p.Status, p.Method, boolToInt(p.Recurring))
	return err
}

// SettlePayment переводит платёж в окончательный статус.
//
// Возвращает false, если платёж УЖЕ был в этом статусе: ЮKassa не гарантирует
// однократную доставку уведомления, и без этой проверки повторное
// payment.succeeded продлило бы подписку второй раз за одни деньги.
func SettlePayment(ctx context.Context, d billingStore, paymentID, status, method string) (bool, error) {
	res, err := d.ExecContext(ctx, `
		UPDATE payments
		   SET status = ?, method = COALESCE(NULLIF(?,''), method), settled_at = CURRENT_TIMESTAMP
		 WHERE payment_id = ? AND status <> ?`,
		status, method, paymentID, status)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetPayment — платёж по его идентификатору в ЮKassa.
func GetPayment(ctx context.Context, d billingStore, paymentID string) (*Payment, error) {
	row := d.QueryRowContext(ctx, `
		SELECT payment_id, user_id, tier, amount_minor, status, COALESCE(method,''),
		       recurring, created_at, settled_at
		  FROM payments WHERE payment_id = ?`, paymentID)
	var p Payment
	var rec int
	err := row.Scan(&p.PaymentID, &p.UserID, &p.Tier, &p.AmountMinor, &p.Status,
		&p.Method, &rec, &p.CreatedAt, &p.SettledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Recurring = rec != 0
	return &p, nil
}

// ApplyPaymentResult commits settlement and access together. A failed grant
// leaves the payment pending so a webhook retry or account refresh can recover.
// Only final provider states are settled; a stale event cannot undo them.
func ApplyPaymentResult(ctx context.Context, d *sql.DB, paymentID, status, method, amount, currency string, paid bool, period time.Duration) (bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	p, err := GetPayment(ctx, tx, paymentID)
	if err != nil {
		return false, err
	}
	if p == nil || p.Status == "succeeded" || p.Status == "canceled" {
		return true, nil
	}
	if status != "succeeded" && status != "canceled" {
		return false, nil
	}
	if status == "succeeded" {
		want := fmt.Sprintf("%d.%02d", p.AmountMinor/100, p.AmountMinor%100)
		if !paid || currency != "RUB" || amount != want || period <= 0 {
			return false, errors.New("payment amount, currency or paid status does not match the order")
		}
	}
	applied, err := SettlePayment(ctx, tx, paymentID, status, method)
	if err != nil {
		return false, err
	}
	if !applied {
		return true, nil
	}
	if status == "succeeded" {
		// Fixed-period purchase: do not save a method for future charges.
		if _, err := ExtendSubscription(ctx, tx, p.UserID, p.Tier, period, "", "", ""); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// PendingPayments — платежи человека, по которым мы ещё не знаем исхода.
//
// ⚠ Нужны потому, что единственный путь зачисления — уведомление от ЮKassa, а
// оно приходит на адрес, заданный в личном кабинете магазина: по ключу магазина
// этот адрес даже не прочитать (API /v3/webhooks требует OAuth). Если он не
// настроен или уведомление потерялось, человек заплатил бы и остался без
// доступа. Поэтому кабинет досматривает свои незакрытые платежи сам.
//
// since задаёт нижнюю границу; нулевая дата позволяет восстановить пропущенное
// зачисление и после долгого отсутствия. На один запрос берём не больше пяти.
func PendingPayments(ctx context.Context, d *sql.DB, userID int64, since time.Time) ([]Payment, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT payment_id, user_id, tier, amount_minor, status, COALESCE(method,''),
		       recurring, created_at, settled_at
		  FROM payments
		 WHERE user_id = ? AND settled_at IS NULL AND created_at > ?
		 ORDER BY created_at DESC LIMIT 5`, userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		var p Payment
		var rec int
		if err := rows.Scan(&p.PaymentID, &p.UserID, &p.Tier, &p.AmountMinor, &p.Status,
			&p.Method, &rec, &p.CreatedAt, &p.SettledAt); err != nil {
			return nil, err
		}
		p.Recurring = rec != 0
		out = append(out, p)
	}
	return out, rows.Err()
}
