package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"tgcontrol/internal/bot"
	"tgcontrol/internal/config"
	"tgcontrol/internal/desktopui"
	"tgcontrol/internal/netwatch"
	"tgcontrol/internal/observability"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/relay"
	"tgcontrol/internal/selfheal"
	"tgcontrol/internal/service"
	"tgcontrol/internal/sessions"
	"tgcontrol/internal/tray"
	"tgcontrol/internal/update"
	"tgcontrol/internal/vbrowser"
	"tgcontrol/internal/version"
	"tgcontrol/internal/web"
)

func main() {
	// `remotai hook` зовёт сам ИИ-агент из своего хука — на каждое событие.
	// Раньше всего остального: ни консоли, ни логов, ни конфига ему не нужно,
	// а каждая лишняя миллисекунда — задержка в работе агента.
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(runHook(os.Args[2:]))
	}
	prepareConsole()
	// `remotai help` / `--help` / `-h` и `version` без дефисов — первое, что
	// человек набирает после установки в терминале. До 2.65.3 аргумент никем не
	// разбирался, и процесс шёл запускать приложение: «Another Remotai instance
	// is already listening… activated its window» (живой Linux-стенд 06.09.2026).
	if len(os.Args) > 1 && isHelpArg(os.Args[1]) {
		fmt.Print(usageText())
		return
	}
	if len(os.Args) > 1 && isVersionArg(os.Args[1]) {
		fmt.Println(version.String())
		return
	}
	if launchPackagedDesktop() {
		return
	}
	if len(os.Args) > 2 && isHelpArg(os.Args[2]) {
		if usage, ok := commandUsage(os.Args[1]); ok {
			fmt.Print(usage)
			return
		}
	}
	// CLI-подкоманда `remotai attach` — короткоживущий клиент к локальному
	// серверу, без логов, трея и прочей инициализации.
	if len(os.Args) > 1 && os.Args[1] == "attach" {
		os.Exit(runAttach(os.Args[2:]))
	}

	// `remotai pair` — headless-онбординг сервера по SSH: печатает код+QR,
	// поллит релей, сохраняет identity. Без веб-сервера, трея и логов.
	if len(os.Args) > 1 && os.Args[1] == "pair" {
		os.Exit(runPair(os.Args[2:]))
	}

	// `remotai update` — обновление агента на сервере целиком: манифест →
	// бинарь под эту платформу → замена → перезапуск службы. На машине без
	// окна и панели другого пути к новой версии не было.
	if len(os.Args) > 1 && os.Args[1] == "update" {
		os.Exit(runUpdate(os.Args[2:]))
	}

	// `remotai install` / `remotai uninstall` — автозапуск (systemd
	// system/user-unit, иначе fallback-команды) + привязка pair. Единая
	// точка онбординга после скачивания бинаря (install.sh / npm / вручную).
	if len(os.Args) > 1 && os.Args[1] == "install" {
		os.Exit(runInstall(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "uninstall" {
		os.Exit(runUninstall(os.Args[2:]))
	}
	// `remotai send` — сообщение/файл владельцу в Telegram через облачного
	// бота. Ручка для AI-агентов и скриптов на этом компьютере.
	if len(os.Args) > 1 && os.Args[1] == "send" {
		os.Exit(runSend(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "status" {
		os.Exit(runStatus(os.Args[2:]))
	}
	// `remotai config` и `remotai doctor` — ручки для AI-агента, который
	// работает в терминале этого компьютера: он должен ПОНИМАТЬ настройки и
	// состояние, а не угадывать их (просьба владельца 05.08.2026). Командой
	// умеет пользоваться любой агент — в отличие от MCP, который есть не у всех.
	if len(os.Args) > 1 && os.Args[1] == "config" {
		os.Exit(runConfig(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(runDoctor(os.Args[2:]))
	}
	// `remotai vpn` — VPN этого компьютера; `remotai remote` — дотянуться до
	// ДРУГОГО компьютера аккаунта (нужно как раз тогда, когда он пропал).
	if len(os.Args) > 1 && os.Args[1] == "vpn" {
		os.Exit(runVPN(os.Args[2:]))
	}
	if len(os.Args) > 1 && (os.Args[1] == "remote" || os.Args[1] == "peer") {
		os.Exit(runRemote(os.Args[2:]))
	}
	if len(os.Args) > 1 && (os.Args[1] == "unpair" || os.Args[1] == "--unpair") {
		os.Exit(runUnpair(os.Args[2:]))
	}
	// `--uninstall-cleanup` — деинсталлятор: чистка self-copy/PATH + отзыв
	// устройства из аккаунта. С `--keep-pairing` привязку сохраняем: человек в
	// деинсталляторе выбрал «оставить», чтобы после повторной установки
	// компьютер вернулся в приложение сам, без нового QR-кода.
	if len(os.Args) > 1 && os.Args[1] == "--uninstall-cleanup" {
		if slices.Contains(os.Args[2:], "--keep-pairing") {
			os.Exit(runCleanupKeepPairing())
		}
		os.Exit(runUninstallCleanup(os.Args[2:]))
	}

	// Скрытый режим `remotai --pty-host` — отдельный процесс-хост, владеющий
	// ConPTY и переживающий рестарт основного процесса (персистентные
	// терминалы). Без трея/веба/логов основного приложения.
	if len(os.Args) > 1 && os.Args[1] == "--pty-host" {
		// Хосту консоль не нужна: при спавне через WMI (Win32_Process.Create)
		// процесс получает видимое консольное окно — освобождаем его, иначе на
		// каждый терминал висит чёрное окно remotai.exe.
		maybeFreeConsole()
		os.Exit(runPtyHost(os.Args[2:]))
	}

	// Release GUI builds have no console. Retain the defensive detach only
	// for developer builds made without the Windows GUI linker flag.
	maybeFreeConsole()

	// Новый процесс после автообновления ждёт, пока старый отпустит порт.
	waitUpdateHandoff()

	// Setup log file in the app data dir.
	setupLogging()

	// Crash reporter: ловим panic и сохраняем JSON-репорт рядом с exe.
	defer observability.InstallGlobal("main")()

	// Load .env from executable directory (important for Windows Service where CWD != exe dir)
	loadEnvFromExeDir()

	// Initialize Sentry (no-op if SENTRY_DSN_GO is not set)
	observability.Init()
	defer observability.Close()

	// Crash reports from previous runs — log to user (UI poll'll see via /diag).
	observability.PromptIfAny()

	// Clean up old binary from previous update
	update.CleanupOldBinary()

	// CLI flags
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--setup":
			config.RunSetup(true)
			return
		case "--version":
			fmt.Println(version.String())
			return
		case "--install-service":
			if err := service.Install(); err != nil {
				log.Fatalf("Install service: %v", err)
			}
			return
		case "--uninstall-service":
			if err := service.Uninstall(); err != nil {
				log.Fatalf("Uninstall service: %v", err)
			}
			return
		case "--start-service":
			if err := service.Start(); err != nil {
				log.Fatalf("Start service: %v", err)
			}
			return
		case "--stop-service":
			if err := service.Stop(); err != nil {
				log.Fatalf("Stop service: %v", err)
			}
			return
		case "--enable-autostart":
			// Вызывается инсталлятором после копирования файлов: создаёт
			// scheduled task на установленный exe (см. internal/web).
			if err := web.EnableAutostart(); err != nil {
				log.Fatalf("Enable autostart: %v", err)
			}
			fmt.Println("Autostart enabled")
			return
		case "--disable-autostart":
			if err := web.DisableAutostart(); err != nil {
				log.Fatalf("Disable autostart: %v", err)
			}
			fmt.Println("Autostart disabled")
			return
		}
	}

	// Режим службы Windows (SCM) — ТОЛЬКО на Windows.
	//
	// ЖИВОЙ МАК 10.08.2026, корень «компьютер не в сети». `RunAsService()` на
	// маке отвечает true законно: launchd выставляет процессу XPC_SERVICE_NAME,
	// и признак «нас запустил менеджер служб» нужен автообновлению и захвату
	// экрана. Но здесь он читался как «иди в SCM-режим», а `service.Run` на
	// darwin всегда возвращает ошибку «на macOS агент запускается launchd как
	// обычный процесс» — и `log.Fatalf` убивал процесс. launchd поднимал его
	// снова, тот снова падал: в логе ровный цикл каждые 10 секунд
	//
	//   Running as Windows service
	//   Service error: на macOS агент запускается launchd как обычный процесс
	//
	// Агент не работал НИ РАЗУ с самого первого мака, а снаружи это выглядело
	// как «привязался и не выходит на связь».
	if runtime.GOOS == "windows" && service.RunAsService() {
		log.Println("Running as Windows service")
		if err := service.Run(func(stopCh <-chan struct{}) {
			runApp(stopCh)
		}); err != nil {
			log.Fatalf("Service error: %v", err)
		}
		return
	}

	// Load config without triggering CLI wizard
	cfg := config.GetNoSetup()
	// Фоновый запуск (сторож автозапуска, раз в 5 минут) не должен трогать
	// окно: агент жив — просто выходим. Раньше каждый тик сторожа поднимал
	// окно приложения поверх работы человека (живая жалоба 31.07).
	silentStart := false
	for _, a := range os.Args[1:] {
		if a == "--background" || a == "--minimized" {
			silentStart = true
			break
		}
	}
	runtimePort, bindInfo, activatedExisting := resolveStartupPort(cfg.Port(), silentStart)
	if activatedExisting {
		if silentStart {
			log.Printf("Remotai уже слушает :%d — сторож завершается тихо.", cfg.Port())
		} else {
			log.Printf("Another Remotai instance is already listening on :%d — activated its window.", cfg.Port())
		}
		return
	}
	if runtimePort != cfg.Port() {
		if err := config.Update(func(c *config.Config) { c.WebPort = strconv.Itoa(runtimePort) }); err != nil {
			log.Printf("Cannot persist fallback port %d: %v", runtimePort, err)
		} else {
			cfg = config.Reload()
		}
	}

	// Reload .env after potential setup
	loadEnvFromExeDir()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --background / --minimized: launched by autostart → run silently in the
	// tray without popping the window. Manual launch shows the window.
	backgroundMode := false
	for _, a := range os.Args[1:] {
		if a == "--background" || a == "--minimized" {
			backgroundMode = true
		}
	}

	// ── Setup mode: if not configured, serve only the web wizard ──
	if !cfg.IsConfigured() {
		log.Println("Config not found — starting setup wizard...")
		fmt.Println("========================================")
		fmt.Println("  Remotai — First Launch Setup")
		fmt.Println("========================================")

		port := cfg.Port()

		// Start web server in setup-only mode on localhost only (no store, no bot)
		setupServer := web.NewServer(nil, nil, "", nil)
		setupServer.SetStartupBindError(bindInfo)
		setupURL := fmt.Sprintf("http://localhost:%d/setup", port)
		openSetupWindow := func() {
			if !desktopui.Show("Remotai — Настройка", setupURL, 520, 820) {
				openBrowser(setupURL)
			}
		}
		setupServer.SetOpenWindow(func() { go openSetupWindow() })
		go func() {
			defer observability.RecoverPanic("setup-server")
			if err := setupServer.StartLocal(ctx, port); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Setup server error: %v", err)
			}
		}()

		// Open the setup wizard in a native window (falls back to the browser
		// if WebView2 is unavailable).
		if !backgroundMode {
			go func() {
				defer observability.RecoverPanic("setup-window")
				time.Sleep(500 * time.Millisecond)
				log.Printf("Opening setup window: %s", setupURL)
				openSetupWindow()
			}()
		}

		fmt.Printf("  Setup wizard: http://localhost:%d/setup\n", port)
		fmt.Println("  Waiting for setup to complete...")

		// The setup process already lives in the tray: closing the window keeps
		// it reachable, and a second launch restores that same window. Tray.Run
		// owns the Windows message loop, so setup completion cancels only this
		// temporary tray and then the process continues into normal mode.
		setupCtx, stopSetupTray := context.WithCancel(ctx)
		setupCompleted := make(chan bool, 1)
		go func() {
			select {
			case <-web.SetupDone:
				setupCompleted <- true
			case <-ctx.Done():
				setupCompleted <- false
			}
			stopSetupTray()
		}()
		bindMessage := ""
		if bindInfo != nil {
			bindMessage = bindInfo.Message
		}
		tray.Run(setupCtx, tray.Options{
			Port:      port,
			SetupMode: true,
			BindError: bindMessage,
			OnQuit:    cancel,
			OnOpen:    func() { go openSetupWindow() },
		})
		if !<-setupCompleted {
			log.Println("Shutdown during setup")
			return
		}
		log.Println("Setup complete — reloading config...")

		// Гасим setup-listener ДО старта основного сервера на том же порту.
		// Иначе на Windows оба сокета сосуществуют и localhost-трафик окна
		// уходит в СТАРЫЙ сервер с nil store/relayStatus — панель после
		// пейринга врёт «Телефон не подключён», а API падают. Shutdown
		// сначала закрывает listener, так что порт свободен сразу.
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := setupServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("Setup server shutdown: %v", err)
		}
		shutdownCancel()

		// Reload config and .env after setup
		cfg = config.Reload()
		loadEnvFromExeDir()

		// Brief pause before starting main app
		time.Sleep(500 * time.Millisecond)
	}

	// Лёгкая самоустановка: exe → %LOCALAPPDATA%\Remotai + user PATH,
	// чтобы `remotai attach` работал из любого терминала.
	go func() {
		defer observability.RecoverPanic("self-install")
		ensureInstalled()
	}()

	// ── Normal mode: full app ──────────────────────────────────

	// central_bot mode requires a working relay client. The relay server is in
	// production (remotai.ru), but if the user picked this mode without a
	// configured relay endpoint (no pairing yet), fall back to a local-only web
	// server. This avoids the silent "no bot, weird state" trap.
	if cfg.IsCentralBot() && !relayClientAvailable() {
		log.Printf("[WARN] central_bot mode selected but relay client is not available yet — running in local-only mode (Telegram bot disabled). Open http://localhost:%d/setup to switch to own_bot.", cfg.Port())
	}

	fmt.Println("========================================")
	fmt.Printf("  Remotai %s\n", version.Version)
	fmt.Printf("  Mode: %s\n", cfg.Mode)
	if cfg.DeviceID != "" {
		fmt.Printf("  Device: %s\n", cfg.DeviceID)
	}
	fmt.Println("========================================")

	// Read env vars
	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" && cfg.IsOwnBot() {
		log.Printf("[WARN] TELEGRAM_BOT_TOKEN not set — running in web-only mode. Visit http://localhost:%d/setup to reconfigure.", cfg.Port())
		// Don't fatal — the local web server + APK still work over LAN/tunnel.
	}

	allowedUsers := parseAllowedUsers(os.Getenv("ALLOWED_USERS"))

	// Ensure API token exists for standalone APK auth
	defaultUID := int64(1)
	for uid := range allowedUsers {
		defaultUID = uid
		break
	}
	cfg.EnsureAPIToken(defaultUID)

	// Create shared state
	store := sessions.NewStore()
	history := sessions.NewHistory()
	topics := sessions.NewTopicStore()

	// Create web server
	webServer := web.NewServer(store, history, botToken, allowedUsers)
	webServer.SetStartupBindError(bindInfo)
	openMainWindow := func() {
		// Окно всегда одно, и «показать окно» (трей, повторный клик по ярлыку)
		// раньше означало Navigate на панель настроек: у человека, у которого в
		// этом окне шёл Claude Code, терминал сменялся экраном настроек, а
		// кнопки «назад» в WebView2 нет. Уже открытое окно просто поднимаем.
		if desktopui.WindowOpen() && raiseAppWindow() {
			return
		}
		// Открываем САМО ПРИЛОЖЕНИЕ, а не панель настроек.
		//
		// Раньше запуск Remotai показывал мастер/панель (`/setup`) — отдельный
		// экран, из которого до терминалов, файлов и экрана компьютера надо было
		// ещё дойти кнопкой. Но продукт — это приложение, а панель ПК давно
		// живёт ВНУТРИ него отдельной вкладкой (см. panel-tab-in-client): два
		// разных «главных экрана» на одном компьютере — это лишняя дверь и
		// вопрос «где я».
		//
		// Исключение ровно одно: пока агент не настроен, показывать приложение
		// нечем — там нет ни аккаунта, ни доступа, — и первым экраном обязан
		// быть мастер.
		if !config.GetNoSetup().IsConfigured() {
			url := fmt.Sprintf("http://localhost:%d/setup?force=1", cfg.Port())
			if !desktopui.Show("Remotai", url, 520, 820) {
				openBrowser(url)
			}
			return
		}
		// Размер — как у окна терминалов из трея: приложению нужна ширина
		// (списки, терминал, панель ПК во вкладке), а не телефонная колонка.
		url := localClientURL(cfg.Port())
		if !desktopui.Show("Remotai", url, 1100, 760) {
			openBrowser(url)
		}
	}
	// «Терминалы» из трея — то же САМОЕ окно, но сразу на нужном экране.
	// Отдельного окна терминалов больше нет: приложение одно, и два окна с
	// одним и тем же содержимым отвечали бы на вопрос «где я» по-разному.
	openTerminalsWindow := func() {
		if desktopui.WindowOpen() && raiseAppWindow() {
			return // окно открыто — не перебиваем то, что человек там делает
		}
		if !config.GetNoSetup().IsConfigured() {
			openMainWindow()
			return
		}
		// Клиент окна exe — HashRouter, поэтому экран задаётся хвостом #/pty.
		url := localClientURL(cfg.Port()) + "#/pty"
		if !desktopui.Show("Remotai", url, 1100, 760) {
			openBrowser(url)
		}
	}
	webServer.SetOpenWindow(func() { go openMainWindow() })
	// «Завершить Remotai» из окна — тот же выход, что и пункт трея «Выйти».
	web.SetQuitHandler(cancel)

	// Виртуальный браузер (Xvfb) умирает вместе с агентом — сиротский X-сервер
	// держал бы /tmp/.X99-lock и мешал следующему запуску.
	defer vbrowser.Stop()

	// Start web server in background
	go func() {
		defer observability.RecoverPanic("web-server")
		port := cfg.Port()
		if err := webServer.Start(ctx, port); err != nil {
			log.Printf("Web server error: %v", err)
		}
	}()

	// Auto-start tunnel if configured. Skipped in cloud (central_bot) mode —
	// remote access there goes through the relay, so a cloudflared tunnel is
	// redundant (and spawns a confusing extra process).
	if !cfg.IsCentralBot() {
		go func() {
			defer observability.RecoverPanic("tunnel-supervisor")
			time.Sleep(time.Second) // let web server bind first
			webServer.StartTunnel(ctx)
		}()
	}

	// Start relay client. Register its event sink so locally-broadcast events
	// are forwarded to remote cloud clients (no-op until connected).
	//
	// Runs in EVERY mode: phone-over-relay connectivity is orthogonal to the
	// Telegram bot flavour. The client idles until pairing configures a relay
	// (ServeForever stands by), and SetRelayKick lets the pairing handler apply
	// a fresh JWT without an app restart.
	relayClient := relay.New(webServer)
	webServer.SetRelayEventSink(relayClient.ForwardEvent)
	webServer.SetRelayKick(relayClient.Kick)
	webServer.SetRelayStatus(relayClient.Status)
	webServer.SetRelayDetail(func() web.RelayConnectionDetail {
		d := relayClient.Detail()
		return web.RelayConnectionDetail{
			Kind: d.Kind, HTTPStatus: d.HTTPStatus, At: d.At, Message: d.Message,
		}
	})
	go func() {
		defer observability.RecoverPanic("relay-client")
		relayClient.ServeForever(ctx)
	}()

	// Фоновое автообновление с remotai.ru. Веб-сервер отдаём как нотификатор:
	// перед неизбежным рестартом автообновление предупреждает клиентов, чьи
	// живые сеансы он оборвёт.
	go func() {
		defer observability.RecoverPanic("auto-update")
		runAutoUpdate(ctx, webServer)
	}()

	// Автозапуск обязан переживать не только перезагрузку, но и закрытие самого
	// агента: без сторожа задача поднимала его лишь при следующем входе в
	// систему, а на ноутбуке от батареи Windows не запускала её вовсе. Проверка
	// идёт на каждом старте — те, кто включил автозапуск давно, получают
	// исправленную задачу с обновлением, а не «когда-нибудь зайдут в настройки».
	go func() {
		defer observability.RecoverPanic("autostart-health")
		web.EnsureAutostartHealthy()
	}()

	// Самодиагностика перерыва: почему компьютера не было и надо ли об этом
	// говорить владельцу. Запускается ПОСЛЕ релей-клиента — сообщение уходит
	// уже по поднявшемуся каналу (см. internal/selfheal).
	go func() {
		defer observability.RecoverPanic("selfheal")
		selfheal.Start(web.SendOwnerText)
	}()

	// Сторож зависшего VPN: мёртвый туннель отрезал этот компьютер от владельца
	// на 42 часа (разбор 02.08.2026), и починить его было некому — доступ шёл
	// через тот же канал. Теперь компьютер разбирает завал сам.
	go func() {
		defer observability.RecoverPanic("netwatch")
		netwatch.Start(netwatch.Deps{
			CloudConnected: func() bool {
				_, connected := relayClient.Status()
				return connected
			},
			RelayHealthURL: func() string {
				base := config.GetNoSetup().RelayHTTPBase()
				if base == "" {
					return ""
				}
				return strings.TrimRight(base, "/") + "/health"
			},
			Notify:   web.SendOwnerText,
			Enabled:  func() bool { return config.GetNoSetup().VPNWatchdogEnabled() },
			OnRepair: selfheal.AddRepair,
		})
	}()

	// Start TTL cleanup goroutine
	go func() {
		defer observability.RecoverPanic("ttl-cleanup")
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				closed := store.CleanExpired()
				for _, name := range closed {
					log.Printf("TTL expired: session %s auto-closed", name)
				}
			}
		}
	}()

	// Run bot (with retry) in a background goroutine; the main goroutine
	// is reserved for the system tray message loop.
	botDone := make(chan struct{})
	go func() {
		defer observability.RecoverPanic("bot-supervisor")
		defer close(botDone)
		if botToken != "" {
			tgBot := bot.New(store, history, topics, webServer, allowedUsers)
			webServer.SetFileSender(tgBot.SendFileToUser)
			webServer.SetBotLiveness(tgBot.Liveness)
			log.Printf("Bot starting... (allowed users: %v)", allowedUsersList(allowedUsers))

			backoff := time.Second
			const maxBackoff = 2 * time.Minute
			for {
				err := tgBot.Start(ctx)
				if err == nil || ctx.Err() != nil {
					return
				}
				log.Printf("Bot error: %v — retrying in %s", err, backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				backoff = min(backoff*2, maxBackoff)
			}
		} else {
			log.Println("No bot token — running in web-only mode (central_bot)")
			<-ctx.Done()
		}
	}()

	// System tray icon (blocks main goroutine until user picks Exit
	// or ctx is cancelled). On non-Windows it just waits on ctx.
	// Show the app window on launch (manual run). Autostart passes --background
	// to stay in the tray silently.
	if !backgroundMode {
		go func() {
			defer observability.RecoverPanic("app-window")
			time.Sleep(800 * time.Millisecond) // let the web server bind
			openMainWindow()
		}()
	}

	bindMessage := ""
	if bindInfo != nil {
		bindMessage = bindInfo.Message
	}
	tray.Run(ctx, tray.Options{
		Port:        cfg.Port(),
		RelayStatus: relayClient.Status,
		BindError:   bindMessage,
		OnQuit:      cancel,
		OnOpen: func() {
			go func() {
				defer observability.RecoverPanic("tray-open-window")
				openMainWindow()
			}()
		},
		OnTerminals: func() {
			go func() {
				defer observability.RecoverPanic("tray-open-terminals")
				openTerminalsWindow()
			}()
		},
		OnHostTerminal: func() {
			go func() {
				defer observability.RecoverPanic("tray-host-terminal")
				// Через собственный loopback-эндпоинт — одна реализация
				// на панель и трей (см. apiSetupOpenTerminal).
				url := fmt.Sprintf("http://127.0.0.1:%d/api/setup/open-terminal", cfg.Port())
				resp, err := http.Post(url, "application/json", nil)
				if err != nil {
					log.Printf("tray: open-terminal failed: %v", err)
					return
				}
				resp.Body.Close()
			}()
		},
	})

	// Tray exited — let in-flight goroutines finish.
	selfheal.MarkStop("user") // человек закрыл приложение сам — это не падение
	cancel()
	select {
	case <-botDone:
	case <-time.After(5 * time.Second):
	}
}

// runApp runs the main application loop, used both in console and service mode.
// When stopCh is closed, the app shuts down gracefully.
func runApp(stopCh <-chan struct{}) {
	loadEnvFromExeDir()
	observability.Init()
	defer observability.Close()
	cfg := config.GetNoSetup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Stop on service signal
	go func() {
		defer observability.RecoverPanic("service-stop-watch")
		<-stopCh
		cancel()
	}()

	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	allowedUsers := parseAllowedUsers(os.Getenv("ALLOWED_USERS"))

	defaultUID := int64(1)
	for uid := range allowedUsers {
		defaultUID = uid
		break
	}
	cfg.EnsureAPIToken(defaultUID)

	store := sessions.NewStore()
	history := sessions.NewHistory()
	topics := sessions.NewTopicStore()

	webServer := web.NewServer(store, history, botToken, allowedUsers)

	go func() {
		defer observability.RecoverPanic("web-server")
		if err := webServer.Start(ctx, cfg.Port()); err != nil {
			log.Printf("Web server error: %v", err)
		}
	}()

	// Auto-start tunnel
	go func() {
		defer observability.RecoverPanic("tunnel-supervisor")
		time.Sleep(time.Second)
		webServer.StartTunnel(ctx)
	}()

	// Relay client — same wiring as console mode (see comment there).
	relayClient := relay.New(webServer)
	webServer.SetRelayEventSink(relayClient.ForwardEvent)
	webServer.SetRelayKick(relayClient.Kick)
	webServer.SetRelayStatus(relayClient.Status)
	go func() {
		defer observability.RecoverPanic("relay-client")
		relayClient.ServeForever(ctx)
	}()

	// Фоновое автообновление (в service-режиме только применяет файл —
	// перезапуск за SCM; под systemd рестарт откладывается до тихой минуты).
	go func() {
		defer observability.RecoverPanic("auto-update")
		runAutoUpdate(ctx, webServer)
	}()

	go func() {
		defer observability.RecoverPanic("ttl-cleanup")
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, name := range store.CleanExpired() {
					log.Printf("TTL expired: session %s auto-closed", name)
				}
			}
		}
	}()

	if botToken != "" {
		tgBot := bot.New(store, history, topics, webServer, allowedUsers)
		webServer.SetFileSender(tgBot.SendFileToUser)
		webServer.SetBotLiveness(tgBot.Liveness)
		log.Printf("Service: bot starting (users: %s)", allowedUsersList(allowedUsers))

		backoff := time.Second
		for {
			err := tgBot.Start(ctx)
			if err == nil || ctx.Err() != nil {
				break
			}
			log.Printf("Bot error: %v — retrying in %s", err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				webServer.StopTunnel()
				return
			}
			backoff = min(backoff*2, 2*time.Minute)
		}
	} else {
		<-ctx.Done()
	}
	webServer.StopTunnel()
}

// runCleanupKeepPairing убирает артефакты установки (копию exe в папке данных и
// запись в user PATH), НЕ отзывая устройство и не трогая config/журналы: этот
// путь выбирает деинсталлятор, когда человек ответил «привязку оставить».
func runCleanupKeepPairing() int {
	if err := cleanupInstalledArtifacts(); err != nil {
		fmt.Fprintf(os.Stderr, "remotai cleanup: %v\n", err)
		return 1
	}
	fmt.Println("✅ Файлы установки удалены, привязка к аккаунту сохранена: после повторной установки компьютер вернётся в приложение сам.")
	return 0
}

// relayClientAvailable reports whether the central_bot relay client is wired
// up. Delegates to internal/relay: relay.Available() returns true once a
// relay endpoint is configured (the relay server is live at remotai.ru).
func relayClientAvailable() bool {
	return relay.Available()
}

// localClientURL строит адрес локального клиента (/miniapp) с api-токеном —
// тот же auth-мост token:<API_TOKEN>, что у браузерного входа с других
// устройств (см. internal/web/setup.go apiSetupLocalAccess).
func localClientURL(port int) string {
	cfg := config.GetNoSetup()
	uid := cfg.APITokenUID
	if uid == 0 {
		uid = 1
	}
	cfg.EnsureAPIToken(uid)
	return fmt.Sprintf("http://localhost:%d/miniapp?token=%s", port, neturl.QueryEscape(cfg.APIToken))
}

// openBrowser opens a URL in the user's default browser. Одна реализация на
// весь продукт (web.OpenExternalURL): её же зовёт панель, когда окно WebView2
// не может открыть внешнюю ссылку само.
func openBrowser(url string) {
	if err := web.OpenExternalURL(url); err != nil {
		log.Printf("Cannot open browser for %s: %v", url, err)
	}
}

func parseAllowedUsers(raw string) map[int64]bool {
	result := make(map[int64]bool)
	if raw == "" {
		return result
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err == nil {
			result[id] = true
		}
	}
	return result
}

func allowedUsersList(m map[int64]bool) string {
	if len(m) == 0 {
		return "everyone"
	}
	var ids []string
	for id := range m {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	return strings.Join(ids, ", ")
}

// setupLogging configures log output to both stderr and a log file.
// The log file lives in the app data dir as "remotai.log".
// If the file exceeds 5 MB it is rotated (old content renamed to .log.old).
func setupLogging() {
	logPath := filepath.Join(paths.Base(), "remotai.log")

	// Rotate if too big (>5 MB).
	const maxSize = 5 * 1024 * 1024
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxSize {
		os.Rename(logPath, logPath+".old")
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}

	// Write to both file (first, guaranteed) and stderr.
	// File must come first: if stderr is unavailable in service mode,
	// MultiWriter short-circuits and the file would never receive writes.
	mw := io.MultiWriter(f, os.Stderr)
	log.SetOutput(mw)
	log.SetFlags(log.Ldate | log.Ltime)
}

// loadEnvFromExeDir loads .env from the app data dir (см. internal/paths),
// падая обратно на папку exe для legacy-раскладок. Explicit paths matter for
// Windows Service where CWD is C:\Windows\System32.
func loadEnvFromExeDir() {
	godotenv.Load(filepath.Join(paths.Base(), ".env"))
	if exe, err := os.Executable(); err == nil {
		godotenv.Load(filepath.Join(filepath.Dir(exe), ".env"))
	}
}
