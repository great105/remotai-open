package agenthooks

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

const codexRuntimeReadLimit = 4 << 20
const codexRuntimeRecordLimit = 64 << 10

// CodexRuntimeReader consumes only lifecycle metadata from one verified root
// thread. It never returns dialog, tool output, prompts, or assistant messages.
// Automatic goal continuations emit task_started without a PTY Enter; notify
// alone cannot tell us that a previously completed turn has started again.
// See openai/codex, protocol TurnStarted/TurnComplete and rollout/policy.rs.
// Owned by the detector goroutine. Reads are bounded and incremental.
type CodexRuntimeReader struct {
	ConfigHome, ThreadID string
	path                 string
	offset               int64
	pending              []byte
	discard              bool
	status, turnID       string
	at                   time.Time
}

func (r *CodexRuntimeReader) TurnID() string { return r.turnID }

func NewCodexRuntimeReader(configHome, threadID string, at time.Time) *CodexRuntimeReader {
	if !filepath.IsAbs(configHome) {
		return nil
	}
	path := CodexSessionPath(configHome, threadID, at)
	if path == "" {
		return nil
	}
	return &CodexRuntimeReader{ConfigHome: configHome, ThreadID: threadID, path: path}
}

func (r *CodexRuntimeReader) Read(now time.Time) (string, time.Time) {
	// Recheck identity even if the file was replaced without shrinking.
	if r == nil || codexScopeFromFile(r.path, r.ThreadID) != "root" {
		return "", time.Time{}
	}
	f, err := os.Open(r.path)
	if err != nil {
		return "", time.Time{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", time.Time{}
	}
	if st.Size() < r.offset {
		r.offset, r.pending, r.discard = 0, nil, false
		r.status, r.turnID, r.at = "", "", time.Time{}
	}
	if _, err = f.Seek(r.offset, io.SeekStart); err != nil {
		return "", time.Time{}
	}
	buf := make([]byte, 32<<10)
	remaining := int64(codexRuntimeReadLimit)
	for remaining > 0 {
		n, readErr := f.Read(buf[:min(int64(len(buf)), remaining)])
		if n > 0 {
			r.offset += int64(n)
			remaining -= int64(n)
			r.consume(buf[:n], now)
		}
		if readErr != nil {
			if readErr != io.EOF {
				return "", time.Time{}
			}
			break
		}
		if n == 0 {
			break
		}
	}
	// Do not report an old completion while catching up with a large file.
	if r.offset < st.Size() {
		return "", time.Time{}
	}
	return r.status, r.at
}

func (r *CodexRuntimeReader) consume(data []byte, now time.Time) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		part := data
		if i >= 0 {
			part = data[:i]
		}
		if !r.discard {
			if len(r.pending)+len(part) > codexRuntimeRecordLimit {
				r.pending, r.discard = nil, true
			} else {
				r.pending = append(r.pending, part...)
			}
		}
		if i < 0 {
			return
		}
		if !r.discard {
			r.record(r.pending, now)
		}
		r.pending, r.discard = r.pending[:0], false
		data = data[i+1:]
	}
}

func (r *CodexRuntimeReader) record(line []byte, now time.Time) {
	// Decode only type, timestamp and turn IDs; no message fields are declared.
	var item struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Type       string `json:"type"`
			TurnID     string `json:"turn_id"`
			RootTurnID string `json:"root_turn_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &item) != nil || item.Type != "event_msg" || item.Payload.TurnID == "" {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, item.Timestamp)
	if err != nil || at.Before(r.at) || at.After(now.Add(5*time.Second)) {
		return
	}
	switch item.Payload.Type {
	case "task_started", "turn_started":
		if item.Payload.RootTurnID != "" && item.Payload.RootTurnID != item.Payload.TurnID {
			return
		}
		r.status, r.turnID, r.at = "working", item.Payload.TurnID, at
	case "task_complete", "turn_complete", "turn_aborted":
		if r.turnID != "" && r.turnID != item.Payload.TurnID {
			return
		}
		r.status, r.turnID, r.at = "ready", item.Payload.TurnID, at
	}
}
