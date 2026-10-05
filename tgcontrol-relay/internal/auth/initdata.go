package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TelegramUser — минимальный набор полей из user= параметра initData.
type TelegramUser struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Username     string `json:"username"`
	LanguageCode string `json:"language_code"`
	PhotoURL     string `json:"photo_url"`
}

type InitData struct {
	QueryID  string
	User     *TelegramUser
	AuthDate time.Time
	Hash     string
	Raw      string
}

// ParseInitData разбирает строку initData (формат `key=value&key=value`)
// и проверяет HMAC по bot token. Возвращает данные пользователя.
// См. https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app
func ParseInitData(raw, botToken string, maxAge time.Duration) (*InitData, error) {
	if raw == "" {
		return nil, errors.New("empty initData")
	}
	if botToken == "" {
		return nil, errors.New("bot token not configured on relay")
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	hash := values.Get("hash")
	if hash == "" {
		return nil, errors.New("hash field missing")
	}
	values.Del("hash")

	// secret = HMAC_SHA256(bot_token, "WebAppData")
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	secretKey := secret.Sum(nil)

	if !checkTelegramHash(values, secretKey, hash) {
		return nil, errors.New("initData hash mismatch")
	}

	authDateStr := values.Get("auth_date")
	if authDateStr == "" {
		return nil, errors.New("auth_date missing")
	}
	ts, err := strconv.ParseInt(authDateStr, 10, 64)
	if err != nil {
		return nil, errors.New("auth_date not integer")
	}
	authDate := time.Unix(ts, 0)
	if maxAge > 0 && time.Since(authDate) > maxAge {
		return nil, errors.New("initData expired (auth_date too old)")
	}

	out := &InitData{
		QueryID:  values.Get("query_id"),
		AuthDate: authDate,
		Hash:     hash,
		Raw:      raw,
	}
	if userJSON := values.Get("user"); userJSON != "" {
		u := &TelegramUser{}
		if err := json.Unmarshal([]byte(userJSON), u); err != nil {
			return nil, errors.New("user field is not valid JSON")
		}
		out.User = u
	}
	if out.User == nil || out.User.ID == 0 {
		return nil, errors.New("user.id missing in initData")
	}
	return out, nil
}

// ParseLoginWidgetData валидирует данные Telegram Login Widget (плоские поля
// id/first_name/last_name/username/photo_url/auth_date/hash, urlencoded).
// Отличие от Mini App initData — другой секрет подписи: SHA256(bot_token)
// без HMAC-обёртки "WebAppData", поэтому ParseInitData для виджета НЕ
// подходит. См. https://core.telegram.org/widgets/login#checking-authorization
//
// maxAge <= 0 — auth_date не проверяется (хэш сам по себе доказательство;
// у виджета нет протухания, как у query_id мини-аппа).
func ParseLoginWidgetData(raw, botToken string, maxAge time.Duration) (*TelegramUser, error) {
	if raw == "" {
		return nil, errors.New("empty login data")
	}
	if botToken == "" {
		return nil, errors.New("bot token not configured on relay")
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	hash := values.Get("hash")
	if hash == "" {
		return nil, errors.New("hash field missing")
	}
	values.Del("hash")

	secretKey := sha256.Sum256([]byte(botToken))
	if !checkTelegramHash(values, secretKey[:], hash) {
		return nil, errors.New("login data hash mismatch")
	}

	authDateStr := values.Get("auth_date")
	if authDateStr == "" {
		return nil, errors.New("auth_date missing")
	}
	ts, err := strconv.ParseInt(authDateStr, 10, 64)
	if err != nil {
		return nil, errors.New("auth_date not integer")
	}
	if maxAge > 0 && time.Since(time.Unix(ts, 0)) > maxAge {
		return nil, errors.New("login data expired (auth_date too old)")
	}

	id, err := strconv.ParseInt(values.Get("id"), 10, 64)
	if err != nil || id == 0 {
		return nil, errors.New("id missing in login data")
	}
	return &TelegramUser{
		ID:        id,
		FirstName: values.Get("first_name"),
		LastName:  values.Get("last_name"),
		Username:  values.Get("username"),
		PhotoURL:  values.Get("photo_url"),
	}, nil
}

// checkTelegramHash сверяет подпись Telegram-данных: data-check-string —
// ключи (кроме hash) отсортированы лексикографически и склеены через \n,
// подпись — HMAC_SHA256(data-check-string, secretKey).
func checkTelegramHash(values url.Values, secretKey []byte, hash string) bool {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(values.Get(k))
	}
	mac := hmac.New(sha256.New, secretKey)
	mac.Write([]byte(b.String()))
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(hash))
}
