package db

import (
	"context"
	"database/sql"
)

// Настройка «уведомления о вопросах агента в Telegram» (колонка users.tg_notify,
// миграция 0008). Управляется командой /notify в боте.

// NotifyEnabled — включены ли уведомления у пользователя. Несуществующий
// пользователь → false без ошибки: слать всё равно некуда.
func NotifyEnabled(ctx context.Context, d *sql.DB, userID int64) (bool, error) {
	var on int
	err := d.QueryRowContext(ctx,
		`SELECT COALESCE(tg_notify, 1) FROM users WHERE id = ?`, userID).Scan(&on)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return on != 0, nil
}

// SetNotifyEnabled включает/выключает уведомления. Вызывается из /notify и
// автоматически, когда Telegram сообщил, что бот заблокирован (403): иначе
// релей будет биться в стену на каждом вопросе агента.
func SetNotifyEnabled(ctx context.Context, d *sql.DB, userID int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := d.ExecContext(ctx, `UPDATE users SET tg_notify = ? WHERE id = ?`, v, userID)
	return err
}
