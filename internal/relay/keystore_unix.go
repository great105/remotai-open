//go:build !windows

package relay

func saveJWTPlatform(jwt string) error { return errPlatformNotImplemented }
func loadJWTPlatform() (string, error) { return "", errPlatformNotImplemented }
func clearJWTPlatform() error          { return errPlatformNotImplemented }
