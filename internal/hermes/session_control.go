package hermes

import "encoding/json"

func (m *Manager) observeSessionSnapshot(raw json.RawMessage) {
	m.observeBridgeSessionSnapshot(raw, nil)
}

func (m *Manager) observeBridgeSessionSnapshot(raw json.RawMessage, source *rpcBridge) {
	j := m.control
	if j == nil {
		return
	}
	var snapshot struct {
		SessionID  string `json:"session_id"`
		StoredID   string `json:"stored_session_id"`
		SessionKey string `json:"session_key"`
		Resumed    string `json:"resumed"`
		Info       struct {
			Cwd      string `json:"cwd"`
			StoredID string `json:"stored_session_id"`
		} `json:"info"`
		Requests []json.RawMessage `json:"open_requests"`
	}
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.SessionID == "" {
		return
	}
	j.mu.Lock()
	m.mu.Lock()
	if m.closing || source != nil && m.bridge != source {
		m.mu.Unlock()
		j.mu.Unlock()
		return
	}
	seq, epoch := m.seq, m.epoch
	m.mu.Unlock()
	stored := snapshot.StoredID
	if stored == "" {
		stored = snapshot.Info.StoredID
	}
	if stored == "" {
		stored = snapshot.Resumed
	}
	j.Sessions[snapshot.SessionID] = snapshot.Info.Cwd
	j.StoredSessions[snapshot.SessionID] = stored
	j.SessionEpochs[snapshot.SessionID] = epoch
	j.mu.Unlock()
	for _, request := range snapshot.Requests {
		m.observeControl(request, seq, epoch)
	}
}
