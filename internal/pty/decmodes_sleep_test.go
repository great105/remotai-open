package pty

import "testing"

// Усыпление снимает режимы, в которых терминал сам шлёт байты программе
// (мышь, фокус, вставка), и оставляет alt-экран сверке с зеркалом.
func TestDropInputModesKeepsAlt(t *testing.T) {
	d := newDecTracker()
	d.scan([]byte("\x1b[?1049h\x1b[?1000h\x1b[?1004h\x1b[?1006h\x1b[?2004h"))
	d.takeDirty()
	d.dropInputModes()
	if got := d.snapshot(); len(got) != 1 || got[0] != 1049 {
		t.Fatalf("остаться должен только alt-экран: %v", got)
	}
	if !d.takeDirty() {
		t.Fatal("изменение набора должно отмечаться для персиста")
	}
}
