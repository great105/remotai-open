package service

import "strings"

// SystemdPathValue quotes one path for systemd's parser and escapes unit
// specifiers. Shell quoting is different and must not be used for unit files.
func SystemdPathValue(path string) string {
	path = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(path)
	return `"` + path + `"`
}

// ':' disables environment expansion; '$HOME' in a filename stays literal.
func SystemdExecPath(path string) string { return ":" + SystemdPathValue(path) }
