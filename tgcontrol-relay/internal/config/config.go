package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — все настройки relay-сервера. Источник — переменные окружения,
// чтобы хорошо ложиться на Docker / Fly.io / systemd EnvironmentFile.
type Config struct {
	Port            int
	PublicURL       string
	DBPath          string
	BotToken        string
	BotUsername     string        // без @, например "RemotaiBot"
	MiniAppURL      string        // URL Mini App для setChatMenuButton
	JWTSecret       string        // HS256 secret; должен быть >= 32 байт
	JWTTTL          time.Duration // время жизни device/user-JWT (продлевается «скользяще» на каждом коннекте)
	PairCodeTTL     time.Duration // время жизни pairing-кода
	PairMaxAttempts int           // максимум попыток ввода кода на пользователя
	AllowInsecure   bool          // разрешить wss-fallback к ws (только для dev)
	LogLevel        string
	CORSOrigins     []string
	MetricsToken    string // METRICS_TOKEN — если задан, /metrics отдаёт ПОЛНЫЙ набор только по этому токену

	// Админ-кабинет и уведомления поддержки (этап «админка»; конфиг уже сейчас).
	AdminIDs    []int64 // ADMIN_IDS — telegram_id админов через запятую
	AdminChatID int64   // ADMIN_CHAT_ID — чат для уведомлений о тикетах поддержки
	AdminToken  string  // ADMIN_TOKEN — bypass-авторизация /v1/admin/* для локали/тестов (опционально)

	// Tier limits (P2.8 optional).
	FreeMaxDevices int
	ProMaxDevices  int
	TeamMaxDevices int
	TrialDays      int  // проба полного Про, отсчёт от первого облачного подключения
	BetaFree       bool // legacy compatibility; never grants paid server/cloud access
	BillingEnabled bool // UI покупки скрыт, пока платёжный контур не запущен
	SelfHosted     bool // самостоятельное размещение: полный доступ без подписки Remotai

	// ЮKassa (магазин 1451249, remotai.ru). Решение PROD-007: в РФ платим
	// через неё, Stripe-заготовка не подключается.
	//
	// ⚠ Ключ живёт ТОЛЬКО в окружении релея. В репозитории его нет и быть не
	// может; на машине владельца копия лежит вне проекта.
	YooKassaShopID    string // YOOKASSA_SHOP_ID
	YooKassaSecretKey string // YOOKASSA_SECRET_KEY
	// Куда возвращать человека после оплаты.
	YooKassaReturnURL string // YOOKASSA_RETURN_URL

	// TURN — для WebRTC Remote Desktop. Оба конца (ПК-агент и телефон) за NAT,
	// поэтому при симметричном NAT прямой P2P не проходит и нужен TURN-relay.
	// Выключен по умолчанию: включается только когда на VPS открыт UDP-порт
	// (firewall). Пока выключен — cloud-WebRTC падает в WS-fallback, LAN-WebRTC
	// работает без TURN.
	TURNEnabled  bool          // TURN_ENABLED
	TURNPublicIP string        // TURN_PUBLIC_IP — внешний IP VPS для relay-кандидатов (обязателен при enabled)
	TURNPort     int           // TURN_PORT — UDP-порт STUN/TURN (default 3478)
	TURNRealm    string        // TURN_REALM (default из PublicURL host)
	TURNSecret   string        // TURN_SECRET — секрет для эфемерных REST-кредов (fallback: JWTSecret)
	TURNCredTTL  time.Duration // TURN_CRED_TTL — срок жизни выданных креденшелов (default 1h)

	// Мульти-регион и вход (docs/auth-multiregion-2026-07.md).
	// REGION=ru: только сервисы, стабильные в РФ (telegram, email, vk, yandex).
	// REGION=global: telegram, email, google, apple.
	Region string // REGION: "ru" (default) | "global"

	SMTPHost string // SMTP_HOST — без него вход по email выключен
	SMTPPort int    // SMTP_PORT (465 implicit TLS, 587 STARTTLS)
	SMTPUser string // SMTP_USER
	SMTPPass string // SMTP_PASS
	SMTPFrom string // SMTP_FROM: "Remotai <login@remotai.ru>"

	VKClientID     string // VK_CLIENT_ID (id.vk.ru)
	VKClientSecret string // VK_CLIENT_SECRET
	YandexClientID string // YANDEX_CLIENT_ID (oauth.yandex.ru)
	YandexSecret   string // YANDEX_CLIENT_SECRET
	GoogleClientID string // GOOGLE_CLIENT_ID
	GoogleSecret   string // GOOGLE_CLIENT_SECRET
	AppleClientID  string // APPLE_CLIENT_ID (Services ID для веба / bundle id)
	AppleTeamID    string // APPLE_TEAM_ID
	AppleKeyID     string // APPLE_KEY_ID
	AppleKeyPEM    string // APPLE_PRIVATE_KEY: содержимое .p8 (PEM)

	EmailCodeTTL     time.Duration // EMAIL_CODE_TTL (default 10m)
	EmailMaxAttempts int           // EMAIL_MAX_ATTEMPTS (default 5)

	// Уведомления «агент ждёт ответа» в Telegram (релейный notifier).
	// Единственный канал, работающий при закрытом приложении: в мини-аппе
	// уведомлений нет вовсе, а нативный APK получает их только пока жив.
	NotifyTG          bool          // NOTIFY_TG_ENABLED (default: BotToken != "")
	NotifyDelay       time.Duration // NOTIFY_TG_DELAY — сколько ждать перед отправкой (default 30s)
	NotifyPtyCooldown time.Duration // NOTIFY_TG_PTY_COOLDOWN — не чаще раза в … на терминал (default 10m)
	NotifyMaxPerHour  int           // NOTIFY_TG_MAX_PER_HOUR — потолок на пользователя (default 12, 0 = без лимита)
	// NOTIFY_TG_MIN_AGENT — с какой версии агента показывать кнопки ответа
	// (POST /api/pty/{id}/input). Пустое значение или "any" — не проверять
	// (для dev-стенда, где agent_version бывает нечисловой).
	//
	// ⚠ Дефолт "2.30.0" — это ВЕРСИЯ БЛИЖАЙШЕГО РЕЛИЗА, в котором у агента
	// появляется сам эндпоинт (в репозитории сейчас 2.29.0, то есть значение
	// намеренно «из будущего»). Сдвинется номер релиза — обязан сдвинуться и
	// дефолт: занизишь — кнопки покажутся агентам без /input и упадут в 404;
	// завысишь — кнопок не будет ни у кого. Держать в одном ряду с бампом
	// версии из релизного чек-листа (CLAUDE.md).
	NotifyMinAgent string
	// NOTIFY_TG_DEEPLINK_DEVICE — класть ли в web_app-ссылку идентификатор ПК
	// (`#pty_<device>_<pty>` вместо `#pty_<pty>`). Включать ТОЛЬКО после
	// релиза клиента, который умеет разбирать новую форму: старый /tg/ склеит
	// её в несуществующий id терминала.
	NotifyDeepLinkDevice bool
}

// FromEnv читает конфиг из переменных окружения с разумными дефолтами.
func FromEnv() (*Config, error) {
	c := &Config{
		Port:        getInt("PORT", 8090),
		PublicURL:   getString("PUBLIC_URL", "http://localhost:8090"),
		DBPath:      getString("DB_PATH", "./relay.db"),
		BotToken:    os.Getenv("BOT_TOKEN"),
		BotUsername: getString("BOT_USERNAME", "RemotaiBot"),
		MiniAppURL:  getString("MINIAPP_URL", "https://remotai.ru/tg/"),
		JWTSecret:   os.Getenv("JWT_HMAC_SECRET"),
		JWTTTL:      getDuration("JWT_TTL", 365*24*time.Hour),
		// Час, а не 15 минут: живой случай 26.08 — человек сфотографировал экран
		// с QR и отправил его в мессенджер, а пока фото дошло, код умер. Скан
		// опоздал на 20 секунд, релей ответил 410. Помощь «пришлите мне код» —
		// штатный способ подключения, и 15 минут для него мало. Брутфорс от
		// этого не дешевеет: код — 8 знаков из алфавита в 26 символов (2·10¹¹
		// вариантов) и сгорает после MaxPairCodeAttempts неудачных попыток.
		PairCodeTTL:     getDuration("PAIR_CODE_TTL", time.Hour),
		PairMaxAttempts: getInt("PAIR_MAX_ATTEMPTS", 5),
		AllowInsecure:   getBool("ALLOW_INSECURE", false),
		LogLevel:        getString("LOG_LEVEL", "info"),
		CORSOrigins:     splitCSV(getString("CORS_ORIGINS", "*")),
		MetricsToken:    os.Getenv("METRICS_TOKEN"),
		AdminIDs:        getInt64CSV("ADMIN_IDS"),
		AdminChatID:     int64(getInt("ADMIN_CHAT_ID", 0)),
		AdminToken:      os.Getenv("ADMIN_TOKEN"),
		FreeMaxDevices:  getInt("FREE_MAX_DEVICES", 2),
		ProMaxDevices:   getInt("PRO_MAX_DEVICES", 5),
		// Канон: Флит — 25 облачных устройств (было 30, расходилось с витриной).
		TeamMaxDevices: getInt("TEAM_MAX_DEVICES", 25),
		// Канон: вход — 30 дней полного Про от первого облачного подключения
		// (было 7 дней от регистрации).
		TrialDays:         getInt("TRIAL_DAYS", 30),
		BetaFree:          getBool("BETA_FREE", false),
		BillingEnabled:    getBool("BILLING_ENABLED", false),
		SelfHosted:        getBool("SELF_HOSTED", false),
		YooKassaShopID:    os.Getenv("YOOKASSA_SHOP_ID"),
		YooKassaSecretKey: os.Getenv("YOOKASSA_SECRET_KEY"),
		YooKassaReturnURL: getString("YOOKASSA_RETURN_URL", "https://remotai.ru/app/#/plan"),
		TURNEnabled:       getBool("TURN_ENABLED", false),
		TURNPublicIP:      os.Getenv("TURN_PUBLIC_IP"),
		TURNPort:          getInt("TURN_PORT", 3478),
		TURNRealm:         getString("TURN_REALM", "remotai.ru"),
		TURNSecret:        os.Getenv("TURN_SECRET"),
		TURNCredTTL:       getDuration("TURN_CRED_TTL", time.Hour),
		Region:            getString("REGION", "ru"),
		SMTPHost:          os.Getenv("SMTP_HOST"),
		SMTPPort:          getInt("SMTP_PORT", 465),
		SMTPUser:          os.Getenv("SMTP_USER"),
		SMTPPass:          os.Getenv("SMTP_PASS"),
		SMTPFrom:          os.Getenv("SMTP_FROM"),
		VKClientID:        os.Getenv("VK_CLIENT_ID"),
		VKClientSecret:    os.Getenv("VK_CLIENT_SECRET"),
		YandexClientID:    os.Getenv("YANDEX_CLIENT_ID"),
		YandexSecret:      os.Getenv("YANDEX_CLIENT_SECRET"),
		GoogleClientID:    os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleSecret:      os.Getenv("GOOGLE_CLIENT_SECRET"),
		AppleClientID:     os.Getenv("APPLE_CLIENT_ID"),
		AppleTeamID:       os.Getenv("APPLE_TEAM_ID"),
		AppleKeyID:        os.Getenv("APPLE_KEY_ID"),
		AppleKeyPEM:       os.Getenv("APPLE_PRIVATE_KEY"),
		EmailCodeTTL:      getDuration("EMAIL_CODE_TTL", 10*time.Minute),
		EmailMaxAttempts:  getInt("EMAIL_MAX_ATTEMPTS", 5),

		NotifyDelay:       getDuration("NOTIFY_TG_DELAY", 30*time.Second),
		NotifyPtyCooldown: getDuration("NOTIFY_TG_PTY_COOLDOWN", 10*time.Minute),
		NotifyMaxPerHour:  getInt("NOTIFY_TG_MAX_PER_HOUR", 12),
		// 2.30.0 = релиз, в котором у агента появляется POST /api/pty/{id}/input
		// (см. комментарий у поля: значение привязано к номеру релиза).
		NotifyMinAgent:       getString("NOTIFY_TG_MIN_AGENT", "2.30.0"),
		NotifyDeepLinkDevice: getBool("NOTIFY_TG_DEEPLINK_DEVICE", false),
	}
	// Без бота отправлять некуда — по умолчанию notifier включён ровно тогда,
	// когда есть BOT_TOKEN.
	c.NotifyTG = getBool("NOTIFY_TG_ENABLED", c.BotToken != "")
	if c.SelfHosted && c.BillingEnabled {
		return nil, errors.New("SELF_HOSTED и BILLING_ENABLED нельзя включать одновременно")
	}
	if c.TURNEnabled && c.TURNPublicIP == "" {
		return nil, errors.New("TURN_ENABLED=true требует TURN_PUBLIC_IP (внешний IP VPS)")
	}
	if c.JWTSecret == "" {
		// безопасный дефолт для dev: эпhemeral секрет, но кричим в лог
		c.JWTSecret = devSecret()
		log.Printf("[CONFIG] WARNING: JWT_HMAC_SECRET не задан — используется ephemeral dev-секрет (все токены умрут при рестарте)")
	}
	if c.TURNSecret == "" {
		c.TURNSecret = c.JWTSecret // server-side-only HMAC secret; reuse is fine (после финализации JWTSecret)
	}
	if len(c.JWTSecret) < 16 {
		return nil, errors.New("JWT_HMAC_SECRET должен быть >= 16 символов")
	}
	if c.BotToken == "" {
		log.Printf("[CONFIG] WARNING: BOT_TOKEN не задан — Telegram-бот не будет запущен. Только REST/WS API.")
	}
	return c, nil
}

func (c *Config) Addr() string { return fmt.Sprintf(":%d", c.Port) }

func getString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// getInt64CSV — список int64 через запятую ("123, 456" → [123, 456]).
// Битые элементы пропускаем — лучше меньше админов, чем упавший старт.
func getInt64CSV(key string) []int64 {
	var out []int64
	for _, p := range splitCSV(os.Getenv(key)) {
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func getBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// devSecret — псевдо-случайный секрет для локальной разработки, чтобы
// сервер запускался без явной настройки JWT_HMAC_SECRET.
func devSecret() string {
	return fmt.Sprintf("dev-secret-%d-%s", time.Now().UnixNano(), os.Getenv("USERNAME"))
}

// AuthProvider — способ входа, доступный на ЭТОМ инстансе. Клиент строит
// экран входа по этому списку и ничего не хардкодит сам.
type AuthProvider struct {
	ID    string `json:"id"`    // 'telegram' | 'email' | 'vk' | 'yandex' | 'google' | 'apple'
	Kind  string `json:"kind"`  // 'deeplink' | 'email_code' | 'oauth'
	Label string `json:"label"` // подпись кнопки
}

// AuthProviders возвращает включённые способы входа. Провайдер попадает в
// список только если (а) разрешён регионом и (б) настроены его креды.
// Пустой OAuth-набор не ломает сервер: у клиента просто меньше кнопок.
func (c *Config) AuthProviders() []AuthProvider {
	out := make([]AuthProvider, 0, 6)
	if c.BotUsername != "" && c.BotToken != "" {
		out = append(out, AuthProvider{ID: "telegram", Kind: "deeplink", Label: "Telegram"})
	}
	if c.SMTPHost != "" && c.SMTPFrom != "" {
		out = append(out, AuthProvider{ID: "email", Kind: "email_code", Label: "Email"})
	}
	if c.Region == "global" {
		if c.GoogleClientID != "" && c.GoogleSecret != "" {
			out = append(out, AuthProvider{ID: "google", Kind: "oauth", Label: "Google"})
		}
		// Apple: id_token-валидация реализована; для redirect-флоу нужен ещё
		// ключ .p8 (client_secret это ES256 JWT).
		if c.AppleClientID != "" && c.AppleTeamID != "" && c.AppleKeyID != "" && c.AppleKeyPEM != "" {
			out = append(out, AuthProvider{ID: "apple", Kind: "oauth", Label: "Apple"})
		}
	} else {
		if c.VKClientID != "" && c.VKClientSecret != "" {
			out = append(out, AuthProvider{ID: "vk", Kind: "oauth", Label: "VK ID"})
		}
		if c.YandexClientID != "" && c.YandexSecret != "" {
			out = append(out, AuthProvider{ID: "yandex", Kind: "oauth", Label: "Яндекс ID"})
		}
	}
	return out
}
