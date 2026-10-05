package web

import "testing"

func TestDesktopPaymentOpensOnlyTrustedHTTPSHost(t *testing.T) {
	url := "https://yoomoney.ru/checkout/payments/v2/contract?orderId=unit-order"
	if got, err := allowedExternalURL(url); err != nil || got != url {
		t.Fatalf("payment cannot open in system browser: %q %v", got, err)
	}
	for _, bad := range []string{
		"https://yoomoney.ru.attacker.example/checkout",
		"https://yoomoney.ru@attacker.example/checkout",
		"file:///C:/Windows/System32/cmd.exe",
		"javascript:alert(1)",
	} {
		if _, err := allowedExternalURL(bad); err == nil {
			t.Errorf("accepted untrusted URL: %s", bad)
		}
	}
}
