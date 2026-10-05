//go:build !windows

package tray

import "context"

type Options struct {
	Port           int
	RelayStatus    func() (configured, connected bool) // состояние облака для пункта статуса (nil → «Нет облака»)
	SetupMode      bool
	BindError      string
	OnQuit         func()
	OnOpen         func() // open the native window (nil → tray opens a browser)
	OnTerminals    func() // open the local client (/miniapp) — terminals, files, screen
	OnHostTerminal func() // open a host terminal window attached to a new session
}

// Run is a no-op on non-Windows platforms; it blocks until ctx is done
// so callers can use it in place of the main blocking loop.
func Run(ctx context.Context, opts Options) {
	<-ctx.Done()
}
