package web

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"tgcontrol/internal/agenthistory"
	"time"
)

type fixtureHistoryReader struct{ entered, finish chan struct{} }

func (f fixtureHistoryReader) ReadAgentHistory(ctx context.Context, cursor string) (agenthistory.Page, error) {
	close(f.entered)
	select {
	case <-ctx.Done():
		return agenthistory.Page{}, ctx.Err()
	case <-f.finish:
		return agenthistory.Page{Text: cursor}, nil
	}
}
func TestHistoryCapabilityDoesNotGrantSizeOwnership(t *testing.T) {
	history := map[string]any{"capabilities": []any{"agent-history-v1"}}
	if !terminalCapability(history, "agent-history-v1") || terminalCapability(history, "size-owner-v1") {
		t.Fatal("separate opt-in required")
	}
	if terminalCapability(map[string]any{}, "agent-history-v1") || !terminalCapability(map[string]any{}, "size-owner-v1") {
		t.Fatal("initial v1 compatibility")
	}
}
func TestHistoryWorkerBoundedAndCancelledWithViewer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := fixtureHistoryReader{make(chan struct{}), make(chan struct{})}
	h := newHistoryChannel(ctx, reader)
	h.request("one", "prepared-text")
	<-reader.entered
	for i := 0; i < 100; i++ {
		h.request("duplicate", "")
	}
	if len(h.requests) != 0 {
		t.Fatal("unbounded queued requests")
	}
	close(reader.finish)
	select {
	case reply := <-h.replies:
		if reply.Request != "one" || reply.Page.Text != "prepared-text" {
			t.Fatalf("reply: %+v", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("read stalled")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	reader2 := fixtureHistoryReader{make(chan struct{}), make(chan struct{})}
	h2 := newHistoryChannel(ctx2, reader2)
	h2.request("two", "")
	<-reader2.entered
	cancel2()
	// A cancelled read must terminate; a bounded error reply is permitted.
	select {
	case <-h2.replies:
	case <-time.After(100 * time.Millisecond):
	}
}

type failingHistoryReader struct{ err error }

func (f failingHistoryReader) ReadAgentHistory(context.Context, string) (agenthistory.Page, error) {
	return agenthistory.Page{Text: "must-not-leak"}, f.err
}

// ST-10 B: неподдержанная версия CLI — отдельный код с агентом и версией, а
// не общее «недоступно». Прочие коды прежние; тексты системных ошибок (пути)
// наружу не уходят.
func TestHistoryReplyNamesUnsupportedVersion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHistoryChannel(ctx, failingHistoryReader{&agenthistory.VersionError{Agent: "claude", Version: "2.1.300"}})
	h.request("r1", "")
	select {
	case reply := <-h.replies:
		if reply.Error != "history_version_unsupported" || reply.Agent != "claude" || reply.AgentVersion != "2.1.300" || reply.Page != nil {
			t.Fatalf("reply: %+v", reply)
		}
		wire, _ := json.Marshal(reply)
		for _, want := range []string{`"t":"agent-history"`, `"v":1`, `"request":"r1"`, `"error":"history_version_unsupported"`, `"agent":"claude"`, `"version":"2.1.300"`} {
			if !strings.Contains(string(wire), want) {
				t.Fatalf("wire %s lacks %s", wire, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("read stalled")
	}

	for err, want := range map[error]string{
		agenthistory.ErrChanged:                            "history_source_changed",
		agenthistory.ErrFormat:                             "history_unsupported_format",
		agenthistory.ErrUnavailable:                        "history_unavailable",
		errors.New(`open C:\Users\secret\x.jsonl: denied`): "history_unavailable",
	} {
		reply := historyErrorReply(historyReply{Type: "agent-history", Version: 1, Request: "x"}, err)
		if reply.Error != want || reply.Agent != "" || reply.AgentVersion != "" || reply.Page != nil {
			t.Errorf("%v → %+v, want %s", err, reply, want)
		}
		wire, _ := json.Marshal(reply)
		if strings.Contains(string(wire), "version") || strings.Contains(string(wire), "secret") {
			t.Errorf("wire leaks: %s", wire)
		}
	}
}
