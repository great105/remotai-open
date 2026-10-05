//go:build windows

package pty

import (
	"bytes"
	"io"
	"testing"
)

// TestFrameRoundTrip verifies the wire codec: header layout, payloads of all
// sizes, and that an unknown frame type is skipped (len consumed) rather than
// corrupting the stream — the forward-compat guarantee for auto-update.
func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := []struct {
		ft frameType
		p  []byte
	}{
		{frClientHello, []byte{1, 0, 80, 0, 24, 0}},
		{frOutput, []byte("hello\x1b[0m world")},
		{0x7f, []byte("UNKNOWN-future-frame")}, // unknown type
		{frOutput, nil},                        // empty payload
		{frExit, []byte{0, 0, 0, 0}},
	}
	for _, w := range want {
		if err := writeFrame(&buf, w.ft, w.p); err != nil {
			t.Fatalf("writeFrame(%#x): %v", w.ft, err)
		}
	}
	for i, w := range want {
		ft, p, err := readFrame(&buf)
		if err != nil {
			t.Fatalf("readFrame #%d: %v", i, err)
		}
		if ft != w.ft {
			t.Fatalf("frame #%d type: got %#x want %#x", i, ft, w.ft)
		}
		if !bytes.Equal(p, w.p) {
			t.Fatalf("frame #%d payload: got %q want %q", i, p, w.p)
		}
	}
	if _, _, err := readFrame(&buf); err != io.EOF {
		t.Fatalf("expected EOF after last frame, got %v", err)
	}
}

// TestPipeClientDecode checks the client decode loop: Output/Snapshot yield raw
// bytes (with leftover buffering when the dst is smaller than the payload),
// Pong is skipped, unknown frames are skipped, and Exit becomes io.EOF + marks
// the session as cleanly exited.
func TestPipeClientDecode(t *testing.T) {
	var buf bytes.Buffer
	writeFrame(&buf, frSnapshot, []byte("SNAP"))
	writeFrame(&buf, frPong, nil)
	writeFrame(&buf, 0x7e, []byte("ignored"))
	writeFrame(&buf, frOutput, []byte("LIVE"))
	writeFrame(&buf, frExit, []byte{0, 0, 0, 0})

	next := func() (frameType, []byte, error) { return readFrame(&buf) }
	c := &pipeClient{}

	// Snapshot decoded across a small (2-byte) destination → leftover buffering.
	small := make([]byte, 2)
	var got bytes.Buffer
	for got.Len() < 4 {
		n, err := c.readDecoded(small, next)
		if err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		got.Write(small[:n])
	}
	if got.String() != "SNAP" {
		t.Fatalf("snapshot: got %q want SNAP", got.String())
	}

	// Next meaningful bytes are "LIVE" (Pong + unknown skipped).
	big := make([]byte, 64)
	n, err := c.readDecoded(big, next)
	if err != nil {
		t.Fatalf("decode live: %v", err)
	}
	if string(big[:n]) != "LIVE" {
		t.Fatalf("live: got %q want LIVE", big[:n])
	}

	// Exit → io.EOF and exitedClean() true.
	if _, err := c.readDecoded(big, next); err != io.EOF {
		t.Fatalf("expected io.EOF on exit frame, got %v", err)
	}
	if !c.exitedClean() {
		t.Fatalf("exitedClean() should be true after frExit")
	}
}
