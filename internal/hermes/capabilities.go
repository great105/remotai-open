package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Read the official machine-readable contract from the installed, pinned build,
// not from a fixed UI inventory. No agent/model invocation is used for discovery.
func (m *Manager) contractMethods() map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(filepath.Join(m.checkout, "apps", "shared", "src", "gateway-contract.openrpc.json"))
	if err != nil {
		return out
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return out
	}
	var catalog struct {
		Methods []struct {
			Name string `json:"name"`
		} `json:"methods"`
	}
	if json.Unmarshal(data, &catalog) != nil {
		return out
	}
	for _, method := range catalog.Methods {
		out[method.Name] = true
	}
	return out
}
func (m *Manager) ControlCapabilities() json.RawMessage {
	methods := m.contractMethods()
	s := m.Status()
	var native struct {
		Exclusive bool `json:"per_session_exclusive_submit"`
	}
	if s.Ready && methods["gateway.capabilities"] {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		raw, err := m.RPC(ctx, "gateway.capabilities", map[string]any{})
		cancel()
		if err == nil {
			_ = json.Unmarshal(raw, &native)
		}
	}
	data, _ := json.Marshal(map[string]any{"connected": s.Ready, "generation": s.BackendGeneration, "source": "installed_openrpc+gateway.capabilities", "steer": methods["session.steer"], "interrupt": methods["session.interrupt"], "queue": methods["prompt.submit"] && native.Exclusive, "methods": methods, "model_quota": "unknown", "tools": "catalog_required", "scheduler": "health_required", "workdir": "session_required"})
	return data
}
func (m *Manager) ControlIntent(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var req struct {
		Intent    string `json:"intent"`
		SessionID string `json:"session_id"`
		Text      string `json:"text"`
	}
	if DecodeControlRequest(raw, &req) != nil || req.SessionID == "" || len(req.SessionID) > 256 || len(req.Text) > 128<<10 {
		return nil, errors.New("некорректное управление Hermes")
	}
	method := ""
	switch req.Intent {
	case "steer":
		method = "session.steer"
	case "interrupt":
		method = "session.interrupt"
	default:
		return nil, errors.New("неподдерживаемое намерение")
	}
	if !m.contractMethods()[method] {
		return nil, errors.New("установленный Hermes не объявляет эту возможность; обновите Hermes, черновик сохранён")
	}
	params := map[string]any{"profile": "default", "session_id": req.SessionID}
	if req.Intent == "steer" {
		if req.Text == "" {
			return nil, errors.New("введите уточнение")
		}
		params["text"] = req.Text
	}
	return m.RPC(ctx, method, params)
}
