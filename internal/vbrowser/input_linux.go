//go:build linux

package vbrowser

// Ввод для экрана Linux-агента. Картинку отдаёт Xvfb (или настоящий X-сервер),
// а нажатия шлёт xdotool — без него человек видит рабочий стол, по которому
// нельзя кликнуть. Живой случай владельца: на сервере стояли Xvfb и
// google-chrome, виртуальный браузер запустился, а каждое нажатие упиралось в
// плашку «Компьютеру нечем принимать нажатия… Установите его на компьютере» —
// то есть в совет пойти в SSH и разобраться самому. Здесь агент ставит пакет
// сам, по одной кнопке с экрана.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// InputTool — бинарь, которым Linux-агент шлёт нажатия (см. internal/input).
const InputTool = "xdotool"

// FindInputTool возвращает путь до xdotool или "" — проверка идёт при каждом
// обращении, поэтому установка подхватывается без перезапуска агента.
func FindInputTool() string {
	if p, err := exec.LookPath(InputTool); err == nil {
		return p
	}
	return ""
}

// pkgManager — системный пакетный менеджер и то, как им ставить пакет.
type pkgManager struct {
	bin     string   // apt-get, dnf, …
	install []string // аргументы установки (без имени пакета)
	refresh []string // обновление списков перед установкой; пусто — не нужно
	pkg     string   // имя пакета в этом дистрибутиве
	manual  string   // команда для человека, если ставить придётся руками
}

// detectPkgManager определяет менеджер по наличию бинаря. Порядок — от самых
// распространённых у VPS-образов к остальным.
func detectPkgManager() *pkgManager {
	// DPkg::Lock::Timeout — не украшательство: на свежей Ubuntu-VM lock держит
	// unattended-upgrades, и без ожидания apt-get падает сразу («Could not get
	// lock»), хотя через минуту всё бы прошло. Замерено на живом сервере.
	apt := []string{"-o", "DPkg::Lock::Timeout=180", "install", "-y"}
	for _, pm := range []*pkgManager{
		{bin: "apt-get", install: apt, refresh: []string{"-o", "DPkg::Lock::Timeout=180", "update"}, pkg: InputTool, manual: "sudo apt install " + InputTool},
		{bin: "dnf", install: []string{"install", "-y"}, pkg: InputTool, manual: "sudo dnf install " + InputTool},
		{bin: "yum", install: []string{"install", "-y"}, pkg: InputTool, manual: "sudo yum install " + InputTool},
		{bin: "zypper", install: []string{"--non-interactive", "install"}, pkg: InputTool, manual: "sudo zypper install " + InputTool},
		{bin: "pacman", install: []string{"-S", "--noconfirm"}, pkg: InputTool, manual: "sudo pacman -S " + InputTool},
		{bin: "apk", install: []string{"add", "--no-cache"}, pkg: InputTool, manual: "sudo apk add " + InputTool},
	} {
		if _, err := exec.LookPath(pm.bin); err == nil {
			return pm
		}
	}
	return nil
}

// manualInstallCmd — что показать человеку, когда сами поставить не можем.
func manualInstallCmd(pm *pkgManager) string {
	if pm == nil {
		return "sudo apt install " + InputTool
	}
	return pm.manual
}

// installPrivilege возвращает префикс команды, дающий права на установку:
// пустой срез для root, {"sudo","-n"} когда sudo не спросит пароль, nil —
// когда установить нельзя (обычный пользователь без беспарольного sudo).
// sudo -n именно потому, что ставить нас просят с телефона: интерактивный
// запрос пароля некому увидеть, и процесс висел бы до таймаута.
func installPrivilege(pm *pkgManager) []string {
	if pm == nil {
		return nil
	}
	if os.Geteuid() == 0 {
		return []string{}
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return nil
	}
	if err := exec.Command(sudo, "-n", "true").Run(); err != nil {
		return nil // пароль спросит — молча этого не сделать
	}
	return []string{sudo, "-n"}
}

// Состояние фоновой установки. Установка идёт ФОНОМ, а не внутри запроса:
// облачный запрос клиента живёт 30 секунд, а apt на свежей VM легко занимает
// минуты (ещё и ждёт lock от unattended-upgrades). Экран спрашивает результат
// поллингом статуса — так он переживает и таймаут, и уход в фон, и обрыв связи.
var (
	inputMu    sync.Mutex
	inputBusy  bool
	inputErr   string
	inputLog   string
	inputSince time.Time
)

// StartInstallInput запускает установку xdotool в фоне. Идемпотентна: пока
// установка идёт, повторное нажатие ничего не плодит. Ошибку возвращает
// только на том, за что и браться нельзя (нет менеджера пакетов или прав) —
// это человеку нужно знать сразу, а не через минуту ожидания.
func StartInstallInput() error {
	inputMu.Lock()
	defer inputMu.Unlock()
	if inputBusy {
		return nil
	}
	if FindInputTool() != "" {
		return nil
	}
	pm := detectPkgManager()
	if pm == nil {
		return fmt.Errorf("неизвестный пакетный менеджер — установите вручную: %s", manualInstallCmd(nil))
	}
	if installPrivilege(pm) == nil {
		return fmt.Errorf("нужны права root — выполните на компьютере: %s", pm.manual)
	}
	inputBusy, inputErr, inputLog, inputSince = true, "", "", time.Now()
	go func() {
		out, err := installInput(context.Background(), pm)
		inputMu.Lock()
		inputBusy, inputLog = false, out
		if err != nil {
			inputErr = err.Error()
			log.Printf("[VBROWSER] install %s failed: %v — %s", InputTool, err, out)
		} else {
			log.Printf("[VBROWSER] %s installed", InputTool)
		}
		inputMu.Unlock()
	}()
	return nil
}

// inputInstallState — снимок для статуса.
func inputInstallState() (busy bool, errText, logTail string) {
	inputMu.Lock()
	defer inputMu.Unlock()
	return inputBusy, inputErr, inputLog
}

// installInput ставит xdotool системным пакетным менеджером и возвращает хвост
// вывода (для экрана и для лога агента).
func installInput(ctx context.Context, pm *pkgManager) (string, error) {
	if p := FindInputTool(); p != "" {
		return p + " уже установлен", nil
	}
	prefix := installPrivilege(pm)
	if prefix == nil {
		return "", fmt.Errorf("нужны права root — выполните на компьютере: %s", pm.manual)
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()

	run := func(args []string) (string, error) {
		full := append(append([]string{}, prefix...), pm.bin)
		full = append(full, args...)
		cmd := exec.CommandContext(ctx, full[0], full[1:]...)
		// noninteractive: apt иначе может открыть диалог настройки пакета и
		// повиснуть на нём в фоне службы.
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	var log strings.Builder
	if len(pm.refresh) > 0 {
		// Списки могли устареть — обновляем, но неудача здесь не приговор:
		// пакет часто уже есть в кэше.
		out, _ := run(pm.refresh)
		log.WriteString(lastLines(strings.TrimSpace(out), 2))
		log.WriteString("\n")
	}
	out, err := run(append(append([]string{}, pm.install...), pm.pkg))
	log.WriteString(strings.TrimSpace(out))
	tail := lastLines(strings.TrimSpace(log.String()), 6)
	if err != nil {
		return tail, fmt.Errorf("%s %s: %w", pm.bin, pm.pkg, err)
	}
	if FindInputTool() == "" {
		return tail, fmt.Errorf("%s поставлен, но не нашёлся в PATH", pm.pkg)
	}
	return tail, nil
}
