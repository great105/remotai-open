package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Device struct {
	ID           string
	UserID       int64
	WorkspaceID  string
	Name         string
	Hostname     string
	Platform     string
	DeviceType   string
	GroupID      string
	AgentVersion string
	LastSeenAt   sql.NullTime
	Online       bool
	PairedAt     time.Time
	RevokedAt    sql.NullTime
}

func InsertDevice(ctx context.Context, d *sql.DB, dev *Device) error {
	if dev.WorkspaceID == "" {
		workspace, err := EnsurePersonalWorkspace(ctx, d, dev.UserID)
		if err != nil {
			return err
		}
		dev.WorkspaceID = workspace.ID
	}
	if dev.DeviceType == "" {
		dev.DeviceType = SuggestedDeviceType(dev.Platform)
	}
	_, err := d.ExecContext(ctx, `
		INSERT INTO devices (
			id, user_id, workspace_id, name, hostname, platform, device_type,
			group_id, agent_version, paired_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, CURRENT_TIMESTAMP)
	`, dev.ID, dev.UserID, dev.WorkspaceID, dev.Name, dev.Hostname, dev.Platform,
		dev.DeviceType, dev.GroupID, dev.AgentVersion)
	return err
}

// ReassignDevice переносит существующее устройство на (нового) владельца —
// используется при повторном пайринге, например когда приложение на телефоне
// переустановили и оно оказалось на новом аккаунте. Авторизацией служит
// валидный pairing-код (как и при первичном пайринге).
func ReassignDevice(ctx context.Context, d *sql.DB, dev *Device) error {
	if dev.WorkspaceID == "" {
		workspace, err := EnsurePersonalWorkspace(ctx, d, dev.UserID)
		if err != nil {
			return err
		}
		dev.WorkspaceID = workspace.ID
	}
	if dev.DeviceType == "" {
		dev.DeviceType = SuggestedDeviceType(dev.Platform)
	}
	_, err := d.ExecContext(ctx, `
		UPDATE devices
		SET user_id = ?, workspace_id = ?, name = ?, hostname = ?, platform = ?,
		    device_type = ?, group_id = NULLIF(?, ''), agent_version = ?,
		    revoked_at = NULL, paired_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, dev.UserID, dev.WorkspaceID, dev.Name, dev.Hostname, dev.Platform,
		dev.DeviceType, dev.GroupID, dev.AgentVersion, dev.ID)
	if err != nil {
		return err
	}
	// Устройство снова живое: снимаем blocklist-маркер отзыва его device-JWT,
	// иначе свежий токен после ре-пейринга/takeover был бы отклонён. Формат
	// маркера 'device:<id>' — см. auth.DeviceRevocationJTI (держать синхронно;
	// пакет auth отсюда не импортируем, чтобы не тянуть его в слой БД).
	_, err = d.ExecContext(ctx, `DELETE FROM jwt_revocations WHERE jti = 'device:' || ?`, dev.ID)
	return err
}

func GetDevice(ctx context.Context, d *sql.DB, id string) (*Device, error) {
	var dev Device
	row := d.QueryRowContext(ctx, `
		SELECT id, user_id, COALESCE(workspace_id, 'personal-' || user_id),
		       name, COALESCE(hostname,''), COALESCE(platform,''),
		       COALESCE(device_type,'computer'), COALESCE(group_id,''),
		       COALESCE(agent_version,''), last_seen_at, online, paired_at, revoked_at
		FROM devices WHERE id = ?
	`, id)
	err := row.Scan(&dev.ID, &dev.UserID, &dev.WorkspaceID, &dev.Name, &dev.Hostname,
		&dev.Platform, &dev.DeviceType, &dev.GroupID, &dev.AgentVersion,
		&dev.LastSeenAt, &dev.Online, &dev.PairedAt, &dev.RevokedAt)
	if err != nil {
		return nil, err
	}
	return &dev, nil
}

// GetDeviceForAgentAuth validates the user principal embedded in a device JWT.
// A stale anonymous principal is accepted only when it maps to the device's
// current owner through an account merge. After a real device takeover the
// owner no longer matches, so the old token remains rejected.
func GetDeviceForAgentAuth(ctx context.Context, d *sql.DB, id string, tokenUserID int64) (*Device, error) {
	dev, err := GetDevice(ctx, d, id)
	if err != nil {
		return nil, err
	}
	if dev.UserID == tokenUserID {
		return dev, nil
	}
	target, err := MergedUserTarget(ctx, d, tokenUserID)
	if err == nil && target == dev.UserID {
		return dev, nil
	}
	return nil, errors.New("device reassigned")
}

func ListDevicesByUser(ctx context.Context, d *sql.DB, userID int64) ([]*Device, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT id, user_id, COALESCE(workspace_id, 'personal-' || user_id),
		       name, COALESCE(hostname,''), COALESCE(platform,''),
		       COALESCE(device_type,'computer'), COALESCE(group_id,''),
		       COALESCE(agent_version,''), last_seen_at, online, paired_at, revoked_at
		FROM devices
		WHERE user_id = ? AND revoked_at IS NULL
		ORDER BY paired_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.ID, &dev.UserID, &dev.WorkspaceID, &dev.Name,
			&dev.Hostname, &dev.Platform, &dev.DeviceType, &dev.GroupID,
			&dev.AgentVersion, &dev.LastSeenAt, &dev.Online, &dev.PairedAt,
			&dev.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, &dev)
	}
	return out, rows.Err()
}

func RenameDevice(ctx context.Context, d *sql.DB, userID int64, id, name string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE devices SET name = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		name, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("device not found")
	}
	return nil
}

func RevokeDevice(ctx context.Context, d *sql.DB, userID int64, id string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE devices SET revoked_at = CURRENT_TIMESTAMP, online = 0 WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("device not found")
	}
	return nil
}

func MarkDeviceOnline(ctx context.Context, d *sql.DB, id string, online bool, agentVersion string) error {
	_, err := d.ExecContext(ctx, `
		UPDATE devices SET online = ?, last_seen_at = CURRENT_TIMESTAMP, agent_version = COALESCE(NULLIF(?, ''), agent_version)
		WHERE id = ?
	`, online, agentVersion, id)
	return err
}

// MarkAllOffline сбрасывает online=0 при старте relay — все агенты переподключатся.
func MarkAllOffline(ctx context.Context, d *sql.DB) error {
	_, err := d.ExecContext(ctx, `UPDATE devices SET online = 0`)
	return err
}
