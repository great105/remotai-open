package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrCodeNotFound = errors.New("pairing code not found")
var ErrCodeExpired = errors.New("pairing code expired")
var ErrCodeConsumed = errors.New("pairing code already consumed")
var ErrCodeTooManyAttempts = errors.New("pairing code locked after too many failed attempts")

// MaxPairCodeAttempts — порог неудачных попыток использования кода, после
// которого код блокируется (анти-брутфорс, см. IncAttempts). Успешный ввод
// (владелец или грант) счётчик не трогает, поэтому легальный мульти-скан
// в пределах TTL под порог не подпадает.
const MaxPairCodeAttempts = 10

type PairingCode struct {
	Code          string
	DeviceID      string
	Hostname      string
	Platform      string
	AgentVersion  string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	ConsumedAt    sql.NullTime
	ConsumedByUID sql.NullInt64
	IssuedJWT     string
	Attempts      int
}

// GenerateCode возвращает 8-символьный код вида FXSK-9KQ7 из алфавита без
// визуально похожих символов: исключены O/0, I/1/L, цифры-двойники букв
// (5↔S, 2↔Z, 8↔B, 6↔G) и U↔V. NormalizeCode дополнительно сворачивает
// двойники, так что даже опечатка вроде «5» вместо «S» всё равно совпадёт.
const codeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ3479"

func GenerateCode() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 0, 9)
	for i, b := range buf {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, codeAlphabet[int(b)%len(codeAlphabet)])
	}
	return string(out), nil
}

// confusableFolds сворачивает визуально похожие символы к каноническому
// алфавиту codeAlphabet. Коды никогда не содержат свёрнутых символов, поэтому
// неверно прочитанный/набранный двойник (5 вместо S и т.п.) всё равно совпадёт.
var confusableFolds = strings.NewReplacer(
	"5", "S", "2", "Z", "8", "B", "6", "G", "U", "V",
)

// NormalizeCode убирает пробелы/дефисы, приводит к uppercase и сворачивает
// визуально похожие символы к каноническому алфавиту.
func NormalizeCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	s = confusableFolds.Replace(s)
	if len(s) == 8 {
		return s[:4] + "-" + s[4:]
	}
	return s
}

func InsertPairingCode(ctx context.Context, d *sql.DB, code, deviceID, hostname, platform, agentVer string, ttl time.Duration) (*PairingCode, error) {
	now := time.Now().UTC()
	exp := now.Add(ttl)
	_, err := d.ExecContext(ctx, `
		INSERT INTO pairing_codes (code, device_id, hostname, platform, agent_version, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, code, deviceID, hostname, platform, agentVer, now, exp)
	if err != nil {
		return nil, err
	}
	return &PairingCode{
		Code: code, DeviceID: deviceID, Hostname: hostname, Platform: platform,
		AgentVersion: agentVer, CreatedAt: now, ExpiresAt: exp,
	}, nil
}

func GetPairingCode(ctx context.Context, d *sql.DB, code string) (*PairingCode, error) {
	var p PairingCode
	row := d.QueryRowContext(ctx, `
		SELECT code, device_id, COALESCE(hostname,''), COALESCE(platform,''),
		       COALESCE(agent_version,''), created_at, expires_at, consumed_at, consumed_by_uid,
		       COALESCE(issued_jwt,''), COALESCE(attempts,0)
		FROM pairing_codes WHERE code = ?
	`, code)
	err := row.Scan(&p.Code, &p.DeviceID, &p.Hostname, &p.Platform, &p.AgentVersion,
		&p.CreatedAt, &p.ExpiresAt, &p.ConsumedAt, &p.ConsumedByUID, &p.IssuedJWT, &p.Attempts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCodeNotFound
		}
		return nil, err
	}
	return &p, nil
}

// IncAttempts увеличивает счётчик неудачных попыток. Используется для троттлинга:
// вызывается из server/pairing.go и bot при неудачном использовании кода;
// после MaxPairCodeAttempts код блокируется (ErrCodeTooManyAttempts).
func IncAttempts(ctx context.Context, d *sql.DB, code string) error {
	_, err := d.ExecContext(ctx, `UPDATE pairing_codes SET attempts = attempts + 1 WHERE code = ?`, code)
	return err
}

// ConsumePairingCode атомарно помечает код как использованный, записывает выданный JWT
// и возвращает обновлённую запись. Если код уже был использован/просрочен — ошибка.
func ConsumePairingCode(ctx context.Context, d *sql.DB, code string, userID int64, jwt string) (*PairingCode, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	p := &PairingCode{}
	row := tx.QueryRowContext(ctx, `
		SELECT code, device_id, COALESCE(hostname,''), COALESCE(platform,''),
		       COALESCE(agent_version,''), created_at, expires_at, consumed_at, consumed_by_uid,
		       COALESCE(issued_jwt,''), COALESCE(attempts,0)
		FROM pairing_codes WHERE code = ?
	`, code)
	if err := row.Scan(&p.Code, &p.DeviceID, &p.Hostname, &p.Platform, &p.AgentVersion,
		&p.CreatedAt, &p.ExpiresAt, &p.ConsumedAt, &p.ConsumedByUID, &p.IssuedJWT, &p.Attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCodeNotFound
		}
		return nil, err
	}
	if p.ConsumedAt.Valid {
		return nil, ErrCodeConsumed
	}
	if p.Attempts >= MaxPairCodeAttempts {
		return nil, ErrCodeTooManyAttempts
	}
	if time.Now().UTC().After(p.ExpiresAt) {
		return nil, ErrCodeExpired
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE pairing_codes SET consumed_at = CURRENT_TIMESTAMP, consumed_by_uid = ?, issued_jwt = ?
		WHERE code = ?
	`, userID, jwt, code); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	p.ConsumedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	p.ConsumedByUID = sql.NullInt64{Int64: userID, Valid: true}
	p.IssuedJWT = jwt
	return p, nil
}

// CleanupExpiredPairing удаляет коды старше ttl*2.
func CleanupExpiredPairing(ctx context.Context, d *sql.DB) (int64, error) {
	res, err := d.ExecContext(ctx, `DELETE FROM pairing_codes WHERE expires_at < datetime('now', '-1 hour')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecentAttemptsByUser считает сколько раз пользователь пытался подтверждать
// код за последние 10 минут (для rate-limit).
func RecentAttemptsByUser(ctx context.Context, d *sql.DB, userID int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pairing_codes
		WHERE consumed_by_uid = ? AND consumed_at > datetime('now','-10 minutes')
	`, userID).Scan(&n)
	return n, err
}

// String представление для логов.
func (p *PairingCode) String() string {
	return fmt.Sprintf("PairingCode(code=%s, device=%s, exp=%s)", p.Code, p.DeviceID, p.ExpiresAt.Format(time.RFC3339))
}
