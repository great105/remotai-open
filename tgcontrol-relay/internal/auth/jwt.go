// Package auth — выдача и верификация JWT для устройств и клиентов.
// Используется HMAC-SHA256 (HS256) с секретом из конфига — это удобнее
// чем RSA для одно-инстансного сервера и переиспользуется на клиенте.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// DeviceClaims — JWT, который выдаётся десктоп-агенту после pairing.
type DeviceClaims struct {
	DeviceID string `json:"device_id"`
	UserID   int64  `json:"user_id"`
	jwt.RegisteredClaims
}

// UserClaims — JWT, который выдаётся Mini App / APK при логине через Telegram OAuth
// (для прямого APK-логина без Mini App). Mini App обычно использует initData напрямую.
type UserClaims struct {
	UserID     int64  `json:"user_id"`
	TelegramID int64  `json:"telegram_id"`
	Tier       string `json:"tier"`
	SessionID  string `json:"sid,omitempty"`
	jwt.RegisteredClaims
}

type Issuer struct {
	secret []byte
	ttl    time.Duration
}

func NewIssuer(secret string, ttl time.Duration) *Issuer {
	return &Issuer{secret: []byte(secret), ttl: ttl}
}

func (i *Issuer) IssueDevice(deviceID string, userID int64) (string, time.Time, error) {
	exp := time.Now().Add(i.ttl)
	jti := uuid.NewString()
	claims := DeviceClaims{
		DeviceID: deviceID,
		UserID:   userID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("device:%s", deviceID),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        jti,
			Issuer:    "tgcontrol-relay",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(i.secret)
	return s, exp, err
}

func (i *Issuer) IssueUser(userID, tgID int64, tier string) (string, time.Time, error) {
	return i.IssueUserForSession(userID, tgID, tier, "")
}

// IssueUserForSession keeps sid stable while a sliding refresh rotates jti.
// Revoking a session therefore invalidates every token ever issued for that
// phone/browser session, including an older token copied before refresh.
func (i *Issuer) IssueUserForSession(userID, tgID int64, tier, sessionID string) (string, time.Time, error) {
	exp := time.Now().Add(i.ttl)
	jti := uuid.NewString()
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	claims := UserClaims{
		UserID:     userID,
		TelegramID: tgID,
		Tier:       tier,
		SessionID:  sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("user:%d", userID),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        jti,
			Issuer:    "tgcontrol-relay",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(i.secret)
	return s, exp, err
}

func (i *Issuer) ParseDevice(tokenStr string) (*DeviceClaims, error) {
	claims := &DeviceClaims{}
	tok, err := jwt.ParseWithClaims(tokenStr, claims, i.keyFunc, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("invalid token")
	}
	if claims.DeviceID == "" {
		return nil, errors.New("not a device token")
	}
	if claims.Subject != "" && !startsWithStr(claims.Subject, "device:") {
		return nil, errors.New("not a device token")
	}
	return claims, nil
}

func (i *Issuer) ParseUser(tokenStr string) (*UserClaims, error) {
	claims := &UserClaims{}
	tok, err := jwt.ParseWithClaims(tokenStr, claims, i.keyFunc, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("invalid token")
	}
	if claims.UserID == 0 {
		return nil, errors.New("not a user token")
	}
	// device-токены имеют ту же структуру UserID, но subject = "device:..."
	if claims.Subject != "" && !startsWithStr(claims.Subject, "user:") {
		return nil, errors.New("not a user token")
	}
	return claims, nil
}

func startsWithStr(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func (i *Issuer) keyFunc(t *jwt.Token) (any, error) {
	if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
	}
	return i.secret, nil
}

// IsRevoked проверяет таблицу jwt_revocations.
func IsRevoked(ctx context.Context, d *sql.DB, jti string) (bool, error) {
	var c int
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM jwt_revocations WHERE jti = ?`, jti).Scan(&c)
	if err != nil {
		return false, err
	}
	return c > 0, nil
}

// RevokeJTI добавляет JWT в blocklist.
func RevokeJTI(ctx context.Context, d *sql.DB, jti string, userID int64, deviceID string, exp time.Time) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO jwt_revocations (jti, user_id, device_id, expires_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(jti) DO NOTHING
	`, jti, userID, deviceID, exp)
	return err
}

// DeviceRevocationJTI — синтетический jti маркера «все device-JWT устройства
// отозваны». Настоящий jti текущего токена агента серверу неизвестен: device-JWT
// перегенерируется на каждом коннекте (refresh-on-connect в ws_agent) и нигде не
// сохраняется, поэтому отзыв идёт маркером по device_id. Формат маркера
// продублирован в db.ReassignDevice (снятие при ре-пейринге) — держать синхронно.
func DeviceRevocationJTI(deviceID string) string { return "device:" + deviceID }

// RevokeDeviceTokens отзывает ВСЕ device-JWT устройства (blocklist-маркер по
// device_id). Вызывается при отзыве устройства (server/devices.go). Идемпотентно.
func RevokeDeviceTokens(ctx context.Context, d *sql.DB, deviceID string, userID int64) error {
	return RevokeJTI(ctx, d, DeviceRevocationJTI(deviceID), userID, deviceID, time.Time{})
}

// IsDeviceRevoked — true, если device-JWT этого устройства отозваны маркером.
func IsDeviceRevoked(ctx context.Context, d *sql.DB, deviceID string) (bool, error) {
	return IsRevoked(ctx, d, DeviceRevocationJTI(deviceID))
}

// ClearDeviceRevocations снимает маркер отзыва (ре-пейринг/takeover устройства).
func ClearDeviceRevocations(ctx context.Context, d *sql.DB, deviceID string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM jwt_revocations WHERE jti = ?`, DeviceRevocationJTI(deviceID))
	return err
}
