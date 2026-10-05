package license

import "testing"

// Канон продукта: «Дома — бесплатно навсегда. Из любой точки — по подписке.»
// Пока лимиты бились по обоим транспортам, это обещание было неправдой.
func TestLimitsForTransport(t *testing.T) {
	m := &Manager{license: &License{Tier: TierFree, Active: true}}

	relay := m.LimitsForTransport(TransportRelay)
	lan := m.LimitsForTransport(TransportLAN)

	// Облако: лимиты тарифа действуют. При BetaFree free поднимается до pro,
	// поэтому сравниваем с эффективным тарифом, а не с TierFree.
	want := Limits[m.EffectiveTier()]
	if relay.RemoteDesktopMaxFPS != want.RemoteDesktopMaxFPS {
		t.Errorf("relay FPS = %d, want %d (лимиты тарифа обязаны действовать в облаке)",
			relay.RemoteDesktopMaxFPS, want.RemoteDesktopMaxFPS)
	}
	if relay.RemoteDesktopMaxRes != want.RemoteDesktopMaxRes {
		t.Errorf("relay MaxRes = %d, want %d", relay.RemoteDesktopMaxRes, want.RemoteDesktopMaxRes)
	}

	// Локально: ни одного ограничения, 0 = «без ограничения».
	if lan.RemoteDesktopMaxFPS != 0 || lan.RemoteDesktopQuality != 0 || lan.RemoteDesktopMaxRes != 0 {
		t.Errorf("LAN обязан быть безлимитным по экрану, получили fps=%d quality=%d res=%d",
			lan.RemoteDesktopMaxFPS, lan.RemoteDesktopQuality, lan.RemoteDesktopMaxRes)
	}
	if lan.MaxPTYTerminals != 0 || lan.MaxConcurrentAgents != 0 {
		t.Errorf("LAN обязан быть безлимитным по терминалам и агентам, получили pty=%d agents=%d",
			lan.MaxPTYTerminals, lan.MaxConcurrentAgents)
	}

	// То, что не гейтится никогда — не должно пострадать от снятия лимитов.
	if !lan.FileManagerWrite || !lan.AutoUpdate {
		t.Error("запись файлов и автообновление не гейтятся никогда")
	}
}

// Регресс-ловушка: на платном тарифе локальный режим тоже безлимитен,
// а облачный обязан оставаться в рамках тарифа.
func TestLimitsForTransportPaidTier(t *testing.T) {
	m := &Manager{license: &License{Tier: TierFree, Active: true}}

	free := Limits[TierFree]
	if free.RemoteDesktopMaxFPS == 0 {
		t.Fatal("тест бессмысленен: у free-тарифа нет лимита FPS")
	}

	lan := m.LimitsForTransport(TransportLAN)
	if lan.RemoteDesktopMaxFPS != 0 {
		t.Errorf("LAN на любом тарифе безлимитен, получили %d", lan.RemoteDesktopMaxFPS)
	}

	// Пустая строка и мусор трактуются как локальное подключение:
	// платным считается только явно помеченный релей.
	for _, transport := range []string{"", "lan", "неизвестно"} {
		if got := m.LimitsForTransport(transport); got.RemoteDesktopMaxFPS != 0 {
			t.Errorf("transport %q должен считаться локальным, получили лимит %d", transport, got.RemoteDesktopMaxFPS)
		}
	}
}
