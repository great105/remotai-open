package tokenusage

import (
	"bytes"
	"encoding/json"
	"time"
)

// seenLimit — сколько последних ключей ответа Claude помнить в файле. Дубли
// одного ответа идут подряд (по блокам content), так что хватает немногих.
const seenLimit = 64

type claudeLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			Output     int64 `json:"output_tokens"`
			Details    struct {
				Thinking int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"message"`
}

var usageMarker = []byte(`"usage"`)

// parseClaude разбирает полные строки data (без хвоста без перевода строки)
// одного файла сессии Claude Code и отдаёт строки итогов.
func parseClaude(st *fileState, data []byte, src Source, fallback time.Time, emit func(Row)) {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 || !bytes.Contains(line, usageMarker) {
			continue
		}
		var l claudeLine
		if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.Message.Usage == nil {
			continue
		}
		if l.Message.Model == "" || l.Message.Model == "<synthetic>" {
			continue
		}
		if l.Message.ID != "" {
			key := l.Message.ID + "|" + l.RequestID
			if containsStr(st.Seen, key) {
				continue
			}
			st.Seen = append(st.Seen, key)
			if len(st.Seen) > seenLimit {
				st.Seen = st.Seen[len(st.Seen)-seenLimit:]
			}
		}
		if l.Cwd != "" {
			st.Project = l.Cwd
		}
		if l.SessionID != "" {
			st.Session = l.SessionID
		}
		u := l.Message.Usage
		day, at := dayOf(l.Timestamp, fallback)
		emit(Row{
			Day: day, Provider: src.Provider, Account: src.AccountID,
			Project: st.Project, Model: l.Message.Model, Session: st.Session, Last: at,
			Tokens: Tokens{
				Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
				Reasoning: u.Details.Thinking, Calls: 1,
			},
		})
	}
}

type codexUsage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
}

type codexLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		Model     string `json:"model"`
		Info      *struct {
			Total *codexUsage `json:"total_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

var (
	codexMeta    = []byte(`"session_meta"`)
	codexContext = []byte(`"turn_context"`)
	codexCount   = []byte(`"token_count"`)
)

// codexTokens переводит накопленный итог Codex в наши счётчики. У OpenAI
// input_tokens включает прочитанное из кеша — вычитаем, чтобы не считать дважды.
func codexTokens(u codexUsage) Tokens {
	fresh := u.Input - u.Cached
	if fresh < 0 {
		fresh = 0
	}
	return Tokens{Input: fresh, Output: u.Output, CacheRead: u.Cached, CacheWrite: u.CacheWrite, Reasoning: u.Reasoning}
}

func minus(a, b Tokens) Tokens {
	return Tokens{
		Input: a.Input - b.Input, Output: a.Output - b.Output, CacheRead: a.CacheRead - b.CacheRead,
		CacheWrite: a.CacheWrite - b.CacheWrite, Reasoning: a.Reasoning - b.Reasoning,
	}
}

func anyNegative(t Tokens) bool {
	return t.Input < 0 || t.Output < 0 || t.CacheRead < 0 || t.CacheWrite < 0 || t.Reasoning < 0
}

// parseCodex разбирает строки rollout-файла Codex. Итог в token_count
// накопительный: расход хода — разница с прошлым итогом этого файла.
func parseCodex(st *fileState, data []byte, src Source, fallback time.Time, emit func(Row)) {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		meta := bytes.Contains(line, codexMeta)
		ctx := bytes.Contains(line, codexContext)
		count := bytes.Contains(line, codexCount)
		if !meta && !ctx && !count {
			continue
		}
		var l codexLine
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		switch {
		case l.Type == "session_meta":
			if id := firstNonEmpty(l.Payload.SessionID, l.Payload.ID); id != "" {
				st.Session = id
			}
			if l.Payload.Cwd != "" {
				st.Project = l.Payload.Cwd
			}
		case l.Type == "turn_context":
			if l.Payload.Model != "" {
				st.Model = l.Payload.Model
			}
			if l.Payload.Cwd != "" {
				st.Project = l.Payload.Cwd
			}
		case l.Type == "event_msg" && l.Payload.Type == "token_count":
			if l.Payload.Info == nil || l.Payload.Info.Total == nil {
				continue
			}
			total := codexTokens(*l.Payload.Info.Total)
			delta := minus(total, st.CodexTotal)
			if anyNegative(delta) {
				// Итог пошёл назад — сессию начали заново (compact/resume).
				delta = total
			}
			st.CodexTotal = total
			if delta.Total() == 0 {
				continue
			}
			delta.Calls = 1
			day, at := dayOf(l.Timestamp, fallback)
			model := st.Model
			if model == "" {
				model = "codex"
			}
			emit(Row{
				Day: day, Provider: src.Provider, Account: src.AccountID,
				Project: st.Project, Model: model, Session: st.Session, Last: at, Tokens: delta,
			})
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
