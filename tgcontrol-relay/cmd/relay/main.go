// Command relay — entrypoint TGControl Cloud Relay сервера.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tgcontrol-relay/internal/bot"
	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/notify"
	"tgcontrol-relay/internal/server"
)

// Сколько раз пробуем поднять Telegram-бота при старте и с какой паузой.
// Три попытки с паузой в пять секунд переживают короткий сбой сети и при этом
// не задерживают запуск сайта дольше пятнадцати секунд.
const (
	botInitAttempts   = 3
	botInitRetryDelay = 5 * time.Second
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("[BOOT] tgcontrol-relay starting (version=%s)", server.Version)
	// Кнопки бота ведут в мини-апп с меткой версии — иначе Telegram отдаёт
	// человеку СВОЮ закэшированную сборку и выпущенная починка до него не
	// доезжает (см. withClientVersion в internal/bot).
	bot.ClientVersion = server.Version

	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("[BOOT] config: %v", err)
	}

	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("[BOOT] db open: %v", err)
	}
	defer conn.Close()
	if err := db.Migrate(conn); err != nil {
		log.Fatalf("[BOOT] db migrate: %v", err)
	}

	srv := server.New(cfg, conn)

	// Telegram бот (optional)
	//
	// ⚠ НЕДОСТУПНЫЙ TELEGRAM НЕ ВАЛИТ РЕЛЕЙ. Здесь стоял `log.Fatalf`, и
	// 05.09.2026 это положило весь сайт: `bot.New` зовёт `getMe`, запросы к
	// Bot API с сервера перестали проходить (само `api.telegram.org` при этом
	// отвечало 302 и по IPv4, и по IPv6 — блокировался именно путь с токеном),
	// процесс падал с exit 1, systemd перезапускал его в цикле, и remotai.ru
	// отдавал 502. Telegram — одна из функций релея, а не условие его жизни:
	// без бота остаются сайт, витрина, оплата, пейринг по коду и весь API.
	//
	// Пробуем несколько раз: короткий сбой сети (секунды) переживаем и
	// поднимаем бота как обычно; долгий — стартуем без него и говорим об этом
	// в лог громко. `/health` честно покажет `bot_enabled: false`.
	if cfg.BotToken != "" {
		var b *bot.Bot
		for attempt := 1; attempt <= botInitAttempts; attempt++ {
			var err error
			if b, err = bot.New(cfg, conn, srv.JWT); err == nil {
				break
			}
			log.Printf("[BOOT] bot init (попытка %d из %d): %v", attempt, botInitAttempts, err)
			b = nil
			if attempt < botInitAttempts {
				time.Sleep(botInitRetryDelay)
			}
		}
		if b == nil {
			log.Printf("[BOOT] ⚠ Telegram недоступен — релей работает БЕЗ бота: " +
				"вход через Telegram и уведомления не работают, всё остальное живо. " +
				"Проверьте доступность api.telegram.org с сервера и перезапустите relay.")
		}
		if b != nil {
			srv.Bot = b

			// Уведомления админу о новых сообщениях поддержки. Ошибки только в лог —
			// приём сообщения не должен зависеть от доступности TG API.
			if cfg.AdminChatID != 0 {
				srv.AdminNotify = func(text string) {
					nctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := b.SendMessage(nctx, cfg.AdminChatID, text); err != nil {
						log.Printf("[SUPPORT] admin notify: %v", err)
					}
				}
			}

			// «Агент ждёт ответа» → личный чат бота, ответ кнопкой → терминал ПК.
			// Проводка колбэками, чтобы server и bot не импортировали друг друга.
			srv.UserNotify = func(ctx context.Context, chatID int64, n notify.Notice) error {
				return b.SendAgentWaiting(ctx, chatID, n)
			}
			// «Поддержка ответила» → личный чат бота с кнопкой на чат поддержки.
			// Работает независимо от NOTIFY_TG: это ответ на обращение самого
			// человека, а не поток событий агентов.
			srv.UserNotifySupport = b.SendSupportReply
			// «Компьютер обновился» → одно спокойное сообщение в личный чат бота.
			srv.UserNotifyText = b.SendUpdateNotice
			// «remotai send» → текст от агента в личный чат владельца.
			srv.UserSendText = b.SendAgentMessage
			srv.UserSendFile = b.SendDocument
			b.PtyInput = srv.SendPtyInput
			b.IsOnline = srv.Hub.IsOnline
			b.AgentRequest = srv.BotAgentRequest
		}
	}

	if cfg.NotifyTG {
		srv.Notifier = server.NewNotifier(srv) // ДО srv.Run: подписка стартует на коннекте агента
		log.Printf("[BOOT] telegram notifier enabled (delay=%s, pty cooldown=%s, max %d/hour)",
			cfg.NotifyDelay, cfg.NotifyPtyCooldown, cfg.NotifyMaxPerHour)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Бот polling в отдельной горутине
	if srv.Bot != nil {
		go func() {
			if err := srv.Bot.Start(ctx); err != nil {
				log.Printf("[BOOT] bot: %v", err)
			}
		}()
	}

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("[BOOT] server: %v", err)
	}
	log.Printf("[BOOT] shutdown complete")
}
