-- A desktop may still hold a device JWT issued to an anonymous account when
-- that account is merged into Telegram/email. Keep the narrow old -> new
-- principal mapping so the next agent reconnect can receive a fresh JWT.
-- No foreign keys are intentional: the old anonymous user is deleted.
CREATE TABLE IF NOT EXISTS user_merge_aliases (
    old_user_id     INTEGER PRIMARY KEY,
    new_user_id     INTEGER NOT NULL,
    merged_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_user_merge_aliases_target
    ON user_merge_aliases(new_user_id);
