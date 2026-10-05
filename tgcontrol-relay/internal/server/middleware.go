package server

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"tgcontrol-relay/internal/auth"
	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/metrics"
)

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allow := false
		if len(s.Config.CORSOrigins) == 0 || (len(s.Config.CORSOrigins) == 1 && s.Config.CORSOrigins[0] == "*") {
			allow = true
		} else {
			for _, o := range s.Config.CORSOrigins {
				if o == origin {
					allow = true
					break
				}
			}
		}
		if allow && origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		} else if len(s.Config.CORSOrigins) == 1 && s.Config.CORSOrigins[0] == "*" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Tg-Init-Data, X-Remotai-Client")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		dur := time.Since(start)
		log.Printf("[HTTP] %s %s -> %d (%s) from=%s", r.Method, r.URL.Path, ww.status, dur, auth.ClientIP(r))
		// WS-апгрейды hijack'аются и живут минутами — их длительность исказила бы
		// latency, поэтому в HTTP-метрики попадают только обычные запросы.
		if !ww.hijacked {
			metrics.ObserveHTTP(ww.status, dur.Nanoseconds())
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (w *statusRecorder) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }

// Hijack forwards to the underlying ResponseWriter so WebSocket upgrades
// (agent + client WS) work through the logging middleware.
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		w.hijacked = true
		return h.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

// rateLimitIP — per-IP лимит на эндпоинты pairing.
func (s *Server) rateLimitIP(perMinute, burst int) func(http.Handler) http.Handler {
	limiter := auth.NewLimiter(perMinute, burst)
	go func() {
		t := time.NewTicker(10 * time.Minute)
		for range t.C {
			limiter.Cleanup(30 * time.Minute)
		}
	}()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := auth.ClientIP(r)
			if !limiter.Allow(ip) {
				w.Header().Set("Retry-After", "60")
				writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireUserAuth — авторизация пользователя через initData (Telegram) или Bearer JWT.
func (s *Server) requireUserAuth(r *http.Request) (*UserAuthClaims, error) {
	// Bearer JWT (header) or ?jwt= query param. Browser WebSocket clients such
	// as the APK can't set Authorization headers on the WS handshake, so they
	// pass the user JWT as a query param instead.
	userTok := bearer(r.Header.Get("Authorization"))
	if userTok == "" || strings.HasPrefix(strings.ToLower(userTok), "tma ") {
		userTok = r.URL.Query().Get("jwt")
	}
	if userTok != "" && !strings.HasPrefix(strings.ToLower(userTok), "tma ") {
		claims, err := s.JWT.ParseUser(userTok)
		if err == nil {
			sessionID := claims.SessionID
			if sessionID == "" {
				sessionID = claims.ID // legacy token: one token = one session
			}
			if revoked, rerr := auth.IsRevoked(r.Context(), s.DB, claims.ID); rerr != nil {
				return nil, rerr
			} else if revoked {
				return nil, errors.New("session revoked")
			}
			if revoked, rerr := db.UserSessionRevoked(r.Context(), s.DB, sessionID, claims.UserID); rerr != nil {
				return nil, rerr
			} else if revoked {
				return nil, errors.New("session revoked")
			}
			kind, name := sessionClientMeta(r)
			expiresAt := time.Time{}
			if claims.ExpiresAt != nil {
				expiresAt = claims.ExpiresAt.Time
			}
			fresh, rerr := db.RecordUserSession(
				r.Context(), s.DB, sessionID, claims.UserID, claims.ID,
				kind, name, r.Header.Get("User-Agent"), auth.ClientIP(r), expiresAt,
			)
			if rerr != nil {
				log.Printf("[AUTH] record session %s: %v", sessionID, rerr)
			} else if fresh {
				// Новый вход возник НЕ на логине, а на обычном запросе: клиент
				// пришёл с токеном, чью сессию мы ещё не видели. Пишем откуда —
				// иначе список входов растёт молча (жалоба 2026-07-30).
				log.Printf("[AUTH] новая сессия %s: %s (%s) ip=%s путь=%s",
					sessionID, name, kind, auth.ClientIP(r), r.URL.Path)
			}
			return &UserAuthClaims{
				UserID: claims.UserID, TelegramID: claims.TelegramID, Tier: claims.Tier,
				Source: "jwt", SessionID: sessionID, JTI: claims.ID, ExpiresAt: expiresAt,
			}, nil
		}
	}
	// Telegram initData в Authorization: tma <initData> или X-Tg-Init-Data
	initRaw := r.Header.Get("X-Tg-Init-Data")
	if initRaw == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "tma ") {
			initRaw = strings.TrimSpace(h[4:])
		}
	}
	if initRaw == "" {
		initRaw = r.URL.Query().Get("initData")
	}
	if initRaw == "" {
		return nil, ErrAuth
	}
	parsed, err := auth.ParseInitData(initRaw, s.Config.BotToken, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	u, err := dbUpsertFromInitData(r.Context(), s, parsed)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(parsed.Raw))
	sessionID := fmt.Sprintf("tma-%d-%x", u.ID, hash[:12])
	if revoked, rerr := db.UserSessionRevoked(r.Context(), s.DB, sessionID, u.ID); rerr != nil {
		return nil, rerr
	} else if revoked {
		return nil, errors.New("session revoked; reopen Telegram Mini App")
	}
	expiresAt := parsed.AuthDate.Add(24 * time.Hour)
	if _, rerr := db.RecordUserSession(
		r.Context(), s.DB, sessionID, u.ID, "", "telegram", "Telegram Mini App",
		r.Header.Get("User-Agent"), auth.ClientIP(r), expiresAt,
	); rerr != nil {
		log.Printf("[AUTH] record Telegram session %s: %v", sessionID, rerr)
	}
	return &UserAuthClaims{
		UserID: u.ID, TelegramID: u.TelegramID, Tier: u.Tier, Source: "initdata",
		SessionID: sessionID, ExpiresAt: expiresAt,
	}, nil
}

type UserAuthClaims struct {
	UserID     int64
	TelegramID int64
	Tier       string
	Source     string // "jwt" | "initdata"
	SessionID  string
	JTI        string
	ExpiresAt  time.Time
}

// slidingUserJWT мини́т свежий user-JWT для «скользящего» продления срока, когда
// клиент авторизовался Bearer-токеном (анонимные cloud-аккаунты, чей JWT иначе
// жёстко протух бы на JWTTTL без какого-либо обновления — телефон тогда получает
// новый аккаунт, а ПК остаётся за старым → 409 при ре-пейринге). Telegram-клиенты
// (Source=="initdata") логинятся через initData каждую сессию, им продление не нужно.
// Хендлеры account-эндпоинтов кладут результат в ответ как refreshed_jwt/_expires_at;
// клиент сохраняет его. Пустая строка при Source!="jwt" или ошибке выпуска.
func (s *Server) slidingUserJWT(r *http.Request, claims *UserAuthClaims) (jwt, expiresAt string) {
	if claims == nil || claims.Source != "jwt" {
		return "", ""
	}
	tok, exp, err := s.JWT.IssueUserForSession(claims.UserID, claims.TelegramID, claims.Tier, claims.SessionID)
	if err != nil {
		return "", ""
	}
	freshClaims, err := s.JWT.ParseUser(tok)
	if err == nil {
		kind, name := sessionClientMeta(r)
		if _, rerr := db.RecordUserSession(
			r.Context(), s.DB, claims.SessionID, claims.UserID, freshClaims.ID,
			kind, name, r.Header.Get("User-Agent"), auth.ClientIP(r), exp,
		); rerr != nil {
			log.Printf("[AUTH] refresh session %s: %v", claims.SessionID, rerr)
		}
	}
	return tok, exp.Format(time.RFC3339)
}

// mintUserSessionJWT starts (or resumes) a durable client session and records enough
// metadata for the user-facing “Сеансы входа” screen. sessionID is empty for a
// new login and stable for refreshes.
func (s *Server) mintUserSessionJWT(r *http.Request, user *db.User, sessionID string) (string, time.Time, error) {
	tok, exp, err := s.JWT.IssueUserForSession(user.ID, user.TelegramID, user.Tier, sessionID)
	if err != nil {
		return "", time.Time{}, err
	}
	claims, err := s.JWT.ParseUser(tok)
	if err != nil {
		return "", time.Time{}, err
	}
	kind, name := sessionClientMeta(r)
	if _, err := db.RecordUserSession(
		r.Context(), s.DB, claims.SessionID, user.ID, claims.ID,
		kind, name, r.Header.Get("User-Agent"), auth.ClientIP(r), exp,
	); err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// sessionClientMeta — как вход называется человеку в списке «Входы в аккаунт».
//
// Имя должно ОТВЕЧАТЬ НА ВОПРОС «это я?». Пока всё, что не Android и не
// Telegram, называлось «Веб-приложение», список из десяти одинаковых строк
// выглядел как чужие подключения — живой вопрос владельца (2026-07-30): «это
// нормально, что много веб-сессий каких-то создаётся?». Различаем окно Remotai
// на компьютере, обычный браузер и запрос из скрипта: по такому списку сразу
// видно, кто и откуда.
func sessionClientMeta(r *http.Request) (kind, name string) {
	kind = strings.ToLower(strings.TrimSpace(r.Header.Get("X-Remotai-Client")))
	switch kind {
	case "android":
		return kind, "Remotai для Android"
	case "telegram":
		return kind, "Telegram Mini App"
	case "desktop":
		return kind, "Окно Remotai на компьютере"
	case "web":
		return kind, browserName(r.Header.Get("User-Agent"))
	}
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	switch {
	case strings.Contains(ua, "android"):
		return "android", "Remotai для Android"
	case strings.Contains(ua, "telegram"):
		return "telegram", "Telegram Mini App"
	default:
		return "web", browserName(r.Header.Get("User-Agent"))
	}
}

// browserName собирает понятное имя из User-Agent: «Chrome на Windows»,
// «Окно Remotai на компьютере», «Скрипт (API)».
func browserName(userAgent string) string {
	ua := strings.ToLower(userAgent)
	if ua == "" {
		return "Веб-приложение"
	}
	// Запросы не из браузера: свои скрипты, curl, интеграции. Их особенно важно
	// называть честно — человек должен видеть, что это не «ещё один телефон».
	for _, marker := range []string{"python", "curl", "wget", "go-http-client", "okhttp", "postman"} {
		if strings.Contains(ua, marker) {
			return "Скрипт (API)"
		}
	}
	// Окно Remotai на Windows — это WebView2: он представляется как Edge с
	// пометкой «Microsoft Windows» в платформенной части.
	if strings.Contains(ua, "microsoft windows") && !strings.Contains(ua, "edg/") {
		return "Окно Remotai на компьютере"
	}

	platform := ""
	switch {
	// iPhone и iPad проверяем ПЕРВЫМИ: их User-Agent содержит «like Mac OS X»,
	// и телефон иначе объявляется компьютером.
	case strings.Contains(ua, "iphone"):
		platform = "iPhone"
	case strings.Contains(ua, "ipad"):
		platform = "iPad"
	case strings.Contains(ua, "android"):
		platform = "Android"
	case strings.Contains(ua, "windows"):
		platform = "Windows"
	case strings.Contains(ua, "mac os") || strings.Contains(ua, "macintosh"):
		platform = "macOS"
	case strings.Contains(ua, "linux"):
		platform = "Linux"
	}
	browser := ""
	switch {
	case strings.Contains(ua, "edg/"):
		browser = "Edge"
	case strings.Contains(ua, "yabrowser"):
		browser = "Яндекс.Браузер"
	case strings.Contains(ua, "firefox") || strings.Contains(ua, "fxios"):
		browser = "Firefox"
	// CriOS — это Chrome на iOS; обычный «safari» в его строке тоже есть.
	case strings.Contains(ua, "chrome") || strings.Contains(ua, "crios"):
		browser = "Chrome"
	case strings.Contains(ua, "safari"):
		browser = "Safari"
	}
	switch {
	case browser != "" && platform != "":
		return browser + " на " + platform
	case browser != "":
		return browser
	case platform != "":
		return "Браузер на " + platform
	default:
		return "Веб-приложение"
	}
}

// helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeErrCode — ошибка с МАШИННЫМ кодом. Клиент не должен разбирать
// англоязычный текст: «agent offline» приходил как обычная 502 и превращался в
// «Ошибка сервера — попробуйте позже», хотя это самый частый случай дня —
// компьютер просто выключен. Клиент читает поле code (api-core.ts, cloudApi).
func writeErrCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

var ErrAuth = httpErr{code: http.StatusUnauthorized, msg: "authentication required"}

type httpErr struct {
	code int
	msg  string
}

func (e httpErr) Error() string { return e.msg }
