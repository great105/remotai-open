// Package server — HTTP/WS API relay-сервера.
package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"tgcontrol-relay/internal/auth"
	"tgcontrol-relay/internal/billing"
	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/mailer"
	"tgcontrol-relay/internal/metrics"
	"tgcontrol-relay/internal/notify"
	"tgcontrol-relay/internal/relayhub"
)

// Bot — минимальный интерфейс, чтобы избежать циклической зависимости bot ↔ server.
type Bot interface {
	Start(ctx context.Context) error
}

// MailSender — отправка транзакционных писем (коды входа). Интерфейс, чтобы
// в тестах подставлять заглушку вместо SMTP.
type MailSender interface {
	Send(to, subject, body string) error
}

type Server struct {
	Config *config.Config
	DB     *sql.DB
	JWT    *auth.Issuer
	Hub    *relayhub.Hub
	Bot    Bot
	Mailer MailSender

	// BillingClient — касса. Обычно nil: клиент собирается из ключей магазина на
	// каждый запрос (см. yooKassa). Задаётся в тестах, где нужен свой httptest
	// вместо api.yookassa.ru: без этого шва проверить зачисление оплаты можно
	// было бы только живыми деньгами.
	BillingClient *billing.Client

	// AdminNotify — отправка уведомления админу в Telegram (новое сообщение
	// поддержки). Подключается из main.go, когда жив бот и задан ADMIN_CHAT_ID;
	// nil — уведомления выключены. Вызов не должен блокировать запрос.
	AdminNotify func(text string)

	// UserNotify — сообщение ПОЛЬЗОВАТЕЛЮ в личный чат бота (вопрос агента).
	// Проводится из main.go тем же приёмом, что AdminNotify, чтобы server не
	// импортировал bot. nil — уведомлений нет (бот не настроен).
	UserNotify func(ctx context.Context, chatID int64, n notify.Notice) error

	// UserNotifySupport — сообщение ПОЛЬЗОВАТЕЛЮ о том, что поддержка ответила
	// (превью ответа + кнопка «Открыть чат поддержки»). Проводится из main.go тем
	// же приёмом, что UserNotify. nil — бот не настроен, уведомления нет.
	UserNotifySupport func(ctx context.Context, chatID int64, preview string) error

	// UserNotifyText — спокойное служебное сообщение ПОЛЬЗОВАТЕЛЮ в личный чат
	// бота без клавиатуры: «ПК обновился до vX» (server/notifier.go,
	// fireAgentUpdated). Проводится из main.go тем же приёмом, что UserNotify.
	// nil — бот не настроен, уведомления нет.
	UserNotifyText func(ctx context.Context, chatID int64, deviceName, version string) error

	// UserSendText — текстовое сообщение ОТ АГЕНТА («remotai send») владельцу
	// в личный чат бота (server/agent_send.go, handleAgentSend). Проводится из
	// main.go тем же приёмом, что UserNotify. nil — бот не настроен.
	UserSendText func(ctx context.Context, chatID int64, deviceName, text string) error

	// UserSendFile — доставка документа пользователю через облачного бота.
	// Файл уже скачан с его ПК во временный spool; bot-пакет сюда не импортируем.
	UserSendFile func(ctx context.Context, chatID int64, filename string, r io.Reader) error

	// Notifier — подписчик на события агентов, превращающий «агент ждёт
	// ответа» в сообщение Telegram. nil — фича выключена.
	Notifier *Notifier

	// agentSends — in-memory rate limit /v1/agent/send (agent_send.go).
	agentSends agentSendLimiter

	httpSrv *http.Server

	// Короткий кэш статистики из БД для /metrics: scrape Prometheus идёт раз в
	// 15–30с, а COUNT-ы бьют по единственному write-соединению SQLite — кэш
	// гасит лишнюю нагрузку без заметной потери свежести.
	statsMu  sync.Mutex
	statsAt  time.Time
	statsVal db.Stats
}

func New(cfg *config.Config, d *sql.DB) *Server {
	return &Server{
		Config: cfg,
		DB:     d,
		JWT:    auth.NewIssuer(cfg.JWTSecret, cfg.JWTTTL),
		Hub:    relayhub.NewHub(),
		Mailer: mailer.New(mailer.Config{
			Host: cfg.SMTPHost,
			Port: cfg.SMTPPort,
			User: cfg.SMTPUser,
			Pass: cfg.SMTPPass,
			From: cfg.SMTPFrom,
		}),
	}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(loggingMiddleware)
	r.Use(s.corsMiddleware)

	r.Get("/health", s.handleHealth)
	r.Get("/metrics", s.handleMetrics) // простой prometheus-text для админа

	// Админ-кабинет: статическая страница (вход через Telegram Login Widget).
	r.Get("/admin", s.handleAdminPage)
	r.Get("/admin/", s.handleAdminPage)

	r.Route("/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			// pair/request+status опрашиваются десктопом часто; щедро, чтобы не
			// ловить ложные 429 за общим выходным IP VPN.
			r.Use(s.rateLimitIP(120, 30))
			r.Post("/pair/request", s.handlePairRequest)
			r.Get("/pair/status", s.handlePairStatus)
		})

		r.Group(func(r chi.Router) {
			// confirm: пространство кодов ~26^8, брутфорс невозможен и при 60/min,
			// поэтому лимит щедрый — иначе легальные пользователи за общим VPN-IP
			// (где счётчик жгут чужие) упираются в 429.
			r.Use(s.rateLimitIP(60, 20))
			r.Post("/pair/confirm", s.handlePairConfirm)
			r.Post("/pair/confirm-native", s.handlePairConfirmNative) // no-Telegram pairing
		})

		r.Group(func(r chi.Router) {
			// auth/tg/start+poll: poll опрашивается часто — лимит щедрый, как у pair.
			r.Use(s.rateLimitIP(120, 30))
			r.Post("/auth/tg/start", s.handleAuthTgStart)     // начать вход через Telegram
			r.Get("/auth/tg/poll", s.handleAuthTgPoll)        // забрать durable user-JWT
			r.Post("/auth/qr/approve", s.handleAuthQRApprove) // телефон подтверждает вход на большом экране
			r.Get("/auth/oauth/poll", s.handleAuthOAuthPoll)  // результат OAuth-входа → JWT
		})
		r.Post("/auth/tg/miniapp", s.handleAuthTgMiniApp) // Mini App: initData → durable JWT

		r.Get("/auth/providers", s.handleAuthProviders) // способы входа этого инстанса (регион + креды)
		r.Get("/latest", s.handleLatest)                // release manifest proxy for Web/TG
		r.Get("/pricing", s.handlePricing)              // полки и цены канона: витрина нужна и до входа

		// ── Деньги: ЮKassa (магазин 1451249) ───────────────────────────────
		// checkout/subscription/unbind требуют входа (проверяют его сами),
		// webhook — открыт наружу по определению: его зовёт ЮKassa. Именно
		// поэтому webhook не верит присланному статусу и перепроверяет платёж
		// у самой ЮKassa по ключу магазина.
		r.Post("/billing/checkout", s.handleBillingCheckout)
		r.Get("/billing/subscription", s.handleBillingSubscription)
		r.Post("/billing/unbind", s.handleBillingUnbind)
		r.Post("/billing/email", s.handleBillingEmail)
		r.Post("/billing/webhook", s.handleBillingWebhook)

		// Email-код: start строго лимитирован (шлём письма), verify как confirm.
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimitIP(20, 5))
			r.Post("/auth/email/start", s.handleAuthEmailStart)
		})
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimitIP(60, 20))
			r.Post("/auth/email/verify", s.handleAuthEmailVerify)
			r.Post("/auth/oauth/{provider}", s.handleAuthOAuth)            // access_token от нативного SDK
			r.Post("/auth/oauth/{provider}/begin", s.handleAuthOAuthBegin) // redirect-флоу: ссылка на провайдера
		})
		// Callback от провайдера приходит из браузера пользователя: без IP-лимита
		// поверх (state одноразовый, TTL 10 мин, брутфорс бессмыслен). POST нужен
		// для Apple form_post.
		r.Get("/auth/oauth/{provider}/callback", s.handleAuthOAuthCallback)
		r.Post("/auth/oauth/{provider}/callback", s.handleAuthOAuthCallback)

		// Beacon аналитики: публичный (лендинг шлёт без auth), rate-limit по IP
		// как у confirm — событий немного, но спамить БД не дадим.
		// В этой же группе — пользовательские эндпоинты поддержки (auth внутри
		// хендлеров через requireUserAuth).
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimitIP(60, 20))
			r.Post("/events", s.handlePostEvent)
			r.Get("/support/messages", s.handleSupportList)
			r.Post("/support/messages", s.handleSupportPost)
			r.Get("/support/unread", s.handleSupportUnread)
		})

		// Админ-кабинет: всё за requireAdmin (Telegram Login Widget / ADMIN_TOKEN).
		r.Route("/admin", func(r chi.Router) {
			r.Use(s.requireAdmin)
			r.Get("/stats", s.handleAdminStats)
			r.Get("/support/threads", s.handleAdminThreads)
			r.Get("/support/threads/{id}/messages", s.handleAdminThreadMessages)
			r.Post("/support/threads/{id}/messages", s.handleAdminPostMessage)
			r.Post("/support/threads/{id}/close", s.handleAdminToggleThread)
		})

		r.Get("/me", s.handleMe)
		r.Get("/me/identities", s.handleListMyIdentities)   // мои способы входа
		r.Post("/me/identities/link", s.handleLinkIdentity) // привязать второй метод
		r.Get("/me/notify", s.handleGetNotifyPrefs)         // сообщения бота: вопрос агента / ответ поддержки
		r.Put("/me/notify", s.handleSetNotifyPrefs)
		r.Get("/me/sessions", s.handleListSessions)
		r.Delete("/me/sessions/{sessionID}", s.handleRevokeSession)
		r.Post("/me/sessions/revoke-others", s.handleRevokeOtherSessions)

		r.Get("/workspaces", s.handleListWorkspaces)
		r.Post("/workspaces", s.handleCreateWorkspace)
		r.Post("/workspace-invites/accept", s.handleAcceptWorkspaceInvite)
		r.Patch("/workspaces/{workspaceID}", s.handleUpdateWorkspace)
		r.Delete("/workspaces/{workspaceID}", s.handleDeleteWorkspace)
		r.Get("/workspaces/{workspaceID}/members", s.handleListWorkspaceMembers)
		r.Post("/workspaces/{workspaceID}/invites", s.handleCreateWorkspaceInvite)
		r.Patch("/workspaces/{workspaceID}/members/{userID}", s.handleUpdateWorkspaceMember)
		r.Delete("/workspaces/{workspaceID}/members/{userID}", s.handleRemoveWorkspaceMember)
		r.Get("/workspaces/{workspaceID}/groups", s.handleListGroups)
		r.Post("/workspaces/{workspaceID}/groups", s.handleCreateGroup)
		r.Delete("/workspaces/{workspaceID}/groups/{groupID}", s.handleDeleteGroup)
		r.Get("/workspaces/{workspaceID}/zones", s.handleListZones)
		r.Post("/workspaces/{workspaceID}/zones", s.handleCreateZone)
		r.Patch("/workspaces/{workspaceID}/zones/{zoneID}", s.handleUpdateZone)
		r.Delete("/workspaces/{workspaceID}/zones/{zoneID}", s.handleDeleteZone)
		r.Get("/workspaces/{workspaceID}/tags", s.handleListTags)
		r.Post("/workspaces/{workspaceID}/tags", s.handleCreateTag)
		r.Delete("/workspaces/{workspaceID}/tags/{tagID}", s.handleDeleteTag)
		r.Get("/workspaces/{workspaceID}/audit", s.handleWorkspaceAudit)

		r.Get("/devices", s.handleListDevices)
		r.Post("/devices/bulk", s.handleBulkDevices)
		r.Post("/devices/{deviceID}/rename", s.handleRenameDevice)
		r.Patch("/devices/{deviceID}", s.handleUpdateDeviceInfrastructure)
		r.Put("/devices/{deviceID}/favorite", s.handleSetDeviceFavorite)
		r.Put("/devices/{deviceID}/tags", s.handleReplaceDeviceTags)
		r.Delete("/devices/{deviceID}", s.handleRevokeDevice)
		r.Get("/devices/{deviceID}/health", s.handleDeviceHealth)
		r.Post("/device/revoke-self", s.handleDeviceRevokeSelf) // device-JWT self-revoke (desktop disconnect)

		r.Get("/turn/credentials", s.handleTURNCredentials)

		r.Get("/agent/connect", s.handleAgentConnect)
		r.Get("/agent/stream", s.handleAgentStream)
		r.Get("/client/{deviceID}/ws", s.handleClientWS)
		r.Post("/client/{deviceID}/request", s.handleClientRequest)
		r.Get("/client/{deviceID}/stream", s.handleClientStream)
		r.Post("/files/send", s.handleSendFileToTelegram)
		// /agent/send — device JWT внутри хендлера; rateLimitIP НЕ вешаем:
		// агенты за общим NAT делили бы один IP-бакет.
		r.Post("/agent/send", s.handleAgentSend)
		// Устройство → соседу по аккаунту: сервер дотягивается до компьютера
		// своим device-JWT (см. peer.go). Лимита по IP тут тоже нет — агенты за
		// общим NAT делили бы один бакет.
		r.Get("/agent/peers", s.handleAgentPeers)
		r.Get("/agent/server-access", s.handleServerAccess)
		r.Post("/agent/server-access", s.handleServerAccess)
		r.Post("/agent/peer/{deviceID}/request", s.handleAgentPeerRequest)
	})

	return r
}

func (s *Server) Run(ctx context.Context) error {
	s.httpSrv = &http.Server{
		Addr:         s.Config.Addr(),
		Handler:      s.Routes(),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	log.Printf("[SERVER] listening on %s public_url=%s", s.Config.Addr(), s.Config.PublicURL)

	// TURN/STUN для WebRTC Remote Desktop (no-op когда TURN_ENABLED=false).
	turnSrv, err := StartTURN(s.Config)
	if err != nil {
		log.Printf("[TURN] start failed: %v (cloud WebRTC media will fall back to WS)", err)
	} else if turnSrv != nil {
		defer turnSrv.Close()
	}

	// Очистка просроченных pairing-кодов и offline-сброс при старте.
	if err := db.MarkAllOffline(ctx, s.DB); err != nil {
		log.Printf("[SERVER] mark offline at boot: %v", err)
	}
	go s.cleanupLoop(ctx)

	errCh := make(chan error, 1)
	go func() {
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.httpSrv.Shutdown(shCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) cleanupLoop(ctx context.Context) {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := db.CleanupExpiredPairing(ctx, s.DB); err == nil && n > 0 {
				log.Printf("[CLEANUP] removed %d expired pairing codes", n)
			}
			if n, err := db.CleanupExpiredLoginTokens(ctx, s.DB); err == nil && n > 0 {
				log.Printf("[CLEANUP] removed %d expired login tokens", n)
			}
			// Реестр присутствия и латчи notifier'а — in-memory карты, растущие
			// по числу пар (device,user) и (device,pty) за всё время аптайма.
			if n := s.Hub.PrunePresence(time.Hour); n > 0 {
				log.Printf("[CLEANUP] removed %d stale presence marks", n)
			}
			if n := s.Notifier.PruneEpisodes(6 * time.Hour); n > 0 {
				log.Printf("[CLEANUP] removed %d finished notify episodes", n)
			}
		}
	}
}

// handleMetrics отдаёт метрики в Prometheus-text формате.
//
// Доступ: если задан METRICS_TOKEN, требуется он (Bearer или ?token=) — иначе
// 401, чтобы бизнес-показатели (число пользователей и т.п.) не утекали наружу
// через публичный remotai.ru/metrics. Если токен НЕ задан — отдаём только
// безопасный минимум (online/uptime/build), а расширенный блок прячем: это
// сохраняет обратную совместимость со старыми health-проверками и при этом не
// публикует приватные счётчики, пока защита не настроена.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	full := true
	if s.Config.MetricsToken != "" {
		if !metricsTokenOK(r, s.Config.MetricsToken) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "metrics token required")
			return
		}
	} else {
		full = false // токен не настроен → не раскрываем приватные показатели
	}

	var b strings.Builder
	writeGauge(&b, "tgcontrol_relay_online_devices", "Desktop agents currently connected", float64(len(s.Hub.OnlineDevices())))
	writeGauge(&b, "tgcontrol_relay_uptime_seconds", "Process uptime", time.Since(startedAt).Seconds())
	b.WriteString("# HELP tgcontrol_relay_build_info Build version (always 1)\n")
	b.WriteString("# TYPE tgcontrol_relay_build_info gauge\n")
	b.WriteString(`tgcontrol_relay_build_info{version="`)
	b.WriteString(Version)
	b.WriteString("\"} 1\n")

	if full {
		st := s.cachedStats(r.Context())
		writeGauge(&b, "tgcontrol_relay_users_total", "Total accounts (incl. anonymous)", float64(st.UsersTotal))
		writeGauge(&b, "tgcontrol_relay_users_active_24h", "Distinct device owners seen in last 24h", float64(st.UsersActive))
		writeGauge(&b, "tgcontrol_relay_users_new_24h", "Accounts created in last 24h", float64(st.UsersNew24h))
		writeGauge(&b, "tgcontrol_relay_devices_total", "Paired (not revoked) devices", float64(st.DevicesTotal))
		writeGauge(&b, "tgcontrol_relay_devices_new_24h", "Devices paired in last 24h", float64(st.DevicesNew24h))
		writeGauge(&b, "tgcontrol_relay_db_size_bytes", "On-disk size of relay.db (+ -wal)", float64(dbFileSize(s.Config.DBPath)))
		metrics.WriteTo(&b)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

// cachedStats возвращает статистику из БД, не чаще раза в 15с пересчитывая её.
func (s *Server) cachedStats(ctx context.Context) db.Stats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if time.Since(s.statsAt) < 15*time.Second && !s.statsAt.IsZero() {
		return s.statsVal
	}
	st, err := db.GatherStats(ctx, s.DB)
	if err != nil {
		log.Printf("[METRICS] gather stats: %v", err)
		return s.statsVal // отдаём прошлый срез, чтобы scrape не падал
	}
	s.statsVal, s.statsAt = st, time.Now()
	return st
}

func metricsTokenOK(r *http.Request, want string) bool {
	got := bearer(r.Header.Get("Authorization"))
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func writeGauge(b *strings.Builder, name, help string, v float64) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(help)
	b.WriteString("\n# TYPE ")
	b.WriteString(name)
	b.WriteString(" gauge\n")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
	b.WriteByte('\n')
}

func dbFileSize(path string) int64 {
	var total int64
	if fi, err := os.Stat(path); err == nil {
		total += fi.Size()
	}
	if fi, err := os.Stat(path + "-wal"); err == nil {
		total += fi.Size()
	}
	return total
}
