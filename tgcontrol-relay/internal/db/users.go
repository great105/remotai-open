package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"time"
)

type User struct {
	ID         int64
	TelegramID int64
	Username   string
	FirstName  string
	Locale     string
	Tier       string
	Founder    bool
	TrialEnd   sql.NullTime
	// Оплаченная подписка — из таблицы subscriptions, подтягивается тем же
	// запросом. ⚠ До 01.09.2026 её здесь не было, и это была дыра в центре
	// денег: оплата писалась в subscriptions, а решение о доступе принималось
	// по users.tier, которого не писал НИКТО. То есть заплатить было можно, а
	// подействовать это не могло.
	PaidTier  string
	PaidUntil sql.NullTime
	// Почта для чека ЮKassa. Не способ входа: войти по ней нельзя, она нужна
	// только чтобы человеку пришёл чек. Спрашиваем один раз при первой оплате.
	BillingEmail string
	CreatedAt    time.Time
	LastSeenAt   sql.NullTime
}

// EffectiveTier — тариф, чьи лимиты реально действуют для пользователя.
// Порядок важен: founder и активный триал перебивают базовый tier, а бета
// поднимает free до pro (это временный режим бесплатной беты).
//
//	founder         → pro всегда (вечный Pro нынешним, срок не проверяется);
//	триал не истёк  → pro (проба для новых);
//	платный tier    → он сам;
//	бета включена   → pro (пока идёт бесплатная бета);
//	иначе           → free.
func (u *User) EffectiveTier(betaFree bool) string {
	if u.Founder {
		return "pro"
	}
	if u.TrialEnd.Valid && time.Now().Before(u.TrialEnd.Time) {
		return "pro"
	}
	// Оплаченная и НЕ истёкшая подписка. Проверка срока обязательна: без неё
	// один платёж давал бы Про навсегда.
	if u.PaidTier != "" && u.PaidTier != "free" && u.PaidUntil.Valid && time.Now().Before(u.PaidUntil.Time) {
		return u.PaidTier
	}
	// Тариф, выданный вручную (админом) — живёт в самой строке пользователя.
	if u.Tier != "" && u.Tier != "free" {
		return u.Tier
	}
	if betaFree {
		return "pro"
	}
	return "free"
}

// TrialDaysLeft возвращает число целых дней, оставшихся у активного триала,
// и false если триала нет или он истёк.
func (u *User) TrialDaysLeft() (int, bool) {
	if !u.TrialEnd.Valid {
		return 0, false
	}
	left := time.Until(u.TrialEnd.Time)
	if left <= 0 {
		return 0, false
	}
	days := int(left.Hours() / 24)
	if days < 1 {
		days = 1 // осталось меньше суток, но триал ещё активен
	}
	return days, true
}

// TrialDays — длительность пробного Pro для новых пользователей.
const TrialDays = 7

// UpsertUser создаёт или обновляет пользователя по telegram_id.
// Возвращает свежий объект из БД.
func UpsertUser(ctx context.Context, d *sql.DB, tgID int64, username, firstName, locale string) (*User, error) {
	if locale == "" {
		locale = "ru"
	}
	// trial_end при регистрации НЕ ставится: по канону 30 дней отсчитываются от
	// ПЕРВОГО ОБЛАЧНОГО ПОДКЛЮЧЕНИЯ, а не от создания аккаунта — иначе у того,
	// кто зарегистрировался и вернулся через месяц, проба сгорала, ни разу не
	// начавшись. Запускает её StartTrialIfNeeded (cloud_access.go).
	// ON CONFLICT (существующий) trial_end и founder НЕ трогает — они
	// сохраняются, чтобы повторный логин не сбрасывал ни триал, ни founder.
	_, err := d.ExecContext(ctx, `
		INSERT INTO users (telegram_id, username, first_name, locale, last_seen_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(telegram_id) DO UPDATE SET
		    username = excluded.username,
		    first_name = excluded.first_name,
		    locale = excluded.locale,
		    last_seen_at = CURRENT_TIMESTAMP
	`, tgID, username, firstName, locale)
	if err != nil {
		return nil, err
	}
	user, err := GetUserByTelegram(ctx, d, tgID)
	if err != nil {
		return nil, err
	}
	if _, err := EnsurePersonalWorkspace(ctx, d, user.ID); err != nil {
		return nil, err
	}
	return user, nil
}

// CreateAnonUser creates a fresh Telegram-less account. Real Telegram IDs are
// positive, so we assign a unique *negative* synthetic telegram_id — this keeps
// the existing NOT NULL / UNIQUE constraint satisfied without a schema change.
// Used by the no-Telegram ("standalone-cloud") pairing flow.
func CreateAnonUser(ctx context.Context, d *sql.DB) (*User, error) {
	for attempt := 0; attempt < 6; attempt++ {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		v := int64(binary.BigEndian.Uint64(b[:]) >> 1) // positive 63-bit
		if v == 0 {
			v = 1
		}
		synthID := -v // negative → never collides with a real Telegram id
		// trial_end здесь тоже не ставится — см. UpsertUser: отсчёт идёт от
		// первого облачного подключения (StartTrialIfNeeded).
		res, err := d.ExecContext(ctx, `
			INSERT INTO users (telegram_id, first_name, tier, last_seen_at)
			VALUES (?, 'App user', 'free', CURRENT_TIMESTAMP)
		`, synthID)
		if err != nil {
			continue // unique collision (astronomically unlikely) → retry
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		user, err := GetUserByID(ctx, d, id)
		if err != nil {
			return nil, err
		}
		if _, err := EnsurePersonalWorkspace(ctx, d, user.ID); err != nil {
			return nil, err
		}
		return user, nil
	}
	return nil, errors.New("could not allocate anonymous account")
}

func GetUserByTelegram(ctx context.Context, d *sql.DB, tgID int64) (*User, error) {
	var u User
	row := d.QueryRowContext(ctx, `
		SELECT u.id, u.telegram_id, COALESCE(u.username,''), COALESCE(u.first_name,''),
		       COALESCE(u.locale,'ru'), COALESCE(u.tier,'free'),
		       COALESCE(u.founder,0), u.trial_end, u.created_at, u.last_seen_at,
		       COALESCE(s.tier,''), s.current_period_end, COALESCE(u.billing_email,'')
		FROM users u LEFT JOIN subscriptions s ON s.user_id = u.id
		WHERE u.telegram_id = ?
	`, tgID)
	err := row.Scan(&u.ID, &u.TelegramID, &u.Username, &u.FirstName, &u.Locale, &u.Tier, &u.Founder, &u.TrialEnd, &u.CreatedAt, &u.LastSeenAt, &u.PaidTier, &u.PaidUntil, &u.BillingEmail)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func GetUserByID(ctx context.Context, d *sql.DB, id int64) (*User, error) {
	var u User
	row := d.QueryRowContext(ctx, `
		SELECT u.id, u.telegram_id, COALESCE(u.username,''), COALESCE(u.first_name,''),
		       COALESCE(u.locale,'ru'), COALESCE(u.tier,'free'),
		       COALESCE(u.founder,0), u.trial_end, u.created_at, u.last_seen_at,
		       COALESCE(s.tier,''), s.current_period_end, COALESCE(u.billing_email,'')
		FROM users u LEFT JOIN subscriptions s ON s.user_id = u.id
		WHERE u.id = ?
	`, id)
	err := row.Scan(&u.ID, &u.TelegramID, &u.Username, &u.FirstName, &u.Locale, &u.Tier, &u.Founder, &u.TrialEnd, &u.CreatedAt, &u.LastSeenAt, &u.PaidTier, &u.PaidUntil, &u.BillingEmail)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CountDevices возвращает кол-во активных (не revoked) устройств пользователя.
func CountDevices(ctx context.Context, d *sql.DB, userID int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM devices WHERE user_id = ? AND revoked_at IS NULL`,
		userID).Scan(&n)
	return n, err
}

// SetBillingEmail запоминает почту для чека, чтобы не спрашивать её при каждой
// оплате. Возвращает ошибку записи: молча потерянная почта означала бы вопрос
// человеку на каждом платеже.
func SetBillingEmail(ctx context.Context, d *sql.DB, userID int64, email string) error {
	_, err := d.ExecContext(ctx, `UPDATE users SET billing_email = ? WHERE id = ?`, email, userID)
	return err
}
