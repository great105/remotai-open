package main

// `remotai pair` — headless-онбординг сервера. Без веб-сервера и GUI: печатает
// в SSH-консоль код, Telegram deep-link и ASCII-QR, поллит статус на релее и,
// как только пользователь подтвердил привязку с телефона/бота, сохраняет
// identity (device-JWT в keystore + config.json, SetupComplete=true). Следующий
// запуск идёт в обычный режим. Это замена loopback-панели + GUI-мастера,
// недоступных на headless-сервере по SSH.
//
// Relay уже полностью готов: /v1/pair/{request,status,confirm,confirm-native}
// публичные + rate-limited, device-JWT, multi-grant, bot deep-link pair_<code>.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"tgcontrol/internal/localize"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"tgcontrol/internal/config"
	"tgcontrol/internal/procutil"
	"tgcontrol/internal/relay"
	"tgcontrol/internal/web"
)

func runPair(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := config.GetNoSetup()
	base := cfg.RelayHTTPBase()
	if base == "" {
		base = config.DefaultRelayURL
	}
	// --relay <url> — привязка к нестандартному релею (dev/self-hosted).
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--relay" {
			base = strings.TrimRight(args[i+1], "/")
		}
	}

	deviceID := config.GetOrCreateDeviceID()
	hostname, _ := os.Hostname()

	// device-JWT этого ПК: пусто при первом пейринге; если сервер УЖЕ привязан —
	// передаём его, релей требует JWT для выпуска кода на ДОБАВЛЕНИЕ устройств
	// (иначе, зная device_id, чужой выпустил бы код = захват сервера).
	deviceJWT := cfg.RelayJWT
	if deviceJWT == "" {
		deviceJWT, _ = relay.LoadJWT()
	}

	reqCtx, reqCancel := context.WithTimeout(ctx, 20*time.Second)
	resp, err := relay.RequestPairingCode(reqCtx, base, deviceID, hostname, deviceJWT)
	reqCancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, localize.Text("remotai: не удалось получить код пейринга: %v\n"), err)
		return 1
	}

	printPairInstructions(resp, base, hostname, deviceID)

	// Поллинг статуса до подтверждения / истечения / Ctrl+C.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, localize.Text("\nremotai: отменено"))
			return 1
		case <-ticker.C:
		}

		if !resp.ExpiresAt.IsZero() && time.Now().After(resp.ExpiresAt) {
			fmt.Fprintln(os.Stderr, localize.Text("\nremotai: код истёк — запустите `remotai pair` заново"))
			return 1
		}

		stCtx, stCancel := context.WithTimeout(ctx, 10*time.Second)
		status, err := relay.CheckPairingStatus(stCtx, base, resp.Code)
		stCancel()
		if err != nil {
			// Сеть моргнула — не падаем, продолжаем поллить.
			fmt.Fprint(os.Stderr, "·")
			continue
		}
		if status.Expired {
			fmt.Fprintln(os.Stderr, localize.Text("\nremotai: код истёк — запустите `remotai pair` заново"))
			return 1
		}
		if status.Confirmed && status.JWT != "" {
			if err := web.PersistPairedIdentity(deviceID, base, status.JWT, status.ExpiresAt); err != nil {
				fmt.Fprintf(os.Stderr, localize.Text("\nremotai: не удалось сохранить привязку: %v\n"), err)
				return 1
			}
			fmt.Println(localize.Text("\n\n✅ Сервер привязан!"))
			if status.DeviceID != "" {
				fmt.Printf("   device_id: %s\n", status.DeviceID)
			}
			// Привязку записал ЭТОТ процесс, а на связь выходит СЛУЖБА — другой
			// процесс, который свой конфиг уже прочитал при старте. Если она
			// работала до привязки (обычный случай: сначала install.sh поднял
			// службу, потом человек привязал), `systemctl enable --now` ничего
			// не изменит: unit уже active и enabled. Сервер так и останется «не
			// в сети» — ровно эта жалоба и пришла от владельца, когда служба
			// была active 3 часа, а машина в приложении числилась выключенной.
			// Поэтому перезапускаем службу сами.
			switch restartServiceIfRunning() {
			case serviceRestarted:
				fmt.Println(localize.Text("   Служба перезапущена — сервер выйдет на связь через несколько секунд."))
			case serviceRestartFailed:
				fmt.Println(localize.Text("   Служба уже работает со СТАРОЙ привязкой — перезапустите её:"))
				fmt.Println("     " + serviceRestartHint())
			default:
				fmt.Println(localize.Text("   Дальше: запустите сервис —"))
				fmt.Println("     " + serviceStartHint())
				fmt.Println(localize.Text("   или просто `remotai` для запуска вручную."))
			}
			return 0
		}
		fmt.Fprint(os.Stderr, ".")
	}
}

// printPairInstructions печатает в консоль код, deep-link и ASCII-QR.
func printPairInstructions(resp *relay.PairingResponse, base, hostname, deviceID string) {
	const bar = "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
	fmt.Println()
	fmt.Println(bar)
	fmt.Println(localize.Text("  Remotai — привязка сервера"))
	fmt.Println(bar)
	fmt.Println()
	fmt.Printf(localize.Text("  Сервер:   %s  (device %s)\n"), hostname, deviceID)
	fmt.Printf(localize.Text("  Код:      %s\n"), resp.Code)
	if !resp.ExpiresAt.IsZero() {
		fmt.Printf(localize.Text("  Годен до: %s\n"), resp.ExpiresAt.Local().Format("15:04:05"))
	}
	fmt.Println()
	// Первым — способ для того, кто уже здесь. Эту команду чаще всего запускают
	// ИЗ терминала Remotai на компьютере (живая реакция владельца на прежний
	// текст: «а я захожу с компьютера)»), и предлагать ему первым делом взять
	// телефон и сканировать QR — значит не замечать, где человек находится.
	// Клиент видит код в выводе и показывает кнопку «Привязать этот сервер».
	fmt.Println(localize.Text("  Подтвердите привязку одним из способов:"))
	fmt.Println(localize.Text("   1) Прямо здесь: в Remotai нажмите «Привязать этот сервер»"))
	fmt.Println(localize.Text("      (кнопка появится над строкой ввода этого терминала)."))
	if resp.BotLink != "" {
		fmt.Printf(localize.Text("   2) Откройте в Telegram:  %s\n"), resp.BotLink)
	}
	fmt.Println(localize.Text("   3) Отсканируйте QR приложением Remotai на телефоне:"))
	fmt.Println()

	payload := web.BuildCloudPairPayload(base, resp.Code)
	if qr, err := qrcode.New(payload, qrcode.Low); err == nil {
		fmt.Println(indent(qr.ToSmallString(false), "  "))
	} else {
		fmt.Printf(localize.Text("   (QR недоступен: %v)\n   Данные: %s\n"), err, payload)
	}

	fmt.Println(localize.Text("  Ожидаю подтверждения (Ctrl+C — отмена)…"))
}

// indent добавляет префикс к каждой строке многострочного текста.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// Исход попытки перезапустить службу после привязки.
type serviceRestartResult int

const (
	// serviceNotRunning — службы нет или она не запущена: человеку нужно её поднять.
	serviceNotRunning serviceRestartResult = iota
	// serviceRestarted — перезапустили сами, привязка подхвачена.
	serviceRestarted
	// serviceRestartFailed — служба работает, но перезапустить не смогли (нет прав).
	serviceRestartFailed
)

// userUnitInstalled — служба поставлена в systemd ПОЛЬЗОВАТЕЛЯ, а не системы.
//
// Установка без sudo (`curl … | sh` от обычного человека, а не от root) кладёт
// юнит в ~/.config/systemd/user/ — см. installUser в install.go. Системный
// менеджер про такой юнит не знает вовсе: `systemctl is-active remotai`
// отвечает кодом 4 «нет такого юнита», хотя служба в это время работает.
// Отличать эти два режима обязаны и подсказки, и перезапуск после привязки.
func userUnitInstalled() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".config", "systemd", "user", "remotai.service"))
	return err == nil
}

// serviceStartHint / serviceRestartHint — как поднять службу НА ЭТОЙ системе.
//
// Совет про systemd, показанный на маке, — не мелочь: человек его выполняет,
// получает «command not found: systemctl» и остаётся с компьютером «не в сети»
// без единой рабочей подсказки. Именно это увидел первый живой мак 09.08.2026.
//
// То же правило внутри Linux: человеку, поставившему без sudo, нельзя советовать
// системную команду. `sudo systemctl enable --now remotai` у него ответит «Unit
// remotai.service not found» — юнит лежит в его собственном менеджере. Живой
// Ubuntu 19.08.2026: установка и привязка прошли, а компьютер остался «не в
// сети», и единственная подсказка на экране вела в тупик.
func serviceStartHint() string {
	switch runtime.GOOS {
	case "darwin":
		return localize.Text("remotai install   (поставит и запустит LaunchAgent)")
	case "windows":
		return localize.Text("запустите Remotai из меню «Пуск»")
	default:
		return linuxStartHint(userUnitInstalled())
	}
}

// linuxStartHint / linuxRestartHint — команды для двух режимов установки.
// Вынесены отдельными функциями, чтобы тест проверял обе ветки на любой
// системе: подсказка для Linux не должна оставаться непроверенной оттого, что
// тесты гоняют на Windows.
func linuxStartHint(userUnit bool) string {
	if userUnit {
		return localize.Text("systemctl --user enable --now remotai   (служба стоит в вашем пользователе)")
	}
	return localize.Text("sudo systemctl enable --now remotai   (если ставили через install.sh)")
}

func linuxRestartHint(userUnit bool) string {
	if userUnit {
		return "systemctl --user restart remotai"
	}
	return "sudo systemctl restart remotai"
}

func serviceRestartHint() string {
	switch runtime.GOOS {
	case "darwin":
		// UID подставляем ЧИСЛОМ, а не оставляем `$(id -u)`.
		//
		// Живой мак 10.08.2026: человек получил команду с `$(id -u)`, набрал её
		// руками и промахнулся дважды — `launch` вместо `launchctl` и `id-u`
		// без пробела. Терминал ответил двумя «command not found», компьютер
		// остался не в сети. Команда, которую печатают человеку, обязана
		// копироваться и работать как есть: подстановки, кавычки и скобки — это
		// приглашение к опечатке.
		return fmt.Sprintf("launchctl kickstart -k gui/%d/ru.remotai.agent", os.Getuid())
	case "windows":
		return localize.Text("перезапустите Remotai из меню «Пуск»")
	default:
		return linuxRestartHint(userUnitInstalled())
	}
}

// restartServiceIfRunning перезапускает службу, если она активна.
//
// Зачем вообще: `remotai pair` сохраняет привязку в конфиг, а на связь с
// облаком выходит СЛУЖБА — отдельный процесс, прочитавший конфиг при старте.
// Пока её не перезапустить, сервер остаётся «не в сети» с виду беспричинно:
// `systemctl status` показывает active (running), а машина в приложении
// выключена. Знать про это человек не обязан.
func restartServiceIfRunning() serviceRestartResult {
	// macOS: служба — это LaunchAgent, и та же беда, что описана выше для
	// systemd, там даже вероятнее. `install.sh` поднимает агента ДО привязки,
	// значит к моменту `pair` он уже работает со старым конфигом.
	//
	// Живой первый мак 09.08.2026: установка прошла, «✅ Сервер привязан!», а
	// телефон показывал «MacBook-Air-Andrej-2.local не в сети». Здесь стояло
	// `runtime.GOOS != "linux" → serviceNotRunning`, и человек получал совет
	// `sudo systemctl enable --now remotai` — на маке, где systemd нет вовсе.
	if runtime.GOOS == "darwin" {
		running, err := restartOwnService()
		if !running {
			return serviceNotRunning
		}
		if err != nil {
			return serviceRestartFailed
		}
		return serviceRestarted
	}
	if runtime.GOOS != "linux" {
		return serviceNotRunning
	}
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return serviceNotRunning
	}
	// is-active возвращает ненулевой код для неактивной службы — это не ошибка
	// исполнения, а ответ «не запущена».
	//
	// procutil.Hidden здесь фактически ничего не делает (эта ветка только для
	// Linux, окон там нет), но правило «любой фоновый exec.Command — через
	// Hidden» проверяется в CI по всему дереву: без него сборка красная, а
	// исключений в проверке нет намеренно — так не появляются вспышки окон.
	if err := procutil.Hidden(exec.Command(systemctl, "is-active", "--quiet", "remotai")).Run(); err != nil {
		// Системного юнита нет — но это ещё не значит, что службы нет. Установка
		// без sudo кладёт её в менеджер пользователя, и системный `is-active`
		// отвечает кодом 4 при живой службе (замерено на Ubuntu 24.04: system → 4,
		// --user → 0). Раньше здесь стоял голый выход «не запущена»: живую службу
		// не перезапускали, она продолжала работать со старым конфигом, и человек
		// сразу после «✅ Сервер привязан!» видел компьютер «не в сети». Ровно та
		// же болезнь, что чинили для macOS 09.08.2026, только в user-режиме Linux.
		if err := procutil.Hidden(exec.Command(systemctl, "--user", "is-active", "--quiet", "remotai")).Run(); err != nil {
			return serviceNotRunning
		}
		if err := procutil.Hidden(exec.Command(systemctl, "--user", "restart", "remotai")).Run(); err != nil {
			return serviceRestartFailed
		}
		return serviceRestarted
	}
	if err := procutil.Hidden(exec.Command(systemctl, "restart", "remotai")).Run(); err != nil {
		return serviceRestartFailed
	}
	return serviceRestarted
}
