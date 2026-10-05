package web

// Часовой пояс браузера — по адресу, с которого он выходит в сеть.
//
// Сервер живёт по UTC, а его IP — например, амстердамский. Сайт видит «телефон
// в Нидерландах, часы по Гринвичу» и справедливо считает это подделкой: у
// настоящего устройства часовой пояс совпадает со страной адреса. Эта мелочь
// стоит дорого — именно на ней антифрод регистрации просит «подтвердите, что вы
// не робот» вместо того, чтобы завести аккаунт.
//
// Узнаём страну ОДИН раз за время жизни агента (адрес сервера не меняется) и
// только тогда, когда браузер действительно поднят: лишних запросов наружу с
// чужой машины быть не должно.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// browserEnv — часовой пояс и язык, согласованные с адресом сервера.
type browserEnv struct {
	Timezone string // "Europe/Amsterdam"
	Locale   string // "en-US" — язык браузера остаётся нейтральным
	Country  string // "NL"
}

var (
	envOnce   sync.Once
	envValue  browserEnv
	envLookup = lookupServerEnv // подменяется в тестах
)

// serverBrowserEnv отдаёт окружение браузера, определяя его при первом вызове.
func serverBrowserEnv() browserEnv {
	envOnce.Do(func() {
		envValue = envLookup()
		if envValue.Timezone != "" {
			log.Printf("[VBROWSER] окружение браузера: %s (%s)", envValue.Timezone, envValue.Country)
		}
	})
	return envValue
}

// lookupServerEnv спрашивает часовой пояс у сервиса геолокации по IP. Сервисов
// два: первый отдаёт готовый пояс, второй — только страну (её переводим по
// таблице). Не получилось ни то, ни другое — возвращаем пустое окружение, и
// браузер остаётся с часами системы: это хуже для сайтов, но ничего не ломает.
func lookupServerEnv() browserEnv {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	if env, ok := lookupIPAPI(ctx); ok {
		return env
	}
	if country, ok := lookupCloudflareCountry(ctx); ok {
		return browserEnv{Timezone: timezoneForCountry(country), Locale: "en-US", Country: country}
	}
	return browserEnv{}
}

func lookupIPAPI(ctx context.Context) (browserEnv, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://ip-api.com/json/?fields=status,countryCode,timezone", nil)
	if err != nil {
		return browserEnv{}, false
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return browserEnv{}, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return browserEnv{}, false
	}
	var v struct {
		Status      string `json:"status"`
		CountryCode string `json:"countryCode"`
		Timezone    string `json:"timezone"`
	}
	if json.Unmarshal(body, &v) != nil || v.Status != "success" || v.Timezone == "" {
		return browserEnv{}, false
	}
	return browserEnv{Timezone: v.Timezone, Locale: "en-US", Country: v.CountryCode}, true
}

func lookupCloudflareCountry(ctx context.Context) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		return "", false
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(body), "\n") {
		if code, ok := strings.CutPrefix(strings.TrimSpace(line), "loc="); ok && code != "" {
			return code, true
		}
	}
	return "", false
}

// timezoneForCountry — грубое соответствие «страна → пояс». Точность здесь
// нужна ровно такая: сайт сверяет пояс со страной адреса, а не с городом.
// Страны с несколькими поясами берут пояс столицы — там живёт большинство
// адресов хостингов.
func timezoneForCountry(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "NL":
		return "Europe/Amsterdam"
	case "DE":
		return "Europe/Berlin"
	case "FR":
		return "Europe/Paris"
	case "GB", "UK":
		return "Europe/London"
	case "RU":
		return "Europe/Moscow"
	case "PL":
		return "Europe/Warsaw"
	case "FI":
		return "Europe/Helsinki"
	case "SE":
		return "Europe/Stockholm"
	case "ES":
		return "Europe/Madrid"
	case "IT":
		return "Europe/Rome"
	case "CH":
		return "Europe/Zurich"
	case "AT":
		return "Europe/Vienna"
	case "CZ":
		return "Europe/Prague"
	case "LT":
		return "Europe/Vilnius"
	case "LV":
		return "Europe/Riga"
	case "EE":
		return "Europe/Tallinn"
	case "MD":
		return "Europe/Chisinau"
	case "UA":
		return "Europe/Kyiv"
	case "TR":
		return "Europe/Istanbul"
	case "AM":
		return "Asia/Yerevan"
	case "GE":
		return "Asia/Tbilisi"
	case "KZ":
		return "Asia/Almaty"
	case "AE":
		return "Asia/Dubai"
	case "IN":
		return "Asia/Kolkata"
	case "SG":
		return "Asia/Singapore"
	case "JP":
		return "Asia/Tokyo"
	case "HK":
		return "Asia/Hong_Kong"
	case "AU":
		return "Australia/Sydney"
	case "BR":
		return "America/Sao_Paulo"
	case "CA":
		return "America/Toronto"
	case "US":
		return "America/New_York"
	default:
		return ""
	}
}
