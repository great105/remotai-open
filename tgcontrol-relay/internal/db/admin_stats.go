package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"time"
)

// AdminStats — сводка для админ-кабинета /v1/admin/stats.
type AdminStats struct {
	Funnel        AdminFunnel  `json:"funnel"`
	RegsByDay     []DayCount   `json:"regs_by_day"`
	Active        ActiveCounts `json:"active"`
	DevicesOnline int64        `json:"devices_online"`
	Sources       []NameCount  `json:"sources"`
	UTM           []NameCount  `json:"utm"`
	Features      []FeatureSum `json:"features"`
	RecentUsers   []RecentUser `json:"recent_users"`
}

// AdminFunnel — вехи воронки за 30 дней (events) + активные за 7д (users).
type AdminFunnel struct {
	LandingVisit    int64 `json:"landing_visit"`
	LandingDownload int64 `json:"landing_download"`
	Register        int64 `json:"register"`
	PairSuccess     int64 `json:"pair_success"`
	Active7d        int64 `json:"active_7d"`
}

type DayCount struct {
	Day string `json:"day"`
	N   int64  `json:"n"`
}

type ActiveCounts struct {
	DAU int64 `json:"dau"`
	WAU int64 `json:"wau"`
	MAU int64 `json:"mau"`
}

type NameCount struct {
	Name string `json:"name"`
	N    int64  `json:"n"`
}

// FeatureSum — использование фичи за 7 и 30 дней (feature_counters).
type FeatureSum struct {
	Feature string `json:"feature"`
	N7      int64  `json:"n7"`
	N30     int64  `json:"n30"`
}

// RecentUser — строка таблицы «последние пользователи». Даты — RFC3339,
// пустая строка если last_seen не было.
type RecentUser struct {
	Username     string `json:"username"`
	FirstName    string `json:"first_name"`
	Source       string `json:"source"`
	CreatedAt    string `json:"created_at"`
	LastSeenAt   string `json:"last_seen_at"`
	DevicesCount int64  `json:"devices_count"`
}

// GatherAdminStats собирает сводку для админки. Запросов несколько, но все
// дешёвые COUNT/GROUP BY; админка дёргает её редко (по polling 15с — кэш
// не нужен, нагрузка копеечная даже без него).
func GatherAdminStats(ctx context.Context, d *sql.DB) (AdminStats, error) {
	var st AdminStats
	q := func(dst *int64, query string, args ...any) error {
		return d.QueryRowContext(ctx, query, args...).Scan(dst)
	}

	// Воронка за 30 дней.
	if err := q(&st.Funnel.LandingVisit,
		`SELECT COUNT(*) FROM events WHERE kind = 'landing_visit' AND ts > datetime('now','-30 day')`); err != nil {
		return st, err
	}
	if err := q(&st.Funnel.LandingDownload,
		`SELECT COUNT(*) FROM events WHERE kind = 'landing_download' AND ts > datetime('now','-30 day')`); err != nil {
		return st, err
	}
	if err := q(&st.Funnel.Register,
		`SELECT COUNT(*) FROM events WHERE kind = 'register' AND ts > datetime('now','-30 day')`); err != nil {
		return st, err
	}
	if err := q(&st.Funnel.PairSuccess,
		`SELECT COUNT(*) FROM events WHERE kind = 'pair_success' AND ts > datetime('now','-30 day')`); err != nil {
		return st, err
	}
	if err := q(&st.Funnel.Active7d,
		`SELECT COUNT(*) FROM users WHERE last_seen_at > datetime('now','-7 day')`); err != nil {
		return st, err
	}

	// Регистрации по дням (30д).
	rows, err := d.QueryContext(ctx, `
		SELECT date(created_at), COUNT(*) FROM users
		WHERE created_at > datetime('now','-30 day')
		GROUP BY date(created_at) ORDER BY date(created_at)`)
	if err != nil {
		return st, err
	}
	st.RegsByDay = []DayCount{}
	for rows.Next() {
		var dc DayCount
		if err := rows.Scan(&dc.Day, &dc.N); err != nil {
			rows.Close()
			return st, err
		}
		st.RegsByDay = append(st.RegsByDay, dc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	// Активность по users.last_seen_at.
	if err := q(&st.Active.DAU, `SELECT COUNT(*) FROM users WHERE last_seen_at > datetime('now','-1 day')`); err != nil {
		return st, err
	}
	if err := q(&st.Active.WAU, `SELECT COUNT(*) FROM users WHERE last_seen_at > datetime('now','-7 day')`); err != nil {
		return st, err
	}
	if err := q(&st.Active.MAU, `SELECT COUNT(*) FROM users WHERE last_seen_at > datetime('now','-30 day')`); err != nil {
		return st, err
	}

	// Устройства онлайн.
	if err := q(&st.DevicesOnline,
		`SELECT COUNT(*) FROM devices WHERE online = 1 AND revoked_at IS NULL`); err != nil {
		return st, err
	}

	// Источники регистраций.
	rows, err = d.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(source, ''), '(не указан)'), COUNT(*)
		FROM users GROUP BY COALESCE(NULLIF(source, ''), '(не указан)')
		ORDER BY COUNT(*) DESC`)
	if err != nil {
		return st, err
	}
	st.Sources = []NameCount{}
	for rows.Next() {
		var nc NameCount
		if err := rows.Scan(&nc.Name, &nc.N); err != nil {
			rows.Close()
			return st, err
		}
		st.Sources = append(st.Sources, nc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	// UTM-источники: парсим users.utm_json в Go (JSON-функций в SQLite может
	// не быть — modernc собирает без расширений), строк всё равно немного.
	st.UTM, err = utmBreakdown(ctx, d)
	if err != nil {
		return st, err
	}

	// Фичи за 7/30 дней.
	rows, err = d.QueryContext(ctx, `
		SELECT feature,
		       SUM(CASE WHEN day >= date('now','-7 day')  THEN count ELSE 0 END),
		       SUM(CASE WHEN day >= date('now','-30 day') THEN count ELSE 0 END)
		FROM feature_counters GROUP BY feature ORDER BY 3 DESC`)
	if err != nil {
		return st, err
	}
	st.Features = []FeatureSum{}
	for rows.Next() {
		var fs FeatureSum
		if err := rows.Scan(&fs.Feature, &fs.N7, &fs.N30); err != nil {
			rows.Close()
			return st, err
		}
		st.Features = append(st.Features, fs)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	// Последние 50 пользователей.
	rows, err = d.QueryContext(ctx, `
		SELECT COALESCE(username, ''), COALESCE(first_name, ''), COALESCE(source, ''),
		       created_at, last_seen_at,
		       (SELECT COUNT(*) FROM devices dv WHERE dv.user_id = u.id AND dv.revoked_at IS NULL)
		FROM users u ORDER BY created_at DESC, id DESC LIMIT 50`)
	if err != nil {
		return st, err
	}
	st.RecentUsers = []RecentUser{}
	for rows.Next() {
		var (
			ru       RecentUser
			created  time.Time
			lastSeen sql.NullTime
		)
		if err := rows.Scan(&ru.Username, &ru.FirstName, &ru.Source, &created, &lastSeen, &ru.DevicesCount); err != nil {
			rows.Close()
			return st, err
		}
		ru.CreatedAt = created.UTC().Format(time.RFC3339)
		if lastSeen.Valid {
			ru.LastSeenAt = lastSeen.Time.UTC().Format(time.RFC3339)
		}
		st.RecentUsers = append(st.RecentUsers, ru)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	return st, nil
}

// utmBreakdown — распределение utm_source по users.utm_json.
func utmBreakdown(ctx context.Context, d *sql.DB) ([]NameCount, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT utm_json FROM users WHERE COALESCE(utm_json, '') != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			continue // битый JSON не должен ронять всю сводку
		}
		if src := m["utm_source"]; src != "" {
			counts[src]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]NameCount, 0, len(counts))
	for name, n := range counts {
		out = append(out, NameCount{Name: name, N: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
