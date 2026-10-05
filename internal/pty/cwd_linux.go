//go:build linux

package pty

import (
	"fmt"
	"os"
	"strconv"
)

// currentCWD reads /proc/<pid>/cwd symlink for the shell process.
func (p *conPTY) currentCWD() (string, error) {
	if p.cmd == nil || p.cmd.Process == nil {
		return "", fmt.Errorf("no process")
	}
	pid := p.cmd.Process.Pid
	link := "/proc/" + strconv.Itoa(pid) + "/cwd"
	return os.Readlink(link)
}
