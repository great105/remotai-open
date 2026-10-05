// Package db — слой персистенции на чистом Go SQLite (modernc.org/sqlite).
// Использует database/sql, без ORM, чтобы оставаться предсказуемым.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open открывает SQLite с journal_mode=WAL и foreign_keys=on.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)", path)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	d.SetMaxOpenConns(1) // SQLite — single writer; serialize.
	if err := d.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return d, nil
}

func Ping(d *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return d.PingContext(ctx)
}

// Migrate применяет все встроенные миграции в лексикографическом порядке.
// Идемпотентна — каждая миграция использует IF NOT EXISTS, а для
// ALTER TABLE ADD COLUMN (SQLite не умеет IF NOT EXISTS для колонок)
// ошибка "duplicate column name" игнорируется.
func Migrate(d *sql.DB) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		buf, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := d.Exec(string(buf)); err != nil {
			// ADD COLUMN без IF NOT EXISTS падает при повторном запуске —
			// колонка уже есть, миграция фактически применена.
			if !strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("apply %s: %w", name, err)
			}
			log.Printf("[DB] migration %s: column already exists, skipping", name)
		} else {
			log.Printf("[DB] migration applied: %s", name)
		}
	}
	if err := backfillFounders(d); err != nil {
		return fmt.Errorf("backfill founders: %w", err)
	}
	return nil
}

// FounderCutoff — дата отсечки founder-когорты: все, кто зарегистрирован не
// позже неё, получают вечный Pro (см. ПРОДВИЖЕНИЕ/founder-cohort.md).
const FounderCutoff = "2026-08-05 23:59:59"

// backfillFounders помечает нынешних пользователей вечным Pro. Вынесен из
// SQL-миграции, потому что ссылается на created_at, а тест частично применённой
// схемы создаёт users без этой колонки. Устойчив и идемпотентен: работает
// только когда обе нужные колонки существуют, и трогает лишь ещё не помеченных.
func backfillFounders(d *sql.DB) error {
	if !hasColumn(d, "users", "created_at") || !hasColumn(d, "users", "founder") {
		return nil
	}
	_, err := d.Exec(
		`UPDATE users SET founder = 1 WHERE founder = 0 AND created_at <= ?`,
		FounderCutoff)
	return err
}

func hasColumn(d *sql.DB, table, col string) bool {
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, col).Scan(&n); err != nil {
		return false
	}
	return n == 1
}
