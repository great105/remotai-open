package db

import (
	"context"
	"database/sql"
)

// Настройка «писать про ошибки в терминале» (колонка users.tg_notify_errors,
// миграция 0019). Независима от tg_notify: тот отвечает за вопросы агента, ради
// которых канал и существует, а ошибки — отдельный, куда более шумный повод
// (триггер широкий: любая строка, начинающаяся с error/FAIL/Traceback).
// Управляется командой /notify в боте.

// NotifyErrorsEnabled — присылать ли сообщения про ошибки в терминале.
// Несуществующий пользователь → false без ошибки: слать всё равно некуда.
func NotifyErrorsEnabled(ctx context.Context, d *sql.DB, userID int64) (bool, error) {
	var on int
	err := d.QueryRowContext(ctx,
		`SELECT COALESCE(tg_notify_errors, 0) FROM users WHERE id = ?`, userID).Scan(&on)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return on != 0, nil
}

// SetNotifyErrorsEnabled включает/выключает сообщения про ошибки в терминале.
func SetNotifyErrorsEnabled(ctx context.Context, d *sql.DB, userID int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := d.ExecContext(ctx, `UPDATE users SET tg_notify_errors = ? WHERE id = ?`, v, userID)
	return err
}
