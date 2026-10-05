//go:build !windows

package netwatch

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/shirou/gopsutil/v4/process"
)

// На сервере тот же сторож полезен ровно так же: sing-box/hiddify-cli умеет
// увести весь трафик в мёртвый туннель и там же оставить. Разница только в
// способе включить обратно — планировщика тут нет, зато почти всегда есть
// systemd-юнит.
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
		v.Task = findVPNUnit(name)
		v.StartHint = startHint(v)
		return v
	}
	return nil
}

func DetectVPNTarget() *VPN { return DetectVPN() }

func StopVPN(v *VPN) error {
	if v == nil || v.PID == 0 {
		return ErrNoVPN
	}
	defer invalidateDetect()
	if v.Task != "" {
		// Юнит останавливаем целиком: убитый процесс systemd поднимет обратно
		// через Restart=, и выключение окажется фикцией.
		if err := exec.Command("systemctl", "stop", v.Task).Run(); err == nil {
			return nil
		}
	}
	p, err := os.FindProcess(v.PID)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}

func StartVPN(v *VPN) (string, error) {
	if v == nil {
		return "", ErrNoVPN
	}
	defer invalidateDetect()
	if v.Task != "" {
		if err := exec.Command("systemctl", "start", v.Task).Run(); err != nil {
			return "", err
		}
		return "systemctl start " + v.Task, nil
	}
	if v.Exe == "" {
		return "", errors.New("не знаю, чем запускать " + v.Name)
	}
	cmd := exec.Command(v.Exe)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { _ = cmd.Wait() }()
	return v.Exe, nil
}

// findVPNUnit — есть ли systemd-юнит с таким именем (sing-box.service и т.п.).
func findVPNUnit(procName string) string {
	unit := strings.TrimSuffix(procName, ".exe") + ".service"
	out, err := exec.Command("systemctl", "is-enabled", unit).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "disabled") {
		return ""
	}
	return unit
}

func startHint(v *VPN) string {
	switch {
	case v.Task != "":
		return "systemctl start " + v.Task
	case v.Exe != "":
		return "запустить " + v.Exe
	default:
		return ""
	}
}
