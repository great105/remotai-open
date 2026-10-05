//go:build windows

package netwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"tgcontrol/internal/procutil"
)

// detectRunningVPN ищет знакомый VPN-клиент среди процессов пользователя.
func detectRunningVPN() *VPN {
	procs, err := process.Processes()
	if err != nil {
		return nil
	}
	for _, p := range procs {
		name, err := p.Name()
		if err != nil {
			continue
		}
		display, ok := vpnDisplayName(name)
		if !ok {
			continue
		}
		exe, _ := p.Exe()
		v := &VPN{Name: display, PID: int(p.Pid), Exe: exe, Running: true}
		v.Task = findVPNTask(display)
		v.StartHint = startHint(v)
		return v
	}
	return nil
}

// DetectVPNTarget — чем включать VPN, даже когда он сейчас не запущен: сначала
// запущенный процесс, потом задача планировщика, потом установленный файл.
func DetectVPNTarget() *VPN {
	if v := DetectVPN(); v != nil {
		return v
	}
	for _, name := range []string{"Hiddify"} {
		if task := findVPNTask(name); task != "" {
			v := &VPN{Name: name, Task: task}
			v.StartHint = startHint(v)
			return v
		}
	}
	for _, p := range []string{
		filepath.Join(os.Getenv("ProgramFiles"), "Hiddify", "Hiddify.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Hiddify", "Hiddify.exe"),
	} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			v := &VPN{Name: "Hiddify", Exe: p}
			v.StartHint = startHint(v)
			return v
		}
	}
	return nil
}

// StopVPN завершает процесс клиента.
//
// ПОЧЕМУ ПРОСТО «УБИТЬ», А НЕ ПОПРОСИТЬ ЗАКРЫТЬСЯ: закрывать нам приходится
// ровно того, кто уже завис (живой клиент сторож не трогает вовсе), а
// tun-адаптер система снимает сама, когда процесс-владелец исчезает — на этом
// и держится возврат маршрутов. Мягкое закрытие у Flutter-приложения
// сворачивает окно в трей и туннель не отпускает.
func StopVPN(v *VPN) error {
	if v == nil || v.PID == 0 {
		return ErrNoVPN
	}
	defer invalidateDetect()
	// Гасим ВСЕ процессы этого клиента, а не только найденный первым: у
	// GUI-клиентов рядом с окном живут вспомогательные процессы того же имени,
	// и оставшийся в живых способен держать туннель дальше.
	killed := 0
	for _, pid := range sameClientPIDs(v) {
		p, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := p.Kill(); err == nil {
			killed++
		}
	}
	if killed == 0 {
		return errors.New("не удалось завершить " + v.Name)
	}
	// Ждём фактического исчезновения: сразу после Kill адаптер ещё в системе,
	// и проверка связи померила бы старое состояние.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if running, _ := pidAlive(v.PID); !running {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

// StartVPN включает VPN обратно. Возвращает, чем именно запустили.
//
// ГЛАВНОЕ ЗДЕСЬ — ЗАДАЧА ПЛАНИРОВЩИКА. Туннельный режим требует прав
// администратора, поэтому Hiddify на этой машине автозапускается задачей
// «Hiddify Autostart (admin)» с RunLevel=HighestAvailable (проверено чтением
// XML задачи). Простой запуск exe из-под агента прав не добавит: клиент
// поднимется, а туннеля не будет — то есть «включил» окажется неправдой.
func StartVPN(v *VPN) (string, error) {
	if v == nil {
		v = DetectVPNTarget()
	}
	if v == nil {
		return "", ErrNoVPN
	}
	defer invalidateDetect()
	if v.Task != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "schtasks", "/run", "/tn", v.Task)
		procutil.Hidden(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("задача «%s»: %v: %s", v.Task, err, strings.TrimSpace(string(out)))
		}
		return "задача планировщика «" + v.Task + "»", nil
	}
	if v.Exe == "" {
		return "", errors.New("не знаю, чем запускать " + v.Name)
	}
	cmd := exec.Command(v.Exe)
	cmd.Dir = filepath.Dir(v.Exe)
	procutil.Hidden(cmd)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { _ = cmd.Wait() }() // не оставляем зомби-запись о дочернем процессе
	return v.Exe, nil
}

func pidAlive(pid int) (bool, error) {
	ok, err := process.PidExists(int32(pid))
	return ok, err
}

// sameClientPIDs — все процессы того же VPN-клиента. Найденный сторожем PID в
// списке всегда: даже если снимок процессов не удался, выключать что-то надо.
func sameClientPIDs(v *VPN) []int {
	out := []int{v.PID}
	procs, err := process.Processes()
	if err != nil {
		return out
	}
	for _, p := range procs {
		if int(p.Pid) == v.PID {
			continue
		}
		name, err := p.Name()
		if err != nil {
			continue
		}
		if display, ok := vpnDisplayName(name); ok && display == v.Name {
			out = append(out, int(p.Pid))
		}
	}
	return out
}

// findVPNTask ищет задачу планировщика, по имени которой видно, что она
// запускает этот клиент. Разбираем только первую колонку CSV — именно она и
// нужна команде `schtasks /run`.
func findVPNTask(display string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "schtasks", "/query", "/fo", "csv", "/nh")
	procutil.Hidden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	needle := strings.ToLower(display)
	firstTask := "" // запасной вариант, если задачи с «admin» в имени нет
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, `"`) {
			continue
		}
		end := strings.Index(line[1:], `"`)
		if end <= 0 {
			continue
		}
		name := line[1 : 1+end]
		if !strings.Contains(strings.ToLower(name), needle) {
			continue
		}
		// Задача с правами администратора ценнее обычной: без них туннель не
		// поднимется. В имени это обычно и написано («… (admin)»).
		if strings.Contains(strings.ToLower(name), "admin") {
			return name
		}
		if firstTask == "" {
			firstTask = name
		}
	}
	return firstTask
}

func startHint(v *VPN) string {
	switch {
	case v.Task != "":
		return "запустить задачу «" + v.Task + "»"
	case v.Exe != "":
		return "запустить " + filepath.Base(v.Exe)
	default:
		return ""
	}
}
