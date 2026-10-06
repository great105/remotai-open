package agenthooks

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var codexThreadID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// CodexSessionScope identifies whether a notify belongs to the foreground
// thread or a spawned subagent. Codex's notify payload has no parent ID, but
// its session_meta record does. Read only that first record, never the dialog.
// An unknown result is deliberately not treated as a completed foreground turn.
func CodexSessionScope(configHome, threadID string, at time.Time) string {
	_, scope := codexSessionFile(configHome, threadID, at)
	return scope
}

// CodexSessionPath binds runtime metadata to the exact verified foreground
// thread. Never select a recent sibling, subagent, or another account's file.
func CodexSessionPath(configHome, threadID string, at time.Time) string {
	path, scope := codexSessionFile(configHome, threadID, at)
	if scope != "root" {
		return ""
	}
	return path
}

func codexSessionFile(configHome, threadID string, at time.Time) (string, string) {
	if configHome == "" || !codexThreadID.MatchString(threadID) {
		return "", ""
	}
	base := filepath.Join(configHome, "sessions")
	for _, day := range []time.Time{at, at.AddDate(0, 0, -1)} {
		dir := filepath.Join(base, day.Format("2006"), day.Format("01"), day.Format("02"))
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), "-"+threadID+".jsonl") {
				path := filepath.Join(dir, entry.Name())
				if scope := codexScopeFromFile(path, threadID); scope != "" {
					return path, scope
				}
			}
		}
	}
	// Long-lived sessions may have started before yesterday. This slow path
	// runs only for those threads; each hook process has no durable cache.
	matched, scope := "", ""
	_ = filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry == nil || entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), "-"+threadID+".jsonl") {
			scope = codexScopeFromFile(path, threadID)
			if scope != "" {
				matched = path
				return io.EOF
			}
		}
		return nil
	})
	return matched, scope
}

func codexScopeFromFile(path, threadID string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	// session_meta is the first JSONL record. Cap the read even if a damaged
	// file has no newline; never scan prompt or transcript records.
	first, err := bufio.NewReader(io.LimitReader(f, 256<<10)).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return ""
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID             string `json:"id"`
			ParentThreadID string `json:"parent_thread_id"`
			AgentPath      string `json:"agent_path"`
			ForkedFromID   string `json:"forked_from_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(first, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != threadID {
		return ""
	}
	if meta.Payload.ParentThreadID != "" || meta.Payload.AgentPath != "" || meta.Payload.ForkedFromID != "" {
		return "subagent"
	}
	return "root"
}
