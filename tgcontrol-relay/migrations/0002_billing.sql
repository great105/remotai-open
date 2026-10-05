CREATE TABLE IF NOT EXISTS subscriptions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id             INTEGER UNIQUE NOT NULL REFERENCES users(id),
    tier                TEXT DEFAULT 'free',
    stripe_customer     TEXT,
    stripe_sub          TEXT,
    current_period_end  TIMESTAMP,
    cancelled_at        TIMESTAMP,
    updated_at          TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_subs_user ON subscriptions(user_id);
