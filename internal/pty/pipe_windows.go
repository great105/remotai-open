//go:build windows

package pty

import (
	"io"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Named-pipe constants not all exported by x/sys/windows — define locally.
const (
	pipeAccessDuplex          = 0x00000003
	fileFlagFirstPipeInstance = 0x00080000
	pipeTypeByte              = 0x00000000
	pipeReadmodeByte          = 0x00000000
	pipeWait                  = 0x00000000
	pipeRejectRemoteClients   = 0x00000008
)

// pipeSDDL restricts the pipe to the object owner (the creating user) and
// LocalSystem (so a service-context remotai can reach a host, and vice-versa).
// No Everyone / Authenticated Users — other logged-in users must not connect.
const pipeSDDL = "D:(A;;GA;;;OW)(A;;GA;;;SY)"

// pipeConn wraps a named-pipe handle opened in OVERLAPPED mode. Overlapped I/O
// is essential: a synchronous handle serializes all operations on the file
// object, so a blocking ReadFile would stall a concurrent WriteFile on the same
// handle (the terminal would freeze after the first frame). With overlapped I/O
// reads and writes proceed independently. Each direction has its own event;
// writes are additionally serialized by wmu.
type pipeConn struct {
	h      windows.Handle
	server bool
	rEvent windows.Handle
	wEvent windows.Handle
	wmu    sync.Mutex
}

func newPipeConn(h windows.Handle, server bool) (*pipeConn, error) {
	re, err := windows.CreateEvent(nil, 1, 0, nil) // manual-reset, unsignaled
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	we, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(re)
		windows.CloseHandle(h)
		return nil, err
	}
	return &pipeConn{h: h, server: server, rEvent: re, wEvent: we}, nil
}

// overlappedIO runs one overlapped ReadFile/WriteFile and waits for completion.
func overlappedIO(h, event windows.Handle, p []byte, write bool) (int, error) {
	windows.ResetEvent(event)
	ov := &windows.Overlapped{HEvent: event}
	var done uint32
	var err error
	if write {
		err = windows.WriteFile(h, p, &done, ov)
	} else {
		err = windows.ReadFile(h, p, &done, ov)
	}
	if err == windows.ERROR_IO_PENDING {
		if _, werr := windows.WaitForSingleObject(event, windows.INFINITE); werr != nil {
			return 0, werr
		}
		err = windows.GetOverlappedResult(h, ov, &done, true)
	}
	return int(done), err
}

func (c *pipeConn) Read(p []byte) (int, error) {
	n, err := overlappedIO(c.h, c.rEvent, p, false)
	if err != nil {
		return n, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (c *pipeConn) Write(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := overlappedIO(c.h, c.wEvent, p[total:], true)
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (c *pipeConn) writeFrame(t frameType, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return writeFrame(c, t, payload)
}

func (c *pipeConn) readFrame() (frameType, []byte, error) { return readFrame(c) }

// connect waits (overlapped) for a client on the server pipe.
func (c *pipeConn) connect() error {
	windows.ResetEvent(c.rEvent)
	ov := &windows.Overlapped{HEvent: c.rEvent}
	err := windows.ConnectNamedPipe(c.h, ov)
	if err == nil || err == windows.ERROR_PIPE_CONNECTED {
		return nil
	}
	if err == windows.ERROR_IO_PENDING {
		if _, werr := windows.WaitForSingleObject(c.rEvent, windows.INFINITE); werr != nil {
			return werr
		}
		var done uint32
		if gerr := windows.GetOverlappedResult(c.h, ov, &done, true); gerr != nil {
			return gerr
		}
		return nil
	}
	return err
}

// disconnect resets a server pipe so the next client can connect, cancelling any
// pending overlapped read so the reader unblocks.
func (c *pipeConn) disconnect() {
	if c.server {
		_ = windows.CancelIoEx(c.h, nil)
		_ = windows.DisconnectNamedPipe(c.h)
	}
}

// wake aborts a parked connect() (pending overlapped ConnectNamedPipe) so
// acceptLoop notices the host is dead and exits. Called from host.markDead after
// the grace window. hostTransport requirement.
func (c *pipeConn) wake() {
	_ = windows.CancelIoEx(c.h, nil)
}

func (c *pipeConn) Close() error {
	_ = windows.CancelIoEx(c.h, nil)
	err := windows.CloseHandle(c.h)
	if c.rEvent != 0 {
		windows.CloseHandle(c.rEvent)
	}
	if c.wEvent != 0 {
		windows.CloseHandle(c.wEvent)
	}
	return err
}

// createPipeServer creates the single-instance overlapped named pipe for a host.
func createPipeServer(name string) (*pipeConn, error) {
	sd, err := windows.SecurityDescriptorFromString(pipeSDDL)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))

	namep, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateNamedPipe(
		namep,
		pipeAccessDuplex|fileFlagFirstPipeInstance|windows.FILE_FLAG_OVERLAPPED,
		pipeTypeByte|pipeReadmodeByte|pipeWait|pipeRejectRemoteClients,
		1,     // nMaxInstances — one host, one client at a time
		65536, // out buffer
		65536, // in buffer
		0,     // default timeout
		sa,
	)
	if err != nil {
		return nil, err
	}
	return newPipeConn(h, true)
}

// dialPipe opens the client end (overlapped), retrying while the pipe is not yet
// created or momentarily busy, until the deadline.
func dialPipe(name string, timeout time.Duration) (*pipeConn, error) {
	namep, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		h, err := windows.CreateFile(
			namep,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0, nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED, 0,
		)
		if err == nil {
			return newPipeConn(h, false)
		}
		if err != windows.ERROR_FILE_NOT_FOUND && err != windows.ERROR_PIPE_BUSY {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(25 * time.Millisecond)
	}
}
