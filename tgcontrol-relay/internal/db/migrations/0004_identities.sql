-- Мульти-провайдерная идентичность (см. docs/auth-multiregion-2026-07.md).
-- Аккаунт = users.id; провайдеры входа прикрепляются записями ниже.

CREATE TABLE IF NOT EXISTS user_identities (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id),
    provider        TEXT NOT NULL,            -- 'telegram' | 'email' | 'vk' | 'yandex' | 'google' | 'apple'
    provider_uid    TEXT NOT NULL,            -- id у провайдера или email (нормализованный, lower)
    display         TEXT,                     -- подпись для UI («a@b.c», «Иван»)
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_used_at    TIMESTAMP,
    UNIQUE(provider, provider_uid)
);

CREATE INDEX IF NOT EXISTS idx_identities_user ON user_identities(user_id);

-- Одноразовые коды входа по email. Сам код не храним, только sha256.
CREATE TABLE IF NOT EXISTS email_login_codes (
    token           TEXT PRIMARY KEY,         -- nonce сессии входа (login_token)
    email           TEXT NOT NULL,
    code_hash       TEXT NOT NULL,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    expires_at      TIMESTAMP NOT NULL,
    consumed_at     TIMESTAMP,
    attempts        INTEGER DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_email_codes_email ON email_login_codes(email);
CREATE INDEX IF NOT EXISTS idx_email_codes_expires ON email_login_codes(expires_at);
