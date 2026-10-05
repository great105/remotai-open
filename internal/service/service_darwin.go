//go:build darwin

// Автозапуск агента на macOS — пользовательский LaunchAgent.
//
// Почему НЕ root-демон (LaunchDaemon): агент работает с терминалами, файлами и
// сессией конкретного человека. Демон стартует до входа в систему, в отдельной
// сессии без доступа к пользовательскому окружению — терминалы открывались бы
// не там и не от того имени. Тот же выбор у Linux-варианта (systemd --user не
// используем только потому, что на серверах агент часто ставят root'ом).
//
// Почему не SMAppService (современный API): он требует .app-бандл, а у нас
// CLI-бинарь. Бандл появится позже, вместе с Remote Desktop (macOS привязывает
// разрешения на запись экрана к бандлу), — тогда и пересмотрим.
package service

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tgcontrol/internal/procutil"
)

// label — идентификатор службы в launchd. Один на все версии: launchctl
// адресует службу именно им, и смена метки означала бы «новая служба», а старая
// осталась бы висеть.
const label = "ru.remotai.agent"

// plistPath — ~/Library/LaunchAgents/ru.remotai.agent.plist.
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// domainTarget — «gui/<uid>»: домен пользовательской сессии, в котором живут
// LaunchAgents. Без него launchctl 2.0 не понимает, о какой службе речь.
func domainTarget() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return "gui/" + u.Uid, nil
}

// launchAgentPlist — содержимое plist.
//
// PATH прописан явно: launchd даёт процессу почти пустое окружение, и агент,
// запущенный им, не нашёл бы ни node, ни claude, ни git, поставленные через
// Homebrew (/opt/homebrew/bin на Apple Silicon, /usr/local/bin на Intel).
// Это первая причина, по которой «в терминале работает, а из автозапуска нет».
//
// KeepAlive.SuccessfulExit=false — поднимать только после ПАДЕНИЯ. Штатный
// выход (обновление, «Завершить») перезапуском не считается; сам перезапуск
// после обновления делаем явным `launchctl kickstart -k` (см. Restart).
func launchAgentPlist(exePath, logDir string) string {
	xmlText := func(value string) string {
		var escaped strings.Builder
		_ = xml.EscapeText(&escaped, []byte(value))
		return escaped.String()
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlText(exePath) + `</string>
		<string>--background</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>StandardOutPath</key>
	<string>` + xmlText(filepath.Join(logDir, "remotai.out.log")) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlText(filepath.Join(logDir, "remotai.err.log")) + `</string>
</dict>
</plist>
`
}

// Install регистрирует LaunchAgent и запускает его.
func Install() error {
	path, err := writeLaunchAgent()
	if err != nil {
		return err
	}
	target, err := domainTarget()
	if err != nil {
		return err
	}
	if err := run("launchctl", "enable", target+"/"+label); err != nil {
		return err
	}
	// bootout removes the registration before its old process has necessarily
	// finished. Immediate bootstrap can fail with error 5 on a real Mac.
	previousPID := launchAgentPID(target + "/" + label)
	_ = run("launchctl", "bootout", target+"/"+label)
	if previousPID > 0 {
		deadline := time.Now().Add(10 * time.Second)
		for syscall.Kill(previousPID, 0) == nil {
			if time.Now().After(deadline) {
				return fmt.Errorf("предыдущий процесс Remotai ещё завершается; повторите установку через несколько секунд")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if err := run("launchctl", "bootstrap", target, path); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	return nil
}

func launchAgentPID(target string) int {
	output, err := procutil.Hidden(exec.Command("launchctl", "print", target)).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "pid" && fields[1] == "=" {
			pid, _ := strconv.Atoi(fields[2])
			return pid
		}
	}
	return 0
}

func writeLaunchAgent() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("не удалось определить путь к программе: %w", err)
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	path, err := plistPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("не удалось создать ~/Library/LaunchAgents: %w", err)
	}
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, "Library", "Logs", "Remotai")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", fmt.Errorf("не удалось создать папку логов: %w", err)
	}
	if err := os.WriteFile(path, []byte(launchAgentPlist(exePath, logDir)), 0o644); err != nil {
		return "", fmt.Errorf("не удалось записать %s: %w", path, err)
	}
	return path, nil
}

// SetAutostart changes what happens at the next login without stopping the
// current agent (and its HTTP response) or starting a second copy beside it.
func SetAutostart(enabled bool) error {
	target, err := domainTarget()
	if err != nil {
		return err
	}
	if enabled {
		if _, err := writeLaunchAgent(); err != nil {
			return err
		}
		return run("launchctl", "enable", target+"/"+label)
	}
	return run("launchctl", "disable", target+"/"+label)
}

func AutostartEnabled() bool {
	if !IsInstalled() {
		return false
	}
	target, err := domainTarget()
	if err != nil {
		return false
	}
	cmd := procutil.Hidden(exec.Command("launchctl", "print-disabled", target))
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return launchAgentEnabled(string(output))
}

func launchAgentEnabled(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=>")
		if ok && strings.TrimSpace(key) == `"`+label+`"` {
			// Newer launchctl prints enabled/disabled; older versions use the
			// inverse boolean (true means disabled). Unknown output is not proof.
			switch strings.TrimSpace(value) {
			case "false", "enabled":
				return true
			default:
				return false
			}
		}
	}
	return true // no override: RunAtLoad in the installed plist applies
}

// Uninstall снимает службу и удаляет plist.
func Uninstall() error {
	target, err := domainTarget()
	if err != nil {
		return err
	}
	_ = run("launchctl", "bootout", target+"/"+label)
	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("не удалось удалить %s: %w", path, err)
	}
	return nil
}

// Start запускает уже установленную службу.
func Start() error {
	target, err := domainTarget()
	if err != nil {
		return err
	}
	return run("launchctl", "kickstart", target+"/"+label)
}

// Stop останавливает службу, оставляя её установленной.
func Stop() error {
	target, err := domainTarget()
	if err != nil {
		return err
	}
	return run("launchctl", "kill", "SIGTERM", target+"/"+label)
}

// Restart — перезапуск ЧУЖИМИ руками, а не самоперезапуском.
//
// Подводный камень launchd, на котором горели все агенты с автообновлением: при
// KeepAlive.SuccessfulExit=false выход с кодом 0 считается «успешным
// завершением», и launchd НЕ поднимает процесс обратно — компьютер остаётся без
// агента до следующего логина. Поэтому после подмены бинаря просим launchd
// перезапустить службу самому: `kickstart -k` убивает текущий процесс и
// стартует новый уже из нового файла.
func Restart() error {
	target, err := domainTarget()
	if err != nil {
		return err
	}
	return run("launchctl", "kickstart", "-k", target+"/"+label)
}

// ManagerCanRestart — launchd умеет перезапустить нас по просьбе, и это
// единственный правильный путь после самообновления: новый процесс обязан
// родиться ВНУТРИ домена службы (см. RunAsService).
func ManagerCanRestart() bool { return true }

// RestartByManager просит launchd поднять службу заново на новом бинаре.
//
// ЗАЧЕМ ОТВЯЗЫВАТЬ launchctl. `kickstart -k` гасит процессы СВОЕЙ службы, а
// launchctl, запущенный нами, в неё и входит: он рискует умереть вместе с нами
// раньше, чем успеет отправить запрос. Setsid выводит его из-под удара — тот же
// приём, что у pty-хоста (см. internal/pty/persist_darwin.go).
//
// Ждать результата бессмысленно: если всё удалось, нас убьют посреди ожидания.
// Поэтому возвращаемся сразу после старта, а не после Wait.
//
// ПРОВЕРЕНО ЗАПУСКОМ на живом маке 10.08.2026 (macOS 14.8.2): `kickstart -k`
// перезапустил агента за 10 секунд, версия сменилась 2.55.16 → 2.55.18.
func RestartByManager() error {
	target, err := domainTarget()
	if err != nil {
		return err
	}
	cmd := exec.Command("launchctl", "kickstart", "-k", target+"/"+label)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	procutil.Hidden(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launchctl kickstart: %w", err)
	}
	go cmd.Wait() // не оставляем зомби, если переживём просьбу
	return nil
}

// IsInstalled — есть ли plist на диске.
func IsInstalled() bool {
	path, err := plistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// IsRunning — знает ли launchd о живой службе. `launchctl print` отвечает
// ошибкой, если службы в домене нет; у живой в выводе есть «state = running».
func IsRunning() bool {
	target, err := domainTarget()
	if err != nil {
		return false
	}
	cmd := exec.Command("launchctl", "print", target+"/"+label)
	procutil.Hidden(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "state = running")
}

// RunAsService — запущены ли МЫ самим launchd. Он выставляет процессу
// XPC_SERVICE_NAME со значением нашей метки; у запуска из терминала переменной
// нет вовсе (либо там «0» для обычных приложений).
//
// Зачем это знать: автообновление в service-режиме не перезапускает себя само —
// перезапуск отдаётся launchd (см. Restart), иначе новый процесс родится вне
// домена службы и будет убит при первом же kickstart.
func RunAsService() bool {
	return os.Getenv("XPC_SERVICE_NAME") == label
}

// UnderSystemd на маке всегда false: systemd тут нет, роль «сервис-менеджер
// сам поднимет процесс» играет launchd (см. RunAsService).
func UnderSystemd() bool { return false }

// Run — точка входа службы Windows; на маке launchd запускает обычный процесс,
// поэтому отдельного режима не требуется.
func Run(appFunc func(stopCh <-chan struct{})) error {
	return fmt.Errorf("на macOS агент запускается launchd как обычный процесс")
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	procutil.Hidden(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%s: %s", err, msg)
	}
	return nil
}
