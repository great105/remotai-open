package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Счётчик неудачных попыток: растёт от IncAttempts, а после MaxPairCodeAttempts
// код перестаёт приниматься (ErrCodeTooManyAttempts), даже если он ещё не
// просрочен и не использован. Анти-брутфорс pairing-кодов (аудит 2026-06-10).
func TestIncAttemptsLocksConsume(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if _, err := InsertPairingCode(ctx, d, "ABCD-EFGH", "dev-attempts", "host", "linux", "test", time.Hour); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for i := 1; i <= MaxPairCodeAttempts; i++ {
		if err := IncAttempts(ctx, d, "ABCD-EFGH"); err != nil {
			t.Fatalf("inc %d: %v", i, err)
		}
		rec, err := GetPairingCode(ctx, d, "ABCD-EFGH")
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if rec.Attempts != i {
			t.Fatalf("попыток: %d, ждали %d", rec.Attempts, i)
		}
	}
	if _, err := ConsumePairingCode(ctx, d, "ABCD-EFGH", 1, "jwt"); !errors.Is(err, ErrCodeTooManyAttempts) {
		t.Fatalf("consume после %d попыток: err=%v, ждали ErrCodeTooManyAttempts", MaxPairCodeAttempts, err)
	}
}

// До порога счётчик не мешает успешному consume — легальный флоу
// (мульти-скан кода в пределах TTL) не ломается.
func TestConsumeWorksBelowAttemptsLimit(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if _, err := InsertPairingCode(ctx, d, "WXYZ-KMNP", "dev-ok", "host", "linux", "test", time.Hour); err != nil {
		t.Fatalf("insert: %v", err)
	}
	u, err := CreateAnonUser(ctx, d) // consumed_by_uid — FK на users
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	for i := 0; i < MaxPairCodeAttempts-1; i++ {
		if err := IncAttempts(ctx, d, "WXYZ-KMNP"); err != nil {
			t.Fatalf("inc: %v", err)
		}
	}
	rec, err := ConsumePairingCode(ctx, d, "WXYZ-KMNP", u.ID, "jwt-ok")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !rec.ConsumedAt.Valid || rec.IssuedJWT != "jwt-ok" {
		t.Fatalf("consume не записал результат: %+v", rec)
	}
}
