//go:build !windows

package update

import (
	"os"
	"os/exec"
)

// startDetached — на Unix обычный exec: подменённый через rename бинарник
// запускается без сюрпризов.
func startDetached(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Start()
}
