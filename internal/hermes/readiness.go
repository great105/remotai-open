package hermes

import (
	"context"
	"encoding/json"
	"errors"
)

// Read-only, explicitly requested diagnostics; never invokes a model/provider or edits tools.
func (m *Manager) ControlReadiness(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if DecodeControlRequest(raw, &req) != nil || req.SessionID == "" || len(req.SessionID) > 256 {
		return nil, errors.New("выберите беседу для проверки инструментов")
	}
	methods := m.contractMethods()
	s := m.Status()
	tools := map[string]any{"status": "unsupported", "remediation": "Обновите Hermes: метод tools.show не объявлен установленным runtime"}
	if methods["tools.show"] {
		data, err := m.RPC(ctx, "tools.show", map[string]any{"session_id": req.SessionID})
		var catalog struct {
			Sections []struct {
				Name  string `json:"name"`
				Tools []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"tools"`
			} `json:"sections"`
			Total *int `json:"total"`
		}
		if err != nil || json.Unmarshal(data, &catalog) != nil || catalog.Total == nil || catalog.Sections == nil {
			tools = map[string]any{"status": "unknown", "remediation": "Проверьте подключение и повторите чтение каталога Hermes"}
		} else {
			tools = map[string]any{"status": "ready", "total": *catalog.Total, "sections": catalog.Sections, "source": "tools.show"}
		}
	}
	data, _ := json.Marshal(map[string]any{"generation": s.BackendGeneration, "connected": s.Ready, "model_quota": "unknown", "tools": tools, "scheduler": map[string]any{"status": "unknown", "remediation": "Откройте Задачи Hermes: там отображается фактический heartbeat scheduler"}, "delivery": map[string]any{"configured": s.DeliveryReady, "enabled": s.DeliveryEnabled, "confirmed": false}, "workdir": map[string]any{"status": "session_scoped"}})
	return data, nil
}
