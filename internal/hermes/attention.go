package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

func (m *Manager) observeControl(raw []byte, seq, epoch uint64) {
	m.observeBridgeControl(raw, seq, epoch, nil)
}
func (m *Manager) observeBridgeControl(raw []byte, seq, epoch uint64, source *rpcBridge) {
	j := m.control
	if j == nil {
		return
	}
	var frame rpcFrame
	if json.Unmarshal(raw, &frame) != nil {
		return
	}
	var params struct {
		SessionID string          `json:"session_id"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	_ = json.Unmarshal(frame.Params, &params)
	j.mu.Lock()
	defer j.mu.Unlock()
	// Generation can retire while the event waits for the journal lock.
	m.mu.Lock()
	defer m.mu.Unlock()
	valid := !m.closing && m.epoch == epoch && (source == nil || m.bridge == source)
	if !valid {
		return
	}
	changed := false
	var task *TaskRecord
	var accepted []*TaskRecord
	var active []*TaskRecord
	for i := range j.Tasks {
		r := &j.Tasks[i]
		if r.SessionID != params.SessionID || r.Generation != epoch {
			continue
		}
		switch r.State {
		case "running", "waiting_user":
			active = append(active, r)
		case "accepted":
			accepted = append(accepted, r)
		}
	}
	if len(active) == 1 {
		task = active[0]
	}
	// Modern bounded admissions distinguish the active slot from the next slot,
	// even when its start frame arrives after both ordered socket writes.
	if frame.Method == "event" && params.Type == "message.start" && len(active) == 0 && len(accepted) == 2 {
		var first *TaskRecord
		for _, r := range accepted {
			if !r.Queued && r.NativeStatus != "queued" && r.ClientRequestID != "" {
				if first != nil {
					first = nil
					break
				}
				first = r
			}
		}
		if first != nil {
			task = first
		}
	}
	if len(active) > 1 || frame.Method == "event" && params.Type == "message.start" && task == nil && len(accepted) > 1 {
		// Legacy multi-input ledgers cannot be reconciled from payload-free start
		// events. Interrupt every ambiguous input, never fabricate separate success.
		for _, r := range append(active, accepted...) {
			r.State = "interrupted"
			r.NativeStatus = "uncertain"
			changed = true
		}
		task = nil
	} else if task == nil && len(accepted) == 1 && (frame.Method != "event" || params.Type == "message.start") {
		task = accepted[0]
	}

	if frame.Method == "event" && params.Type == "session.info" {
		var info struct {
			Cwd      string `json:"cwd"`
			StoredID string `json:"stored_session_id"`
		}
		if json.Unmarshal(params.Payload, &info) == nil && info.Cwd != "" {
			j.Sessions[params.SessionID] = info.Cwd
			if info.StoredID != "" {
				j.StoredSessions[params.SessionID] = info.StoredID
			}
			j.SessionEpochs[params.SessionID] = epoch
		}
	}
	if frame.Method == "event" && params.Type == "tool.complete" && task != nil {
		m.observeResultLocked(task, params.Payload, seq, epoch)
		changed = true
	}
	if frame.Method != "event" && len(frame.ID) > 0 {
		kind := "request"
		switch frame.Method {
		case "approval", "approval.request":
			kind = "approval"
		case "clarify":
			kind = "question"
		}
		id := fmt.Sprintf("%d:request:%s", epoch, rpcID(frame.ID))
		for _, r := range j.Attention {
			if r.ID == id {
				return
			}
		}
		record := AttentionRecord{ID: id, RequestID: rpcID(frame.ID), RawID: frame.ID, SessionID: params.SessionID, Generation: epoch, Kind: kind, Method: frame.Method, State: "pending"}
		if _, reused := j.LegacyReplyIDs[record.RequestID]; reused {
			j.LegacyReplyIDs[record.RequestID] = 0
		} else if len(j.LegacyReplyIDs) < 4096 {
			j.LegacyReplyIDs[record.RequestID] = epoch
		}
		// Only the official nonsensitive request schemas may persist their display.
		// Password/sudo/arbitrary server requests retain no supplied text or params.
		if kind == "approval" || kind == "question" {
			if len(frame.Params) <= 32<<10 {
				record.Params = append(json.RawMessage(nil), frame.Params...)
			}
		}
		if task != nil {
			record.RunID = task.RunID
			record.StoredSessionID = task.StoredSessionID
			task.State = "waiting_user"
		}
		j.Attention = append(j.Attention, record)
		changed = true
	}
	if frame.Method == "event" {
		var payload struct {
			ID         json.RawMessage   `json:"id"`
			RequestID  json.RawMessage   `json:"request_id"`
			RequestIDs []json.RawMessage `json:"request_ids"`
			Status     string            `json:"status"`
		}
		_ = json.Unmarshal(params.Payload, &payload)
		if params.Type == "request.cancel" || params.Type == "approval.cancelled" {
			ids := append(payload.RequestIDs, payload.ID, payload.RequestID)
			for i := range j.Attention {
				r := &j.Attention[i]
				for _, id := range ids {
					if len(id) > 0 && r.Generation == epoch && r.RequestID == rpcID(id) && r.State == "pending" {
						r.State = "expired"
						changed = true
					}
				}
			}
		}
		if task != nil {
			switch params.Type {
			case "message.start":
				task.State = "running"
				if task.NativeTurnSeq == 0 {
					task.NativeTurnSeq = seq
				}
				changed = true
			case "message.complete", "error":
				task.State = "completed"
				if params.Type == "error" || payload.Status == "error" {
					task.State = "failed"
				}
				if payload.Status == "interrupted" || payload.Status == "cancelled" {
					task.State = "interrupted"
				}
				for i := range j.Attention {
					r := &j.Attention[i]
					if r.Generation == epoch && r.SessionID == task.SessionID && r.RunID == task.RunID && r.State == "pending" {
						r.State = "expired"
					}
				}
				j.Attention = append(j.Attention, AttentionRecord{ID: fmt.Sprintf("%d:run:%s:%d", epoch, task.RunID, seq), SessionID: task.SessionID, StoredSessionID: task.StoredSessionID, RunID: task.RunID, Generation: epoch, Kind: task.State, State: "unread"})
				changed = true
			}
		}
	}
	// Never prune pending requests. Terminal inbox entries are bounded separately.
	if len(j.Attention) > 256 {
		for i := 0; i < len(j.Attention) && len(j.Attention) > 256; {
			if j.Attention[i].State != "pending" && j.Attention[i].State != "consuming" {
				j.Attention = append(j.Attention[:i], j.Attention[i+1:]...)
			} else {
				i++
			}
		}
	}
	if changed {
		if err := m.saveControlLocked(); err != nil {
			m.state.LastError = "Не удалось сохранить журнал Hermes; проверьте свободное место"
		}
	}
}

// A reply is consumed DURABLY before the socket write. On an ambiguous write it
// stays uncertain and cannot be retried automatically or from a second device.
func (m *Manager) ControlReply(ctx context.Context, raw json.RawMessage) error {
	var req struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if DecodeControlRequest(raw, &req) != nil || req.ID == "" || len(req.ID) > 512 || !json.Valid(req.Result) || len(req.Result) > 32<<10 {
		return errors.New("некорректный ответ Hermes")
	}
	j := m.control
	if j == nil {
		return ErrNotReady
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	var record *AttentionRecord
	for i := range j.Attention {
		if j.Attention[i].ID == req.ID {
			record = &j.Attention[i]
			break
		}
	}
	if record == nil || record.State != "pending" {
		return errors.New("запрос уже закрыт; обновите список")
	}
	// Holding manager's lifecycle lock through write binds exact socket/epoch.
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.bridge
	if !m.ready || m.closing || b == nil || record.Generation != m.epoch {
		record.State = "expired"
		_ = m.saveControlLocked()
		return errors.New("Hermes перезапущен; старый запрос недействителен")
	}
	if err := validateApprovalReply(record, req.Result); err != nil {
		return err
	}
	record.State = "consuming"
	if err := m.saveControlLocked(); err != nil {
		record.State = "pending"
		return err
	}
	err := b.write(ctx, map[string]any{"jsonrpc": "2.0", "id": record.RawID, "result": req.Result})
	if err != nil {
		record.State = "uncertain"
	} else {
		record.State = "answered"
	}
	if saveErr := m.saveControlLocked(); saveErr != nil {
		return saveErr
	}
	return err
}
