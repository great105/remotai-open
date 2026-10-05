package hermes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const maxArtifact = 8 << 20

func sensitiveArtifactPath(path string) bool {
	for _, part := range strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.HasPrefix(part, ".env") || strings.HasPrefix(part, ".tgcontrol") || part == ".hermes" || part == ".ssh" || part == ".git" || part == "keystore.properties" || part == "secrets.env" || part == "auth.json" || strings.HasSuffix(part, ".enc") || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") {
			return true
		}
	}
	return false
}

type ResultRecord struct {
	ID              string `json:"id"`
	RunID           string `json:"run_id"`
	SessionID       string `json:"session_id"`
	StoredSessionID string `json:"stored_session_id"`
	Generation      uint64 `json:"generation"`
	EventSeq        uint64 `json:"event_seq"`
	ToolID          string `json:"tool_id"`
	Tool            string `json:"tool"`
	Kind            string `json:"kind"`
	Verified        bool   `json:"verified"`
	Outcome         string `json:"outcome"`
	Path            string `json:"path,omitempty"`
	Root            string `json:"root,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	Preview         string `json:"preview,omitempty"`
	Diff            string `json:"diff,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
}

func scopedArtifact(root, path string) ([]byte, string, error) {
	if root == "" || path == "" || strings.ContainsAny(path, "\x00\r\n") || strings.Contains(path, ":") && !filepath.IsAbs(path) {
		return nil, "", errors.New("нет подтверждённой папки файла")
	}
	// Rel removes the root ancestry: validate its normalized absolute path too.
	fullRoot, err := filepath.Abs(root)
	if err != nil || sensitiveArtifactPath(filepath.Clean(fullRoot)) {
		return nil, "", errors.New("приватный файл не может быть результатом")
	}
	root = filepath.Clean(fullRoot)
	rel := path
	if filepath.IsAbs(path) {
		var err error
		rel, err = filepath.Rel(root, path)
		if err != nil {
			return nil, "", err
		}
	}
	rel = filepath.Clean(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, "", errors.New("файл вне папки беседы")
	}
	if strings.Contains(rel, ":") || sensitiveArtifactPath(rel) {
		return nil, "", errors.New("приватный файл не может быть результатом")
	}
	// Reject links atomically for every component, including the root ancestry.
	f, err := openArtifactNoFollow(root, rel, nil)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxArtifact {
		return nil, "", errors.New("файл недоступен или слишком большой")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxArtifact+1))
	if err != nil || len(data) > maxArtifact {
		return nil, "", errors.New("файл слишком большой")
	}
	return data, rel, nil
}
func (m *Manager) observeResultLocked(task *TaskRecord, payload json.RawMessage, seq, epoch uint64) {
	if task == nil {
		return
	}
	var tool struct {
		ID   string `json:"tool_id"`
		Name string `json:"name"`
		Args struct {
			Path string `json:"path"`
		} `json:"args"`
		Result json.RawMessage `json:"result"`
		Diff   string          `json:"inline_diff"`
	}
	if json.Unmarshal(payload, &tool) != nil || tool.ID == "" {
		return
	}
	id := fmt.Sprintf("%d:%s:tool:%s", epoch, task.RunID, tool.ID)
	for _, r := range m.control.Results {
		if r.Generation == epoch && r.RunID == task.RunID && r.ToolID == tool.ID {
			return
		}
	}
	record := ResultRecord{ID: id, RunID: task.RunID, SessionID: task.SessionID, StoredSessionID: task.StoredSessionID, Generation: epoch, EventSeq: seq, ToolID: tool.ID, Tool: tool.Name, Kind: "tool", Outcome: "reported"}
	var result struct {
		Verified bool            `json:"verified"`
		Success  *bool           `json:"success"`
		Error    json.RawMessage `json:"error"`
		ExitCode *int            `json:"exit_code"`
		Modified []string        `json:"files_modified"`
		Resolved string          `json:"resolved_path"`
	}
	if json.Unmarshal(tool.Result, &result) == nil {
		record.ExitCode = result.ExitCode
		if result.ExitCode != nil {
			record.Verified = true
			record.Outcome = "failed"
			if *result.ExitCode == 0 && (result.Success == nil || *result.Success) && (len(result.Error) == 0 || string(result.Error) == "null" || string(result.Error) == `""`) {
				record.Outcome = "completed"
			}
		}
	}
	// Raw previews/diffs may contain credentials, even for ordinary filenames.
	// Keep only provenance/hash durably; content is available by explicit scoped
	// download. Never copy arbitrary tool output into the durable result journal.
	noError := len(result.Error) == 0 || string(result.Error) == "null" || string(result.Error) == `""`
	if (tool.Name == "write_file" || tool.Name == "patch" || tool.Name == "read_file") && (result.Verified || result.Success != nil && *result.Success) && (result.Success == nil || *result.Success) && noError {
		paths := result.Modified
		// Native operation identity wins over the caller's logical path. Never
		// verify an unrelated same-basename file in the accepted working folder.
		if result.Resolved != "" {
			if len(paths) == 0 {
				paths = []string{result.Resolved}
			} else if len(paths) != 1 || filepath.Clean(paths[0]) != filepath.Clean(result.Resolved) {
				paths = []string{""} // conflicting native evidence: fail closed
			}
		} else if len(paths) == 0 && tool.Args.Path != "" {
			paths = []string{tool.Args.Path}
		}
		if len(paths) > 16 {
			paths = paths[:16]
		}
		if len(paths) == 0 {
			paths = []string{""}
		}
		for index, path := range paths {
			file := record
			if len(paths) > 1 {
				file.ID = fmt.Sprintf("%s:file:%d", id, index)
			}
			file.Kind = "file"
			file.Verified = false
			file.Outcome = "unverified"
			if data, rel, err := scopedArtifact(task.Cwd, path); err == nil {
				file.Path = rel
				file.Root = task.Cwd
				hash := sha256.Sum256(data)
				file.SHA256 = hex.EncodeToString(hash[:])
				file.Verified = true
				file.Outcome = "file_verified"

			}
			m.control.Results = append(m.control.Results, file)
		}
	} else {
		m.control.Results = append(m.control.Results, record)
	}
	if len(m.control.Results) > 256 {
		m.control.Results = m.control.Results[len(m.control.Results)-256:]
	}
}
func (m *Manager) ReadArtifact(id string) ([]byte, string, error) {
	j := m.control
	if j == nil {
		return nil, "", ErrNotReady
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, r := range j.Results {
		if r.ID == id && r.Kind == "file" && r.Verified {
			data, rel, err := scopedArtifact(r.Root, r.Path)
			if err != nil {
				return nil, "", err
			}
			hash := sha256.Sum256(data)
			if hex.EncodeToString(hash[:]) != r.SHA256 {
				return nil, "", errors.New("файл изменился после результата; требуется новая проверка")
			}
			return data, filepath.Base(rel), nil
		}
	}
	return nil, "", errors.New("нет подтверждённого результата с этим ID")
}
