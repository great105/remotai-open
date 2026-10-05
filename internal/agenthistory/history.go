// Package agenthistory reads session-bound, versioned conversation sources.
// It never resumes an agent or sends input to a live PTY.
package agenthistory

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const MaxText = 256 << 10

var (
	ErrUnavailable = errors.New("history_unavailable")
	ErrFormat      = errors.New("history_unsupported_format")
	ErrChanged     = errors.New("history_source_changed")
	// ErrVersionUnsupported — формат знакомый, но версия CLI не сверена с
	// фикстурами. Отдельный код нужен интерфейсу (ST-10 B): «версия X не
	// поддержана» — это не поломка, а причина, которую человек может устранить
	// или хотя бы понять; прежнее общее «недоступно» её прятало.
	ErrVersionUnsupported = errors.New("history_version_unsupported")
)

// VersionError — источник истории записан версией CLI, которую мы не сверяли
// (claude.go: принятые версии JSONL; codex.go: версия app-server). Version
// берётся из самого источника и урезается: это имя версии, не содержимое.
//
// errors.Is(err, ErrFormat) остаётся истинным: неподдержанная версия — частный
// случай незнакомого формата, и прежние проверки ErrFormat её не теряют.
type VersionError struct {
	Agent   string
	Version string
}

func (e *VersionError) Error() string { return ErrVersionUnsupported.Error() }

func (e *VersionError) Is(target error) bool {
	return target == ErrVersionUnsupported || target == ErrFormat
}

// versionError собирает VersionError, оставляя от версии только печатные
// символы и не больше 32 байт: строка приходит из файла или вывода CLI.
func versionError(agent, version string) error {
	clean := make([]byte, 0, len(version))
	for i := 0; i < len(version) && len(clean) < 32; i++ {
		if c := version[i]; c > 0x20 && c < 0x7f {
			clean = append(clean, c)
		}
	}
	return &VersionError{Agent: agent, Version: string(clean)}
}

// Source is supplied exclusively by the terminal's hook spool, never by a
// browser request. Key identifies its generation without exposing local paths.
type Source struct{ Agent, SessionID, Transcript, ConfigHome string }

func (s Source) Key() string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}

type Page struct {
	Source  string `json:"source"`
	Agent   string `json:"agent"`
	Version string `json:"version"`
	Schema  string `json:"schema"`
	Text    string `json:"text"`
	Next    string `json:"next,omitempty"`
	Partial bool   `json:"partial"`
}

func Read(ctx context.Context, source Source, cursor, binary string) (Page, error) {
	if source.SessionID == "" || len(source.SessionID) > 128 || len(cursor) > 8192 {
		return Page{}, ErrUnavailable
	}
	switch source.Agent {
	case "claude":
		return readClaude(ctx, source, cursor)
	case "codex":
		return readCodex(ctx, source, cursor, binary)
	default:
		return Page{}, ErrUnavailable
	}
}

func encodeCursor(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeCursor(raw string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, v) != nil {
		return ErrChanged
	}
	return nil
}

func bounded(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit], true
}

// Only visible message and tool blocks are rendered. Images, thinking and
// unfamiliar block types are omitted and explicitly mark the page partial.
func visibleContent(raw json.RawMessage) (string, bool) {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain, false
	}
	var blocks []struct {
		Type, Text, Name string
		Input, Content   json.RawMessage
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", true
	}
	parts := []string{}
	partial := false
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "tool_use":
			parts = append(parts, "[tool: "+block.Name+"]\n"+string(block.Input))
		case "tool_result":
			text, omitted := visibleContent(block.Content)
			partial = partial || omitted
			parts = append(parts, "[tool result]\n"+text)
		default:
			partial = true
		}
	}
	return strings.Join(parts, "\n"), partial
}
