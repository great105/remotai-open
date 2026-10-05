package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Identity — прикреплённый способ входа к аккаунту.
type Identity struct {
	ID          int64
	UserID      int64
	Provider    string
	ProviderUID string
	Display     string
	CreatedAt   time.Time
	LastUsedAt  sql.NullTime
}

var ErrIdentityNotFound = errors.New("identity not found")

// NormalizeUID приводит provider_uid к каноническому виду: для email-подобных
// провайдеров это lower+trim, для числовых id менять нечего.
func NormalizeUID(uid string) string { return strings.ToLower(strings.TrimSpace(uid)) }

// GetUserByIdentity ищет аккаунт по (provider, provider_uid).
func GetUserByIdentity(ctx context.Context, d *sql.DB, provider, uid string) (*User, error) {
	var userID int64
	err := d.QueryRowContext(ctx,
		`SELECT user_id FROM user_identities WHERE provider = ? AND provider_uid = ?`,
		provider, NormalizeUID(uid)).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrIdentityNotFound
	}
	if err != nil {
		return nil, err
	}
	// Фиксируем факт входа этим способом (для UI «способы входа» и аудита).
	_, _ = d.ExecContext(ctx,
		`UPDATE user_identities SET last_used_at = CURRENT_TIMESTAMP
		 WHERE provider = ? AND provider_uid = ?`, provider, NormalizeUID(uid))
	return GetUserByID(ctx, d, userID)
}

// LinkIdentity прикрепляет способ входа к аккаунту. Повторный вызов с той же
// парой (provider, uid) безопасен: обновляет display и возвращает владельца.
// Если identity принадлежит ДРУГОМУ аккаунту — ошибка, авто-слияние двух
// постоянных аккаунтов не делаем.
func LinkIdentity(ctx context.Context, d *sql.DB, userID int64, provider, uid, display string) error {
	uid = NormalizeUID(uid)
	_, err := d.ExecContext(ctx, `
		INSERT INTO user_identities (user_id, provider, provider_uid, display, last_used_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(provider, provider_uid) DO UPDATE SET
		    display = excluded.display,
		    last_used_at = CURRENT_TIMESTAMP
	`, userID, provider, uid, display)
	if err != nil {
		return err
	}
	// Проверяем, что identity не указывает на чужой аккаунт.
	var owner int64
	if err := d.QueryRowContext(ctx,
		`SELECT user_id FROM user_identities WHERE provider = ? AND provider_uid = ?`,
		provider, uid).Scan(&owner); err != nil {
		return err
	}
	if owner != userID {
		return fmt.Errorf("identity %s:%s belongs to another account", provider, uid)
	}
	return nil
}

// ListIdentities возвращает все способы входа аккаунта (для будущего UI).
func ListIdentities(ctx context.Context, d *sql.DB, userID int64) ([]Identity, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT id, user_id, provider, provider_uid, COALESCE(display,''), created_at, last_used_at
		FROM user_identities WHERE user_id = ? ORDER BY created_at
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var it Identity
		if err := rows.Scan(&it.ID, &it.UserID, &it.Provider, &it.ProviderUID, &it.Display, &it.CreatedAt, &it.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ── Email login codes ────────────────────────────────────────────────

var ErrEmailCodeNotFound = errors.New("email login code not found")

type EmailLoginCode struct {
	Token      string
	Email      string
	CodeHash   string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ConsumedAt sql.NullTime
	Attempts   int
}

func HashEmailCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// CreateEmailLoginCode сохраняет сессию входа по email. Старые неиспользованные
// коды этого email сжигаются, чтобы «последний запрошенный» был единственным
// действующим (защита от путаницы при повторной отправке).
func CreateEmailLoginCode(ctx context.Context, d *sql.DB, token, email, code string, ttl time.Duration) error {
	email = NormalizeUID(email)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE email_login_codes SET consumed_at = CURRENT_TIMESTAMP
		 WHERE email = ? AND consumed_at IS NULL`, email); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO email_login_codes (token, email, code_hash, expires_at)
		VALUES (?, ?, ?, ?)
	`, token, email, HashEmailCode(code), time.Now().UTC().Add(ttl)); err != nil {
		return err
	}
	return tx.Commit()
}

// VerifyEmailLoginCode проверяет код: сессия жива, не сожжена, хеш совпал.
// При успехе сессия сжигается (single-use). При неверном коде растёт attempts;
// после maxAttempts сессия тоже сжигается (анти-брутфорс: 6 цифр = 1e6).
func VerifyEmailLoginCode(ctx context.Context, d *sql.DB, token, code string, maxAttempts int) (string, error) {
	var c EmailLoginCode
	err := d.QueryRowContext(ctx, `
		SELECT token, email, code_hash, created_at, expires_at, consumed_at, attempts
		FROM email_login_codes WHERE token = ?
	`, token).Scan(&c.Token, &c.Email, &c.CodeHash, &c.CreatedAt, &c.ExpiresAt, &c.ConsumedAt, &c.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrEmailCodeNotFound
	}
	if err != nil {
		return "", err
	}
	if c.ConsumedAt.Valid || time.Now().UTC().After(c.ExpiresAt) {
		return "", ErrEmailCodeNotFound
	}
	if HashEmailCode(code) != c.CodeHash {
		attempts := c.Attempts + 1
		if attempts >= maxAttempts {
			_, _ = d.ExecContext(ctx,
				`UPDATE email_login_codes SET consumed_at = CURRENT_TIMESTAMP, attempts = ? WHERE token = ?`,
				attempts, token)
		} else {
			_, _ = d.ExecContext(ctx,
				`UPDATE email_login_codes SET attempts = ? WHERE token = ?`, attempts, token)
		}
		return "", errors.New("invalid code")
	}
	if _, err := d.ExecContext(ctx,
		`UPDATE email_login_codes SET consumed_at = CURRENT_TIMESTAMP WHERE token = ?`, token); err != nil {
		return "", err
	}
	return c.Email, nil
}

// CountRecentEmailCodes считает коды, запрошенные email-ом за окно (rate-limit
// отправки писем поверх IP-лимита: один shared IP не должен спамить чужую почту).
func CountRecentEmailCodes(ctx context.Context, d *sql.DB, email string, window time.Duration) (int, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM email_login_codes WHERE email = ? AND created_at > ?`,
		NormalizeUID(email), time.Now().UTC().Add(-window)).Scan(&n)
	return n, err
}

// ── Misc ─────────────────────────────────────────────────────────────

// RandDigits генерирует криптостойкий числовой код заданной длины (6 цифр для email).
func RandDigits(n int) (string, error) {
	var b [8]byte
	out := make([]byte, 0, n)
	for len(out) < n {
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		for _, x := range b {
			if len(out) < n {
				out = append(out, '0'+x%10)
			}
		}
	}
	return string(out), nil
}
