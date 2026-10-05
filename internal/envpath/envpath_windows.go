//go:build windows

package envpath

// UserBinDirs is a no-op on Windows: PATH augmentation and user-profile
// discovery there are handled by buildEnvBlock() / findCLI() which scan
// C:\Users\*\AppData\Roaming\npm directly (LocalSystem service context).
func UserBinDirs(home string) []string { return nil }
