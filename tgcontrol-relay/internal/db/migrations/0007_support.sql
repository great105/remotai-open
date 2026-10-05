-- Поддержка: чат пользователя с админом прямо в приложении.
-- Тред один на пользователя (user_id UNIQUE), создаётся при первом сообщении.

CREATE TABLE IF NOT EXISTS support_threads (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_msg_at  DATETIME,
    unread_user  INTEGER NOT NULL DEFAULT 0,    -- непрочитано пользователем (ответы админа)
    unread_admin INTEGER NOT NULL DEFAULT 0     -- непрочитано админом (сообщения юзера)
);

CREATE TABLE IF NOT EXISTS support_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id  INTEGER NOT NULL REFERENCES support_threads(id) ON DELETE CASCADE,
    sender     TEXT NOT NULL CHECK (sender IN ('user', 'admin')),
    text       TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_support_messages_thread ON support_messages(thread_id, id);
