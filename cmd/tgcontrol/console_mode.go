package main

// Background and installer commands must never acquire the user's console.
func cliConsoleRequested(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "attach", "pair", "update", "install", "uninstall", "send", "status",
		"config", "doctor", "vpn", "remote", "peer", "unpair", "--unpair",
		"--version", "-v", "-V", "version", "--help", "-h", "-help", "help", "/?":
		return true
	}
	return false
}
