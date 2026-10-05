package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"tgcontrol-relay/internal/db"
)

func workspaceJSON(w db.Workspace) map[string]any {
	return map[string]any{
		"id":            w.ID,
		"kind":          w.Kind,
		"name":          w.Name,
		"role":          w.Role,
		"owner_user_id": w.OwnerUserID,
		"member_count":  w.MemberCount,
		"device_count":  w.DeviceCount,
		"online_count":  w.OnlineCount,
		"created_at":    w.CreatedAt,
	}
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	workspaces, err := db.ListWorkspacesForUser(r.Context(), s.DB, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(workspaces))
	for _, workspace := range workspaces {
		item := workspaceJSON(workspace)
		owner, oerr := db.GetUserByID(r.Context(), s.DB, workspace.OwnerUserID)
		if oerr == nil {
			item["max_devices"] = s.maxDevicesForTier(s.effectiveTier(owner))
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": out})
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<13)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.Name = trimToLen(strings.TrimSpace(body.Name), 64)
	workspace, err := db.CreateCompanyWorkspace(r.Context(), s.DB, claims.UserID, body.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workspace": workspaceJSON(*workspace)})
}

func (s *Server) handleUpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<13)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.Name = trimToLen(strings.TrimSpace(body.Name), 64)
	if err := db.RenameWorkspace(r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID, body.Name); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	workspaceID := chi.URLParam(r, "workspaceID")
	// Причину отказа называем МАШИННЫМ кодом. Безкодовый 409 клиент считал
	// конфликтом пейринга и советовал «Отключить облако» на ПК — это отвязывает
	// рабочую машину и к удалению компании отношения не имеет (находка N131).
	if workspace, werr := db.GetWorkspaceForUser(r.Context(), s.DB, workspaceID, claims.UserID); werr == nil {
		if workspace.Kind == db.WorkspacePersonal {
			writeErrCode(w, http.StatusConflict, "personal_workspace",
				"Личное пространство удалить нельзя.")
			return
		}
		// Порядок причин тот же, что в ArchiveWorkspace: не владельцу про
		// устройства говорить бессмысленно — ему откажут по роли.
		if workspace.Role == db.RoleOwner && workspace.DeviceCount > 0 {
			writeErrCode(w, http.StatusConflict, "workspace_not_empty",
				"Сначала перенесите или отвяжите устройства этой компании.")
			return
		}
	}
	if err := db.ArchiveWorkspace(r.Context(), s.DB, workspaceID, claims.UserID); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListWorkspaceMembers(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	members, err := db.ListWorkspaceMembers(r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(members))
	for _, member := range members {
		out = append(out, map[string]any{
			"user_id": member.UserID, "role": member.Role, "username": member.Username,
			"first_name": member.FirstName, "display": member.LoginDisplay, "joined_at": member.JoinedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

func (s *Server) handleCreateWorkspaceInvite(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	invite, err := db.CreateWorkspaceInvite(
		r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID,
		strings.ToLower(strings.TrimSpace(body.Role)), 7*24*time.Hour,
	)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"code": invite.Code, "role": invite.Role, "expires_at": invite.ExpiresAt,
	})
}

func (s *Server) handleAcceptWorkspaceInvite(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	workspace, err := db.AcceptWorkspaceInvite(r.Context(), s.DB, body.Code, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusGone, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": workspaceJSON(*workspace)})
}

func (s *Server) handleUpdateWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	memberID, err := strconv.ParseInt(chi.URLParam(r, "userID"), 10, 64)
	if err != nil || memberID <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid member id")
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := db.UpdateWorkspaceMemberRole(
		r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID,
		memberID, strings.ToLower(strings.TrimSpace(body.Role)),
	); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRemoveWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	memberID, err := strconv.ParseInt(chi.URLParam(r, "userID"), 10, 64)
	if err != nil || memberID <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid member id")
		return
	}
	if err := db.RemoveWorkspaceMember(
		r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID, memberID,
	); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	groups, err := db.ListGroups(r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	group, err := db.CreateGroup(r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID, trimToLen(body.Name, 48))
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"group": group})
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := db.DeleteGroup(r.Context(), s.DB, chi.URLParam(r, "workspaceID"), chi.URLParam(r, "groupID"), claims.UserID); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Zones are the public product name for the historical device_groups storage.
// Keep /groups available for older clients, while all new clients use /zones.
func (s *Server) handleListZones(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	zones, err := db.ListGroups(r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"zones": zones})
}

func (s *Server) handleCreateZone(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	zone, err := db.CreateGroup(r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID, trimToLen(body.Name, 48))
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"zone": zone})
}

func (s *Server) handleUpdateZone(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := db.RenameGroup(
		r.Context(), s.DB, chi.URLParam(r, "workspaceID"), chi.URLParam(r, "zoneID"),
		claims.UserID, trimToLen(body.Name, 48),
	); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteZone(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := db.DeleteGroup(
		r.Context(), s.DB, chi.URLParam(r, "workspaceID"), chi.URLParam(r, "zoneID"), claims.UserID,
	); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	tags, err := db.ListTags(r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

func (s *Server) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	tag, err := db.CreateTag(
		r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID,
		trimToLen(body.Name, 32), body.Color,
	)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tag": tag})
}

func (s *Server) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := db.DeleteTag(r.Context(), s.DB, chi.URLParam(r, "workspaceID"), chi.URLParam(r, "tagID"), claims.UserID); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWorkspaceAudit(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := db.ListAudit(r.Context(), s.DB, chi.URLParam(r, "workspaceID"), claims.UserID, limit)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(events))
	for _, event := range events {
		var metadata any
		_ = json.Unmarshal([]byte(event.MetadataJSON), &metadata)
		item := map[string]any{
			"id": event.ID, "actor": event.ActorDisplay, "action": event.Action,
			"target_type": event.TargetType, "target_id": event.TargetID,
			"metadata": metadata, "created_at": event.CreatedAt,
		}
		if event.ActorUserID.Valid {
			item["actor_user_id"] = event.ActorUserID.Int64
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *Server) handleUpdateDeviceInfrastructure(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Name        *string `json:"name"`
		DeviceType  *string `json:"device_type"`
		WorkspaceID *string `json:"workspace_id"`
		GroupID     *string `json:"group_id"`
		ZoneID      *string `json:"zone_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Name != nil {
		value := trimToLen(*body.Name, 64)
		body.Name = &value
	}
	if body.GroupID != nil && body.ZoneID != nil && strings.TrimSpace(*body.GroupID) != strings.TrimSpace(*body.ZoneID) {
		writeErr(w, http.StatusBadRequest, "group_id and zone_id conflict")
		return
	}
	groupID := body.ZoneID
	if groupID == nil {
		groupID = body.GroupID
	}
	if err := db.UpdateDeviceInfrastructure(
		r.Context(), s.DB, chi.URLParam(r, "deviceID"), claims.UserID,
		body.Name, body.DeviceType, body.WorkspaceID, groupID,
	); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSetDeviceFavorite(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		Favorite bool `json:"favorite"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := db.SetDeviceFavorite(r.Context(), s.DB, chi.URLParam(r, "deviceID"), claims.UserID, body.Favorite); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleReplaceDeviceTags(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		TagIDs []string `json:"tag_ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(body.TagIDs) > 32 {
		writeErr(w, http.StatusBadRequest, "too many tags")
		return
	}
	if err := db.ReplaceDeviceTags(r.Context(), s.DB, chi.URLParam(r, "deviceID"), claims.UserID, body.TagIDs); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /v1/devices/bulk applies the same infrastructure action to a bounded
// set. Per-device permission checks remain authoritative; partial success is
// returned explicitly so the UI never implies an action affected every node.
func (s *Server) handleBulkDevices(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var body struct {
		DeviceIDs []string        `json:"device_ids"`
		Action    string          `json:"action"`
		Value     json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(body.DeviceIDs) == 0 || len(body.DeviceIDs) > 100 {
		writeErr(w, http.StatusBadRequest, "device_ids must contain 1..100 items")
		return
	}
	var succeeded []string
	failures := map[string]string{}
	for _, deviceID := range body.DeviceIDs {
		var actionErr error
		switch body.Action {
		case "favorite":
			var value bool
			actionErr = json.Unmarshal(body.Value, &value)
			if actionErr == nil {
				actionErr = db.SetDeviceFavorite(r.Context(), s.DB, deviceID, claims.UserID, value)
			}
		case "device_type":
			var value string
			actionErr = json.Unmarshal(body.Value, &value)
			if actionErr == nil {
				actionErr = db.UpdateDeviceInfrastructure(r.Context(), s.DB, deviceID, claims.UserID, nil, &value, nil, nil)
			}
		case "group", "zone":
			var value string
			actionErr = json.Unmarshal(body.Value, &value)
			if actionErr == nil {
				actionErr = db.UpdateDeviceInfrastructure(r.Context(), s.DB, deviceID, claims.UserID, nil, nil, nil, &value)
			}
		case "move":
			var value string
			actionErr = json.Unmarshal(body.Value, &value)
			if actionErr == nil {
				actionErr = db.UpdateDeviceInfrastructure(r.Context(), s.DB, deviceID, claims.UserID, nil, nil, &value, nil)
			}
		case "tags":
			var value []string
			actionErr = json.Unmarshal(body.Value, &value)
			if actionErr == nil {
				actionErr = db.ReplaceDeviceTags(r.Context(), s.DB, deviceID, claims.UserID, value)
			}
		default:
			actionErr = fmt.Errorf("unsupported action %q", body.Action)
		}
		if actionErr != nil {
			failures[deviceID] = actionErr.Error()
		} else {
			succeeded = append(succeeded, deviceID)
		}
	}
	status := http.StatusOK
	if len(succeeded) == 0 {
		status = http.StatusForbidden
	}
	writeJSON(w, status, map[string]any{
		"ok": len(failures) == 0, "succeeded": succeeded, "failures": failures,
	})
}

func workspaceForPairing(ctx *http.Request, d *sql.DB, userID int64, workspaceID string) (*db.Workspace, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return db.EnsurePersonalWorkspace(ctx.Context(), d, userID)
	}
	workspace, err := db.GetWorkspaceForUser(ctx.Context(), d, strings.TrimSpace(workspaceID), userID)
	if err != nil {
		return nil, err
	}
	if !db.RoleAtLeast(workspace.Role, db.RoleAdmin) {
		return nil, errors.New("workspace admin required to add devices")
	}
	return workspace, nil
}

func zoneForPairing(ctx *http.Request, d *sql.DB, workspaceID, zoneID string) (string, error) {
	zoneID = strings.TrimSpace(zoneID)
	if zoneID == "" {
		return "", nil
	}
	zone, err := db.GroupForDevice(ctx.Context(), d, zoneID)
	if err != nil {
		return "", errors.New("zone not found")
	}
	if zone.WorkspaceID != workspaceID {
		return "", errors.New("zone belongs to another workspace")
	}
	return zone.ID, nil
}
