package db

import (
	"context"
	"testing"
	"time"
)

func TestPaymentGrantFailureCanBeRetried(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	uid := newUser(t, d, nextTgID()).ID
	if err := RecordPayment(ctx, d, Payment{PaymentID: "atomic", UserID: uid, Tier: "pro", AmountMinor: 90000, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TRIGGER fail_grant BEFORE INSERT ON subscriptions BEGIN SELECT RAISE(ABORT, 'simulated storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	apply := func() (bool, error) {
		return ApplyPaymentResult(ctx, d, "atomic", "succeeded", "sbp", "900.00", "RUB", true, 30*24*time.Hour)
	}
	if _, err := apply(); err == nil {
		t.Fatal("expected grant failure")
	}
	p, _ := GetPayment(ctx, d, "atomic")
	if p.Status != "pending" || p.SettledAt.Valid {
		t.Fatal("failed grant consumed payment")
	}
	if _, err := d.Exec(`DROP TRIGGER fail_grant`); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := apply(); err != nil || duplicate {
		t.Fatalf("retry: duplicate=%v err=%v", duplicate, err)
	}
	before, err := GetSubscription(ctx, d, uid)
	if err != nil || before == nil || !before.Active(time.Now()) {
		t.Fatalf("missing access: %v", err)
	}
	if duplicate, err := apply(); err != nil || !duplicate {
		t.Fatalf("duplicate retry: %v %v", duplicate, err)
	}
	after, _ := GetSubscription(ctx, d, uid)
	if !before.PaidUntil.Time.Equal(after.PaidUntil.Time) {
		t.Fatal("duplicate added paid days")
	}
	if _, err := ApplyPaymentResult(ctx, d, "atomic", "canceled", "sbp", "900.00", "RUB", false, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	p, _ = GetPayment(ctx, d, "atomic")
	if p.Status != "succeeded" {
		t.Fatal("stale cancellation overwrote success")
	}
}

func TestPaymentRejectsWrongAmountAndIgnoresIntermediateState(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	uid := newUser(t, d, nextTgID()).ID
	if err := RecordPayment(ctx, d, Payment{PaymentID: "verify", UserID: uid, Tier: "pro", AmountMinor: 90000, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		amount, currency string
		paid             bool
	}{{"10.00", "RUB", true}, {"900.00", "USD", true}, {"900.00", "RUB", false}} {
		if _, err := ApplyPaymentResult(ctx, d, "verify", "succeeded", "bank_card", tc.amount, tc.currency, tc.paid, 30*24*time.Hour); err == nil {
			t.Fatalf("accepted mismatched payment: %+v", tc)
		}
	}
	if _, err := ApplyPaymentResult(ctx, d, "verify", "waiting_for_capture", "bank_card", "900.00", "RUB", false, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	p, _ := GetPayment(ctx, d, "verify")
	if p.Status != "pending" || p.SettledAt.Valid {
		t.Fatal("intermediate state prevents reconciliation")
	}
	if sub, _ := GetSubscription(ctx, d, uid); sub != nil {
		t.Fatal("unpaid order granted access")
	}
}
