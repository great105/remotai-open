package main

// `remotai install` / `remotai uninstall` — установка «как у больших CLI»:
// после скачивания бинаря (install.sh, npm-обёртка или вручную) одна команда
// настраивает автозапуск и сразу запускает привязку `remotai pair`.
//
// Режимы автозапуска (Linux):
//   - system: /etc/systemd/system/remotai.service (root && systemctl). Сервис-
//     юзер: REMOTAI_USER → SUDO_USER → root. Профиль юнита — см.
//     deploy/systemd/remotai.service (Restart=always, KillMode=process, без
//     ProtectSystem — автообновление подменяет бинарь).
//   - user: ~/.config/systemd/user/remotai.service (systemctl --user) +
//     best-effort loginctl enable-linger, чтобы сервис переживал логаут.
//   - fallback: systemctl нет — не падаем, печатаем готовые команды запуска
//     (nohup) и автозапуска (@reboot в crontab).
//
// ВАЖНО: device-JWT пишется под os.UserHomeDir вызвавшего (см. internal/paths),
// поэтому pair должен идти под ТЕМ ЖЕ юзером, что и сервис. В system-режиме с
// сервис-юзером ≠ root pair запускается через `sudo -H -u <user> <exe> pair`
// (-H — чтобы HOME указывал на дом сервис-юзера, иначе JWT «потеряется»).

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"tgcontrol/internal/desktopentry"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/procutil"
	"tgcontrol/internal/service"
	"tgcontrol/internal/web"
)

const systemUnitPath = "/etc/systemd/system/remotai.service"

type installOptions struct {
	noPair      bool
	forceUser   bool
	forceSystem bool
	purge       bool
}

// installMode — выбранный способ автозапуска.
type installMode int

const (
	modeSystem installMode = iota
	modeUser
	modeFallback
)

func runInstall(args []string) int {
	opts, err := parseInstallFlags(args, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: %v\n", err)
		fmt.Fprintln(os.Stderr, "Использование: remotai install [--user|--system] [--no-pair]")
		return 2
	}

	if runtime.GOOS == "windows" {
		fmt.Println("Remotai на Windows не требует `remotai install`:")
		fmt.Println("  откройте панель → «Запуск и локальная сеть» → «Запускать при входе в Windows»")
		fmt.Println("  или инсталлятором remotai-setup.exe — https://remotai.ru")
		return 0
	}
	// macOS: автозапуск — пользовательский LaunchAgent (см. service_darwin.go).
	// Системного варианта нет намеренно: агент работает с терминалами и файлами
	// конкретного человека, а LaunchDaemon стартует до входа в систему и в
	// чужой сессии.
	if runtime.GOOS == "darwin" {
		return installDarwin(opts)
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintf(os.Stderr, "remotai install: платформа %s не поддерживается — скачайте агент с https://remotai.ru\n", runtime.GOOS)
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: не удалось определить путь к бинарю: %v\n", err)
		return 1
	}

	mode, err := resolveInstallMode(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: %v\n", err)
		return 1
	}

	serviceUser := ""
	switch mode {
	case modeSystem:
		serviceUser = installSystem(exe)
		if serviceUser == "" {
			return 1 // ошибка уже напечатана
		}
	case modeUser:
		if !installUser(exe) {
			return 1
		}
	case modeFallback:
		printFallbackInstructions(exe)
	}
	if mode == modeUser || (mode == modeFallback && !isRoot()) {
		entry, err := desktopentry.Install(exe)
		if err != nil {
			fmt.Fprintf(os.Stderr, "remotai install: не удалось добавить ярлык приложения: %v\n", err)
			return 1
		}
		fmt.Printf("✓ Remotai добавлен в меню приложений: %s\n", entry)
	}

	// ── Привязка ────────────────────────────────────────────────────
	if opts.noPair {
		fmt.Println()
		fmt.Println("Привязка пропущена (--no-pair). Когда будете готовы:")
		if serviceUser != "" && serviceUser != "root" {
			fmt.Printf("  sudo -H -u %s %s pair\n", serviceUser, exe)
		} else {
			fmt.Printf("  %s pair\n", exe)
		}
		return 0
	}

	// pair — под тем же юзером, что и сервис (см. шапку файла).
	if mode == modeSystem && serviceUser != "root" {
		fmt.Printf("\n→ Запускаю привязку под пользователем %s…\n", serviceUser)
		// Окно НЕ гасим: привязка диалоговая — человек вводит пароль sudo и
		// читает QR прямо в своём терминале, которым команда и пользуется.
		cmd := exec.Command("sudo", "-H", "-u", serviceUser, exe, "pair")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode()
			}
			fmt.Fprintf(os.Stderr, "remotai install: не удалось запустить pair: %v\n", err)
			return 1
		}
		return 0
	}
	return runPair(nil)
}

func runUninstall(args []string) int {
	opts, err := parseInstallFlags(args, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai uninstall: %v\n", err)
		fmt.Fprintln(os.Stderr, "Использование: remotai uninstall [--purge]")
		return 2
	}

	if runtime.GOOS == "windows" {
		_ = web.DisableAutostart()
		_ = runUnpair(nil)
		if err := cleanupInstalledArtifacts(); err != nil {
			fmt.Fprintf(os.Stderr, "remotai uninstall: очистка PATH/self-copy: %v\n", err)
		}
		fmt.Println("Remotai на Windows удаляется из «Установка и удаление программ»")
		fmt.Println("(ставился инсталлятором remotai-setup.exe).")
		return 0
	}
	if runtime.GOOS == "darwin" {
		return uninstallDarwin(opts.purge)
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintf(os.Stderr, "remotai uninstall: платформа %s не поддерживается\n", runtime.GOOS)
		return 1
	}
	_ = runUnpair(nil)

	// Какие юниты реально стоят — по факту наличия файлов.
	home, _ := os.UserHomeDir()
	userUnitPath := filepath.Join(home, ".config", "systemd", "user", "remotai.service")
	hasSystem := fileExists(systemUnitPath)
	hasUser := fileExists(userUnitPath)

	if !hasSystem && !hasUser {
		fmt.Println("remotai: systemd-юниты не найдены — автозапуск не настроен.")
	}

	// Сервис-юзер system-юнита (для --purge: конфиг живёт в его доме).
	serviceUser := ""
	if hasSystem {
		serviceUser = parseUnitUser(systemUnitPath)
		if !isRoot() {
			fmt.Fprintln(os.Stderr, "remotai uninstall: system-юнит требует root — запустите: sudo remotai uninstall")
			return 1
		}
		fmt.Println("→ Останавливаю и отключаю remotai.service (system)…")
		warnCmd("systemctl", "disable", "--now", "remotai.service")
		if err := os.Remove(systemUnitPath); err != nil {
			fmt.Fprintf(os.Stderr, "remotai uninstall: не удалось удалить %s: %v\n", systemUnitPath, err)
		}
		warnCmd("systemctl", "daemon-reload")
		fmt.Println("  system-юнит удалён.")
	}
	if hasUser {
		fmt.Println("→ Останавливаю и отключаю remotai.service (user)…")
		warnCmd("systemctl", "--user", "disable", "--now", "remotai.service")
		if err := os.Remove(userUnitPath); err != nil {
			fmt.Fprintf(os.Stderr, "remotai uninstall: не удалось удалить %s: %v\n", userUnitPath, err)
		}
		warnCmd("systemctl", "--user", "daemon-reload")
		fmt.Println("  user-юнит удалён.")
	}

	if err := desktopentry.Remove(); err != nil {
		fmt.Fprintf(os.Stderr, "remotai uninstall: ярлык приложения: %v\n", err)
		return 1
	}
	if !opts.purge {
		if hasSystem || hasUser {
			fmt.Println("\nБинарь и конфиг оставлены. Полное удаление: remotai uninstall --purge")
		}
		return 0
	}

	// ── --purge: бинарь + конфиг/identity ───────────────────────────
	if exe, err := os.Executable(); err == nil {
		if err := os.Remove(exe); err != nil {
			fmt.Fprintf(os.Stderr, "remotai uninstall: не удалось удалить %s: %v\n", exe, err)
		} else {
			fmt.Printf("  бинарь удалён: %s\n", exe)
		}
	}
	// Конфиг вызвавшего (installed-раскладка ~/.config/remotai). Portable
	// (config.json рядом с exe) не трогаем: Base() там — КАТАЛОГ БИНАРЯ,
	// RemoveAll по нему снёс бы, например, /usr/local/bin целиком.
	if base := paths.Base(); !paths.Portable() && filepath.Base(base) == "remotai" {
		if err := os.RemoveAll(base); err != nil {
			fmt.Fprintf(os.Stderr, "remotai uninstall: не удалось удалить %s: %v\n", base, err)
		} else {
			fmt.Printf("  конфиг удалён: %s\n", base)
		}
	}
	// Конфиг сервис-юнита живёт в доме СЕРВИС-юзера — при purge под root
	// без этого шага identity осталась бы лежать у него.
	if serviceUser != "" && serviceUser != "root" {
		if u, err := user.Lookup(serviceUser); err == nil && u.HomeDir != "" {
			dir := filepath.Join(u.HomeDir, ".config", "remotai")
			if fileExists(dir) {
				if err := os.RemoveAll(dir); err != nil {
					fmt.Fprintf(os.Stderr, "remotai uninstall: не удалось удалить %s: %v\n", dir, err)
				} else {
					fmt.Printf("  конфиг удалён: %s\n", dir)
				}
			}
		}
	}
	fmt.Println("\n✅ Remotai удалён. При повторной установке потребуется новая привязка.")
	return 0
}

// parseInstallFlags разбирает флаги install/uninstall. allowPurge — --purge
// имеет смысл только у uninstall.
func parseInstallFlags(args []string, allowPurge bool) (installOptions, error) {
	var opts installOptions
	for _, a := range args {
		switch a {
		case "--no-pair":
			if allowPurge {
				return opts, fmt.Errorf("неизвестный флаг %s", a)
			}
			opts.noPair = true
		case "--user":
			opts.forceUser = true
		case "--system":
			opts.forceSystem = true
		case "--purge":
			if !allowPurge {
				return opts, fmt.Errorf("неизвестный флаг %s", a)
			}
			opts.purge = true
		default:
			return opts, fmt.Errorf("неизвестный флаг %s", a)
		}
	}
	if opts.forceUser && opts.forceSystem {
		return opts, fmt.Errorf("флаги --user и --system несовместимы")
	}
	return opts, nil
}

// resolveInstallMode выбирает режим автозапуска: явные флаги, иначе авто —
// root&&работающий systemd → system, systemctl --user → user, иначе fallback.
func resolveInstallMode(opts installOptions) (installMode, error) {
	switch {
	case opts.forceSystem:
		if !isRoot() {
			return modeFallback, fmt.Errorf("--system требует root — запустите: sudo remotai install --system")
		}
		if !hasSystemctl() {
			return modeFallback, fmt.Errorf("--system требует systemd (systemctl не найден)")
		}
		if !hasRunningSystemd() {
			return modeFallback, fmt.Errorf("systemctl есть, но systemd не отвечает (WSL/контейнер?) — проверьте: systemctl is-system-running")
		}
		return modeSystem, nil
	case opts.forceUser:
		if hasUserSystemd() {
			return modeUser, nil
		}
		fmt.Println("→ systemctl --user недоступен — переключаюсь на ручной режим (fallback).")
		return modeFallback, nil
	default:
		if isRoot() {
			if hasRunningSystemd() {
				return modeSystem, nil
			}
			if hasSystemctl() {
				fmt.Println("→ systemctl есть, но systemd не запущен (WSL/контейнер?) — ручной режим (fallback).")
			} else {
				fmt.Println("→ systemd не найден — настраиваю запуск вручную (fallback).")
			}
		} else if hasUserSystemd() {
			fmt.Println("→ Режим: systemd user-unit (запуск не под root).")
			return modeUser, nil
		} else {
			fmt.Println("→ systemd --user недоступен — настраиваю запуск вручную (fallback).")
		}
		return modeFallback, nil
	}
}

// installSystem пишет system-unit и включает сервис. Возвращает сервис-юзера
// ("" при ошибке — она уже напечатана).
func installSystem(exe string) string {
	serviceUser := serviceUserName()
	fmt.Printf("→ Создаю %s (User=%s)…\n", systemUnitPath, serviceUser)
	if err := os.WriteFile(systemUnitPath, []byte(systemdUnit(exe, serviceUser, true)), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: не удалось записать %s: %v\n", systemUnitPath, err)
		return ""
	}
	if err := runCmd("systemctl", "daemon-reload"); err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: %v\n", err)
		return ""
	}
	if err := runCmd("systemctl", "enable", "--now", "remotai.service"); err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: %v\n", err)
		fmt.Fprintln(os.Stderr, "  Диагностика: systemctl status remotai && journalctl -u remotai -e")
		return ""
	}
	fmt.Println("✅ Сервис запущен (systemd: remotai.service).")
	fmt.Println("   Статус:  systemctl status remotai")
	fmt.Println("   Логи:    journalctl -u remotai -f")
	return serviceUser
}

// installUser пишет user-unit, включает сервис и best-effort linger.
func installUser(exe string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: не удалось определить домашний каталог: %v\n", err)
		return false
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: не удалось создать %s: %v\n", unitDir, err)
		return false
	}
	unitPath := filepath.Join(unitDir, "remotai.service")
	fmt.Printf("→ Создаю %s…\n", unitPath)
	if err := os.WriteFile(unitPath, []byte(systemdUnit(exe, "", false)), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: не удалось записать %s: %v\n", unitPath, err)
		return false
	}
	if err := runCmd("systemctl", "--user", "daemon-reload"); err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: %v\n", err)
		return false
	}
	if err := runCmd("systemctl", "--user", "enable", "--now", "remotai.service"); err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: %v\n", err)
		fmt.Fprintln(os.Stderr, "  Диагностика: systemctl --user status remotai && journalctl --user -u remotai -e")
		return false
	}

	// linger: без него user-manager гаснет на логауте и сервис умирает вместе
	// с SSH-сессией. Best-effort: без парольного sudo просто предупреждаем.
	lingerArgs := []string{"enable-linger"}
	if u := os.Getenv("USER"); u != "" {
		lingerArgs = append(lingerArgs, u)
	}
	var lingerErr error
	if isRoot() {
		lingerErr = runCmd("loginctl", lingerArgs...)
	} else {
		lingerErr = runCmd("sudo", append([]string{"-n", "loginctl"}, lingerArgs...)...)
	}
	if lingerErr != nil {
		u := os.Getenv("USER")
		fmt.Fprintln(os.Stderr, "remotai install: [WARN] не удалось включить linger — сервис умрёт при логауте.")
		fmt.Fprintf(os.Stderr, "  Выполните вручную: sudo loginctl enable-linger %s\n", u)
	}

	fmt.Println("✅ Сервис запущен (systemd --user: remotai.service).")
	fmt.Println("   Статус:  systemctl --user status remotai")
	fmt.Println("   Логи:    journalctl --user -u remotai -f")
	return true
}

// printFallbackInstructions — система без systemd: печатаем готовые команды
// запуска и автозапуска вместо юнита.
func printFallbackInstructions(exe string) {
	logPath := "~/.config/remotai/remotai.log"
	fmt.Println()
	fmt.Println("systemd не обнаружен — автозапуск не настроен. Запуск вручную:")
	fmt.Println()
	fmt.Printf("  nohup %s --background >> %s 2>&1 &\n", exe, logPath)
	fmt.Println()
	if _, err := exec.LookPath("crontab"); err == nil {
		fmt.Println("Автозапуск при перезагрузке (через crontab):")
		fmt.Println()
		fmt.Printf("  (crontab -l 2>/dev/null; echo '@reboot %s --background >> %s 2>&1') | crontab -\n", exe, logPath)
	} else {
		fmt.Println("crontab не найден — автозапуск настройте средствами вашего init")
		fmt.Println("(OpenRC/runit/s6) или добавьте команду выше в стартовые скрипты.")
	}
	fmt.Println()
}

// systemdUnit — шаблон юнита (профиль deploy/systemd/remotai.service). Для
// user-режима userName пуст и WantedBy=default.target (multi-user.target в
// user-менеджере отсутствует).
func systemdUnit(exe, userName string, system bool) string {
	// ExecStart is parsed by systemd, not a shell. Quote one argument, escape
	// specifiers and disable environment expansion with ':' so paths containing
	// spaces, '%' or '$' still address the executable the user installed.
	exe = service.SystemdExecPath(exe)
	userLine := ""
	if userName != "" {
		userLine = "User=" + userName + "\n"
	}
	wantedBy := "default.target"
	if system {
		wantedBy = "multi-user.target"
	}
	return fmt.Sprintf(`[Unit]
Description=Remotai — remote control agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
%sExecStart=%s --background
Restart=always
RestartSec=3
KillMode=process

[Install]
WantedBy=%s
`, userLine, exe, wantedBy)
}

// serviceUserName — под кем работает сервис: REMOTAI_USER → SUDO_USER → root
// (пейрить и запускать под одним юзером — иначе device-JWT «потеряется»).
func serviceUserName() string {
	if u := os.Getenv("REMOTAI_USER"); u != "" {
		return u
	}
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	return "root"
}

// parseUnitUser достаёт User= из system-юнита ("" — если не задан/нет файла).
func parseUnitUser(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "User=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "User="))
		}
	}
	return ""
}

func hasSystemctl() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// hasRunningSystemd — systemctl есть И systemd реально отвечает. В контейнерах
// и WSL без systemd systemctl установлен, но не может подключиться к шине —
// такие системы отправляем в fallback вместо ошибки на записи юнита.
// daemon-reload безопасен и идемпотентен; зовём только под root (auto-режим),
// где polkit не встанет на пути.
func hasRunningSystemd() bool {
	if !hasSystemctl() {
		return false
	}
	// Окно гасим: это проба, а не действие человека (на Linux — пустышка).
	return procutil.Hidden(exec.Command("systemctl", "daemon-reload")).Run() == nil
}

// hasUserSystemd — работает ли `systemctl --user` (есть user-менеджер и шина).
// daemon-reload безопасен и идемпотентен — используем его как пробу.
func hasUserSystemd() bool {
	if !hasSystemctl() {
		return false
	}
	// Окно гасим: та же проба, только для user-менеджера.
	return procutil.Hidden(exec.Command("systemctl", "--user", "daemon-reload")).Run() == nil
}

// runCmd запускает команду и при ошибке возвращает её текст + вывод —
// пользователю сразу видно, чем чинить.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	// Окно гасим: вспомогательная команда установки. Вывод мы и так забираем
	// пайпом (CombinedOutput) и печатаем сами — своё окно ей ни к чему.
	procutil.Hidden(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, msg)
		}
		return fmt.Errorf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	return nil
}

// warnCmd — как runCmd, но ошибка уходит в предупреждение (uninstall идёт
// дальше: юнит может быть уже остановлен/выключен).
func warnCmd(name string, args ...string) {
	if err := runCmd(name, args...); err != nil {
		fmt.Fprintf(os.Stderr, "  [WARN] %v\n", err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
