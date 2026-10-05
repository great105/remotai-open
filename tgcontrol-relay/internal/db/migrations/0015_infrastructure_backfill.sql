-- Repair/backfill is intentionally separate from ALTER migrations: if an
-- interrupted deployment added a column but did not populate it, the next
-- startup still executes this file.
INSERT OR IGNORE INTO workspaces (id, kind, name, owner_user_id)
SELECT 'personal-' || id, 'personal', 'Личное', id
FROM users;

INSERT OR IGNORE INTO workspace_members (workspace_id, user_id, role, added_by)
SELECT 'personal-' || id, id, 'owner', id
FROM users;

UPDATE devices
SET workspace_id = 'personal-' || user_id
WHERE workspace_id IS NULL OR workspace_id = '';

-- A Linux agent is most often a headless server. This is only the initial
-- suggestion; users can override the type from the infrastructure screen.
UPDATE devices
SET device_type = 'server'
WHERE lower(COALESCE(platform, '')) = 'linux'
  AND (device_type IS NULL OR device_type = '' OR device_type = 'computer');

CREATE INDEX IF NOT EXISTS idx_devices_workspace
    ON devices(workspace_id, revoked_at, paired_at DESC);

CREATE INDEX IF NOT EXISTS idx_devices_workspace_type
    ON devices(workspace_id, device_type, revoked_at);

CREATE INDEX IF NOT EXISTS idx_devices_group
    ON devices(group_id, revoked_at);
