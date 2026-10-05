package db

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func newUser(t *testing.T, d *sql.DB, tgID int64) *User {
	t.Helper()
	u, err := UpsertUser(context.Background(), d, tgID, "u", "U", "ru")
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	return u
}

// Канон: 30 дней отсчитываются от ПЕРВОГО ОБЛАЧНОГО ПОДКЛЮЧЕНИЯ.
// Регистрация пробу не запускает — иначе она сгорала у тех, кто завёл аккаунт
// и вернулся через месяц.
func TestTrialDoesNotStartOnRegistration(t *testing.T) {
	d := openTestDB(t)
	u := newUser(t, d, 1001)

	if u.TrialEnd.Valid {
		t.Fatal("регистрация не должна запускать пробу — отсчёт идёт от первого облачного подключения")
	}

	dec, err := CloudAccess(context.Background(), d, u.ID, false)
	if err != nil {
		t.Fatalf("CloudAccess: %v", err)
	}
	if dec.Allowed {
		t.Error("без пробы и подписки облако должно быть закрыто")
	}
	if dec.Code != "subscription_required" {
		t.Errorf("code = %q, want subscription_required", dec.Code)
	}
}

func TestStartTrialIfNeeded(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	u := newUser(t, d, 1002)

	if err := StartTrialIfNeeded(ctx, d, u.ID, 30); err != nil {
		t.Fatalf("StartTrialIfNeeded: %v", err)
	}
	after, _ := GetUserByID(ctx, d, u.ID)
	if !after.TrialEnd.Valid {
		t.Fatal("проба не стартовала")
	}
	left := time.Until(after.TrialEnd.Time)
	if left < 29*24*time.Hour || left > 31*24*time.Hour {
		t.Errorf("проба на %v, ожидалось около 30 дней", left)
	}

	// Повторный вызов (второе подключение) не продлевает пробу — иначе она
	// была бы вечной: каждый заход сдвигал бы срок.
	first := after.TrialEnd.Time
	if err := StartTrialIfNeeded(ctx, d, u.ID, 30); err != nil {
		t.Fatalf("second StartTrialIfNeeded: %v", err)
	}
	again, _ := GetUserByID(ctx, d, u.ID)
	if !again.TrialEnd.Time.Equal(first) {
		t.Errorf("повторное подключение сдвинуло пробу: было %v, стало %v", first, again.TrialEnd.Time)
	}

	// Пока проба идёт — облако открыто.
	dec, err := CloudAccess(ctx, d, u.ID, false)
	if err != nil {
		t.Fatalf("CloudAccess: %v", err)
	}
	if !dec.Allowed {
		t.Error("во время пробы облако должно быть открыто")
	}
}

// Founder — вечный Про (71 аккаунт до 05.08.2026). С них не должно списываться
// ничего и никогда, и проба им не нужна.
func TestFounderKeepsCloudWithoutTrial(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	u := newUser(t, d, 1003)

	if _, err := d.Exec(`UPDATE users SET founder = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("set founder: %v", err)
	}

	dec, err := CloudAccess(ctx, d, u.ID, false)
	if err != nil {
		t.Fatalf("CloudAccess: %v", err)
	}
	if !dec.Allowed || dec.Tier != "pro" {
		t.Errorf("founder обязан иметь облако: allowed=%v tier=%s", dec.Allowed, dec.Tier)
	}

	// Пробу founder'у не выдаём — она ему не нужна и только путала бы отчёты.
	if err := StartTrialIfNeeded(ctx, d, u.ID, 30); err != nil {
		t.Fatalf("StartTrialIfNeeded: %v", err)
	}
	after, _ := GetUserByID(ctx, d, u.ID)
	if after.TrialEnd.Valid {
		t.Error("founder'у проба не нужна")
	}
}

// Истёкшая проба закрывает облако — это и есть 31-й день из канона.
func TestExpiredTrialClosesCloud(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	u := newUser(t, d, 1004)

	if _, err := d.Exec(`UPDATE users SET trial_end = datetime('now','-1 day') WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("expire trial: %v", err)
	}

	dec, err := CloudAccess(ctx, d, u.ID, false)
	if err != nil {
		t.Fatalf("CloudAccess: %v", err)
	}
	if dec.Allowed {
		t.Error("после пробы облако должно закрываться")
	}

	// Повторно проба не выдаётся: trial_end уже не NULL.
	if err := StartTrialIfNeeded(ctx, d, u.ID, 30); err != nil {
		t.Fatalf("StartTrialIfNeeded: %v", err)
	}
	dec2, _ := CloudAccess(ctx, d, u.ID, false)
	if dec2.Allowed {
		t.Error("вторая проба тому же аккаунту выдаваться не должна")
	}
}

// A paid entitlement grants access; the legacy beta switch does not.
func TestPaidOpensCloudAndLegacyBetaCannotBypass(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	paid := newUser(t, d, 1005)
	if _, err := d.Exec(`UPDATE users SET tier = 'pro' WHERE id = ?`, paid.ID); err != nil {
		t.Fatalf("set tier: %v", err)
	}
	if dec, _ := CloudAccess(ctx, d, paid.ID, false); !dec.Allowed {
		t.Error("платный тариф обязан открывать облако")
	}

	beta := newUser(t, d, 1006)
	if dec, _ := CloudAccess(ctx, d, beta.ID, true); dec.Allowed {
		t.Error("legacy beta cannot grant paid cloud access")
	}
	if dec, _ := CloudAccess(ctx, d, beta.ID, false); dec.Allowed {
		t.Error("с выключенной бетой тот же аккаунт должен упереться в оплату")
	}
}
