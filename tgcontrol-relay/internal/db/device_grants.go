package db

import (
	"context"
	"database/sql"
	"errors"
)

// BindDeviceForPairing привязывает пользователя userID к устройству по уже
// проверенному (валидному, не просроченному) коду. Единая точка для всех путей
// пейринга (Mini App / APK / бот). Решение:
//   - устройства нет           → создаём, userID = ВЛАДЕЛЕЦ            (primary)
//   - владелец == userID       → re-pair: обновляем метаданные, снимаем revoke (primary)
//   - устройство отозвано       → takeover: новый владелец, гранты сброшены (primary)
//   - чужой ЖИВОЙ ПК           → ГРАНТ доступа (владелец не меняется)  (НЕ primary)
//
// maxDevices — лимит ВЛАДЕЕМЫХ устройств тарифа; проверяется только когда
// заводится новый владельческий слот (create/takeover). Грант и re-pail лимит не
// трогают. primary=true означает, что ПК должен получить device-JWT (звонящий
// выдаёт его и консьюмит код для статус-поллинга десктопа).
func BindDeviceForPairing(ctx context.Context, d *sql.DB, dev *Device, userID int64, maxDevices int) (primary, overLimit bool, err error) {
	existing, gerr := GetDevice(ctx, d, dev.ID)
	notFound := errors.Is(gerr, sql.ErrNoRows)
	if gerr != nil && !notFound {
		return false, false, gerr
	}
	if !notFound && existing.UserID != userID && !existing.RevokedAt.Valid {
		return false, false, GrantDeviceAccess(ctx, d, dev.ID, userID) // чужой живой → грант
	}
	if !notFound && existing.UserID == userID {
		// Pairing establishes account access; moving/reclassifying an existing
		// machine is a separate explicit infrastructure action. Always preserve
		// company/group/type here so re-entering a code cannot silently move it.
		dev.WorkspaceID = existing.WorkspaceID
		dev.DeviceType = existing.DeviceType
		dev.GroupID = existing.GroupID
		return true, false, ReassignDevice(ctx, d, dev) // свой → re-pair
	}
	// No explicit destination means «Личное» for a new/taken-over device. This
	// happens after the owned-device preservation branch above, so a generic
	// Telegram re-pair never drags an existing company device out of its space.
	if dev.WorkspaceID == "" {
		workspace, werr := EnsurePersonalWorkspace(ctx, d, userID)
		if werr != nil {
			return false, false, werr
		}
		dev.WorkspaceID = workspace.ID
	}
	// Новый владельческий слот (create | takeover отозванного) → лимит.
	cnt, cerr := CountDevices(ctx, d, userID)
	if dev.WorkspaceID != "" {
		cnt, cerr = CountWorkspaceDevices(ctx, d, dev.WorkspaceID)
	}
	if cerr != nil {
		return false, false, cerr
	}
	if maxDevices > 0 && cnt >= maxDevices {
		return false, true, nil
	}
	if notFound {
		if ierr := InsertDevice(ctx, d, dev); ierr != nil {
			// гонка: создано параллельно — перечитаем и привяжем как существующее
			ex2, e2 := GetDevice(ctx, d, dev.ID)
			if e2 != nil {
				return false, false, e2
			}
			if ex2.UserID == userID || ex2.RevokedAt.Valid {
				return true, false, ReassignDevice(ctx, d, dev)
			}
			return false, false, GrantDeviceAccess(ctx, d, dev.ID, userID)
		}
		return true, false, nil
	}
	if rerr := ReassignDevice(ctx, d, dev); rerr != nil { // takeover отозванного
		return false, false, rerr
	}
	return true, false, ClearDeviceGrants(ctx, d, dev.ID)
}

// GrantDeviceAccess допускает пользователя к управлению устройством (помимо
// владельца). Идемпотентно: повторный грант снимает прежний revoked_at и
// обновляет granted_at. Владельцу грант не нужен (у него доступ по user_id).
func GrantDeviceAccess(ctx context.Context, d *sql.DB, deviceID string, userID int64) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO device_grants (device_id, user_id) VALUES (?, ?)
		ON CONFLICT(device_id, user_id) DO UPDATE SET revoked_at = NULL, granted_at = CURRENT_TIMESTAMP
	`, deviceID, userID)
	return err
}

// UserCanAccessDevice — true, если пользователь владелец устройства, участник
// его workspace ИЛИ имеет старый точечный грант, и устройство не отозвано.
func UserCanAccessDevice(ctx context.Context, d *sql.DB, deviceID string, userID int64) (bool, error) {
	var one int
	err := d.QueryRowContext(ctx, `
		SELECT 1 FROM devices
		WHERE id = ? AND revoked_at IS NULL AND (
			user_id = ?
			OR EXISTS (
				SELECT 1 FROM workspace_members wm
				WHERE wm.workspace_id = devices.workspace_id AND wm.user_id = ?
			)
			OR EXISTS (
				SELECT 1 FROM device_grants g
				WHERE g.device_id = ? AND g.user_id = ? AND g.revoked_at IS NULL
			)
		)
	`, deviceID, userID, userID, deviceID, userID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListDevicesForUser возвращает устройства, которыми пользователь ВЛАДЕЕТ или к
// которым ДОПУЩЕН грантом (без отозванных, без дублей). Заменяет
// ListDevicesByUser в клиентском списке устройств.
func ListDevicesForUser(ctx context.Context, d *sql.DB, userID int64) ([]*Device, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT id, user_id, workspace_id, name, hostname, platform, device_type,
		       group_id, agent_version, last_seen_at, online, paired_at, revoked_at
		FROM (
			SELECT dv.id, dv.user_id,
			       COALESCE(dv.workspace_id, 'personal-' || dv.user_id) workspace_id,
			       dv.name, COALESCE(dv.hostname,'') hostname,
			       COALESCE(dv.platform,'') platform,
			       COALESCE(dv.device_type,'computer') device_type,
			       COALESCE(dv.group_id,'') group_id,
			       COALESCE(dv.agent_version,'') agent_version,
			       dv.last_seen_at, dv.online, dv.paired_at, dv.revoked_at
			FROM devices dv
			WHERE dv.revoked_at IS NULL AND (
				dv.user_id = ?
				OR EXISTS (
					SELECT 1 FROM workspace_members wm
					WHERE wm.workspace_id = dv.workspace_id AND wm.user_id = ?
				)
				OR EXISTS (
					SELECT 1 FROM device_grants g
					WHERE g.device_id = dv.id AND g.user_id = ? AND g.revoked_at IS NULL
				)
			)
		)
		ORDER BY paired_at DESC
	`, userID, userID, userID)
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

// ClearDeviceGrants удаляет все гранты устройства — при СМЕНЕ владельца
// (takeover отозванного ПК): прежние допущенные не должны сохранять доступ.
func ClearDeviceGrants(ctx context.Context, d *sql.DB, deviceID string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM device_grants WHERE device_id = ?`, deviceID)
	return err
}

// RevokeDeviceGrants помечает все гранты устройства отозванными — при revoke
// устройства владельцем/самим ПК (все допущенные теряют доступ вместе с ПК).
func RevokeDeviceGrants(ctx context.Context, d *sql.DB, deviceID string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE device_grants SET revoked_at = CURRENT_TIMESTAMP WHERE device_id = ? AND revoked_at IS NULL`,
		deviceID)
	return err
}
