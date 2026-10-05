package web

import (
	"path/filepath"
	"tgcontrol/internal/hermes"
)

func (s *Server) restoreEnabledHermesOwners() {
	root, err := hermesLocalRoot(1)
	if err != nil {
		return
	}
	owners, err := (&hermes.Manager{}).EnabledOwners(filepath.Dir(root))
	if err != nil {
		return
	}
	for _, uid := range owners {
		_, _ = s.hermesForUser(uid)
	}
}
