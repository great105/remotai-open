-- device_grants — дополнительные пользователи, допущенные управлять устройством
-- (помимо владельца devices.user_id). Скан QR с экрана ПК добавляет сюда строку,
-- что и даёт «сколько угодно устройств управляют одним ПК». Доступ = владелец
-- ИЛИ активный грант. Владелец остаётся один (он управляет привязкой и revoke).
CREATE TABLE IF NOT EXISTS device_grants (
    device_id  TEXT    NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id)   ON DELETE CASCADE,
    granted_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    revoked_at TIMESTAMP,
    PRIMARY KEY (device_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_grants_user   ON device_grants(user_id);
CREATE INDEX IF NOT EXISTS idx_grants_device ON device_grants(device_id);
