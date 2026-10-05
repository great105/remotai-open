// Command e2e-standalone — облегчённый релей для Playwright e2e (МОЕ/e2e).
//
// Поднимает ПОЛНЫЙ HTTP/WS API tgcontrol-relay, но БЕЗ Telegram-бота: боевой
// cmd/relay при старте дёргает getMe, и с тестовым токеном падает на буте.
// Здесь BOT_TOKEN из env используется только как ключ проверки initData —
// ровно так же устроен Go-тест TestPairingEndToEnd (internal/server).
//
// Порт: env PORT (0 = свободный порт, реальный адрес печатается в stdout
// строкой "READY host:port" — её ждёт тестовый харнесс).
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/server"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("[E2E] config: %v", err)
	}

	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("[E2E] db open: %v", err)
	}
	defer conn.Close()
	if err := db.Migrate(conn); err != nil {
		log.Fatalf("[E2E] db migrate: %v", err)
	}

	srv := server.New(cfg, conn)

	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		log.Fatalf("[E2E] listen: %v", err)
	}
	// Харнесс парсит эту строку, менять формат нельзя (см. e2e/fixtures/harness.ts).
	fmt.Printf("READY %s\n", ln.Addr().String())
	log.Printf("[E2E] relay harness on %s (db=%s)", ln.Addr(), cfg.DBPath)
	if err := http.Serve(ln, srv.Routes()); err != nil {
		log.Printf("[E2E] serve: %v", err)
		os.Exit(1)
	}
}
