-- Telegram-логин через deep-link + одноразовый nonce.
-- Клиент (APK/web/exe) получает nonce в POST /v1/auth/tg/start, открывает
-- t.me/<bot>?start=login_<nonce>; бот привязывает nonce к telegram_id; клиент
-- забирает durable user-JWT в GET /v1/auth/tg/poll. Single-use, короткий TTL.
CREATE TABLE IF NOT EXISTS login_tokens (
    nonce         TEXT PRIMARY KEY,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at    TIMESTAMP NOT NULL,
    user_id       INTEGER REFERENCES users(id),
    confirmed_at  TIMESTAMP,
    consumed_at   TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_login_tokens_expires ON login_tokens(expires_at);
