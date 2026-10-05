package db

import (
	"context"
	"database/sql"
	"errors"
)

// IsAnonTelegramID — анонимные аккаунты получают ОТРИЦАТЕЛЬНЫЙ синтетический
// telegram_id (см. CreateAnonUser); реальные Telegram-id всегда положительны.
func IsAnonTelegramID(tgID int64) bool { return tgID < 0 }

// MergeAccounts сливает анонимный аккаунт fromUserID в постоянный toUserID:
// переносит устройства и историю пейринга, затем удаляет анонимный аккаунт.
// Вызывается при первом входе через любой провайдер (Telegram, email, OAuth),
// когда у клиента уже был анонимный аккаунт с привязанными ПК.
//
// Безопасность: разрешено только from=аноним → to=постоянный. Постоянный это
// аккаунт с реальным telegram_id или хотя бы одной записью в user_identities.
// devices.id — глобальный PRIMARY KEY (одна строка на device_id), поэтому
// коллизий при переносе нет.
func MergeAccounts(ctx context.Context, d *sql.DB, fromUserID, toUserID int64) error {
	if fromUserID == toUserID {
		return nil
	}
	from, err := GetUserByID(ctx, d, fromUserID)
	if err != nil {
		return err
	}
	if !IsAnonTelegramID(from.TelegramID) {
		return errors.New("refuse to merge: source is not an anonymous account")
	}
	to, err := GetUserByID(ctx, d, toUserID)
	if err != nil {
		return err
	}
	if IsAnonTelegramID(to.TelegramID) {
		// Аккаунт, созданный email/OAuth-входом, тоже имеет synthetic tg id;
		// постоянным его делает наличие identity.
		var n int
		if err := d.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM user_identities WHERE user_id = ?`, toUserID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return errors.New("refuse to merge: target is not a permanent account")
		}
	}
	fromPersonal := personalWorkspaceID(fromUserID)
	toPersonal, err := EnsurePersonalWorkspace(ctx, d, toUserID)
	if err != nil {
		return err
	}
	if _, err := EnsurePersonalWorkspace(ctx, d, fromUserID); err != nil {
		return err
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Личный workspace анонима схлопывается в личный workspace постоянного
	// аккаунта. Тип/теги не должны удерживать ссылку на удаляемое пространство:
	// группы сбрасываем, workspace-scoped теги удаляем.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM device_tags
		WHERE device_id IN (SELECT id FROM devices WHERE workspace_id = ?)
	`, fromPersonal); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE devices SET workspace_id = ?, group_id = NULL
		WHERE workspace_id = ?
	`, toPersonal.ID, fromPersonal); err != nil {
		return err
	}
	// Переносим устройства анонима на постоянный аккаунт. user_id остаётся
	// агентским principal и обязан совпасть с device-JWT после нового пейринга.
	if _, err := tx.ExecContext(ctx,
		`UPDATE devices SET user_id = ? WHERE user_id = ?`, toUserID, fromUserID); err != nil {
		return err
	}
	// Компаниями и членствами анонимного аккаунта теперь владеет постоянный.
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspaces SET owner_user_id = ?
		WHERE owner_user_id = ? AND id != ?
	`, toUserID, fromUserID, fromPersonal); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_members (workspace_id, user_id, role, added_by, joined_at)
		SELECT workspace_id, ?, role, COALESCE(added_by, ?), joined_at
		FROM workspace_members
		WHERE user_id = ? AND workspace_id != ?
		ON CONFLICT(workspace_id, user_id) DO UPDATE SET
			role = CASE
				WHEN excluded.role = 'owner' THEN 'owner'
				WHEN workspace_members.role = 'owner' THEN 'owner'
				WHEN excluded.role = 'admin' THEN 'admin'
				WHEN workspace_members.role = 'admin' THEN 'admin'
				WHEN excluded.role = 'operator' THEN 'operator'
				ELSE workspace_members.role
			END
	`, toUserID, toUserID, fromUserID, fromPersonal); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_members WHERE user_id = ?`, fromUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, fromPersonal); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO device_favorites (user_id, device_id, created_at)
		SELECT ?, device_id, created_at FROM device_favorites WHERE user_id = ?
	`, toUserID, fromUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_favorites WHERE user_id = ?`, fromUserID); err != nil {
		return err
	}
	// Legacy point grants are account access too. Preserve them when a guest
	// connects Telegram/email instead of silently hiding shared machines.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO device_grants (device_id, user_id, granted_at, revoked_at)
		SELECT device_id, ?, granted_at, revoked_at
		FROM device_grants WHERE user_id = ?
		ON CONFLICT(device_id, user_id) DO UPDATE SET
			granted_at = CASE
				WHEN excluded.granted_at > device_grants.granted_at THEN excluded.granted_at
				ELSE device_grants.granted_at
			END,
			revoked_at = CASE
				WHEN excluded.revoked_at IS NULL OR device_grants.revoked_at IS NULL THEN NULL
				ELSE device_grants.revoked_at
			END
	`, toUserID, fromUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_grants WHERE user_id = ?`, fromUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_invites SET created_by = ? WHERE created_by = ?`, toUserID, fromUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_members SET added_by = ? WHERE added_by = ?`, toUserID, fromUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_audit SET actor_user_id = ? WHERE actor_user_id = ?`, toUserID, fromUserID); err != nil {
		return err
	}
	// Переносим историю пейринга (FK pairing_codes.consumed_by_uid → users.id),
	// иначе DELETE пользователя нарушит foreign_keys=on.
	if _, err := tx.ExecContext(ctx,
		`UPDATE pairing_codes SET consumed_by_uid = ? WHERE consumed_by_uid = ?`, toUserID, fromUserID); err != nil {
		return err
	}
	// The desktop can be online or offline during the merge and may still hold
	// a valid device JWT with fromUserID. This exact mapping lets the relay
	// accept that JWT only while the device still belongs to toUserID, then
	// rotate it on connect. A later real takeover does not match the mapping.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_merge_aliases (old_user_id, new_user_id, merged_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(old_user_id) DO UPDATE SET
			new_user_id = excluded.new_user_id,
			merged_at = CURRENT_TIMESTAMP
	`, fromUserID, toUserID); err != nil {
		return err
	}
	// Удаляем опустевший анонимный аккаунт.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM users WHERE id = ?`, fromUserID); err != nil {
		return err
	}
	return tx.Commit()
}

// MergedUserTarget resolves a deleted anonymous principal to the permanent
// account it was merged into. It is deliberately one-way and only used in
// conjunction with the device's current owner check.
func MergedUserTarget(ctx context.Context, d *sql.DB, oldUserID int64) (int64, error) {
	var target int64
	err := d.QueryRowContext(ctx, `
		SELECT new_user_id FROM user_merge_aliases WHERE old_user_id = ?
	`, oldUserID).Scan(&target)
	return target, err
}
