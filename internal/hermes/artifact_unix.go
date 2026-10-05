//go:build !windows

package hermes

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
)

// Every lookup is relative to an already opened directory. O_NOFOLLOW is
// applied atomically, not via a pre-open Lstat/EvalSymlinks check.
func openArtifactNoFollow(root, rel string, beforeOpen func(string)) (*os.File, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := append(strings.Split(strings.TrimPrefix(abs, "/"), "/"), strings.Split(rel, string(filepath.Separator))...)
	for i, part := range parts {
		if part == "" {
			continue
		}
		if beforeOpen != nil {
			beforeOpen(part)
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, e := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), filepath.Join(abs, rel)), nil
}
