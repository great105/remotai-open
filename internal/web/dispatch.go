package web

import (
	"fmt"

	"tgcontrol/internal/config"
	"tgcontrol/internal/sessions"
)

func (s *Server) ensureAgentAllowed(agentType string) error {
	cfg := config.Get()
	if cfg.IsAgentAllowed(agentType) {
		return nil
	}
	return fmt.Errorf("agent %q is not allowed", agentType)
}

func (s *Server) startSessionRun(uid int64, name string) (*sessions.Session, error) {
	cfg := config.Get()
	maxConcurrent := cfg.MaxConcurrentSessions

	// Apply tier-based limit if stricter than config
	if s.licenseManager != nil {
		limits := s.licenseManager.GetLimits()
		if limits.MaxConcurrentAgents > 0 {
			if maxConcurrent == 0 || limits.MaxConcurrentAgents < maxConcurrent {
				maxConcurrent = limits.MaxConcurrentAgents
			}
		}
	}

	return s.store.StartRun(int(uid), name, maxConcurrent)
}
