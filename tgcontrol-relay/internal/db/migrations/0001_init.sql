CREATE TABLE IF NOT EXISTS users (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    telegram_id       BIGINT UNIQUE NOT NULL,
    username          TEXT,
    first_name        TEXT,
    locale            TEXT DEFAULT 'ru',
    tier              TEXT DEFAULT 'free',
    created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_seen_at      TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_users_tg ON users(telegram_id);

CREATE TABLE IF NOT EXISTS devices (
    id                TEXT PRIMARY KEY,
    user_id           INTEGER NOT NULL REFERENCES users(id),
    name              TEXT NOT NULL,
    hostname          TEXT,
    platform          TEXT,
    agent_version     TEXT,
    last_seen_at      TIMESTAMP,
    online            BOOLEAN DEFAULT 0,
    paired_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    revoked_at        TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_devices_user ON devices(user_id);
CREATE INDEX IF NOT EXISTS idx_devices_online ON devices(online);

CREATE TABLE IF NOT EXISTS pairing_codes (
    code              TEXT PRIMARY KEY,
    device_id         TEXT NOT NULL,
    hostname          TEXT,
    platform          TEXT,
    agent_version     TEXT,
    created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    expires_at        TIMESTAMP NOT NULL,
    consumed_at       TIMESTAMP,
    consumed_by_uid   INTEGER REFERENCES users(id),
    issued_jwt        TEXT,
    attempts          INTEGER DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_pairing_expires ON pairing_codes(expires_at);
CREATE INDEX IF NOT EXISTS idx_pairing_device ON pairing_codes(device_id);

CREATE TABLE IF NOT EXISTS jwt_revocations (
    jti               TEXT PRIMARY KEY,
    user_id           INTEGER,
    device_id         TEXT,
    revoked_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    expires_at        TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_revocations_expires ON jwt_revocations(expires_at);

CREATE TABLE IF NOT EXISTS rate_limits (
    bucket            TEXT PRIMARY KEY,
    tokens            REAL NOT NULL,
    updated_at        TIMESTAMP NOT NULL
);
