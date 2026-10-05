package pty

import "tgcontrol/internal/agents"

// claudeRuntimeStatus uses the config directory announced by this terminal's
// Claude hook. Each PTY can run a different Claude account, so the machine's
// default ~/.claude directory is only correct before an account is known.
func (s *Session) claudeRuntimeStatus(pid int) (agents.RuntimeStatus, bool) {
	s.historyMu.Lock()
	source := s.historySource
	s.historyMu.Unlock()
	if source.Agent == "claude" {
		return agents.ClaudeRuntimeStatusAt(pid, source.ConfigHome)
	}
	return agents.ClaudeRuntimeStatus(pid)
}
