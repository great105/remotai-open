package pty

import (
	"io"
	"time"
)

// WriteInputChunks paces ConPTY input without splitting UTF-8 code points.
// Callers own serialization for the complete operation, including delays.
func WriteInputChunks(w io.Writer, data []byte) error {
	for start := 0; start < len(data); {
		end := min(start+512, len(data))
		for end < len(data) && data[end]&0xC0 == 0x80 {
			end++
		}
		n, err := w.Write(data[start:end])
		if err != nil {
			return err
		}
		if n != end-start {
			return io.ErrShortWrite
		}
		start = end
		if start < len(data) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}
