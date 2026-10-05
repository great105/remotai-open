//go:build linux || darwin

package pty

// newHostServer creates the Linux unix-socket server transport for a host.
// The shared host loop (host_common.go) drives it via the hostTransport iface.
func newHostServer(id string) (hostTransport, error) {
	u, err := createUnixServer(pipeName(id))
	if err != nil {
		return nil, err
	}
	return u, nil
}

// exitCode returns the shell process exit code. drainLoop calls this via
// markDead once the PTY hit EOF (the shell is gone); Wait reaps the child and
// fills ProcessState. Returns 0 if unavailable, -1 if killed by a signal.
func (p *conPTY) exitCode() int {
	if p.cmd == nil {
		return 0
	}
	if p.cmd.ProcessState == nil {
		_ = p.cmd.Wait()
	}
	if st := p.cmd.ProcessState; st != nil {
		return st.ExitCode()
	}
	return 0
}
