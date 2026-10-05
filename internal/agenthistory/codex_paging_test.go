package agenthistory

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCodexLatestReplySurvivesOlderLargeOutput(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"data": []any{
			map[string]any{"items": []any{map[string]any{"type": "agentMessage", "text": "NEWEST_FINAL_REPLY"}}},
			map[string]any{"items": []any{map[string]any{"type": "commandExecution", "command": "fixture", "aggregatedOutput": strings.Repeat("old-output\n", 32000)}}},
		},
		"nextCursor": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := codexPage(Page{Source: "fixture"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(page.Text, "NEWEST_FINAL_REPLY") {
		t.Fatal("newest reply was excluded by an older tool output")
	}
	if page.Next == "" {
		t.Fatal("the remaining older output is unreachable")
	}
}

func TestCodexChunkCursorsCoverEntireBatchBeforeOlderTurns(t *testing.T) {
	output := strings.Repeat("中😀e\u0301\n", MaxText/4)
	for _, upstream := range []any{nil, "older-turns"} {
		raw, _ := json.Marshal(map[string]any{"data": []any{
			map[string]any{"items": []any{map[string]any{"type": "agentMessage", "text": "FINAL"}}},
			map[string]any{"items": []any{map[string]any{"type": "commandExecution", "command": "fixture", "aggregatedOutput": output}}},
		}, "nextCursor": upstream})
		cursor := codexCursor{Source: "fixture", Cursor: "current-batch"}
		recovered := ""
		seen := map[string]bool{}
		chunks := 0
		for {
			page, err := codexPage(Page{Source: "fixture"}, raw, cursor)
			if err != nil || !utf8.ValidString(page.Text) || len(page.Text) > MaxText || page.Text == "" {
				t.Fatalf("invalid chunk: length=%d err=%v", len(page.Text), err)
			}
			chunks++
			recovered = page.Text + recovered
			if page.Next == "" {
				if upstream != nil {
					t.Fatal("upstream continuation lost")
				}
				break
			}
			if seen[page.Next] || chunks > 20 {
				t.Fatal("cursor cycle")
			}
			seen[page.Next] = true
			cursor = codexCursor{}
			if err := decodeCursor(page.Next, &cursor); err != nil {
				t.Fatal(err)
			}
			if cursor.Before == 0 {
				if cursor.Cursor != upstream {
					t.Fatal("incorrect older-turn cursor")
				}
				break
			}
			if cursor.Cursor != "current-batch" {
				t.Fatal("remaining item was skipped by advancing upstream early")
			}
			changed := json.RawMessage(strings.Replace(string(raw), "FINAL", "CHANGED", 1))
			if _, err := codexPage(Page{Source: "fixture"}, changed, cursor); !errors.Is(err, ErrChanged) {
				t.Fatal("changed source accepted for an old chunk offset")
			}
		}
		want := "[command]\nfixture\n" + output + "\n\n[assistant]\nFINAL"
		if recovered != want || chunks < 2 {
			t.Fatal("chunk pagination lost, duplicated or reordered text")
		}
	}
}
