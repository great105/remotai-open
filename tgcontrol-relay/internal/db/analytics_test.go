package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// openTestDB — in-memory sqlite c миграциями (по образцу server.initSQLite).
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := Open(":memory:")
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	if err := Migrate(d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestMigrateIdempotent(t *testing.T) {
	d := openTestDB(t)
	// Повторный прогон (как при рестарте сервера): ALTER TABLE ADD COLUMN
	// не должен ронять старт на "duplicate column name".
	if err := Migrate(d); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestMigrateRepairsPartiallyAppliedUserColumns(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "partial.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	if _, err := d.Exec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			telegram_id BIGINT UNIQUE NOT NULL,
			source TEXT DEFAULT ''
		)`); err != nil {
		t.Fatalf("seed partial users: %v", err)
	}
	if err := Migrate(d); err != nil {
		t.Fatalf("Migrate partial: %v", err)
	}

	var n int
	if err := d.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'utm_json'
	`).Scan(&n); err != nil {
		t.Fatalf("inspect users: %v", err)
	}
	if n != 1 {
		t.Fatalf("utm_json columns = %d, want 1", n)
	}
}

func TestMigrateBackfillsLegacyInfrastructure(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "legacy-infrastructure.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "0011_infrastructure.sql" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		buf, readErr := fs.ReadFile(migrationsFS, "migrations/"+name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		if _, execErr := d.Exec(string(buf)); execErr != nil {
			t.Fatalf("apply legacy migration %s: %v", name, execErr)
		}
	}

	if _, err := d.Exec(`
		INSERT INTO users (id, telegram_id, username, first_name)
		VALUES (77, 770077, 'legacy-owner', 'Legacy');
		INSERT INTO devices (id, user_id, name, hostname, platform, agent_version)
		VALUES
			('legacy-windows', 77, 'Office PC', 'office-pc', 'windows', '2.32.2'),
			('legacy-linux', 77, 'Production', 'prod-01', 'linux', '2.32.2');
	`); err != nil {
		t.Fatalf("seed legacy account: %v", err)
	}

	if err := Migrate(d); err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}

	var kind, workspaceName, role string
	var ownerID int64
	if err := d.QueryRow(`
		SELECT kind, name, owner_user_id
		FROM workspaces WHERE id = 'personal-77'
	`).Scan(&kind, &workspaceName, &ownerID); err != nil {
		t.Fatalf("personal workspace: %v", err)
	}
	if kind != WorkspacePersonal || workspaceName != "Личное" || ownerID != 77 {
		t.Fatalf("workspace = kind %q name %q owner %d", kind, workspaceName, ownerID)
	}
	if err := d.QueryRow(`
		SELECT role FROM workspace_members
		WHERE workspace_id = 'personal-77' AND user_id = 77
	`).Scan(&role); err != nil {
		t.Fatalf("workspace owner membership: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("workspace role = %q, want %q", role, RoleOwner)
	}

	checkDevice := func(id, wantType string) {
		t.Helper()
		var workspaceID, deviceType string
		var groupID sql.NullString
		if err := d.QueryRow(`
			SELECT workspace_id, device_type, group_id
			FROM devices WHERE id = ?
		`, id).Scan(&workspaceID, &deviceType, &groupID); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if workspaceID != "personal-77" || deviceType != wantType || groupID.Valid {
			t.Fatalf("%s = workspace %q type %q group %+v", id, workspaceID, deviceType, groupID)
		}
	}
	checkDevice("legacy-windows", DeviceComputer)
	checkDevice("legacy-linux", DeviceServer)

	if err := Migrate(d); err != nil {
		t.Fatalf("second migrate after backfill: %v", err)
	}
}

func TestInsertEvent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	// Анонимное событие без utm/meta.
	if err := InsertEvent(ctx, d, nil, "landing_visit", "", nil, nil); err != nil {
		t.Fatalf("insert anon: %v", err)
	}
	// Событие с пользователем и метками.
	u, err := UpsertUser(ctx, d, 42, "tester", "Тест", "ru")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	uid := u.ID
	utm := map[string]string{"utm_source": "tg", "utm_campaign": "launch"}
	meta := map[string]string{"platform": "apk"}
	if err := InsertEvent(ctx, d, &uid, "register", "tg", utm, meta); err != nil {
		t.Fatalf("insert user event: %v", err)
	}

	var (
		n        int
		gotKind  string
		gotUser  sql.NullInt64
		gotUTM   string
		gotTS    string
		nullUser sql.NullInt64
	)
	if err := d.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("events count = %d, want 2", n)
	}
	row := d.QueryRow(`SELECT user_id, kind, utm_json, ts FROM events WHERE kind = 'register'`)
	if err := row.Scan(&gotUser, &gotKind, &gotUTM, &gotTS); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !gotUser.Valid || gotUser.Int64 != uid {
		t.Fatalf("user_id = %+v, want %d", gotUser, uid)
	}
	if gotUTM != `{"utm_campaign":"launch","utm_source":"tg"}` {
		t.Fatalf("utm_json = %q", gotUTM)
	}
	if _, err := time.Parse(time.RFC3339, gotTS); err != nil {
		t.Fatalf("ts %q не похож на CURRENT_TIMESTAMP: %v", gotTS, err)
	}
	// У анонимного события user_id NULL.
	if err := d.QueryRow(`SELECT user_id FROM events WHERE kind = 'landing_visit'`).Scan(&nullUser); err != nil {
		t.Fatalf("scan anon: %v", err)
	}
	if nullUser.Valid {
		t.Fatalf("anon user_id = %v, want NULL", nullUser.Int64)
	}
}

// Дедуп «ПК обновился»: (user, device, version) — ровно один сигнал.
func TestAgentUpdatedEventDedup(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	u, err := UpsertUser(ctx, d, 4242, "tester", "Тест", "ru")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	has, err := HasAgentUpdatedEvent(ctx, d, u.ID, "dev1", "2.46.5")
	if err != nil || has {
		t.Fatalf("пустая база: has=%v err=%v, ждали false", has, err)
	}
	if err := MarkAgentUpdatedEvent(ctx, d, u.ID, "dev1", "2.46.5"); err != nil {
		t.Fatalf("mark: %v", err)
	}

	// Та же тройка — дедуп.
	has, err = HasAgentUpdatedEvent(ctx, d, u.ID, "dev1", "2.46.5")
	if err != nil || !has {
		t.Fatalf("та же версия: has=%v err=%v, ждали true", has, err)
	}
	// Другая версия / другое устройство / другой пользователь — не дедуп.
	for _, c := range []struct {
		name    string
		userID  int64
		device  string
		version string
	}{
		{"новая версия", u.ID, "dev1", "2.46.6"},
		{"другой ПК", u.ID, "dev2", "2.46.5"},
		{"другой пользователь", u.ID + 1, "dev1", "2.46.5"},
	} {
		has, err = HasAgentUpdatedEvent(ctx, d, c.userID, c.device, c.version)
		if err != nil || has {
			t.Fatalf("%s: has=%v err=%v, ждали false", c.name, has, err)
		}
	}

	// Отметка лежит в events обычной строкой воронки.
	var kind, source, meta string
	if err := d.QueryRow(`SELECT kind, source, meta_json FROM events`).Scan(&kind, &source, &meta); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if kind != "agent_updated" || meta != `{"device":"dev1","version":"2.46.5"}` {
		t.Fatalf("events row: kind=%q source=%q meta=%q", kind, source, meta)
	}
}

func TestBumpFeatureCounter(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := BumpFeatureCounter(ctx, d, 7, "terminal"); err != nil {
			t.Fatalf("bump terminal: %v", err)
		}
	}
	if err := BumpFeatureCounter(ctx, d, 7, "files"); err != nil {
		t.Fatalf("bump files: %v", err)
	}
	if err := BumpFeatureCounter(ctx, d, 8, "terminal"); err != nil {
		t.Fatalf("bump other user: %v", err)
	}

	var cnt int
	if err := d.QueryRow(`SELECT count FROM feature_counters WHERE user_id = 7 AND feature = 'terminal'`).Scan(&cnt); err != nil {
		t.Fatalf("select: %v", err)
	}
	if cnt != 3 {
		t.Fatalf("count = %d, want 3", cnt)
	}
	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM feature_counters`).Scan(&rows); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if rows != 3 {
		t.Fatalf("rows = %d, want 3 (upsert не плодит строки)", rows)
	}
}

func TestSetUserSourceIfEmpty(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	u, err := UpsertUser(ctx, d, 43, "tester2", "Тест", "ru")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	// Первое касание — проставляется.
	ok, err := SetUserSourceIfEmpty(ctx, d, u.ID, "tg", map[string]string{"utm_source": "channel_a"})
	if err != nil {
		t.Fatalf("set source: %v", err)
	}
	if !ok {
		t.Fatal("первый вызов должен обновить источник")
	}
	var src, utm string
	if err := d.QueryRow(`SELECT source, utm_json FROM users WHERE id = ?`, u.ID).Scan(&src, &utm); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if src != "tg" || utm != `{"utm_source":"channel_a"}` {
		t.Fatalf("source=%q utm=%q, want tg/{utm_source:channel_a}", src, utm)
	}

	// Второе касание — первое НЕ затирается.
	ok, err = SetUserSourceIfEmpty(ctx, d, u.ID, "apk", map[string]string{"utm_source": "channel_b"})
	if err != nil {
		t.Fatalf("set source 2: %v", err)
	}
	if ok {
		t.Fatal("повторный вызов не должен менять источник")
	}
	if err := d.QueryRow(`SELECT source, utm_json FROM users WHERE id = ?`, u.ID).Scan(&src, &utm); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if src != "tg" || utm != `{"utm_source":"channel_a"}` {
		t.Fatalf("источник затёрся: source=%q utm=%q", src, utm)
	}
}
