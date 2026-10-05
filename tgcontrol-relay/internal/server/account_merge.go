package server

import (
	"context"
	"log"

	"tgcontrol-relay/internal/db"
)

// mergeAccounts keeps the database operation authoritative, then reconnects
// only desktops owned by the guest account. Their old anonymous device JWT is
// accepted through user_merge_aliases and rotated to the permanent owner in
// the normal agent welcome flow.
func (s *Server) mergeAccounts(ctx context.Context, fromUserID, toUserID int64) error {
	devices, listErr := db.ListDevicesByUser(ctx, s.DB, fromUserID)
	if err := db.MergeAccounts(ctx, s.DB, fromUserID, toUserID); err != nil {
		return err
	}
	if listErr != nil {
		log.Printf("[AUTH] merged account %d -> %d; could not enumerate agent reconnects: %v", fromUserID, toUserID, listErr)
		return nil
	}
	for _, device := range devices {
		s.closeDeviceConnections(device.ID, "account-merge")
	}
	return nil
}
