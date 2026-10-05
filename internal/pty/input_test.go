package pty

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

type pasteSpy struct {
	sizeSpy
	writesMu sync.Mutex
	writes   [][]byte
	first    chan struct{}
	release  chan struct{}
}

func (p *pasteSpy) Write(b []byte) (int, error) {
	p.writesMu.Lock()
	p.writes = append(p.writes, append([]byte(nil), b...))
	first := len(p.writes) == 1
	p.writesMu.Unlock()
	if first {
		close(p.first)
		<-p.release
	}
	return len(b), nil
}

func TestPasteSerializesOtherViewerKeysAndPreservesUTF8(t *testing.T) {
	backend := &pasteSpy{first: make(chan struct{}), release: make(chan struct{})}
	session := &Session{pty: backend}
	text := "\x1b[200~" + strings.Repeat("Я😀é", 220) + "\x1b[201~\r"
	done := make(chan error, 1)
	go func() { done <- session.WritePaste([]byte(text)) }()
	<-backend.first
	keyDone := make(chan error, 1)
	go func() { _, err := session.Write([]byte("OTHER-VIEWER")); keyDone <- err }()
	close(backend.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-keyDone; err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for _, chunk := range backend.writes {
		if !utf8.Valid(chunk) {
			t.Fatalf("split UTF-8 in %d byte chunk", len(chunk))
		}
		joined = append(joined, chunk...)
	}
	if !bytes.Equal(joined, []byte(text+"OTHER-VIEWER")) {
		t.Fatal("other viewer interleaved with paste")
	}
}

type shortInputWriter struct{ calls int }

func (w *shortInputWriter) Write(b []byte) (int, error) { w.calls++; return len(b) - 1, nil }

func TestPasteShortWriteIsFailureAndDoesNotRetry(t *testing.T) {
	w := &shortInputWriter{}
	if err := WriteInputChunks(w, bytes.Repeat([]byte("x"), 2048)); err != io.ErrShortWrite {
		t.Fatalf("error=%v", err)
	}
	if w.calls != 1 {
		t.Fatalf("retried uncertain input %d times", w.calls)
	}
}
