package auth

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

func TestJWTRoundtrip(t *testing.T) {
	iss := NewIssuer("super-secret-test-key-1234567890", 30*time.Minute)
	tok, exp, err := iss.IssueDevice("dev1", 42)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) < 25*time.Minute {
		t.Fatalf("expiry too close: %s", exp)
	}
	claims, err := iss.ParseDevice(tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.DeviceID != "dev1" || claims.UserID != 42 {
		t.Fatalf("wrong claims: %+v", claims)
	}

	if _, err := iss.ParseUser(tok); err == nil {
		t.Fatalf("device token must NOT parse as user token")
	}
}

func TestInitDataHMAC(t *testing.T) {
	const botToken = "1234:ABCDEF"
	user := `{"id":1,"first_name":"Test","username":"tester"}`
	authDate := strconv.FormatInt(time.Now().Unix(), 10)
	values := url.Values{}
	values.Set("auth_date", authDate)
	values.Set("query_id", "AAEAAA")
	values.Set("user", user)
	// Build data-check-string
	var b strings.Builder
	b.WriteString("auth_date=" + authDate + "\n")
	b.WriteString("query_id=AAEAAA\n")
	b.WriteString("user=" + user)
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(b.String()))
	values.Set("hash", hex.EncodeToString(mac.Sum(nil)))

	raw := values.Encode()
	parsed, err := ParseInitData(raw, botToken, 24*time.Hour)
	if err != nil {
		t.Fatalf("ParseInitData: %v", err)
	}
	if parsed.User == nil || parsed.User.ID != 1 {
		t.Fatalf("bad user: %+v", parsed.User)
	}
}

func TestLoginWidgetDataHMAC(t *testing.T) {
	const botToken = "1234:ABCDEF"
	authDate := strconv.FormatInt(time.Now().Unix(), 10)
	values := url.Values{}
	values.Set("id", "555")
	values.Set("first_name", "Evgeny")
	values.Set("username", "tolkotakit")
	values.Set("auth_date", authDate)
	// data-check-string: отсортированные ключи через \n.
	var b strings.Builder
	b.WriteString("auth_date=" + authDate + "\n")
	b.WriteString("first_name=Evgeny\n")
	b.WriteString("id=555\n")
	b.WriteString("username=tolkotakit")
	// Секрет виджета — SHA256(bot_token), БЕЗ "WebAppData".
	secret := sha256.Sum256([]byte(botToken))
	mac := hmac.New(sha256.New, secret[:])
	mac.Write([]byte(b.String()))
	values.Set("hash", hex.EncodeToString(mac.Sum(nil)))

	u, err := ParseLoginWidgetData(values.Encode(), botToken, 0)
	if err != nil {
		t.Fatalf("ParseLoginWidgetData: %v", err)
	}
	if u.ID != 555 || u.Username != "tolkotakit" || u.FirstName != "Evgeny" {
		t.Fatalf("bad user: %+v", u)
	}

	// Битая подпись → ошибка.
	values.Set("username", "hacker")
	if _, err := ParseLoginWidgetData(values.Encode(), botToken, 0); err == nil {
		t.Fatal("подделанные данные не должны проходить")
	}
}

func TestLoginWidgetDataExpires(t *testing.T) {
	const botToken = "1234:ABCDEF"
	values := url.Values{}
	values.Set("id", "555")
	values.Set("auth_date", strconv.FormatInt(time.Now().Add(-25*time.Hour).Unix(), 10))
	secret := sha256.Sum256([]byte(botToken))
	mac := hmac.New(sha256.New, secret[:])
	mac.Write([]byte("auth_date=" + values.Get("auth_date") + "\nid=555"))
	values.Set("hash", hex.EncodeToString(mac.Sum(nil)))

	if _, err := ParseLoginWidgetData(values.Encode(), botToken, 24*time.Hour); err == nil {
		t.Fatal("просроченные данные Login Widget не должны проходить")
	}
}

func TestRateLimiter(t *testing.T) {
	l := NewLimiter(60, 2) // burst 2
	if !l.Allow("ip1") || !l.Allow("ip1") {
		t.Fatal("first two must pass")
	}
	if l.Allow("ip1") {
		t.Fatal("third must be denied immediately")
	}
}
