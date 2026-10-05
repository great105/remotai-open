package pty

// Беседы агентов, которые сейчас в терминалах Remotai — для экрана «Беседы»
// (internal/agentsessions). Нажатие на беседу, которая уже идёт или спит в
// терминале, обязано открыть ЭТОТ терминал: второй `claude --resume` той же
// беседы — две копии агента, пишущие в один файл.

// AgentConversation — беседа агента в терминале.
type AgentConversation struct {
	PtyID     string
	Agent     string
	SessionID string
	Sleeping  bool
}

// AgentConversations — беседы Claude и Codex во всех живых локальных
// терминалах: спящие (запись сна в Meta) и идущие (номер беседы того
// процесса, что на переднем плане, — тот же способ, что у усыпления).
func (m *Manager) AgentConversations() []AgentConversation {
	m.mu.RLock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.RUnlock()

	var out []AgentConversation
	for _, s := range list {
		if s.Shell == "ssh" || !s.IsAlive() {
			continue
		}
		var sleeping *SleepRecord
		if m.meta != nil {
			sleeping = m.meta.Get(s.ID).Sleep
		}
		if sleeping != nil && sleeping.SessionID != "" {
			out = append(out, AgentConversation{PtyID: s.ID, Agent: sleeping.Agent, SessionID: sleeping.SessionID, Sleeping: true})
			// Агента могли разбудить или запустить руками: если на переднем
			// плане уже другой процесс со своей беседой, её тоже отдаём ниже.
		}
		if s.conn() == nil {
			continue
		}
		fg := s.ForegroundProcess()
		kind := AgentKind(fg.Name)
		if fg.PID == 0 || (kind != "claude" && kind != "codex") {
			continue
		}
		sid := s.sleepSessionID(kind, fg)
		if sid == "" || (sleeping != nil && sleeping.Agent == kind && sleeping.SessionID == sid) {
			continue
		}
		out = append(out, AgentConversation{PtyID: s.ID, Agent: kind, SessionID: sid})
	}
	return out
}

// PIDAlive — жив ли процесс с этим PID (для «беседа открыта вне Remotai»).
func PIDAlive(pid int) bool {
	return pid > 0 && processAlive(uint32(pid))
}
