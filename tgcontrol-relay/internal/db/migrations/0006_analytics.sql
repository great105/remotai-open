-- Аналитика: воронка событий + дневные счётчики фич.
-- Собираем факт использования фичи, НЕ контент (см. план: позиция «без слежки»).

-- Редкие вехи воронки: landing_visit, landing_download, register, app_open,
-- pair_success, first_terminal. utm_json/meta_json — JSON-объекты строка-строка.
CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          DATETIME DEFAULT CURRENT_TIMESTAMP,
    user_id     INTEGER,                 -- NULL для анонимных (лендинг до регистрации)
    kind        TEXT NOT NULL,
    source      TEXT DEFAULT '',
    utm_json    TEXT DEFAULT '',
    meta_json   TEXT DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_events_kind_ts ON events(kind, ts);
CREATE INDEX IF NOT EXISTS idx_events_user ON events(user_id);

-- Частые события (terminal/files/screen/ssh/system) — дневной upsert,
-- чтобы не раздувать БД по строке на запрос.
CREATE TABLE IF NOT EXISTS feature_counters (
    user_id     INTEGER NOT NULL,
    feature     TEXT NOT NULL,
    day         TEXT NOT NULL,           -- date('now'), UTC
    count       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, feature, day)
);
