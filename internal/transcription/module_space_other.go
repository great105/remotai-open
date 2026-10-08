//go:build !windows

package transcription

// Automatic installation is restricted to Windows x64 before this path.
func checkModuleSpace(_ string, _ uint64) error { return nil }
