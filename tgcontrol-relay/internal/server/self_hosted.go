package server

import (
	"context"

	"tgcontrol-relay/internal/db"
)

// Self-hosting is an operator setting, never a flag supplied by a client.
// Authentication, device ownership and workspace roles still apply.
func (s *Server) selfHosted() bool {
	return s.Config != nil && s.Config.SelfHosted
}

func (s *Server) effectiveTier(u *db.User) string {
	if s.selfHosted() {
		return "team"
	}
	return u.EffectiveTier(false)
}

func (s *Server) cloudAccess(ctx context.Context, userID int64) (db.CloudDecision, error) {
	if s.selfHosted() {
		if _, err := db.GetUserByID(ctx, s.DB, userID); err != nil {
			return db.CloudDecision{}, err
		}
		return db.CloudDecision{Allowed: true, Tier: "team"}, nil
	}
	return db.CloudAccess(ctx, s.DB, userID, false)
}

func (s *Server) billingEnabled() bool {
	return s.Config != nil && !s.selfHosted() && s.Config.BillingEnabled
}
