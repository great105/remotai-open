package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"tgcontrol/internal/hermes"
)

type hermesActivityEntry struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	ToolName        string   `json:"tool_name"`
	SourceTimeText  string   `json:"source_time_text"`
	Status          string   `json:"status,omitempty"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
}

type hermesActivitySnapshot struct {
	Supported         bool                  `json:"supported"`
	Available         bool                  `json:"available"`
	Truncated         bool                  `json:"truncated"`
	HistoryIncomplete bool                  `json:"history_incomplete"`
	Source            string                `json:"source"`
	SourceTimeZone    string                `json:"source_time_zone"`
	CapturedAtMS      int64                 `json:"captured_at_ms"`
	Entries           []hermesActivityEntry `json:"entries"`
	WindowID          string                `json:"window_id,omitempty"`
}

var hermesActivityKey = rand.Text()
var hermesActivityStart = regexp.MustCompile(`^([0-2][0-9]:[0-5][0-9]:[0-5][0-9]) tool[ ]{1,9}\| -> ([A-Za-z][A-Za-z0-9_]{0,79})\(`)
var hermesActivityResult = regexp.MustCompile(`^([0-2][0-9]:[0-5][0-9]:[0-5][0-9]) result[ ]{1,9}\| ([A-Za-z][A-Za-z0-9_]{0,79}) (ok|ERROR)(?: ([0-9]{1,7}\.[0-9])s)?: `)

// Names are a conservative literal registry.register(name=...) subset pinned
// to installed Hermes 819cc3cbe02104c420ea32f1f1a924247d42eaf8 (tools/*.py).
// Table-driven/plugin/MCP/future names not represented here are omitted. This
// is a privacy vocabulary, not a claim of runtime tool availability.
var hermesActivityTools = map[string]bool{
	"annotate_preview":         true,
	"apply_layout":             true,
	"browser_cdp":              true,
	"browser_dialog":           true,
	"browser_exec":             true,
	"browser_vault_enter_code": true,
	"browser_vault_fill":       true,
	"browser_vault_list":       true,
	"browser_vault_save_login": true,
	"browser_vault_unlock":     true,
	"clarify":                  true,
	"close_terminal":           true,
	"computer_use":             true,
	"cronjob_manage":           true,
	"delegate_task":            true,
	"desktop_preview":          true,
	"desktop_project":          true,
	"drive_preview":            true,
	"execute_code":             true,
	"feishu_doc_read":          true,
	"focus_pane":               true,
	"gui_tour":                 true,
	"image_generate":           true,
	"manage_catalog":           true,
	"manage_connections":       true,
	"memory":                   true,
	"patch":                    true,
	"process_manage":           true,
	"react_to_message":         true,
	"read_file":                true,
	"read_terminal":            true,
	"read_window_below":        true,
	"search_files":             true,
	"session_search":           true,
	"show_tip":                 true,
	"skill_manage":             true,
	"skill_view":               true,
	"skills_list":              true,
	"terminal":                 true,
	"text_to_speech":           true,
	"todo_list":                true,
	"video_analyze":            true,
	"video_generate":           true,
	"vision_analyze":           true,
	"web_extract":              true,
	"web_search":               true,
	"write_file":               true,
	"x_search":                 true,
}

func hermesActivityLiteralID(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

func hermesActivityOpaque(value string) string {
	h := hmac.New(sha256.New, []byte(hermesActivityKey))
	_, _ = h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// This is a replace-only window, NOT an authenticated audit trail or native
// call IDs. Arbitrary log text cannot prove the origin of a forged known-name
// header. Publish only registry-vetted metadata; never retain/hash raw suffixes.
func hermesActivityProject(text string, truncated bool) []hermesActivityEntry {
	entries := make([]hermesActivityEntry, 0, 200)
	lines := strings.Split(text, "\n")
	// Native writer newline-terminates every event. A byte tail can begin midway
	// through an event; even a plausible first header is not evidence of a boundary.
	if truncated && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > 0 {
		lines = lines[:len(lines)-1]
	} // drop empty terminator OR partial last row
	for _, line := range lines {
		if len(line) > 4096 || strings.ContainsAny(line, "\r\x00") {
			continue
		}
		var entry hermesActivityEntry
		if m := hermesActivityStart.FindStringSubmatch(line); m != nil && m[1][:2] < "24" && hermesActivityTools[m[2]] && strings.HasSuffix(line, ")") {
			entry = hermesActivityEntry{Kind: "tool_start", ToolName: m[2], SourceTimeText: m[1]}
		} else if m := hermesActivityResult.FindStringSubmatch(line); m != nil && m[1][:2] < "24" && hermesActivityTools[m[2]] {
			entry = hermesActivityEntry{Kind: "tool_result", ToolName: m[2], SourceTimeText: m[1], Status: strings.ToLower(m[3])}
			if m[4] != "" {
				duration, err := strconv.ParseFloat(m[4], 64)
				if err != nil || duration > 86400 {
					continue
				}
				entry.DurationSeconds = &duration
			}
		} else {
			continue
		}
		if len(entries) == 200 {
			copy(entries, entries[1:])
			entries = entries[:199]
		}
		entries = append(entries, entry)
	}
	return entries
}

type hermesActivityTail struct {
	SubagentID string `json:"subagent_id"`
	Available  bool   `json:"available"`
	Truncated  bool   `json:"truncated"`
	Text       string `json:"text"`
}

// Bound before decoding. Reject duplicate keys/null/type confusion instead of
// allowing JSON's last-key-wins behavior to manufacture an empty known window.
func hermesActivityDecode(raw json.RawMessage, childID string) (hermesActivityTail, error) {
	var tail hermesActivityTail
	invalid := errors.New("invalid native activity envelope")
	if len(raw) > 128<<10 || !utf8.Valid(raw) {
		return tail, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return tail, invalid
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return tail, invalid
		}
		key, ok := keyToken.(string)
		if !ok {
			return tail, invalid
		}
		if _, exists := fields[key]; exists {
			return tail, invalid
		}
		switch key {
		case "subagent_id", "available", "truncated", "text":
		default:
			return tail, invalid
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return tail, invalid
		}
		fields[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || len(fields) != 4 {
		return tail, invalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return tail, invalid
	}
	if json.Unmarshal(fields["subagent_id"], &tail.SubagentID) != nil || tail.SubagentID != childID || json.Unmarshal(fields["available"], &tail.Available) != nil || json.Unmarshal(fields["truncated"], &tail.Truncated) != nil || json.Unmarshal(fields["text"], &tail.Text) != nil || len(tail.Text) > 16<<10 || !utf8.ValidString(tail.Text) {
		return hermesActivityTail{}, invalid
	}
	return tail, nil
}

func (s *Server) apiHermesSubagentActivity(w http.ResponseWriter, r *http.Request, uid int64) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		jsonErrorCode(w, 405, "method_not_allowed", "Этот способ запроса Hermes недоступен.", nil)
		return
	}
	if r.URL.EscapedPath() != "/api/hermes/subagents/activity" {
		jsonErrorCode(w, 400, "bad_request", "Некорректный адрес журнала Hermes.", nil)
		return
	}
	query, queryErr := hermesAuthenticatedQuery(r)
	valid := queryErr == nil && len(query) == 2
	for key, values := range query {
		if (key != "session_id" && key != "subagent_id") || len(values) != 1 || !hermesActivityLiteralID(values[0]) {
			valid = false
		}
	}
	if !valid {
		jsonErrorCode(w, 400, "bad_request", "Некорректные параметры журнала Hermes.", nil)
		return
	}
	sessionID, childID := query.Get("session_id"), query.Get("subagent_id")
	mgr := s.hermesForRequest(w, uid)
	if mgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	generation := mgr.Status().BackendGeneration
	raw, err := mgr.RPC(ctx, "subagent.tail", map[string]string{"session_id": sessionID, "subagent_id": childID})
	capturedAt := time.Now().UnixMilli()
	snapshot := hermesActivitySnapshot{Supported: true, HistoryIncomplete: true, Source: "native_live_log", SourceTimeZone: "unknown", CapturedAtMS: capturedAt, Entries: []hermesActivityEntry{}}
	if generation != mgr.Status().BackendGeneration {
		jsonErrorCode(w, 502, "hermes_activity_generation_changed", "Изменилась сессия Hermes. Обновите журнал.", nil)
		return
	}
	if err != nil {
		var rpcErr *hermes.RPCError
		if errors.As(err, &rpcErr) {
			if rpcErr.Code == -32601 {
				snapshot.Supported = false
				jsonResp(w, snapshot)
				return
			}
			message := "Не удалось прочитать журнал Hermes."
			if rpcErr.Code == 4001 {
				message = "session not found or not owned by this transport"
			}
			jsonErrorCode(w, 502, "hermes_rpc_failed", message, map[string]string{"rpc_code": strconv.Itoa(rpcErr.Code)})
		} else {
			jsonErrorCode(w, 502, "hermes_rpc_failed", "Не удалось прочитать журнал Hermes.", nil)
		}
		return
	}
	tail, err := hermesActivityDecode(raw, childID)
	if err != nil {
		jsonErrorCode(w, 502, "hermes_activity_invalid", "Некорректный журнал Hermes.", nil)
		return
	}
	snapshot.Available = tail.Available
	snapshot.Truncated = tail.Truncated
	if tail.Available {
		snapshot.Entries = hermesActivityProject(tail.Text, tail.Truncated)
		safe, _ := json.Marshal(snapshot.Entries)
		snapshot.WindowID = hermesActivityOpaque(fmt.Sprintf("%d:%p:%d:%q:%q:%t:%s", uid, mgr, generation, sessionID, childID, tail.Truncated, safe))
		for i := range snapshot.Entries {
			snapshot.Entries[i].ID = hermesActivityOpaque(fmt.Sprintf("%s:%d", snapshot.WindowID, i))
		}
	}
	jsonResp(w, snapshot)
}
