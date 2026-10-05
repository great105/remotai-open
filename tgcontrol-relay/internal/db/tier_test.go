package db

import (
	"database/sql"
	"testing"
	"time"
)

func nullTime(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: true} }

func TestEffectiveTier(t *testing.T) {
	future := nullTime(time.Now().Add(48 * time.Hour))
	past := nullTime(time.Now().Add(-48 * time.Hour))

	cases := []struct {
		name string
		u    User
		beta bool
		want string
	}{
		{"free без беты", User{Tier: "free"}, false, "free"},
		{"free в бете", User{Tier: "free"}, true, "pro"},
		{"founder без беты", User{Tier: "free", Founder: true}, false, "pro"},
		{"founder перебивает всё", User{Tier: "free", Founder: true}, true, "pro"},
		{"активный триал без беты", User{Tier: "free", TrialEnd: future}, false, "pro"},
		{"истёкший триал без беты", User{Tier: "free", TrialEnd: past}, false, "free"},
		{"истёкший триал в бете", User{Tier: "free", TrialEnd: past}, true, "pro"},
		{"платный pro без беты", User{Tier: "pro"}, false, "pro"},
		{"платный team без беты", User{Tier: "team"}, false, "team"},
	}
	for _, c := range cases {
		if got := c.u.EffectiveTier(c.beta); got != c.want {
			t.Errorf("%s: EffectiveTier(beta=%v) = %q, want %q", c.name, c.beta, got, c.want)
		}
	}
}

func TestTrialDaysLeft(t *testing.T) {
	// нет триала
	if _, ok := (&User{}).TrialDaysLeft(); ok {
		t.Error("без trial_end должно быть ok=false")
	}
	// истёкший
	if _, ok := (&User{TrialEnd: nullTime(time.Now().Add(-time.Hour))}).TrialDaysLeft(); ok {
		t.Error("истёкший триал должен давать ok=false")
	}
	// активный: 3 дня с хвостом
	d, ok := (&User{TrialEnd: nullTime(time.Now().Add(3*24*time.Hour + time.Hour))}).TrialDaysLeft()
	if !ok || d != 3 {
		t.Errorf("активный триал: got days=%d ok=%v, want 3 true", d, ok)
	}
	// меньше суток, но активен → 1
	d, ok = (&User{TrialEnd: nullTime(time.Now().Add(2 * time.Hour))}).TrialDaysLeft()
	if !ok || d != 1 {
		t.Errorf("под-суточный триал: got days=%d ok=%v, want 1 true", d, ok)
	}
}

// TestBackfillFounders: зарегистрированные до отсечки становятся founder,
// более поздние — нет.
func TestBackfillFounders(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.Exec(`INSERT INTO users (telegram_id, created_at) VALUES (1001, '2026-06-01 10:00:00')`); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO users (telegram_id, created_at) VALUES (1002, '2026-09-01 10:00:00')`); err != nil {
		t.Fatalf("insert new: %v", err)
	}
	if err := backfillFounders(d); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	old, err := GetUserByTelegram(t.Context(), d, 1001)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := GetUserByTelegram(t.Context(), d, 1002)
	if err != nil {
		t.Fatal(err)
	}
	if !old.Founder {
		t.Error("пользователь до отсечки должен быть founder")
	}
	if fresh.Founder {
		t.Error("пользователь после отсечки не должен быть founder")
	}
}

// TestTrialGrantedToNewUser проверяет жизненный цикл пробы: регистрация её НЕ
// запускает (канон: отсчёт от первого облачного подключения), запускает первое
// облачное подключение, а повторный логин уже начатую пробу не сбрасывает.
func TestTrialGrantedToNewUser(t *testing.T) {
	d := openTestDB(t)
	u, err := UpsertUser(t.Context(), d, 424242, "newbie", "New", "ru")
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	if u.TrialEnd.Valid {
		t.Fatal("регистрация не должна запускать пробу: 30 дней идут от первого облачного подключения")
	}

	// Первое облачное подключение — вот теперь проба стартует.
	if err := StartTrialIfNeeded(t.Context(), d, u.ID, 30); err != nil {
		t.Fatalf("StartTrialIfNeeded: %v", err)
	}
	u, err = GetUserByID(t.Context(), d, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if !u.TrialEnd.Valid {
		t.Fatal("после первого облачного подключения проба обязана идти")
	}
	days, ok := u.TrialDaysLeft()
	if !ok || days < 29 || days > 30 {
		t.Errorf("свежая проба: days=%d ok=%v, ожидалось ~30", days, ok)
	}
	firstEnd := u.TrialEnd.Time

	// Повторный логин не должен трогать триал.
	time.Sleep(5 * time.Millisecond)
	u2, err := UpsertUser(t.Context(), d, 424242, "newbie", "New", "ru")
	if err != nil {
		t.Fatalf("UpsertUser повтор: %v", err)
	}
	if !u2.TrialEnd.Valid || !u2.TrialEnd.Time.Equal(firstEnd) {
		t.Errorf("повторный логин сбросил trial_end: было %v, стало %v", firstEnd, u2.TrialEnd)
	}
}
