//go:build darwin

// Текущая папка терминала на macOS.
//
// На Linux это symlink /proc/<pid>/cwd — одно чтение. На маке /proc нет, а
// нужный вызов (proc_pidinfo с PROC_PIDVNODEPATHINFO) живёт в libproc и
// нормальной обёртки в стандартной библиотеке не имеет. Берём его через
// purego — тем же способом, каким уже грузим openh264 и libopus
// (internal/codec): принцип «чистый Go без CGO» держит кросс-сборку под все
// платформы с любой машины, и ради одной функции его ломать нельзя.
package pty

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	"tgcontrol/internal/procutil"
)

const (
	// PROC_PIDVNODEPATHINFO — «текущий и корневой каталог процесса».
	procPIDVnodePathInfo = 9
	// Размер struct proc_vnodepathinfo: два vnode_info_path по 1184 байта
	// (vnode_info 160 + MAXPATHLEN 1024). Буфер берём с запасом: ядро само
	// говорит, сколько байт заполнило, а лишнее нам не мешает.
	vnodePathInfoSize = 4096
	// Смещение поля vip_path внутри первого vnode_info_path (pvi_cdir) —
	// сразу после vnode_info. Если Apple когда-нибудь сдвинет структуру,
	// сработает запасной путь: ищем первую строку, начинающуюся с «/».
	cdirPathOffset = 160
)

var (
	libprocOnce sync.Once
	procPidinfo func(pid int32, flavor int32, arg uint64, buffer unsafe.Pointer, size int32) int32
	libprocErr  error
)

// loadLibproc открывает libSystem (там же живёт libproc) ОДИН раз на процесс.
func loadLibproc() {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		libprocErr = fmt.Errorf("libSystem: %w", err)
		return
	}
	defer func() {
		// RegisterLibFunc паникует, если символа нет: на неожиданной версии
		// macOS это не должно ронять агент — просто уйдём на запасной путь.
		if r := recover(); r != nil {
			libprocErr = fmt.Errorf("proc_pidinfo недоступна: %v", r)
			procPidinfo = nil
		}
	}()
	purego.RegisterLibFunc(&procPidinfo, lib, "proc_pidinfo")
}

// currentCWD — рабочий каталог процесса-шелла.
func (p *conPTY) currentCWD() (string, error) {
	if p.cmd == nil || p.cmd.Process == nil {
		return "", fmt.Errorf("no process")
	}
	pid := int32(p.cmd.Process.Pid)
	if dir, err := cwdViaLibproc(pid); err == nil && dir != "" {
		return dir, nil
	}
	return cwdViaLsof(pid)
}

func cwdViaLibproc(pid int32) (string, error) {
	libprocOnce.Do(loadLibproc)
	if procPidinfo == nil {
		return "", libprocErr
	}
	buf := make([]byte, vnodePathInfoSize)
	n := procPidinfo(pid, procPIDVnodePathInfo, 0, unsafe.Pointer(&buf[0]), int32(len(buf)))
	if n <= 0 {
		return "", fmt.Errorf("proc_pidinfo вернул %d", n)
	}
	data := buf[:n]
	if len(data) > cdirPathOffset && data[cdirPathOffset] == '/' {
		if path := cString(data[cdirPathOffset:]); path != "" {
			return path, nil
		}
	}
	// Запасной разбор: первая строка, начинающаяся со слэша.
	if i := bytes.IndexByte(data, '/'); i >= 0 {
		if path := cString(data[i:]); len(path) > 1 {
			return path, nil
		}
	}
	return "", fmt.Errorf("путь не найден в ответе proc_pidinfo")
}

// cwdViaLsof — запасной путь, если libproc недоступна. Дороже (запуск
// процесса), поэтому только как фолбэк.
func cwdViaLsof(pid int32) (string, error) {
	cmd := exec.Command("/usr/sbin/lsof", "-a", "-d", "cwd", "-p", fmt.Sprint(pid), "-Fn")
	procutil.Hidden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n"), nil
		}
	}
	return "", fmt.Errorf("lsof не сообщил cwd")
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
