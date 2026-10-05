package db

import (
	"context"
	"testing"

	"tgcontrol-relay/internal/auth"
)

// Маркер отзыва device-JWT: ставится при отзыве устройства, проверяется при
// приёме токена (agent/connect, agent/stream), снимается при ре-пейринге.
// Настоящий jti токена агента серверу неизвестен (refresh-on-connect), поэтому
// отзыв — синтетическим jti по device_id.
func TestDeviceRevocationRoundTrip(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if ok, err := auth.IsDeviceRevoked(ctx, d, "dev-1"); err != nil || ok {
		t.Fatalf("до отзыва: revoked=%v err=%v", ok, err)
	}
	if err := auth.RevokeDeviceTokens(ctx, d, "dev-1", 42); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Идемпотентность: revoke-self/revoke могут сработать повторно.
	if err := auth.RevokeDeviceTokens(ctx, d, "dev-1", 42); err != nil {
		t.Fatalf("повторный revoke: %v", err)
	}
	if ok, err := auth.IsDeviceRevoked(ctx, d, "dev-1"); err != nil || !ok {
		t.Fatalf("после отзыва: revoked=%v err=%v", ok, err)
	}
	// Тот же маркер виден и через IsRevoked по синтетическому jti.
	if ok, err := auth.IsRevoked(ctx, d, auth.DeviceRevocationJTI("dev-1")); err != nil || !ok {
		t.Fatalf("IsRevoked(marker): revoked=%v err=%v", ok, err)
	}
	if err := auth.ClearDeviceRevocations(ctx, d, "dev-1"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if ok, err := auth.IsDeviceRevoked(ctx, d, "dev-1"); err != nil || ok {
		t.Fatalf("после clear: revoked=%v err=%v", ok, err)
	}
}

// Ре-пейринг/takeover (ReassignDevice) снимает маркер отзыва — иначе свежий
// device-JWT после повторной привязки отклонялся бы навсегда. Заодно тест
// фиксирует совпадение формата маркера между auth.DeviceRevocationJTI и
// продублированным SQL в ReassignDevice.
func TestReassignDeviceClearsRevocationMarker(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	u, err := CreateAnonUser(ctx, d)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	dev := &Device{ID: "dev-repair", UserID: u.ID, Name: "PC", Platform: "linux"}
	if err := InsertDevice(ctx, d, dev); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	if err := auth.RevokeDeviceTokens(ctx, d, dev.ID, u.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if ok, _ := auth.IsDeviceRevoked(ctx, d, dev.ID); !ok {
		t.Fatalf("маркер не поставлен")
	}
	if err := ReassignDevice(ctx, d, dev); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if ok, err := auth.IsDeviceRevoked(ctx, d, dev.ID); err != nil || ok {
		t.Fatalf("после ре-пейринга маркер должен быть снят: revoked=%v err=%v", ok, err)
	}
}
