//go:build windows

package service

import (
	"fmt"
	"log"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceName = "TGControl"
const serviceDisplayName = "TGControl Remote Control"
const serviceDescription = "Telegram Mini App for remote PC control"

// tgControlService implements svc.Handler.
type tgControlService struct {
	stopCh chan struct{}
}

// Execute is the Windows service main loop.
func (s *tgControlService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				close(s.stopCh)
				return false, 0
			}
		}
	}
}

// Install registers TGControl as a Windows service.
func Install() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", serviceName)
	}

	// StartAutomatic: if the user explicitly chose the service mode,
	// they expect it to come up on boot without manual intervention.
	// (Default install path is Scheduled Task in user session — see installer/setup.iss.)
	s, err = m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// Set recovery: restart on failure after 60 seconds
	err = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400) // Reset failure count after 24h
	if err != nil {
		log.Printf("Warning: could not set recovery actions: %v", err)
	}

	fmt.Printf("Service %s installed successfully.\n", serviceName)
	return nil
}

// Uninstall removes the TGControl Windows service.
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()

	// Try to stop the service first
	s.Control(svc.Stop)
	time.Sleep(2 * time.Second)

	err = s.Delete()
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
	}

	fmt.Printf("Service %s removed.\n", serviceName)
	return nil
}

// Start starts the TGControl Windows service.
func Start() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()

	return s.Start()
}

// Stop stops the TGControl Windows service.
func Stop() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()

	_, err = s.Control(svc.Stop)
	return err
}

// IsInstalled checks if the service is registered.
func IsInstalled() bool {
	m, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return false
	}
	s.Close()
	return true
}

// IsRunning checks if the service is currently running.
func IsRunning() bool {
	m, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return false
	}
	defer s.Close()

	status, err := s.Query()
	if err != nil {
		return false
	}
	return status.State == svc.Running
}

// RunAsService returns true if the current process was launched as a Windows service.
func RunAsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return isService
}

// UnderSystemd is always false on Windows (systemd is a Linux init system).
// The Windows service path is detected via RunAsService instead.
func UnderSystemd() bool { return false }

// ManagerCanRestart — просить SCM о перезапуске мы не умеем и не начинаем:
// поведение службы на Windows отлажено и трогать его по следам macOS-дефекта
// незачем. Обновление здесь по-прежнему вступает в силу при следующем старте
// службы.
func ManagerCanRestart() bool { return false }

// RestartByManager — см. ManagerCanRestart.
func RestartByManager() error {
	return fmt.Errorf("перезапуск службы Windows по просьбе агента не поддерживается")
}

// Run starts the main application loop as a Windows service.
// The appFunc is called in a goroutine; when the service receives a stop signal,
// the stopCh is closed, and appFunc should exit gracefully.
func Run(appFunc func(stopCh <-chan struct{})) error {
	s := &tgControlService{stopCh: make(chan struct{})}
	go appFunc(s.stopCh)
	return svc.Run(serviceName, s)
}
