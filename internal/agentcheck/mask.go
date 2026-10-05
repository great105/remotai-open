package agentcheck

import (
	"net/url"
	"regexp"
	"strings"
)

// Mask — как ключ показывается человеку: «sk-…a1b2». Последние четыре знака
// нужны, чтобы узнать СВОЙ ключ среди нескольких; больше не показываем ничего.
// Короткие строки (меньше 12 знаков) не раскрываем даже хвостом: у такого
// «ключа» четыре знака — это треть секрета.
func Mask(secret string) string {
	s := strings.TrimSpace(secret)
	if s == "" {
		return ""
	}
	prefix := ""
	if strings.HasPrefix(s, "sk-") {
		prefix = "sk-"
	}
	if len(s) < 12 {
		return prefix + "…"
	}
	return prefix + "…" + s[len(s)-4:]
}

// DisplayURL — адрес без того, что может оказаться секретом: логина и пароля
// в URL, строки запроса и якоря. Схема, хост и путь — это и есть «куда ходит».
func DisplayURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	out := u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/")
	return out
}

// Host — только имя хоста: им говорим «нет сети до …».
func Host(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

var keyLike = regexp.MustCompile(`(sk-[A-Za-z0-9_\-]{6,}|Bearer\s+[A-Za-z0-9._\-]{12,}|eyJ[A-Za-z0-9._\-]{20,})`)

// Scrub вычищает из текста ответа (ошибки провайдера, вывод CLI) всё, что
// похоже на ключ, и сам ключ проверки, если провайдер эхом вернул его в
// сообщении. Обрезает до max знаков: причина нужна человеку одной строкой.
func Scrub(text string, secrets []string, max int) string {
	s := text
	for _, sec := range secrets {
		sec = strings.TrimSpace(sec)
		if len(sec) >= 6 {
			s = strings.ReplaceAll(s, sec, Mask(sec))
		}
	}
	s = keyLike.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "Bearer") {
			return "Bearer …"
		}
		return Mask(m)
	})
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len([]rune(s)) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}
