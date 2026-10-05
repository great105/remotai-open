package hermes

// The journal records admission and outcome, never executes or replays work.
// Hermes remains the sole owner of the agent and its native busy queue.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// AdmissionRejection is reserved for known refusals before journal or wire.
// Fingerprint conflicts and ambiguous outcomes MUST NOT use this type.
type AdmissionRejection struct{ Reason string }

func (e *AdmissionRejection) Error() string           { return e.Reason }
func (e *AdmissionRejection) AdmissionRejected() bool { return true }

type TaskRecord struct {
	ClientRequestID string `json:"client_request_id"`
	RunID           string `json:"run_id"`
	SessionID       string `json:"session_id"`
	StoredSessionID string `json:"stored_session_id"`
	Generation      uint64 `json:"generation"`
	State           string `json:"state"`
	NativeStatus    string `json:"native_status,omitempty"`
	Queued          bool   `json:"queued,omitempty"`
	NativeTurnSeq   uint64 `json:"native_turn_seq,omitempty"`
	Fingerprint     string `json:"fingerprint"`
	NativeTextHash  string `json:"native_text_hash,omitempty"`
	CreatedAt       string `json:"created_at"`
	Cwd             string `json:"cwd,omitempty"`
}
type AttentionRecord struct {
	ID              string          `json:"id"`
	RequestID       string          `json:"request_id"`
	RawID           json.RawMessage `json:"raw_id,omitempty"`
	SessionID       string          `json:"session_id"`
	StoredSessionID string          `json:"stored_session_id"`
	RunID           string          `json:"run_id"`
	Generation      uint64          `json:"generation"`
	Kind            string          `json:"kind"`
	Method          string          `json:"method"`
	State           string          `json:"state"`
	Params          json.RawMessage `json:"params,omitempty"`
	Delivery        string          `json:"delivery,omitempty"`
}
type controlJournal struct {
	mu             sync.Mutex
	sealed         bool
	Tasks          []TaskRecord      `json:"tasks"`
	Attention      []AttentionRecord `json:"attention"`
	Epoch          uint64            `json:"epoch"`
	Results        []ResultRecord    `json:"results"`
	Sessions       map[string]string `json:"sessions"`
	LegacyReplyIDs map[string]uint64 `json:"legacy_reply_ids,omitempty"`
	StoredSessions map[string]string `json:"-"`
	SessionEpochs  map[string]uint64 `json:"-"`
}

func (m *Manager) loadControl() error {
	j := &controlJournal{Tasks: []TaskRecord{}}
	data, err := os.ReadFile(filepath.Join(m.root, "control.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("журнал Hermes слишком большой")
	}
	if len(data) > 0 {
		if err = json.Unmarshal(data, j); err != nil {
			return err
		}
	}
	if len(j.Tasks) > 256 {
		return errors.New("журнал Hermes переполнен")
	}
	for i := range j.Tasks {
		switch j.Tasks[i].State {
		case "accepted", "running", "waiting_user":
			j.Tasks[i].State = "interrupted"
			j.Tasks[i].NativeStatus = "uncertain"
		}
	}
	for i := range j.Attention {
		if j.Attention[i].Delivery == "sending" {
			j.Attention[i].Delivery = "uncertain"
		}
		if j.Attention[i].State == "pending" || j.Attention[i].State == "consuming" {
			j.Attention[i].State = "expired"
		}
	}
	if j.Attention == nil {
		j.Attention = []AttentionRecord{}
	}
	if j.LegacyReplyIDs == nil {
		j.LegacyReplyIDs = map[string]uint64{}
		for _, r := range j.Attention {
			if _, exists := j.LegacyReplyIDs[r.RequestID]; exists {
				j.LegacyReplyIDs[r.RequestID] = 0
			} else if len(j.LegacyReplyIDs) < 4096 {
				j.LegacyReplyIDs[r.RequestID] = r.Generation
			}
		}
	}
	// Reserve a non-overlapping epoch range for every application lifetime.
	j.Epoch += 1000000
	m.epoch = j.Epoch
	if j.Results == nil {
		j.Results = []ResultRecord{}
	}
	j.Sessions = map[string]string{}
	j.StoredSessions = map[string]string{}
	j.SessionEpochs = map[string]uint64{}
	m.control = j
	return m.saveControlLocked()
}
func (m *Manager) saveControlLocked() error {
	if m.control.sealed {
		return ErrClosed
	}
	data, err := json.Marshal(m.control)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("журнал Hermes переполнен")
	}
	f, err := os.CreateTemp(m.root, ".control-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.root, "control.json"))
}
func (m *Manager) ControlSnapshot() json.RawMessage {
	if m.control == nil {
		return json.RawMessage(`{"tasks":[]}`)
	}
	j := m.control
	j.mu.Lock()
	defer j.mu.Unlock()
	m.mu.Lock()
	epoch := m.epoch
	live := m.ready && !m.closing
	m.mu.Unlock()
	changed := false
	for i := range j.Tasks {
		r := &j.Tasks[i]
		if (r.Generation != epoch || !live) && (r.State == "accepted" || r.State == "running" || r.State == "waiting_user") {
			r.State = "interrupted"
			r.NativeStatus = "uncertain"
			changed = true
		}
	}
	for i := range j.Attention {
		r := &j.Attention[i]
		if (r.Generation != epoch || !live) && (r.State == "pending" || r.State == "consuming") {
			r.State = "expired"
			changed = true
		}
	}
	if changed {
		if err := m.saveControlLocked(); err != nil {
			m.mu.Lock()
			m.state.LastError = "Не удалось сохранить восстановленный журнал Hermes"
			m.mu.Unlock()
		}
	}
	data, _ := json.Marshal(j)
	return data
}
func (m *Manager) ReadAttention(id string) error {
	j := m.control
	if j == nil {
		return ErrNotReady
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := range j.Attention {
		r := &j.Attention[i]
		if r.ID == id {
			if r.State == "read" {
				return nil
			}
			if r.State != "unread" {
				return errors.New("активный запрос нельзя отметить прочитанным")
			}
			r.State = "read"
			if err := m.saveControlLocked(); err != nil {
				r.State = "unread"
				return err
			}
			return nil
		}
	}
	return errors.New("уведомление не найдено")
}
func (m *Manager) SubmitTask(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var req struct {
		ClientRequestID string  `json:"client_request_id"`
		SessionID       string  `json:"session_id"`
		StoredSessionID string  `json:"stored_session_id"`
		Text            string  `json:"text"`
		Queued          bool    `json:"queued"`
		Generation      *uint64 `json:"generation,omitempty"`
	}
	if DecodeControlRequest(raw, &req) != nil || len(req.ClientRequestID) < 1 || len(req.ClientRequestID) > 128 || req.SessionID == "" || len(req.SessionID) > 256 || len(req.StoredSessionID) > 256 || req.Text == "" || len(req.Text) > 128<<10 {
		return nil, errors.New("некорректная задача Hermes")
	}
	sessionIdentity := req.StoredSessionID
	if sessionIdentity == "" {
		sessionIdentity = req.SessionID
	}
	canonical, _ := json.Marshal(struct {
		Session string
		Text    string
		Queued  bool
	}{sessionIdentity, req.Text, req.Queued})
	sum := sha256.Sum256(canonical)
	fingerprint := hex.EncodeToString(sum[:])
	j := m.control
	if j == nil {
		return nil, ErrNotReady
	}
	// A receipt remains retrievable while offline or after a capability downgrade.
	j.mu.Lock()
	for _, task := range j.Tasks {
		if task.ClientRequestID == req.ClientRequestID {
			j.mu.Unlock()
			if task.Fingerprint != fingerprint {
				return nil, errors.New("ID задачи уже использован с другим содержимым")
			}
			return taskReceipt(task), nil
		}
	}
	j.mu.Unlock()
	if req.Queued {
		var caps struct {
			Queue bool `json:"queue"`
		}
		_ = json.Unmarshal(m.ControlCapabilities(), &caps)
		if !caps.Queue {
			return nil, errors.New("нативная очередь не подтверждена Hermes; обновите Hermes, черновик сохранён")
		}
	}
	b, err := m.ensureBridge(ctx)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, task := range j.Tasks {
		if task.ClientRequestID == req.ClientRequestID {
			if task.Fingerprint != fingerprint {
				return nil, errors.New("ID задачи уже использован с другим содержимым")
			}
			return taskReceipt(task), nil
		}
	}
	m.mu.Lock()
	generation, ready, closing, sameBridge := m.epoch, m.ready, m.closing, m.bridge == b
	m.mu.Unlock()
	if !ready || closing || !sameBridge {
		return nil, ErrNotReady
	}
	if req.Generation != nil && *req.Generation != generation {
		return nil, errors.New("Hermes перезапущен; обновите беседу, черновик сохранён")
	}
	if j.SessionEpochs[req.SessionID] != generation || req.StoredSessionID != "" && req.StoredSessionID != j.StoredSessions[req.SessionID] {
		return nil, errors.New("Беседа не подтверждена этим Hermes; откройте её заново, черновик сохранён")
	}
	if len(j.Tasks) >= 256 {
		return nil, &AdmissionRejection{Reason: "журнал задач заполнен; новые задачи не принимаются"}
	}
	// The native runtime merges multiple text-only queued inputs. Admit at most
	// one active input plus one next-turn input; reject before persistence/write.
	// Python str.strip also strips U+001C..U+001F (Go TrimSpace does not).
	nativeText := strings.TrimFunc(req.Text, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
	nativeSum := sha256.Sum256([]byte(nativeText))
	nativeHash := hex.EncodeToString(nativeSum[:])
	active, queued := 0, 0
	for _, r := range j.Tasks {
		if r.SessionID != req.SessionID || r.Generation != generation {
			continue
		}
		if r.State == "accepted" || r.State == "running" || r.State == "waiting_user" {
			active++
			// Legacy active receipts have no normalized hash: conservatively refuse
			// queued work rather than admit an input the native runtime may discard.
			if req.Queued && (r.NativeTextHash == "" || r.NativeTextHash == nativeHash) {
				return nil, &AdmissionRejection{Reason: "Повтор активной задачи или её содержимое не подтверждено; дождитесь завершения, черновик сохранён"}
			}
			if r.State == "accepted" && (r.Queued || r.NativeStatus == "queued") {
				queued++
			}
		}
	}
	if active > 0 && (!req.Queued || active >= 2 || queued > 0) {
		return nil, &AdmissionRejection{Reason: "Следующая задача уже ожидает или беседа занята; дождитесь завершения, черновик сохранён"}
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	task := TaskRecord{ClientRequestID: req.ClientRequestID, RunID: hex.EncodeToString(id), SessionID: req.SessionID, StoredSessionID: j.StoredSessions[req.SessionID], Generation: generation, State: "accepted", Queued: req.Queued && active > 0, Fingerprint: fingerprint, NativeTextHash: nativeHash, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	task.Cwd = j.Sessions[req.SessionID]
	j.Tasks = append(j.Tasks, task)
	if err := m.saveControlLocked(); err != nil {
		j.Tasks = j.Tasks[:len(j.Tasks)-1]
		return nil, err
	}
	// Admission is durable BEFORE the single socket write. No retry after any
	// ambiguous boundary, including disconnect and application restart.
	life, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	// Holding j.mu through this write gives admission and wire the SAME order.
	// Never acquire a replacement socket or retry already-admitted work.
	await, writeErr := b.beginCall(life, "prompt.submit", map[string]any{"profile": "default", "session_id": req.SessionID, "text": req.Text, "queued": req.Queued, "surface": "desktop"})
	go func() {
		defer cancel()
		var result json.RawMessage
		err := writeErr
		if err == nil {
			result, err = await()
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		for i := range j.Tasks {
			r := &j.Tasks[i]
			if r.RunID != task.RunID {
				continue
			}
			var reply struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(result, &reply)
			// A terminal event can precede the RPC response; never revive it.
			if r.State != "accepted" {
				break
			}
			r.NativeStatus = reply.Status
			if err != nil || (reply.Status != "streaming" && reply.Status != "queued") {
				r.State = "interrupted"
				r.NativeStatus = "uncertain"
			} else if reply.Status == "streaming" {
				r.State = "running"
			}
			_ = m.saveControlLocked()
			break
		}
	}()
	return taskReceipt(task), nil
}
func taskReceipt(task TaskRecord) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"status": task.State, "native_status": task.NativeStatus, "run_id": task.RunID, "client_request_id": task.ClientRequestID})
	return data
}
