package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// OAuthState — сессия OAuth-входа (redirect-флоу через релей).
type OAuthState struct {
	State        string
	Provider     string
	CodeVerifier string
	DeviceID     string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ConfirmedAt  sql.NullTime
	ConsumedAt   sql.NullTime
	UserID       sql.NullInt64
	LinkUserID   sql.NullInt64
}

var ErrOAuthStateNotFound = errors.New("oauth state not found")

// CreateOAuthState сохраняет новую сессию входа.
func CreateOAuthState(ctx context.Context, d *sql.DB, state, provider, verifier, deviceID string, ttl time.Duration, linkUserID int64) error {
	var link any
	if linkUserID > 0 {
		link = linkUserID
	}
	_, err := d.ExecContext(ctx, `
		INSERT INTO oauth_login_states (state, provider, code_verifier, device_id, expires_at, link_user_id)
		VALUES (?, ?, ?, ?, ?, ?)
	`, state, provider, verifier, deviceID, time.Now().UTC().Add(ttl), link)
	return err
}

func GetOAuthState(ctx context.Context, d *sql.DB, state string) (*OAuthState, error) {
	var s OAuthState
	err := d.QueryRowContext(ctx, `
		SELECT state, provider, COALESCE(code_verifier,''), COALESCE(device_id,''),
		       created_at, expires_at, confirmed_at, consumed_at, user_id, link_user_id
		FROM oauth_login_states WHERE state = ?
	`, state).Scan(&s.State, &s.Provider, &s.CodeVerifier, &s.DeviceID,
		&s.CreatedAt, &s.ExpiresAt, &s.ConfirmedAt, &s.ConsumedAt, &s.UserID, &s.LinkUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOAuthStateNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ConfirmOAuthState привязывает сессию к пользователю (после успешного
// callback). Протухшую или уже использованную сессию отвергает, как и
// ConfirmLoginToken: иначе провайдер рапортует успех, а клиент получит expired.
func ConfirmOAuthState(ctx context.Context, d *sql.DB, state string, userID int64) error {
	s, err := GetOAuthState(ctx, d, state)
	if errors.Is(err, ErrOAuthStateNotFound) {
		return ErrOAuthStateNotFound
	}
	if err != nil {
		return err
	}
	if s.ConsumedAt.Valid || time.Now().UTC().After(s.ExpiresAt) {
		return ErrLoginTokenExpired // переиспользуем смысл: «время вышло»
	}
	res, err := d.ExecContext(ctx,
		`UPDATE oauth_login_states SET confirmed_at = CURRENT_TIMESTAMP, user_id = ? WHERE state = ?`,
		userID, state)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOAuthStateNotFound
	}
	return nil
}

// ConsumeOAuthState помечает сессию использованной (JWT выдан, single-use).
func ConsumeOAuthState(ctx context.Context, d *sql.DB, state string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE oauth_login_states SET consumed_at = CURRENT_TIMESTAMP WHERE state = ?`, state)
	return err
}
