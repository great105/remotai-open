package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	WorkspacePersonal = "personal"
	WorkspaceCompany  = "company"

	RoleOwner    = "owner"
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"

	DeviceComputer = "computer"
	DeviceServer   = "server"
)

type Workspace struct {
	ID          string
	Kind        string
	Name        string
	OwnerUserID int64
	Role        string
	MemberCount int
	DeviceCount int
	OnlineCount int
	CreatedAt   time.Time
}

type WorkspaceMember struct {
	UserID       int64
	Role         string
	Username     string
	FirstName    string
	LoginDisplay string
	JoinedAt     time.Time
}

type WorkspaceInvite struct {
	Code        string
	WorkspaceID string
	Role        string
	CreatedBy   int64
	CreatedAt   time.Time
	ExpiresAt   time.Time
	MaxUses     int
	UsedCount   int
}

type DeviceGroup struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	SortOrder   int    `json:"sort_order"`
}

type WorkspaceTag struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
}

type AuditEvent struct {
	ID           int64
	WorkspaceID  string
	ActorUserID  sql.NullInt64
	ActorDisplay string
	Action       string
	TargetType   string
	TargetID     string
	MetadataJSON string
	CreatedAt    time.Time
}

// SuggestedDeviceType is a pairing-time suggestion only. Linux is commonly
// installed headlessly; users can override the value for Linux desktops and
// Windows servers from the infrastructure screen.
func SuggestedDeviceType(platform string) string {
	if strings.EqualFold(strings.TrimSpace(platform), "linux") {
		return DeviceServer
	}
	return DeviceComputer
}

func ValidDeviceType(value string) bool {
	return value == DeviceComputer || value == DeviceServer
}

func ValidWorkspaceRole(value string) bool {
	switch value {
	case RoleOwner, RoleAdmin, RoleOperator, RoleViewer:
		return true
	default:
		return false
	}
}

func RoleRank(role string) int {
	switch role {
	case RoleOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

func RoleAtLeast(role, required string) bool {
	return RoleRank(role) >= RoleRank(required)
}

func personalWorkspaceID(userID int64) string {
	return fmt.Sprintf("personal-%d", userID)
}

// EnsurePersonalWorkspace is intentionally safe to call on every account
// request. It repairs partially migrated databases and creates the personal
// workspace for users registered after the infrastructure migration.
func EnsurePersonalWorkspace(ctx context.Context, d *sql.DB, userID int64) (*Workspace, error) {
	id := personalWorkspaceID(userID)
	if _, err := d.ExecContext(ctx, `
		INSERT OR IGNORE INTO workspaces (id, kind, name, owner_user_id)
		VALUES (?, 'personal', 'Личное', ?)
	`, id, userID); err != nil {
		return nil, err
	}
	if _, err := d.ExecContext(ctx, `
		INSERT INTO workspace_members (workspace_id, user_id, role, added_by)
		VALUES (?, ?, 'owner', ?)
		ON CONFLICT(workspace_id, user_id) DO UPDATE SET role = 'owner'
	`, id, userID, userID); err != nil {
		return nil, err
	}
	return GetWorkspaceForUser(ctx, d, id, userID)
}

func GetWorkspaceForUser(ctx context.Context, d *sql.DB, workspaceID string, userID int64) (*Workspace, error) {
	var w Workspace
	err := d.QueryRowContext(ctx, `
		SELECT w.id, w.kind, w.name, w.owner_user_id, wm.role, w.created_at,
		       (SELECT COUNT(*) FROM workspace_members m WHERE m.workspace_id = w.id),
		       (SELECT COUNT(*) FROM devices dv WHERE dv.workspace_id = w.id AND dv.revoked_at IS NULL),
		       (SELECT COUNT(*) FROM devices dv WHERE dv.workspace_id = w.id AND dv.revoked_at IS NULL AND dv.online = 1)
		FROM workspaces w
		JOIN workspace_members wm ON wm.workspace_id = w.id AND wm.user_id = ?
		WHERE w.id = ? AND w.archived_at IS NULL
	`, userID, workspaceID).Scan(
		&w.ID, &w.Kind, &w.Name, &w.OwnerUserID, &w.Role, &w.CreatedAt,
		&w.MemberCount, &w.DeviceCount, &w.OnlineCount,
	)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func ListWorkspacesForUser(ctx context.Context, d *sql.DB, userID int64) ([]Workspace, error) {
	if _, err := EnsurePersonalWorkspace(ctx, d, userID); err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `
		SELECT w.id, w.kind, w.name, w.owner_user_id, wm.role, w.created_at,
		       (SELECT COUNT(*) FROM workspace_members m WHERE m.workspace_id = w.id),
		       (SELECT COUNT(*) FROM devices dv WHERE dv.workspace_id = w.id AND dv.revoked_at IS NULL),
		       (SELECT COUNT(*) FROM devices dv WHERE dv.workspace_id = w.id AND dv.revoked_at IS NULL AND dv.online = 1)
		FROM workspaces w
		JOIN workspace_members wm ON wm.workspace_id = w.id
		WHERE wm.user_id = ? AND w.archived_at IS NULL
		ORDER BY CASE w.kind WHEN 'personal' THEN 0 ELSE 1 END, lower(w.name), w.created_at
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(
			&w.ID, &w.Kind, &w.Name, &w.OwnerUserID, &w.Role, &w.CreatedAt,
			&w.MemberCount, &w.DeviceCount, &w.OnlineCount,
		); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func CreateCompanyWorkspace(ctx context.Context, d *sql.DB, ownerUserID int64, name string) (*Workspace, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("workspace name required")
	}
	id := "ws-" + uuid.NewString()
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces (id, kind, name, owner_user_id)
		VALUES (?, 'company', ?, ?)
	`, id, name, ownerUserID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_members (workspace_id, user_id, role, added_by)
		VALUES (?, ?, 'owner', ?)
	`, id, ownerUserID, ownerUserID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	_ = RecordAudit(ctx, d, id, ownerUserID, "workspace.created", "workspace", id, map[string]any{"name": name})
	return GetWorkspaceForUser(ctx, d, id, ownerUserID)
}

func RenameWorkspace(ctx context.Context, d *sql.DB, workspaceID string, actorUserID int64, name string) error {
	role, err := WorkspaceRole(ctx, d, workspaceID, actorUserID)
	if err != nil || !RoleAtLeast(role, RoleAdmin) {
		return errors.New("workspace admin required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("workspace name required")
	}
	if _, err := d.ExecContext(ctx, `
		UPDATE workspaces SET name = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND archived_at IS NULL
	`, name, workspaceID); err != nil {
		return err
	}
	return RecordAudit(ctx, d, workspaceID, actorUserID, "workspace.renamed", "workspace", workspaceID, map[string]any{"name": name})
}

func ArchiveWorkspace(ctx context.Context, d *sql.DB, workspaceID string, actorUserID int64) error {
	w, err := GetWorkspaceForUser(ctx, d, workspaceID, actorUserID)
	if err != nil {
		return err
	}
	if w.Kind == WorkspacePersonal {
		return errors.New("personal workspace cannot be removed")
	}
	if w.Role != RoleOwner {
		return errors.New("workspace owner required")
	}
	if w.DeviceCount > 0 {
		return errors.New("move or remove workspace devices first")
	}
	_, err = d.ExecContext(ctx, `
		UPDATE workspaces SET archived_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND archived_at IS NULL
	`, workspaceID)
	return err
}

func WorkspaceRole(ctx context.Context, d *sql.DB, workspaceID string, userID int64) (string, error) {
	var role string
	err := d.QueryRowContext(ctx, `
		SELECT wm.role
		FROM workspace_members wm
		JOIN workspaces w ON w.id = wm.workspace_id
		WHERE wm.workspace_id = ? AND wm.user_id = ? AND w.archived_at IS NULL
	`, workspaceID, userID).Scan(&role)
	return role, err
}

func DeviceRole(ctx context.Context, d *sql.DB, deviceID string, userID int64) (string, error) {
	var role string
	err := d.QueryRowContext(ctx, `
		SELECT wm.role
		FROM devices dv
		JOIN workspace_members wm ON wm.workspace_id = dv.workspace_id AND wm.user_id = ?
		WHERE dv.id = ? AND dv.revoked_at IS NULL
	`, userID, deviceID).Scan(&role)
	if err == nil {
		return role, nil
	}
	// Legacy per-device grant behaves like operator access.
	var one int
	if gerr := d.QueryRowContext(ctx, `
		SELECT 1 FROM device_grants
		WHERE device_id = ? AND user_id = ? AND revoked_at IS NULL
	`, deviceID, userID).Scan(&one); gerr == nil {
		return RoleOperator, nil
	}
	return "", err
}

func CountWorkspaceDevices(ctx context.Context, d *sql.DB, workspaceID string) (int, error) {
	var count int
	err := d.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM devices WHERE workspace_id = ? AND revoked_at IS NULL
	`, workspaceID).Scan(&count)
	return count, err
}

func GroupForDevice(ctx context.Context, d *sql.DB, groupID string) (*DeviceGroup, error) {
	if strings.TrimSpace(groupID) == "" {
		return nil, sql.ErrNoRows
	}
	var group DeviceGroup
	err := d.QueryRowContext(ctx, `
		SELECT id, workspace_id, name, sort_order FROM device_groups WHERE id = ?
	`, groupID).Scan(&group.ID, &group.WorkspaceID, &group.Name, &group.SortOrder)
	if err != nil {
		return nil, err
	}
	return &group, nil
}

func ListWorkspaceMembers(ctx context.Context, d *sql.DB, workspaceID string, actorUserID int64) ([]WorkspaceMember, error) {
	role, err := WorkspaceRole(ctx, d, workspaceID, actorUserID)
	if err != nil || !RoleAtLeast(role, RoleViewer) {
		return nil, errors.New("workspace access required")
	}
	rows, err := d.QueryContext(ctx, `
		SELECT u.id, wm.role, COALESCE(u.username,''), COALESCE(u.first_name,''),
		       CASE WHEN u.username IS NOT NULL AND u.username != '' THEN '@' || u.username
		            WHEN u.first_name IS NOT NULL THEN u.first_name ELSE 'Пользователь' END,
		       wm.joined_at
		FROM workspace_members wm
		JOIN users u ON u.id = wm.user_id
		WHERE wm.workspace_id = ?
		ORDER BY CASE wm.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 WHEN 'operator' THEN 2 ELSE 3 END,
		         lower(COALESCE(u.username, u.first_name, ''))
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceMember
	for rows.Next() {
		var m WorkspaceMember
		if err := rows.Scan(&m.UserID, &m.Role, &m.Username, &m.FirstName, &m.LoginDisplay, &m.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func CreateWorkspaceInvite(ctx context.Context, d *sql.DB, workspaceID string, actorUserID int64, role string, ttl time.Duration) (*WorkspaceInvite, error) {
	actorRole, err := WorkspaceRole(ctx, d, workspaceID, actorUserID)
	if err != nil || !RoleAtLeast(actorRole, RoleAdmin) {
		return nil, errors.New("workspace admin required")
	}
	if role != RoleAdmin && role != RoleOperator && role != RoleViewer {
		return nil, errors.New("invalid invite role")
	}
	if role == RoleAdmin && actorRole != RoleOwner {
		return nil, errors.New("only owner can invite admins")
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	var invite WorkspaceInvite
	for attempt := 0; attempt < 8; attempt++ {
		code, gerr := GenerateCode()
		if gerr != nil {
			return nil, gerr
		}
		invite = WorkspaceInvite{
			Code: code, WorkspaceID: workspaceID, Role: role, CreatedBy: actorUserID,
			CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(ttl), MaxUses: 1,
		}
		_, err = d.ExecContext(ctx, `
			INSERT INTO workspace_invites (code, workspace_id, role, created_by, created_at, expires_at, max_uses)
			VALUES (?, ?, ?, ?, ?, ?, 1)
		`, invite.Code, workspaceID, role, actorUserID, invite.CreatedAt, invite.ExpiresAt)
		if err == nil {
			_ = RecordAudit(ctx, d, workspaceID, actorUserID, "member.invited", "invite", invite.Code, map[string]any{"role": role})
			return &invite, nil
		}
		if !strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, err
		}
	}
	return nil, errors.New("could not allocate invite code")
}

func AcceptWorkspaceInvite(ctx context.Context, d *sql.DB, code string, userID int64) (*Workspace, error) {
	code = NormalizeCode(code)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var workspaceID, role string
	var expires time.Time
	var maxUses, used int
	var revoked sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT workspace_id, role, expires_at, max_uses, used_count, revoked_at
		FROM workspace_invites WHERE code = ?
	`, code).Scan(&workspaceID, &role, &expires, &maxUses, &used, &revoked); err != nil {
		return nil, err
	}
	if revoked.Valid || time.Now().UTC().After(expires) || used >= maxUses {
		return nil, errors.New("invite expired or already used")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_members (workspace_id, user_id, role, added_by)
		SELECT workspace_id, ?, role, created_by FROM workspace_invites WHERE code = ?
		ON CONFLICT(workspace_id, user_id) DO UPDATE SET role = excluded.role
	`, userID, code); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspace_invites SET used_count = used_count + 1 WHERE code = ?
	`, code); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	_ = RecordAudit(ctx, d, workspaceID, userID, "member.joined", "user", fmt.Sprint(userID), map[string]any{"role": role})
	return GetWorkspaceForUser(ctx, d, workspaceID, userID)
}

func UpdateWorkspaceMemberRole(ctx context.Context, d *sql.DB, workspaceID string, actorUserID, memberUserID int64, role string) error {
	actorRole, err := WorkspaceRole(ctx, d, workspaceID, actorUserID)
	if err != nil || actorRole != RoleOwner {
		return errors.New("workspace owner required")
	}
	if role != RoleAdmin && role != RoleOperator && role != RoleViewer {
		return errors.New("invalid member role")
	}
	var ownerID int64
	if err := d.QueryRowContext(ctx, `SELECT owner_user_id FROM workspaces WHERE id = ?`, workspaceID).Scan(&ownerID); err != nil {
		return err
	}
	if memberUserID == ownerID {
		return errors.New("owner role cannot be changed")
	}
	res, err := d.ExecContext(ctx, `
		UPDATE workspace_members SET role = ? WHERE workspace_id = ? AND user_id = ?
	`, role, workspaceID, memberUserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("member not found")
	}
	return RecordAudit(ctx, d, workspaceID, actorUserID, "member.role_changed", "user", fmt.Sprint(memberUserID), map[string]any{"role": role})
}

func RemoveWorkspaceMember(ctx context.Context, d *sql.DB, workspaceID string, actorUserID, memberUserID int64) error {
	actorRole, err := WorkspaceRole(ctx, d, workspaceID, actorUserID)
	if err != nil {
		return err
	}
	if actorUserID != memberUserID && !RoleAtLeast(actorRole, RoleAdmin) {
		return errors.New("workspace admin required")
	}
	var ownerID int64
	if err := d.QueryRowContext(ctx, `SELECT owner_user_id FROM workspaces WHERE id = ?`, workspaceID).Scan(&ownerID); err != nil {
		return err
	}
	if memberUserID == ownerID {
		return errors.New("workspace owner cannot leave")
	}
	res, err := d.ExecContext(ctx, `DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, workspaceID, memberUserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("member not found")
	}
	return RecordAudit(ctx, d, workspaceID, actorUserID, "member.removed", "user", fmt.Sprint(memberUserID), nil)
}

func ListGroups(ctx context.Context, d *sql.DB, workspaceID string, userID int64) ([]DeviceGroup, error) {
	if _, err := WorkspaceRole(ctx, d, workspaceID, userID); err != nil {
		return nil, errors.New("workspace access required")
	}
	rows, err := d.QueryContext(ctx, `
		SELECT id, workspace_id, name, sort_order
		FROM device_groups WHERE workspace_id = ?
		ORDER BY sort_order, lower(name)
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DeviceGroup, 0)
	for rows.Next() {
		var group DeviceGroup
		if err := rows.Scan(&group.ID, &group.WorkspaceID, &group.Name, &group.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, group)
	}
	return out, rows.Err()
}

func CreateGroup(ctx context.Context, d *sql.DB, workspaceID string, actorUserID int64, name string) (*DeviceGroup, error) {
	role, err := WorkspaceRole(ctx, d, workspaceID, actorUserID)
	if err != nil || !RoleAtLeast(role, RoleAdmin) {
		return nil, errors.New("workspace admin required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("group name required")
	}
	group := &DeviceGroup{ID: "grp-" + uuid.NewString(), WorkspaceID: workspaceID, Name: name}
	if _, err := d.ExecContext(ctx, `
		INSERT INTO device_groups (id, workspace_id, name, sort_order) VALUES (?, ?, ?, 0)
	`, group.ID, workspaceID, name); err != nil {
		return nil, err
	}
	_ = RecordAudit(ctx, d, workspaceID, actorUserID, "group.created", "group", group.ID, map[string]any{"name": name})
	return group, nil
}

// RenameGroup keeps the historical device_groups storage compatible while the
// product exposes groups as user-facing infrastructure zones.
func RenameGroup(ctx context.Context, d *sql.DB, workspaceID, groupID string, actorUserID int64, name string) error {
	role, err := WorkspaceRole(ctx, d, workspaceID, actorUserID)
	if err != nil || !RoleAtLeast(role, RoleAdmin) {
		return errors.New("workspace admin required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("zone name required")
	}
	res, err := d.ExecContext(ctx, `
		UPDATE device_groups SET name = ?
		WHERE id = ? AND workspace_id = ?
	`, name, groupID, workspaceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("zone not found")
	}
	return RecordAudit(ctx, d, workspaceID, actorUserID, "group.renamed", "group", groupID, map[string]any{"name": name})
}

func DeleteGroup(ctx context.Context, d *sql.DB, workspaceID, groupID string, actorUserID int64) error {
	role, err := WorkspaceRole(ctx, d, workspaceID, actorUserID)
	if err != nil || !RoleAtLeast(role, RoleAdmin) {
		return errors.New("workspace admin required")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE devices SET group_id = NULL WHERE workspace_id = ? AND group_id = ?`, workspaceID, groupID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM device_groups WHERE id = ? AND workspace_id = ?`, groupID, workspaceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("group not found")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return RecordAudit(ctx, d, workspaceID, actorUserID, "group.deleted", "group", groupID, nil)
}

func ListTags(ctx context.Context, d *sql.DB, workspaceID string, userID int64) ([]WorkspaceTag, error) {
	if _, err := WorkspaceRole(ctx, d, workspaceID, userID); err != nil {
		return nil, errors.New("workspace access required")
	}
	rows, err := d.QueryContext(ctx, `
		SELECT id, workspace_id, name, color
		FROM workspace_tags WHERE workspace_id = ? ORDER BY lower(name)
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WorkspaceTag, 0)
	for rows.Next() {
		var tag WorkspaceTag
		if err := rows.Scan(&tag.ID, &tag.WorkspaceID, &tag.Name, &tag.Color); err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}

func CreateTag(ctx context.Context, d *sql.DB, workspaceID string, actorUserID int64, name, color string) (*WorkspaceTag, error) {
	role, err := WorkspaceRole(ctx, d, workspaceID, actorUserID)
	if err != nil || !RoleAtLeast(role, RoleAdmin) {
		return nil, errors.New("workspace admin required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("tag name required")
	}
	color = strings.TrimSpace(color)
	if len(color) != 7 || color[0] != '#' {
		color = "#2ee6b0"
	}
	tag := &WorkspaceTag{ID: "tag-" + uuid.NewString(), WorkspaceID: workspaceID, Name: name, Color: color}
	if _, err := d.ExecContext(ctx, `
		INSERT INTO workspace_tags (id, workspace_id, name, color) VALUES (?, ?, ?, ?)
	`, tag.ID, workspaceID, name, color); err != nil {
		return nil, err
	}
	_ = RecordAudit(ctx, d, workspaceID, actorUserID, "tag.created", "tag", tag.ID, map[string]any{"name": name, "color": color})
	return tag, nil
}

func DeleteTag(ctx context.Context, d *sql.DB, workspaceID, tagID string, actorUserID int64) error {
	role, err := WorkspaceRole(ctx, d, workspaceID, actorUserID)
	if err != nil || !RoleAtLeast(role, RoleAdmin) {
		return errors.New("workspace admin required")
	}
	res, err := d.ExecContext(ctx, `DELETE FROM workspace_tags WHERE id = ? AND workspace_id = ?`, tagID, workspaceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("tag not found")
	}
	return RecordAudit(ctx, d, workspaceID, actorUserID, "tag.deleted", "tag", tagID, nil)
}

func TagsForDevice(ctx context.Context, d *sql.DB, deviceID string) ([]WorkspaceTag, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT t.id, t.workspace_id, t.name, t.color
		FROM workspace_tags t
		JOIN device_tags dt ON dt.tag_id = t.id
		WHERE dt.device_id = ?
		ORDER BY lower(t.name)
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceTag
	for rows.Next() {
		var tag WorkspaceTag
		if err := rows.Scan(&tag.ID, &tag.WorkspaceID, &tag.Name, &tag.Color); err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}

func ReplaceDeviceTags(ctx context.Context, d *sql.DB, deviceID string, actorUserID int64, tagIDs []string) error {
	dev, err := GetDevice(ctx, d, deviceID)
	if err != nil {
		return err
	}
	role, err := WorkspaceRole(ctx, d, dev.WorkspaceID, actorUserID)
	if err != nil || !RoleAtLeast(role, RoleOperator) {
		return errors.New("workspace operator required")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_tags WHERE device_id = ?`, deviceID); err != nil {
		return err
	}
	for _, tagID := range tagIDs {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO device_tags (device_id, tag_id)
			SELECT ?, id FROM workspace_tags WHERE id = ? AND workspace_id = ?
		`, deviceID, tagID, dev.WorkspaceID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("tag not found")
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return RecordAudit(ctx, d, dev.WorkspaceID, actorUserID, "device.tags_changed", "device", deviceID, map[string]any{"tag_ids": tagIDs})
}

func IsDeviceFavorite(ctx context.Context, d *sql.DB, deviceID string, userID int64) (bool, error) {
	var count int
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_favorites WHERE user_id = ? AND device_id = ?`, userID, deviceID).Scan(&count)
	return count > 0, err
}

func SetDeviceFavorite(ctx context.Context, d *sql.DB, deviceID string, userID int64, favorite bool) error {
	if _, err := DeviceRole(ctx, d, deviceID, userID); err != nil {
		return errors.New("device access required")
	}
	if favorite {
		_, err := d.ExecContext(ctx, `
			INSERT OR IGNORE INTO device_favorites (user_id, device_id) VALUES (?, ?)
		`, userID, deviceID)
		return err
	}
	_, err := d.ExecContext(ctx, `DELETE FROM device_favorites WHERE user_id = ? AND device_id = ?`, userID, deviceID)
	return err
}

func UpdateDeviceInfrastructure(ctx context.Context, d *sql.DB, deviceID string, actorUserID int64, name, deviceType, targetWorkspaceID, groupID *string) error {
	dev, err := GetDevice(ctx, d, deviceID)
	if err != nil {
		return err
	}
	currentRole, err := WorkspaceRole(ctx, d, dev.WorkspaceID, actorUserID)
	if err != nil || !RoleAtLeast(currentRole, RoleAdmin) {
		return errors.New("workspace admin required")
	}
	nextWorkspace := dev.WorkspaceID
	if targetWorkspaceID != nil && strings.TrimSpace(*targetWorkspaceID) != "" {
		nextWorkspace = strings.TrimSpace(*targetWorkspaceID)
		targetRole, err := WorkspaceRole(ctx, d, nextWorkspace, actorUserID)
		if err != nil || !RoleAtLeast(targetRole, RoleAdmin) {
			return errors.New("target workspace admin required")
		}
	}
	nextName := dev.Name
	if name != nil {
		nextName = strings.TrimSpace(*name)
		if nextName == "" {
			return errors.New("device name required")
		}
	}
	nextType := dev.DeviceType
	if deviceType != nil {
		nextType = strings.TrimSpace(*deviceType)
		if !ValidDeviceType(nextType) {
			return errors.New("invalid device type")
		}
	}
	nextGroup := dev.GroupID
	if groupID != nil {
		nextGroup = strings.TrimSpace(*groupID)
	}
	if nextGroup != "" {
		var groupWorkspace string
		if err := d.QueryRowContext(ctx, `SELECT workspace_id FROM device_groups WHERE id = ?`, nextGroup).Scan(&groupWorkspace); err != nil {
			return errors.New("group not found")
		}
		if groupWorkspace != nextWorkspace {
			return errors.New("group belongs to another workspace")
		}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if nextWorkspace != dev.WorkspaceID {
		// Tags are workspace-scoped and cannot cross the boundary. Delete them
		// in the same transaction as the move so a failed update never loses
		// the device's existing classification.
		if _, err := tx.ExecContext(ctx, `DELETE FROM device_tags WHERE device_id = ?`, deviceID); err != nil {
			return err
		}
		nextGroup = ""
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE devices
		SET workspace_id = ?, name = ?, device_type = ?, group_id = NULLIF(?, '')
		WHERE id = ? AND revoked_at IS NULL
	`, nextWorkspace, nextName, nextType, nextGroup, deviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("device not found")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	action := "device.updated"
	if nextWorkspace != dev.WorkspaceID {
		action = "device.moved"
		_ = RecordAudit(ctx, d, dev.WorkspaceID, actorUserID, action, "device", deviceID, map[string]any{"to_workspace_id": nextWorkspace})
	}
	return RecordAudit(ctx, d, nextWorkspace, actorUserID, action, "device", deviceID, map[string]any{
		"name": nextName, "device_type": nextType, "group_id": nextGroup,
	})
}

func RevokeDeviceAsMember(ctx context.Context, d *sql.DB, deviceID string, actorUserID int64) error {
	dev, err := GetDevice(ctx, d, deviceID)
	if err != nil {
		return err
	}
	role, err := WorkspaceRole(ctx, d, dev.WorkspaceID, actorUserID)
	if err != nil || !RoleAtLeast(role, RoleAdmin) {
		return errors.New("workspace admin required")
	}
	res, err := d.ExecContext(ctx, `
		UPDATE devices SET revoked_at = CURRENT_TIMESTAMP, online = 0
		WHERE id = ? AND revoked_at IS NULL
	`, deviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("device not found")
	}
	return RecordAudit(ctx, d, dev.WorkspaceID, actorUserID, "device.removed", "device", deviceID, map[string]any{"name": dev.Name})
}

func RecordAudit(ctx context.Context, d *sql.DB, workspaceID string, actorUserID int64, action, targetType, targetID string, metadata any) error {
	payload := []byte("{}")
	if metadata != nil {
		if encoded, err := json.Marshal(metadata); err == nil {
			payload = encoded
		}
	}
	var actor any
	if actorUserID != 0 {
		actor = actorUserID
	}
	_, err := d.ExecContext(ctx, `
		INSERT INTO workspace_audit (workspace_id, actor_user_id, action, target_type, target_id, metadata_json)
		VALUES (?, ?, ?, ?, ?, ?)
	`, workspaceID, actor, action, targetType, targetID, string(payload))
	return err
}

func ListAudit(ctx context.Context, d *sql.DB, workspaceID string, userID int64, limit int) ([]AuditEvent, error) {
	if _, err := WorkspaceRole(ctx, d, workspaceID, userID); err != nil {
		return nil, errors.New("workspace access required")
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := d.QueryContext(ctx, `
		SELECT a.id, a.workspace_id, a.actor_user_id,
		       COALESCE(NULLIF('@' || u.username, '@'), u.first_name, 'Система'),
		       a.action, a.target_type, COALESCE(a.target_id,''), a.metadata_json, a.created_at
		FROM workspace_audit a
		LEFT JOIN users u ON u.id = a.actor_user_id
		WHERE a.workspace_id = ?
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT ?
	`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(&event.ID, &event.WorkspaceID, &event.ActorUserID,
			&event.ActorDisplay, &event.Action, &event.TargetType, &event.TargetID,
			&event.MetadataJSON, &event.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}
