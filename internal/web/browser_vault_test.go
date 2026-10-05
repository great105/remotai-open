package web

import (
	"strings"
	"testing"
	"time"

	"tgcontrol/internal/cdp"
)

// Пароль обязан подходить формам: длина, все четыре вида символов и никаких
// кавычек/пробелов, на которых спотыкается разметка и копирование.
func TestGeneratePasswordFitsForms(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pass, err := generatePassword(20)
		if err != nil {
			t.Fatalf("генерация: %v", err)
		}
		if len([]rune(pass)) != 20 {
			t.Fatalf("длина %d вместо 20: %q", len([]rune(pass)), pass)
		}
		if seen[pass] {
			t.Fatalf("повтор пароля: %q", pass)
		}
		seen[pass] = true
		if strings.ContainsAny(pass, " \"'\\`<>") {
			t.Fatalf("символ, ломающий формы: %q", pass)
		}
		var lower, upper, digit, mark bool
		for _, r := range pass {
			switch {
			case r >= 'a' && r <= 'z':
				lower = true
			case r >= 'A' && r <= 'Z':
				upper = true
			case r >= '0' && r <= '9':
				digit = true
			default:
				mark = true
			}
		}
		if !lower || !upper || !digit || !mark {
			t.Fatalf("пароль не проходит требования сайтов: %q", pass)
		}
	}
	// Короткую длину не принимаем молча: восьмизначный пароль сегодня отвергают
	// сами сайты, и человек получил бы «пароль слишком простой» вместо аккаунта.
	pass, err := generatePassword(4)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(pass)) < 16 {
		t.Fatalf("слишком короткая длина принята: %d", len([]rune(pass)))
	}
}

func TestNormalizeVaultHostAndMatching(t *testing.T) {
	cases := map[string]string{
		"https://www.Example.com/login?x=1": "example.com",
		"example.com":                       "example.com",
		"http://sub.example.com:8443/":      "sub.example.com",
		"":                                  "",
		// Локальный файл и служебные страницы — не сайты: запись «пароль для
		// file» появлялась в списке и выглядела мусором (находка на стенде).
		"file:///tmp/form.html": "",
		"chrome://new-tab-page": "",
	}
	for in, want := range cases {
		if got := normalizeVaultHost(in); got != want {
			t.Fatalf("normalizeVaultHost(%q) = %q, want %q", in, got, want)
		}
	}
	// Вход, сохранённый на домене, нужен и на поддомене: иначе человек ищет
	// пароль руками там, где он уже есть.
	if !hostMatches("google.com", "accounts.google.com") {
		t.Fatal("поддомен обязан считаться тем же сайтом")
	}
	if hostMatches("example.com", "example.org") {
		t.Fatal("разные сайты совпали")
	}
}

// Анкета раскладывается по видам полей, а уже заполненное человеком поле
// затирать нельзя — кроме пароля, он всегда наш.
func TestFillFromProfileAndFieldChoice(t *testing.T) {
	values := map[string]string{}
	fillFromProfile(values, BrowserProfile{
		FirstName: "Иван", LastName: "Петров", Email: "i@example.com", Phone: "+31000000",
	})
	if values["name"] != "Иван Петров" || values["email"] != "i@example.com" ||
		values["login"] != "i@example.com" || values["phone"] != "+31000000" {
		t.Fatalf("анкета разложена неверно: %v", values)
	}

	if _, ok := valueForField(values, cdp.FormField{Kind: "email", Empty: false}); ok {
		t.Fatal("заполненное человеком поле затёрто")
	}
	if v, ok := valueForField(values, cdp.FormField{Kind: "email", Empty: true}); !ok || v == "" {
		t.Fatal("пустое поле не заполнено")
	}
	values["password"] = "secret"
	if _, ok := valueForField(values, cdp.FormField{Kind: "password", Empty: false}); !ok {
		t.Fatal("пароль обязан перезаписываться: там мог остаться прошлый")
	}
}

// «Часто открываю» ранжируется свежестью, иначе на виду навсегда останется
// сайт, куда ходили сто раз год назад.
func TestPlaceRankPrefersRecent(t *testing.T) {
	now := time.Now().Unix()
	fresh := TopSite{Count: 3, Last: now}
	old := TopSite{Count: 20, Last: now - 200*86400}
	if placeRank(fresh) <= placeRank(old) {
		t.Fatalf("свежий сайт (%.2f) не обошёл старый (%.2f)", placeRank(fresh), placeRank(old))
	}
}
