-- Сессии OAuth-входа через redirect-флоу релея (begin → провайдер → callback
-- → poll). Аналог login_tokens для Telegram, но с PKCE (VK ID требует).

CREATE TABLE IF NOT EXISTS oauth_login_states (
    state           TEXT PRIMARY KEY,         -- одноразовый nonce сессии
    provider        TEXT NOT NULL,            -- 'vk' | 'yandex' | 'google'
    code_verifier   TEXT,                     -- PKCE (VK ID); для остальных пусто
    device_id       TEXT,                     -- VK ID требует device_id на exchange
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    expires_at      TIMESTAMP NOT NULL,
    confirmed_at    TIMESTAMP,
    consumed_at     TIMESTAMP,
    user_id         INTEGER REFERENCES users(id)
);

CREATE INDEX IF NOT EXISTS idx_oauth_states_expires ON oauth_login_states(expires_at);
