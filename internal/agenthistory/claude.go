package agenthistory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const claudeWindow = 4 << 20

type claudeCursor struct {
	Source                string
	End, Size, PrefixSize int64
	Prefix, Parent        string
}
type claudeMessage struct {
	Type, UUID, ParentUUID, SessionID, Version string
	IsSidechain                                bool
	Message                                    struct{ Content json.RawMessage }
}
type claudeRecord struct {
	message     claudeMessage
	offset, end int64
}

func fingerprint(file *os.File, n int64) string {
	h := sha256.New()
	_, _ = io.Copy(h, io.NewSectionReader(file, 0, n))
	return hex.EncodeToString(h.Sum(nil))
}

// The parent chain, not physical JSONL ordering, determines the conversation.
// One bounded window is scanned per request. A missing ancestor is exposed as
// partial; the next cursor can continue looking in the preceding window.
func readClaude(ctx context.Context, source Source, cursor string) (Page, error) {
	page := Page{Source: source.Key(), Agent: "claude", Schema: "claude-jsonl-chain-v1"}
	path := source.Transcript
	if !filepath.IsAbs(path) || filepath.Base(path) != source.SessionID+".jsonl" {
		return page, ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return page, ErrUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return page, ErrUnavailable
	}
	c := claudeCursor{Source: page.Source, End: info.Size(), Size: info.Size(), PrefixSize: min(4096, info.Size())}
	if cursor != "" {
		if decodeCursor(cursor, &c) != nil || c.Source != page.Source || c.End < 0 || c.End > c.Size || c.Size > info.Size() || c.PrefixSize < 0 || c.PrefixSize > 4096 || c.PrefixSize > c.Size || c.Prefix != fingerprint(file, c.PrefixSize) {
			return page, ErrChanged
		}
	} else {
		c.Prefix = fingerprint(file, c.PrefixSize)
	}
	start := max(int64(0), c.End-claudeWindow)
	data := make([]byte, c.End-start)
	if _, err := file.ReadAt(data, start); err != nil && err != io.EOF {
		return page, ErrUnavailable
	}
	// Ignore incomplete first/last records. A concurrently appended final line
	// is not part of this frozen page; a malformed complete line is a gap.
	if start > 0 {
		if cut := bytes.IndexByte(data, '\n'); cut >= 0 {
			data = data[cut+1:]
			start += int64(cut + 1)
		} else {
			data = nil
			page.Partial = true
		}
	}
	records := map[string]claudeRecord{}
	last := ""
	position := start
	for len(data) > 0 {
		if ctx.Err() != nil {
			return page, ctx.Err()
		}
		cut := bytes.IndexByte(data, '\n')
		if cut < 0 {
			page.Partial = true
			break
		}
		line := data[:cut]
		if !utf8.Valid(line) {
			return page, ErrFormat
		}
		data = data[cut+1:]
		var msg claudeMessage
		if len(line) > 0 && json.Unmarshal(line, &msg) != nil {
			page.Partial = true
		}
		if msg.Type == "user" || msg.Type == "assistant" {
			if msg.SessionID != source.SessionID || msg.UUID == "" {
				return page, ErrFormat
			}
			if msg.Version != "2.1.268" && msg.Version != "2.1.270" {
				// Версия не сверена с фикстурами: отдельный код с самой версией,
				// чтобы интерфейс сказал «версия X не поддержана» (ST-10 B).
				return page, versionError("claude", msg.Version)
			}
			page.Version = msg.Version
			if !msg.IsSidechain {
				records[msg.UUID] = claudeRecord{msg, position, position + int64(cut+1)}
				last = msg.UUID
			}
		} else if msg.Type == "system" && msg.UUID != "" && msg.SessionID == source.SessionID && !msg.IsSidechain {
			// Follow structural links without inventing conversation messages.
			records[msg.UUID] = claudeRecord{msg, position, position + int64(cut+1)}
		}
		position += int64(cut + 1)
	}
	parent := c.Parent
	if cursor == "" {
		parent = last
	}
	parts := []string{}
	total := 0
	seen := map[string]bool{}
	nextEnd := start
	for parent != "" {
		if seen[parent] {
			return page, ErrFormat
		}
		seen[parent] = true
		record, ok := records[parent]
		if !ok {
			page.Partial = true
			break
		}
		if record.message.Type == "system" {
			page.Partial = true
			parent = record.message.ParentUUID
			nextEnd = record.offset
			continue
		}
		text, omitted := visibleContent(record.message.Message.Content)
		text = "[" + record.message.Type + "]\n" + text
		if len(parts) > 0 && (total+len(text)+2 > MaxText || len(parts) >= 40) {
			nextEnd = record.end
			break
		}
		text, clipped := bounded(text, MaxText-total)
		page.Partial = page.Partial || omitted || clipped
		parts = append(parts, text)
		total += len(text) + 2
		parent = record.message.ParentUUID
		nextEnd = record.offset
	}
	for left, right := 0, len(parts)-1; left < right; left, right = left+1, right-1 {
		parts[left], parts[right] = parts[right], parts[left]
	}
	page.Text = strings.Join(parts, "\n\n")
	if parent != "" && nextEnd > 0 {
		c.End = nextEnd
		c.Parent = parent
		page.Next = encodeCursor(c)
	} else if parent != "" {
		page.Partial = true
	}
	if page.Version == "" && page.Next == "" {
		return page, ErrUnavailable
	}
	return page, nil
}
