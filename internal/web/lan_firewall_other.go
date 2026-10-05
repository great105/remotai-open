//go:build !windows

package web

func lanFirewallStatus(_ int) (bool, string) {
	return true, "Проверка системного брандмауэра доступна только в Windows"
}

func allowLANFirewall(_ int) error { return nil }
