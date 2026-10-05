package server

import "testing"

// Имя входа должно отвечать на вопрос «это я?».
//
// Пока всё, кроме Android и Telegram, называлось «Веб-приложение», список из
// десяти одинаковых строк читался как чужие подключения — живой вопрос
// владельца (2026-07-30): «это нормально, что много веб-сессий каких-то
// создаётся?». Отдельно важен «Скрипт (API)»: свои же диагностические запросы
// не должны выглядеть как ещё один телефон в аккаунте.
func TestBrowserName(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Microsoft Windows 10.0.26200; ru-RU) WebView/1.0":        "Окно Remotai на компьютере",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/150 Safari/537.36": "Chrome на Windows",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605 Version/17 Safari/605":    "Safari на macOS",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) CriOS/150 Mobile Safari/604":    "Chrome на iPhone",
		"Python-urllib/3.13": "Скрипт (API)",
		"curl/8.4.0":         "Скрипт (API)",
		"":                   "Веб-приложение",
	}
	for ua, want := range cases {
		if got := browserName(ua); got != want {
			t.Errorf("browserName(%q) = %q, ожидалось %q", ua, got, want)
		}
	}
}
