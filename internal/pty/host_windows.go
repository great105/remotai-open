//go:build windows

package pty

import "golang.org/x/sys/windows"

// newHostServer creates the Windows named-pipe server transport for a host.
// The shared host loop (host_common.go) drives it via the hostTransport iface.
func newHostServer(id string) (hostTransport, error) {
	c, err := createPipeServer(pipeName(id))
	if err != nil {
		return nil, err
	}
	return c, nil
}

// exitCode reads the shell process exit code (0 if unavailable).
func (p *conPTY) exitCode() int {
	var code uint32
	if err := windows.GetExitCodeProcess(windows.Handle(p.procHandle), &code); err != nil {
		return 0
	}
	return int(code)
}
