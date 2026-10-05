package db

import (
	"context"
	"database/sql"
	"encoding/json"
)

// InsertEvent пишет веху воронки в events. userID может быть nil — анонимное
// событие (лендинг до регистрации). utm/meta маршалятся в JSON; пустые мапы
// сохраняются как ” (дефолт колонки).
func InsertEvent(ctx context.Context, d *sql.DB, userID *int64, kind, source string, utm, meta map[string]string) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO events (user_id, kind, source, utm_json, meta_json)
		VALUES (?, ?, ?, ?, ?)
	`, userID, kind, source, marshalMap(utm), marshalMap(meta))
	return err
}

// ── Дедуп уведомления «агент обновился» ─────────────────────────────────────
//
// Релейный notifier (server/notifier.go, fireAgentUpdated) шлёт человеку одно
// сообщение на (пользователь, устройство, версия). Отметка об отправке — строка
// events kind='agent_updated' с meta {"device":…, "version":…}: заодно это и
// запись воронки, как остальные события.

// HasAgentUpdatedEvent — отправляли ли уже этому пользователю сообщение «ПК
// обновился до version» про это устройство. Сравнение meta_json ТОЧНОЕ, а не
// LIKE: InsertEvent маршалит meta через encoding/json (ключи сортируются),
// поэтому строка детерминирована и LIKE-джокеры в значениях не страшны.
// Объёмы kind='agent_updated' копеечные (раз в релиз на устройство), отдельный
// индекс не нужен.
func HasAgentUpdatedEvent(ctx context.Context, d *sql.DB, userID int64, deviceID, version string) (bool, error) {
	var n int
	err := d.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM events
		WHERE kind = 'agent_updated' AND user_id = ? AND meta_json = ?
	`, userID, agentUpdatedMeta(deviceID, version)).Scan(&n)
	return n > 0, err
}

// MarkAgentUpdatedEvent фиксирует отправку уведомления «ПК обновился»
// (см. HasAgentUpdatedEvent).
func MarkAgentUpdatedEvent(ctx context.Context, d *sql.DB, userID int64, deviceID, version string) error {
	return InsertEvent(ctx, d, &userID, "agent_updated", "agent", nil,
		map[string]string{"device": deviceID, "version": version})
}

func agentUpdatedMeta(deviceID, version string) string {
	return marshalMap(map[string]string{"device": deviceID, "version": version})
}

// BumpFeatureCounter увеличивает дневной счётчик фичи (day = date('now'), UTC).
// Одна строка на (user, feature, day) — частые события не раздувают БД.
func BumpFeatureCounter(ctx context.Context, d *sql.DB, userID int64, feature string) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO feature_counters (user_id, feature, day, count)
		VALUES (?, ?, date('now'), 1)
		ON CONFLICT(user_id, feature, day) DO UPDATE SET
		    count = count + 1
	`, userID, feature)
	return err
}

// SetUserSourceIfEmpty проставляет users.source/utm_json только если источник
// ещё не задан — первое касание не затирается последующими регистрациями/входами.
// Возвращает true, если запись обновлена (источник был пуст).
func SetUserSourceIfEmpty(ctx context.Context, d *sql.DB, userID int64, source string, utm map[string]string) (bool, error) {
	res, err := d.ExecContext(ctx, `
		UPDATE users SET source = ?, utm_json = ?
		WHERE id = ? AND COALESCE(source, '') = ''
	`, source, marshalMap(utm), userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// marshalMap — компактный JSON для utm/meta; пустая мапа → ”.
func marshalMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
