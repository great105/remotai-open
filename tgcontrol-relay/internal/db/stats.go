package db

import (
	"context"
	"database/sql"
)

// Stats — мгновенный срез показателей для /metrics. Считается одним проходом
// дешёвых COUNT-ов по индексированным колонкам; вызывающая сторона кэширует
// результат, чтобы не дёргать single-writer SQLite на каждый scrape.
type Stats struct {
	UsersTotal    int64 // всего аккаунтов (включая анонимные)
	UsersActive   int64 // уникальных владельцев устройств, видевшихся за 24ч
	UsersNew24h   int64 // новых аккаунтов за 24ч
	DevicesTotal  int64 // привязанных (не отозванных) устройств
	DevicesNew24h int64 // новых привязок за 24ч
}

// GatherStats собирает срез показателей. Все запросы read-only.
func GatherStats(ctx context.Context, d *sql.DB) (Stats, error) {
	var s Stats
	q := func(dst *int64, query string, args ...any) error {
		return d.QueryRowContext(ctx, query, args...).Scan(dst)
	}
	if err := q(&s.UsersTotal, `SELECT COUNT(*) FROM users`); err != nil {
		return s, err
	}
	// last_seen_at пишется на каждом connect/disconnect агента (MarkDeviceOnline),
	// поэтому «активный за сутки» = у пользователя есть устройство, видевшееся за 24ч.
	if err := q(&s.UsersActive, `
		SELECT COUNT(DISTINCT user_id) FROM devices
		WHERE revoked_at IS NULL AND last_seen_at > datetime('now','-1 day')`); err != nil {
		return s, err
	}
	if err := q(&s.UsersNew24h, `SELECT COUNT(*) FROM users WHERE created_at > datetime('now','-1 day')`); err != nil {
		return s, err
	}
	if err := q(&s.DevicesTotal, `SELECT COUNT(*) FROM devices WHERE revoked_at IS NULL`); err != nil {
		return s, err
	}
	if err := q(&s.DevicesNew24h, `SELECT COUNT(*) FROM devices WHERE paired_at > datetime('now','-1 day')`); err != nil {
		return s, err
	}
	return s, nil
}
