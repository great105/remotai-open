package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type UserSession struct {
	SessionID  string
	UserID     int64
	CurrentJTI string
	ClientKind string
	ClientName string
	UserAgent  string
	IPAddress  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  sql.NullTime
	RevokedAt  sql.NullTime
}

// RecordUserSession заводит сессию входа или обновляет существующую. Второе
// значение — «запись создана впервые»: по нему вызывающий пишет в лог, ОТКУДА
// взялся новый вход. Без этого всплеск одинаковых строк в списке входов
// («много веб-сессий каких-то») нечем объяснить: логины видно в логе, а
// молчаливое появление сессии на обычном запросе — нет.
func RecordUserSession(
	ctx context.Context,
	d *sql.DB,
	sessionID string,
	userID int64,
	currentJTI, clientKind, clientName, userAgent, ipAddress string,
	expiresAt time.Time,
) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, errors.New("session id required")
	}
	if clientKind == "" {
		clientKind = "web"
	}
	var expiry any
	if !expiresAt.IsZero() {
		expiry = expiresAt.UTC()
	}
	var existed int
	_ = d.QueryRowContext(ctx,
		`SELECT 1 FROM user_sessions WHERE session_id = ? AND user_id = ?`,
		sessionID, userID,
	).Scan(&existed)

	_, err := d.ExecContext(ctx, `
		INSERT INTO user_sessions (
			session_id, user_id, current_jti, client_kind, client_name,
			user_agent, ip_address, expires_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			current_jti = excluded.current_jti,
			client_kind = CASE WHEN excluded.client_kind != '' THEN excluded.client_kind ELSE user_sessions.client_kind END,
			client_name = CASE WHEN excluded.client_name != '' THEN excluded.client_name ELSE user_sessions.client_name END,
			user_agent = CASE WHEN excluded.user_agent != '' THEN excluded.user_agent ELSE user_sessions.user_agent END,
			ip_address = CASE WHEN excluded.ip_address != '' THEN excluded.ip_address ELSE user_sessions.ip_address END,
			last_seen_at = CURRENT_TIMESTAMP,
			expires_at = excluded.expires_at
		WHERE user_sessions.user_id = excluded.user_id AND user_sessions.revoked_at IS NULL
	`, sessionID, userID, currentJTI, clientKind, clientName, userAgent, ipAddress, expiry)
	return existed == 0, err
}

func UserSessionRevoked(ctx context.Context, d *sql.DB, sessionID string, userID int64) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	var revoked sql.NullTime
	err := d.QueryRowContext(ctx, `
		SELECT revoked_at FROM user_sessions WHERE session_id = ? AND user_id = ?
	`, sessionID, userID).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return revoked.Valid, nil
}

func ListUserSessions(ctx context.Context, d *sql.DB, userID int64) ([]UserSession, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT session_id, user_id, COALESCE(current_jti,''), client_kind,
		       client_name, user_agent, ip_address, created_at, last_seen_at,
		       expires_at, revoked_at
		FROM user_sessions
		WHERE user_id = ? AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP)
		ORDER BY last_seen_at DESC, created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserSession
	for rows.Next() {
		var session UserSession
		if err := rows.Scan(
			&session.SessionID, &session.UserID, &session.CurrentJTI,
			&session.ClientKind, &session.ClientName, &session.UserAgent,
			&session.IPAddress, &session.CreatedAt, &session.LastSeenAt,
			&session.ExpiresAt, &session.RevokedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

func RevokeUserSession(ctx context.Context, d *sql.DB, userID int64, sessionID string) error {
	res, err := d.ExecContext(ctx, `
		UPDATE user_sessions SET revoked_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND session_id = ? AND revoked_at IS NULL
	`, userID, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("session not found")
	}
	return nil
}

func RevokeOtherUserSessions(ctx context.Context, d *sql.DB, userID int64, currentSessionID string) (int64, error) {
	res, err := d.ExecContext(ctx, `
		UPDATE user_sessions SET revoked_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND session_id != ? AND revoked_at IS NULL
	`, userID, currentSessionID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
