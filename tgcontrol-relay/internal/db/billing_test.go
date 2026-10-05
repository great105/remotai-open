package db

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Уникальный telegram_id на каждого тестового пользователя: таблица users
// держит его уникальным, и общий литерал ронял бы соседние тесты.
var tgSeq atomic.Int64

func nextTgID() int64 { return 700000 + tgSeq.Add(1) }

// Оплаченный срок продлевается от БОЛЬШЕЙ из дат — конца периода или «сейчас».
// Иначе человек, заплативший за неделю до конца месяца, терял оплаченный
// остаток: деньги отдал, а дни исчезли.
func TestExtendKeepsUnusedDays(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	uid := newUser(t, d, nextTgID()).ID

	first, err := ExtendSubscription(ctx, d, uid, "pro", 30*24*time.Hour, "pm-1", "4444", "MasterCard")
	if err != nil {
		t.Fatalf("первая оплата: %v", err)
	}
	// Платит снова, не дождавшись конца: срок обязан прибавиться к прежнему.
	second, err := ExtendSubscription(ctx, d, uid, "pro", 30*24*time.Hour, "pm-1", "4444", "MasterCard")
	if err != nil {
		t.Fatalf("вторая оплата: %v", err)
	}
	if !second.After(first.Add(29 * 24 * time.Hour)) {
		t.Errorf("остаток сгорел: было до %s, стало до %s", first, second)
	}
}

// Автосписание приходит без данных карты — привязку это стирать не должно.
func TestExtendWithoutMethodKeepsCard(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	uid := newUser(t, d, nextTgID()).ID

	if _, err := ExtendSubscription(ctx, d, uid, "pro", time.Hour, "pm-7", "1234", "Visa"); err != nil {
		t.Fatalf("первая оплата: %v", err)
	}
	if _, err := ExtendSubscription(ctx, d, uid, "pro", time.Hour, "", "", ""); err != nil {
		t.Fatalf("продление: %v", err)
	}
	s, err := GetSubscription(ctx, d, uid)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if s.PaymentMethodID != "pm-7" || s.CardLast4 != "1234" {
		t.Errorf("привязка стёрта продлением: %+v", s)
	}
}

// ⚠ Требование ЮKassa: при отвязке токен удаляется У НАС. Проверяем именно
// отсутствие токена, а не флаг: помеченная строка — это всё ещё хранимый токен.
// При этом оплаченные дни остаются: человек заплатил за месяц.
func TestUnbindRemovesTokenButKeepsPaidDays(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	uid := newUser(t, d, nextTgID()).ID

	until, err := ExtendSubscription(ctx, d, uid, "pro", 30*24*time.Hour, "pm-9", "4444", "MIR")
	if err != nil {
		t.Fatalf("оплата: %v", err)
	}
	if err := UnbindCard(ctx, d, uid); err != nil {
		t.Fatalf("отвязка: %v", err)
	}
	s, err := GetSubscription(ctx, d, uid)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if s.PaymentMethodID != "" || s.CardLast4 != "" {
		t.Errorf("токен карты остался после отвязки: %+v", s)
	}
	if s.HasCard() {
		t.Error("HasCard обязан стать ложью")
	}
	if !s.PaidUntil.Valid || !s.PaidUntil.Time.Equal(until.Truncate(time.Second)) && s.PaidUntil.Time.Before(time.Now()) {
		t.Errorf("оплаченные дни сгорели при отвязке: %+v", s.PaidUntil)
	}
	if !s.UnboundAt.Valid {
		t.Error("время отвязки не записано — нечем будет ответить на «я отвязал, а списали»")
	}
}

// ⚠ Главная защита: ЮKassa не гарантирует однократную доставку уведомления.
// Повторное payment.succeeded не должно продлевать подписку второй раз.
func TestSettleIsIdempotent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	uid := newUser(t, d, nextTgID()).ID

	if err := RecordPayment(ctx, d, Payment{
		PaymentID: "pay-1", UserID: uid, Tier: "pro", AmountMinor: 59900, Status: "pending",
	}); err != nil {
		t.Fatalf("журнал: %v", err)
	}
	first, err := SettlePayment(ctx, d, "pay-1", "succeeded", "sbp")
	if err != nil {
		t.Fatalf("первое уведомление: %v", err)
	}
	if !first {
		t.Fatal("первое уведомление обязано быть принято")
	}
	again, err := SettlePayment(ctx, d, "pay-1", "succeeded", "sbp")
	if err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if again {
		t.Error("повторное уведомление принято второй раз — подписка продлится дважды за одни деньги")
	}
}

// Один и тот же платёж не заводится в журнал дважды.
func TestRecordPaymentDedupes(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	uid := newUser(t, d, nextTgID()).ID
	p := Payment{PaymentID: "pay-2", UserID: uid, Tier: "pro", AmountMinor: 59900, Status: "pending"}
	if err := RecordPayment(ctx, d, p); err != nil {
		t.Fatalf("первый: %v", err)
	}
	if err := RecordPayment(ctx, d, p); err != nil {
		t.Fatalf("повтор не должен быть ошибкой: %v", err)
	}
	got, err := GetPayment(ctx, d, "pay-2")
	if err != nil || got == nil {
		t.Fatalf("платёж не найден: %v", err)
	}
	if got.AmountMinor != 59900 {
		t.Errorf("сумма испорчена повтором: %+v", got)
	}
}

// Отсутствие подписки — обычное состояние, а не ошибка: у большинства людей
// её нет и не должно быть (локальное управление бесплатно навсегда).
func TestNoSubscriptionIsNotAnError(t *testing.T) {
	d := openTestDB(t)
	s, err := GetSubscription(context.Background(), d, 4242)
	if err != nil {
		t.Fatalf("ошибка на пустоте: %v", err)
	}
	if s != nil {
		t.Errorf("ожидали пусто, получили %+v", s)
	}
	if s.HasCard() || s.Active(time.Now()) {
		t.Error("пустая подписка не может быть активной")
	}
}

// ⚠ ГЛАВНАЯ ПРОВЕРКА ВСЕЙ КАССЫ: оплата обязана открывать доступ.
//
// До 01.09.2026 здесь была дыра в самом центре денег: платёж записывался в
// subscriptions, а решение о доступе принималось по users.tier, который не
// писал НИКТО в рабочем коде. То есть заплатить было можно, а подействовать
// это не могло — человек отдал бы деньги и остался бы без облака.
func TestPaidSubscriptionOpensCloud(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	u := newUser(t, d, nextTgID())

	// Без оплаты и без беты облако закрыто — это норма канона.
	dec, err := CloudAccess(ctx, d, u.ID, false)
	if err != nil {
		t.Fatalf("доступ: %v", err)
	}
	if dec.Allowed {
		t.Fatal("без оплаты и без пробы облако должно быть закрыто")
	}

	if _, err := ExtendSubscription(ctx, d, u.ID, "pro", 30*24*time.Hour, "pm-1", "4444", "MIR"); err != nil {
		t.Fatalf("оплата: %v", err)
	}

	dec, err = CloudAccess(ctx, d, u.ID, false)
	if err != nil {
		t.Fatalf("доступ после оплаты: %v", err)
	}
	if !dec.Allowed || dec.Tier != "pro" {
		t.Errorf("оплата не открыла облако: %+v", dec)
	}
}

// И обратное: истёкшая оплата доступ не даёт. Один платёж — не Про навсегда.
func TestExpiredSubscriptionClosesCloud(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	u := newUser(t, d, nextTgID())

	// Оплата, срок которой уже прошёл.
	if _, err := d.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, tier, current_period_end, provider)
		VALUES (?, 'pro', ?, 'yookassa')`, u.ID, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	dec, err := CloudAccess(ctx, d, u.ID, false)
	if err != nil {
		t.Fatalf("доступ: %v", err)
	}
	if dec.Allowed {
		t.Error("истёкшая подписка не должна открывать облако")
	}
}

// Основатель не платит никогда — оплата ему просто не нужна.
func TestFounderKeepsProWithoutPaying(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	u := newUser(t, d, nextTgID())
	if _, err := d.ExecContext(ctx, `UPDATE users SET founder = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	dec, err := CloudAccess(ctx, d, u.ID, false)
	if err != nil {
		t.Fatalf("доступ: %v", err)
	}
	if !dec.Allowed || dec.Tier != "pro" {
		t.Errorf("основатель обязан иметь Про без оплаты: %+v", dec)
	}
}
