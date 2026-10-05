-- Account infrastructure model:
--   user -> workspaces (personal/company) -> groups -> managed devices.
-- Existing devices are backfilled by 0015 after the new device columns exist.

CREATE TABLE IF NOT EXISTS workspaces (
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL CHECK (kind IN ('personal', 'company')),
    name            TEXT NOT NULL,
    owner_user_id   INTEGER NOT NULL REFERENCES users(id),
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    archived_at     TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_personal_owner
    ON workspaces(owner_user_id)
    WHERE kind = 'personal' AND archived_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_workspaces_owner
    ON workspaces(owner_user_id, archived_at);

CREATE TABLE IF NOT EXISTS workspace_members (
    workspace_id    TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role            TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'operator', 'viewer')),
    added_by        INTEGER REFERENCES users(id),
    joined_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (workspace_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_workspace_members_user
    ON workspace_members(user_id, workspace_id);

CREATE TABLE IF NOT EXISTS workspace_invites (
    code            TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    role            TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'viewer')),
    created_by      INTEGER NOT NULL REFERENCES users(id),
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at      TIMESTAMP NOT NULL,
    max_uses        INTEGER NOT NULL DEFAULT 1,
    used_count      INTEGER NOT NULL DEFAULT 0,
    revoked_at      TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_workspace_invites_workspace
    ON workspace_invites(workspace_id, expires_at);

CREATE TABLE IF NOT EXISTS device_groups (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    sort_order      INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(workspace_id, name)
);

CREATE INDEX IF NOT EXISTS idx_device_groups_workspace
    ON device_groups(workspace_id, sort_order, name);

CREATE TABLE IF NOT EXISTS workspace_tags (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    color           TEXT NOT NULL DEFAULT '#2ee6b0',
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(workspace_id, name)
);

CREATE INDEX IF NOT EXISTS idx_workspace_tags_workspace
    ON workspace_tags(workspace_id, name);

CREATE TABLE IF NOT EXISTS device_tags (
    device_id       TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    tag_id          TEXT NOT NULL REFERENCES workspace_tags(id) ON DELETE CASCADE,
    PRIMARY KEY (device_id, tag_id)
);

CREATE INDEX IF NOT EXISTS idx_device_tags_tag
    ON device_tags(tag_id, device_id);

CREATE TABLE IF NOT EXISTS device_favorites (
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id       TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, device_id)
);

CREATE TABLE IF NOT EXISTS workspace_audit (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id    TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    actor_user_id   INTEGER REFERENCES users(id) ON DELETE SET NULL,
    action          TEXT NOT NULL,
    target_type     TEXT NOT NULL,
    target_id       TEXT,
    metadata_json   TEXT NOT NULL DEFAULT '{}',
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_workspace_audit_feed
    ON workspace_audit(workspace_id, created_at DESC, id DESC);

-- Durable login sessions for APK/web clients. session_id survives sliding JWT
-- refresh; current_jti is replaced on refresh. Telegram Mini App requests are
-- represented by a short-lived session derived from initData auth_date.
CREATE TABLE IF NOT EXISTS user_sessions (
    session_id      TEXT PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    current_jti     TEXT,
    client_kind     TEXT NOT NULL DEFAULT 'web',
    client_name     TEXT NOT NULL DEFAULT '',
    user_agent      TEXT NOT NULL DEFAULT '',
    ip_address      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at      TIMESTAMP,
    revoked_at      TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_user_sessions_user
    ON user_sessions(user_id, revoked_at, last_seen_at DESC);

-- Every existing account gets a stable personal workspace. A deterministic id
-- keeps the migration idempotent and makes anon -> permanent merges predictable.
INSERT OR IGNORE INTO workspaces (id, kind, name, owner_user_id)
SELECT 'personal-' || id, 'personal', 'Личное', id
FROM users;

INSERT OR IGNORE INTO workspace_members (workspace_id, user_id, role, added_by)
SELECT 'personal-' || id, id, 'owner', id
FROM users;
