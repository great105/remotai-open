package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

var ErrLoginTokenNotFound = errors.New("login token not found")
var ErrLoginTokenExpired = errors.New("login token expired")

// LoginToken — одноразовый токен Telegram-логина (см. migrations/0002).
type LoginToken struct {
	Nonce       string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	UserID      sql.NullInt64
	ConfirmedAt sql.NullTime
	ConsumedAt  sql.NullTime
}

// GenerateNonce — 32 байта CSPRNG в base64url без паддинга (~43 символа).
// Пространство 2^256 → угадать невозможно; nonce и есть секрет логина.
func GenerateNonce() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func CreateLoginToken(ctx context.Context, d *sql.DB, nonce string, ttl time.Duration) (*LoginToken, error) {
	now := time.Now().UTC()
	exp := now.Add(ttl)
	if _, err := d.ExecContext(ctx, `
		INSERT INTO login_tokens (nonce, created_at, expires_at)
		VALUES (?, ?, ?)
	`, nonce, now, exp); err != nil {
		return nil, err
	}
	return &LoginToken{Nonce: nonce, CreatedAt: now, ExpiresAt: exp}, nil
}

func GetLoginToken(ctx context.Context, d *sql.DB, nonce string) (*LoginToken, error) {
	var t LoginToken
	row := d.QueryRowContext(ctx, `
		SELECT nonce, created_at, expires_at, user_id, confirmed_at, consumed_at
		FROM login_tokens WHERE nonce = ?
	`, nonce)
	err := row.Scan(&t.Nonce, &t.CreatedAt, &t.ExpiresAt, &t.UserID, &t.ConfirmedAt, &t.ConsumedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLoginTokenNotFound
		}
		return nil, err
	}
	return &t, nil
}

// ConfirmLoginToken привязывает nonce к пользователю — вызывается ботом после
// /start login_<nonce>. Идемпотентно; не трогает уже использованный токен.
// Срок проверяем здесь же (Go-сравнение, как в poll-хендлере): иначе бот
// рапортовал бы «✅ подтверждено» по протухшему nonce, а клиент при опросе
// получал бы expired — пользователь видел бы «время вышло» после «успеха».
// Возвращает ErrLoginTokenExpired/ErrLoginTokenNotFound для понятного ответа бота.
func ConfirmLoginToken(ctx context.Context, d *sql.DB, nonce string, userID int64) error {
	t, err := GetLoginToken(ctx, d, nonce)
	if err != nil {
		return err // ErrLoginTokenNotFound либо ошибка БД
	}
	if t.ConsumedAt.Valid {
		return ErrLoginTokenNotFound // уже использован — как несуществующий
	}
	if time.Now().UTC().After(t.ExpiresAt) {
		return ErrLoginTokenExpired
	}
	res, err := d.ExecContext(ctx, `
		UPDATE login_tokens
		SET user_id = ?, confirmed_at = CURRENT_TIMESTAMP
		WHERE nonce = ? AND consumed_at IS NULL
		  AND (confirmed_at IS NULL OR user_id = ?)
	`, userID, nonce, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrLoginTokenNotFound
	}
	return nil
}

// ConsumeLoginToken атомарно забирает подтверждённый токен. Только один
// параллельный poll может выиграть эту операцию и перейти к выдаче JWT.
func ConsumeLoginToken(ctx context.Context, d *sql.DB, nonce string) error {
	res, err := d.ExecContext(ctx, `
		UPDATE login_tokens SET consumed_at = CURRENT_TIMESTAMP
		WHERE nonce = ? AND consumed_at IS NULL AND confirmed_at IS NOT NULL
	`, nonce)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrLoginTokenNotFound
	}
	return nil
}

// CleanupExpiredLoginTokens удаляет старьё (через час после истечения).
func CleanupExpiredLoginTokens(ctx context.Context, d *sql.DB) (int64, error) {
	res, err := d.ExecContext(ctx, `DELETE FROM login_tokens WHERE expires_at < datetime('now','-1 hour')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
