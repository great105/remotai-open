package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
)

func newInfrastructureTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	d := initSQLite(t)
	cfg := &config.Config{
		JWTSecret:       "test-secret-please-change-1234567890",
		JWTTTL:          time.Hour,
		PairCodeTTL:     time.Minute,
		PairMaxAttempts: 5,
		FreeMaxDevices:  3,
		ProMaxDevices:   10,
		TeamMaxDevices:  50,
		CORSOrigins:     []string{"*"},
		// Прокси к компьютеру теперь проходит денежный гейт (proxy.go): эти
		// тесты про роли и права, а не про деньги, — пускаем как в остальных
		// серверных тестах.
		TrialDays: 30,
	}
	srv := New(cfg, d)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(func() {
		ts.Close()
		d.Close()
	})
	return srv, ts
}

func requestJSON(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Remotai-Client", "android")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	return resp.StatusCode, payload
}

func issueTestUser(t *testing.T, srv *Server, telegramID int64, username string) (*db.User, string) {
	t.Helper()
	user, err := db.UpsertUser(context.Background(), srv.DB, telegramID, username, username, "ru")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	token, _, err := srv.JWT.IssueUser(user.ID, user.TelegramID, user.Tier)
	if err != nil {
		t.Fatalf("issue user: %v", err)
	}
	return user, token
}

func TestInfrastructureWorkspaceInviteAndDeviceLifecycle(t *testing.T) {
	srv, ts := newInfrastructureTestServer(t)
	owner, ownerJWT := issueTestUser(t, srv, 11001, "owner")
	operator, operatorJWT := issueTestUser(t, srv, 11002, "operator")

	status, created := requestJSON(t, http.MethodPost, ts.URL+"/v1/workspaces", ownerJWT, map[string]any{"name": "ООО Север"})
	if status != http.StatusCreated {
		t.Fatalf("create workspace status=%d body=%v", status, created)
	}
	workspace := created["workspace"].(map[string]any)
	workspaceID := workspace["id"].(string)

	status, inviteBody := requestJSON(t, http.MethodPost, ts.URL+"/v1/workspaces/"+workspaceID+"/invites", ownerJWT, map[string]any{"role": "operator"})
	if status != http.StatusCreated {
		t.Fatalf("create invite status=%d body=%v", status, inviteBody)
	}
	code := inviteBody["code"].(string)
	status, accepted := requestJSON(t, http.MethodPost, ts.URL+"/v1/workspace-invites/accept", operatorJWT, map[string]any{"code": code})
	if status != http.StatusOK {
		t.Fatalf("accept invite status=%d body=%v", status, accepted)
	}

	device := &db.Device{
		ID: "infra-device", UserID: owner.ID, WorkspaceID: workspaceID,
		Name: "Production API", Hostname: "prod-1", Platform: "linux",
		DeviceType: db.DeviceServer, AgentVersion: "test",
	}
	if err := db.InsertDevice(context.Background(), srv.DB, device); err != nil {
		t.Fatalf("insert device: %v", err)
	}

	status, listed := requestJSON(t, http.MethodGet, ts.URL+"/v1/devices?workspace_id="+workspaceID, operatorJWT, nil)
	if status != http.StatusOK {
		t.Fatalf("list devices status=%d body=%v", status, listed)
	}
	devices := listed["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("devices=%d want=1 body=%v", len(devices), listed)
	}
	got := devices[0].(map[string]any)
	if got["device_type"] != "server" || got["workspace_role"] != "operator" {
		t.Fatalf("device infrastructure fields=%v", got)
	}

	// Operator controls nodes but cannot move/reclassify infrastructure.
	status, denied := requestJSON(t, http.MethodPatch, ts.URL+"/v1/devices/infra-device", operatorJWT, map[string]any{"device_type": "computer"})
	if status != http.StatusForbidden {
		t.Fatalf("operator patch status=%d body=%v", status, denied)
	}

	status, zoneBody := requestJSON(t, http.MethodPost, ts.URL+"/v1/workspaces/"+workspaceID+"/zones", ownerJWT, map[string]any{"name": "Продакшен"})
	if status != http.StatusCreated {
		t.Fatalf("create zone status=%d body=%v", status, zoneBody)
	}
	zone := zoneBody["zone"].(map[string]any)
	zoneID := zone["id"].(string)
	status, renamedZone := requestJSON(t, http.MethodPatch, ts.URL+"/v1/workspaces/"+workspaceID+"/zones/"+zoneID, ownerJWT, map[string]any{"name": "Production"})
	if status != http.StatusOK {
		t.Fatalf("rename zone status=%d body=%v", status, renamedZone)
	}
	status, zoneList := requestJSON(t, http.MethodGet, ts.URL+"/v1/workspaces/"+workspaceID+"/zones", operatorJWT, nil)
	if status != http.StatusOK || len(zoneList["zones"].([]any)) != 1 || zoneList["zones"].([]any)[0].(map[string]any)["name"] != "Production" {
		t.Fatalf("list zones status=%d body=%v", status, zoneList)
	}
	// Historical /groups stays readable while old clients roll forward.
	status, legacyGroups := requestJSON(t, http.MethodGet, ts.URL+"/v1/workspaces/"+workspaceID+"/groups", operatorJWT, nil)
	if status != http.StatusOK || len(legacyGroups["groups"].([]any)) != 1 {
		t.Fatalf("legacy groups status=%d body=%v", status, legacyGroups)
	}
	status, tagBody := requestJSON(t, http.MethodPost, ts.URL+"/v1/workspaces/"+workspaceID+"/tags", ownerJWT, map[string]any{"name": "Критичный", "color": "#ef4444"})
	if status != http.StatusCreated {
		t.Fatalf("create tag status=%d body=%v", status, tagBody)
	}
	tag := tagBody["tag"].(map[string]any)

	status, patched := requestJSON(t, http.MethodPatch, ts.URL+"/v1/devices/infra-device", ownerJWT, map[string]any{
		"zone_id": zoneID, "device_type": "server", "name": "Production API 1",
	})
	if status != http.StatusOK {
		t.Fatalf("owner patch status=%d body=%v", status, patched)
	}
	status, tagged := requestJSON(t, http.MethodPut, ts.URL+"/v1/devices/infra-device/tags", operatorJWT, map[string]any{"tag_ids": []string{tag["id"].(string)}})
	if status != http.StatusOK {
		t.Fatalf("operator tags status=%d body=%v", status, tagged)
	}
	status, favorite := requestJSON(t, http.MethodPut, ts.URL+"/v1/devices/infra-device/favorite", operatorJWT, map[string]any{"favorite": true})
	if status != http.StatusOK {
		t.Fatalf("favorite status=%d body=%v", status, favorite)
	}

	status, listed = requestJSON(t, http.MethodGet, ts.URL+"/v1/devices?workspace_id="+workspaceID, operatorJWT, nil)
	got = listed["devices"].([]any)[0].(map[string]any)
	gotZone, _ := got["zone"].(map[string]any)
	gotLegacyGroup, _ := got["group"].(map[string]any)
	if got["favorite"] != true || len(got["tags"].([]any)) != 1 || gotZone["id"] != zoneID || gotLegacyGroup["id"] != zoneID {
		t.Fatalf("zone/favorite/tags missing: %v", got)
	}

	status, bulk := requestJSON(t, http.MethodPost, ts.URL+"/v1/devices/bulk", ownerJWT, map[string]any{
		"device_ids": []string{"infra-device"}, "action": "zone", "value": "",
	})
	if status != http.StatusOK || bulk["ok"] != true {
		t.Fatalf("bulk clear zone status=%d body=%v", status, bulk)
	}
	status, deletedZone := requestJSON(t, http.MethodDelete, ts.URL+"/v1/workspaces/"+workspaceID+"/zones/"+zoneID, ownerJWT, nil)
	if status != http.StatusOK || deletedZone["ok"] != true {
		t.Fatalf("delete zone status=%d body=%v", status, deletedZone)
	}

	status, audit := requestJSON(t, http.MethodGet, ts.URL+"/v1/workspaces/"+workspaceID+"/audit", ownerJWT, nil)
	if status != http.StatusOK || len(audit["events"].([]any)) < 4 {
		t.Fatalf("audit status=%d body=%v", status, audit)
	}

	_ = operator // explicit evidence that separate account joined
}

func TestPairingNewDeviceIntoZone(t *testing.T) {
	srv, ts := newInfrastructureTestServer(t)
	owner, ownerJWT := issueTestUser(t, srv, 11501, "zone-pair-owner")
	workspace, err := db.CreateCompanyWorkspace(context.Background(), srv.DB, owner.ID, "ООО Зоны")
	if err != nil {
		t.Fatal(err)
	}
	status, emptyZones := requestJSON(t, http.MethodGet, ts.URL+"/v1/workspaces/"+workspace.ID+"/zones", ownerJWT, nil)
	if status != http.StatusOK {
		t.Fatalf("empty zones status=%d body=%v", status, emptyZones)
	}
	if _, ok := emptyZones["zones"].([]any); !ok {
		t.Fatalf("empty zones must be a JSON array: %v", emptyZones)
	}
	status, emptyTags := requestJSON(t, http.MethodGet, ts.URL+"/v1/workspaces/"+workspace.ID+"/tags", ownerJWT, nil)
	if status != http.StatusOK {
		t.Fatalf("empty tags status=%d body=%v", status, emptyTags)
	}
	if _, ok := emptyTags["tags"].([]any); !ok {
		t.Fatalf("empty tags must be a JSON array: %v", emptyTags)
	}
	zone, err := db.CreateGroup(context.Background(), srv.DB, workspace.ID, owner.ID, "ЦОД")
	if err != nil {
		t.Fatal(err)
	}
	code, err := db.GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertPairingCode(
		context.Background(), srv.DB, code, "zone-pair-device",
		"dc-api-01", "linux", "2.33.2", time.Minute,
	); err != nil {
		t.Fatal(err)
	}

	status, body := requestJSON(t, http.MethodPost, ts.URL+"/v1/pair/confirm-native", ownerJWT, map[string]any{
		"code": code, "workspace_id": workspace.ID, "device_type": "server", "zone_id": zone.ID,
	})
	if status != http.StatusOK || body["workspace_id"] != workspace.ID || body["zone_id"] != zone.ID {
		t.Fatalf("pair into zone status=%d body=%v", status, body)
	}
	device, err := db.GetDevice(context.Background(), srv.DB, "zone-pair-device")
	if err != nil || device.GroupID != zone.ID || device.DeviceType != db.DeviceServer {
		t.Fatalf("paired zone device=%+v err=%v", device, err)
	}

	telegramCode, err := db.GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertPairingCode(
		context.Background(), srv.DB, telegramCode, "zone-pair-telegram-device",
		"office-pc-01", "windows", "2.33.2", time.Minute,
	); err != nil {
		t.Fatal(err)
	}
	status, telegramBody := requestJSON(t, http.MethodPost, ts.URL+"/v1/pair/confirm", ownerJWT, map[string]any{
		"code": telegramCode, "workspace_id": workspace.ID, "device_type": "computer", "zone_id": zone.ID,
	})
	if status != http.StatusOK || telegramBody["workspace_id"] != workspace.ID || telegramBody["zone_id"] != zone.ID {
		t.Fatalf("telegram pair into zone status=%d body=%v", status, telegramBody)
	}
	telegramDevice, err := db.GetDevice(context.Background(), srv.DB, "zone-pair-telegram-device")
	if err != nil || telegramDevice.GroupID != zone.ID || telegramDevice.DeviceType != db.DeviceComputer {
		t.Fatalf("telegram paired zone device=%+v err=%v", telegramDevice, err)
	}
}

func TestRevokeOtherUserSessionsInvalidatesStableSID(t *testing.T) {
	srv, ts := newInfrastructureTestServer(t)
	user, err := db.UpsertUser(context.Background(), srv.DB, 12001, "sessionuser", "Session", "ru")
	if err != nil {
		t.Fatal(err)
	}
	token1, _, err := srv.JWT.IssueUserForSession(user.ID, user.TelegramID, user.Tier, "phone-one")
	if err != nil {
		t.Fatal(err)
	}
	token2, _, err := srv.JWT.IssueUserForSession(user.ID, user.TelegramID, user.Tier, "phone-two")
	if err != nil {
		t.Fatal(err)
	}
	for i, token := range []string{token1, token2} {
		status, body := requestJSON(t, http.MethodGet, ts.URL+"/v1/me", token, nil)
		if status != http.StatusOK {
			t.Fatalf("seed session %d status=%d body=%v", i, status, body)
		}
	}
	status, sessions := requestJSON(t, http.MethodGet, ts.URL+"/v1/me/sessions", token1, nil)
	if status != http.StatusOK || len(sessions["sessions"].([]any)) != 2 {
		t.Fatalf("sessions status=%d body=%v", status, sessions)
	}
	status, revoked := requestJSON(t, http.MethodPost, ts.URL+"/v1/me/sessions/revoke-others", token1, map[string]any{})
	if status != http.StatusOK || int(revoked["revoked"].(float64)) != 1 {
		t.Fatalf("revoke others status=%d body=%v", status, revoked)
	}
	status, body := requestJSON(t, http.MethodGet, ts.URL+"/v1/me", token2, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked token status=%d want=401 body=%v", status, body)
	}
	status, body = requestJSON(t, http.MethodGet, ts.URL+"/v1/me", token1, nil)
	if status != http.StatusOK {
		t.Fatalf("current token status=%d body=%v", status, body)
	}
}

func TestViewerCannotOperateDevice(t *testing.T) {
	srv, ts := newInfrastructureTestServer(t)
	owner, _ := issueTestUser(t, srv, 13001, "viewerowner")
	viewer, viewerJWT := issueTestUser(t, srv, 13002, "viewer")
	workspace, err := db.CreateCompanyWorkspace(context.Background(), srv.DB, owner.ID, "Наблюдение")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.DB.Exec(`
		INSERT INTO workspace_members (workspace_id, user_id, role, added_by)
		VALUES (?, ?, 'viewer', ?)
	`, workspace.ID, viewer.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertDevice(context.Background(), srv.DB, &db.Device{
		ID: "viewer-device", UserID: owner.ID, WorkspaceID: workspace.ID,
		Name: "Read only", Platform: "linux", DeviceType: db.DeviceServer,
	}); err != nil {
		t.Fatal(err)
	}
	status, body := requestJSON(t, http.MethodPost, ts.URL+"/v1/client/viewer-device/request", viewerJWT, map[string]any{
		"method": "POST", "path": "/api/system/power", "body": "",
	})
	if status != http.StatusForbidden || body["code"] != "viewer_read_only" {
		t.Fatalf("viewer operation status=%d body=%v", status, body)
	}
	status, body = requestJSON(t, http.MethodGet, ts.URL+"/v1/devices", viewerJWT, nil)
	if status != http.StatusOK {
		t.Fatalf("viewer list status=%d body=%v", status, body)
	}
	if got := len(body["devices"].([]any)); got != 1 {
		t.Fatalf("viewer devices=%d want=1", got)
	}
	_ = fmt.Sprint(owner.ID) // keep owner evidence visible to compiler
}

func TestPairingExistingDevicePreservesCompanyClassification(t *testing.T) {
	srv, ts := newInfrastructureTestServer(t)
	owner, ownerJWT := issueTestUser(t, srv, 14001, "pair-owner")
	workspace, err := db.CreateCompanyWorkspace(context.Background(), srv.DB, owner.ID, "ООО Контур")
	if err != nil {
		t.Fatal(err)
	}
	group, err := db.CreateGroup(context.Background(), srv.DB, workspace.ID, owner.ID, "Рабочие станции")
	if err != nil {
		t.Fatal(err)
	}
	tag, err := db.CreateTag(context.Background(), srv.DB, workspace.ID, owner.ID, "Важное", "#ef4444")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertDevice(context.Background(), srv.DB, &db.Device{
		ID: "company-linux-desktop", UserID: owner.ID, WorkspaceID: workspace.ID,
		Name: "Linux Desktop", Hostname: "designer-01", Platform: "linux",
		DeviceType: db.DeviceComputer, GroupID: group.ID, AgentVersion: "2.32.2",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceDeviceTags(context.Background(), srv.DB, "company-linux-desktop", owner.ID, []string{tag.ID}); err != nil {
		t.Fatal(err)
	}
	code, err := db.GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertPairingCode(
		context.Background(), srv.DB, code, "company-linux-desktop",
		"designer-01", "linux", "2.33.0", time.Minute,
	); err != nil {
		t.Fatal(err)
	}

	// Generic native pairing deliberately omits workspace/type. It reconnects
	// account access but must not move the device to personal or infer server.
	status, body := requestJSON(t, http.MethodPost, ts.URL+"/v1/pair/confirm-native", ownerJWT, map[string]any{"code": strings.ToLower(code)})
	if status != http.StatusOK {
		t.Fatalf("pair status=%d body=%v", status, body)
	}
	if body["workspace_id"] != workspace.ID || body["device_type"] != db.DeviceComputer {
		t.Fatalf("pair response classification changed: %v", body)
	}
	device, err := db.GetDevice(context.Background(), srv.DB, "company-linux-desktop")
	if err != nil {
		t.Fatal(err)
	}
	if device.WorkspaceID != workspace.ID || device.DeviceType != db.DeviceComputer || device.GroupID != group.ID {
		t.Fatalf("classification changed: workspace=%q type=%q group=%q", device.WorkspaceID, device.DeviceType, device.GroupID)
	}
	tags, err := db.TagsForDevice(context.Background(), srv.DB, device.ID)
	if err != nil || len(tags) != 1 || tags[0].ID != tag.ID {
		t.Fatalf("tags changed: %+v err=%v", tags, err)
	}
}

func TestMergeAccountPreservesSharedAccessAndRotatesAgentPrincipal(t *testing.T) {
	srv, _ := newInfrastructureTestServer(t)
	ctx := context.Background()
	anon, err := db.CreateAnonUser(ctx, srv.DB)
	if err != nil {
		t.Fatal(err)
	}
	permanent, err := db.UpsertUser(ctx, srv.DB, 15001, "permanent", "Permanent", "ru")
	if err != nil {
		t.Fatal(err)
	}
	sharer, err := db.UpsertUser(ctx, srv.DB, 15002, "sharer", "Sharer", "ru")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertDevice(ctx, srv.DB, &db.Device{
		ID: "guest-owned", UserID: anon.ID, Name: "Guest PC", Platform: "windows",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertDevice(ctx, srv.DB, &db.Device{
		ID: "shared-with-guest", UserID: sharer.ID, Name: "Shared PC", Platform: "windows",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantDeviceAccess(ctx, srv.DB, "shared-with-guest", anon.ID); err != nil {
		t.Fatal(err)
	}
	oldDeviceJWT, _, err := srv.JWT.IssueDevice("guest-owned", anon.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldClaims, err := srv.JWT.ParseDevice(oldDeviceJWT)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.MergeAccounts(ctx, srv.DB, anon.ID, permanent.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.UserCanAccessDevice(ctx, srv.DB, "shared-with-guest", permanent.ID); err != nil || !ok {
		t.Fatalf("shared access was not transferred: ok=%v err=%v", ok, err)
	}
	device, err := db.GetDeviceForAgentAuth(ctx, srv.DB, "guest-owned", oldClaims.UserID)
	if err != nil || device.UserID != permanent.ID {
		t.Fatalf("merged device JWT not accepted for rotation: device=%+v err=%v", device, err)
	}

	// A later real takeover changes the current owner. The merge alias must no
	// longer authorize the old guest token.
	if _, err := srv.DB.Exec(`UPDATE devices SET revoked_at = CURRENT_TIMESTAMP WHERE id = 'guest-owned'`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReassignDevice(ctx, srv.DB, &db.Device{
		ID: "guest-owned", UserID: sharer.ID, Name: "Taken over", Platform: "windows",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetDeviceForAgentAuth(ctx, srv.DB, "guest-owned", oldClaims.UserID); err == nil {
		t.Fatal("old merged principal authorized after a real takeover")
	}
}

func TestOldDeviceTokenCannotSelfRevokeAfterTakeover(t *testing.T) {
	srv, ts := newInfrastructureTestServer(t)
	ctx := context.Background()
	oldOwner, _ := issueTestUser(t, srv, 16001, "old-owner")
	newOwner, _ := issueTestUser(t, srv, 16002, "new-owner")
	if err := db.InsertDevice(ctx, srv.DB, &db.Device{
		ID: "taken-device", UserID: oldOwner.ID, Name: "Taken", Platform: "windows",
	}); err != nil {
		t.Fatal(err)
	}
	oldJWT, _, err := srv.JWT.IssueDevice("taken-device", oldOwner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.DB.Exec(`UPDATE devices SET revoked_at = CURRENT_TIMESTAMP WHERE id = 'taken-device'`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReassignDevice(ctx, srv.DB, &db.Device{
		ID: "taken-device", UserID: newOwner.ID, Name: "New owner", Platform: "windows",
	}); err != nil {
		t.Fatal(err)
	}

	status, body := requestJSON(t, http.MethodPost, ts.URL+"/v1/device/revoke-self", oldJWT, map[string]any{})
	if status != http.StatusUnauthorized {
		t.Fatalf("old token self-revoke status=%d body=%v", status, body)
	}
	device, err := db.GetDevice(ctx, srv.DB, "taken-device")
	if err != nil || device.RevokedAt.Valid {
		t.Fatalf("new owner's device was revoked: device=%+v err=%v", device, err)
	}
}

func TestNativePairingRejectsExpiredSessionInsteadOfForkingAccount(t *testing.T) {
	srv, ts := newInfrastructureTestServer(t)
	code, err := db.GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertPairingCode(
		context.Background(), srv.DB, code, "expired-session-device",
		"office", "windows", "2.33.0", time.Minute,
	); err != nil {
		t.Fatal(err)
	}
	var usersBefore int
	if err := srv.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&usersBefore); err != nil {
		t.Fatal(err)
	}

	status, body := requestJSON(t, http.MethodPost, ts.URL+"/v1/pair/confirm-native", "expired.or.invalid", map[string]any{"code": code})
	if status != http.StatusUnauthorized || body["code"] != "session_expired" {
		t.Fatalf("expired session status=%d body=%v", status, body)
	}
	var usersAfter int
	if err := srv.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&usersAfter); err != nil {
		t.Fatal(err)
	}
	if usersAfter != usersBefore {
		t.Fatalf("invalid session created a guest account: before=%d after=%d", usersBefore, usersAfter)
	}
}
