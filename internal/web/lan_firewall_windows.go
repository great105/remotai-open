//go:build windows

package web

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"tgcontrol/internal/procutil"
)

func lanFirewallStatus(port int) (bool, string) {
	// Без окна: это тихая проверка состояния брандмауэра при открытии экранов
	// доступа по локальной сети. Вывод netsh разбираем сами.
	out, err := procutil.Hidden(exec.Command(
		"netsh", "advfirewall", "firewall", "show", "rule", "name=Remotai",
	)).CombinedOutput()
	text := strings.ToLower(string(out))
	if err == nil &&
		!strings.Contains(text, "no rules match") &&
		!strings.Contains(text, "правила, удовлетворяющие") {
		return true, "Правило Remotai найдено в брандмауэре Windows"
	}
	return false, fmt.Sprintf("Входящие TCP-подключения к порту %d могут блокироваться", port)
}

// allowLANFirewall opens the standard UAC prompt. The HTTP request must not
// impersonate elevation; Windows visibly asks the user to approve netsh.
func allowLANFirewall(port int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	params := fmt.Sprintf(
		`advfirewall firewall add rule name="Remotai" dir=in action=allow program="%s" protocol=TCP localport=%s profile=private enable=yes`,
		exe, strconv.Itoa(port),
	)
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString("netsh.exe")
	args, _ := syscall.UTF16PtrFromString(params)
	shell32 := syscall.NewLazyDLL("shell32.dll")
	// Последний аргумент — nShowCmd. SW_HIDE (0) вместо SW_SHOWNORMAL (1): netsh
	// консольный и без этого вспыхивал бы чёрным окном после подтверждения.
	// Запрос прав UAC рисует Windows на защищённом рабочем столе — он от
	// nShowCmd не зависит и остаётся видимым, как и задумано.
	const swHide = 0
	ret, _, callErr := shell32.NewProc("ShellExecuteW").Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(args)),
		0,
		swHide,
	)
	if ret <= 32 {
		return fmt.Errorf("не удалось запросить разрешение брандмауэра: %v (код %d)", callErr, ret)
	}
	return nil
}
