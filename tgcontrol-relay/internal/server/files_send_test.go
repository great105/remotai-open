package server

import (
	"bytes"
	"context"
	"strconv"
	"testing"
	"time"

	"tgcontrol-relay/internal/protocol"
)

type fileAgentStub struct {
	data  []byte
	calls int
}

func (s *fileAgentStub) Send(_ context.Context, cmd protocol.Cmd, _ time.Duration) (*protocol.CmdResult, error) {
	s.calls++
	offset, _ := strconv.Atoi(cmd.Query["offset"])
	limit, _ := strconv.Atoi(cmd.Query["len"])
	end := offset + limit
	if end > len(s.data) {
		end = len(s.data)
	}
	body := append([]byte(nil), s.data[offset:end]...)
	return &protocol.CmdResult{
		StatusCode: 200,
		Body:       body,
		Headers: map[string]string{
			"X-File-Size":  strconv.Itoa(len(s.data)),
			"X-File-Mtime": "123",
		},
	}, nil
}

func TestFetchAgentFileUsesBoundedChunks(t *testing.T) {
	data := bytes.Repeat([]byte("x"), telegramFetchChunk+123)
	agent := &fileAgentStub{data: data}
	var dst bytes.Buffer
	size, name, err := fetchAgentFile(context.Background(), agent, `C:\tmp\report.bin`, "", &dst)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) || name != "report.bin" {
		t.Fatalf("size=%d name=%q", size, name)
	}
	if agent.calls != 2 {
		t.Fatalf("calls=%d, want 2 bounded requests", agent.calls)
	}
	if !bytes.Equal(dst.Bytes(), data) {
		t.Fatal("spooled bytes differ")
	}
}

func TestFetchAgentFileRejectsTelegramOversizeBeforeSpooling(t *testing.T) {
	agent := &fileAgentStub{data: make([]byte, telegramFileLimit+1)}
	var dst bytes.Buffer
	_, _, err := fetchAgentFile(context.Background(), agent, "/tmp/huge.bin", "", &dst)
	if err == nil {
		t.Fatal("want too-large error")
	}
	if _, ok := err.(*telegramTooLargeError); !ok {
		t.Fatalf("got %T: %v", err, err)
	}
	if dst.Len() != 0 {
		t.Fatalf("downloaded %d bytes before rejecting size", dst.Len())
	}
}

func TestSafeTelegramFilename(t *testing.T) {
	if got := safeTelegramFilename("../bad\r\nname.txt"); got != "..badname.txt" {
		t.Fatalf("got %q", got)
	}
	if got := pathBaseAny(`C:\work\result.zip`); got != "result.zip" {
		t.Fatalf("base=%q", got)
	}
}
