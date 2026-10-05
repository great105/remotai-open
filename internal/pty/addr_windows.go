//go:build windows

package pty

// pipeName is the named-pipe path for a session's persistent pty-host.
func pipeName(id string) string { return `\\.\pipe\remotai-pty-` + id }
