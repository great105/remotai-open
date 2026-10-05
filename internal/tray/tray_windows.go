//go:build windows

package tray

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"fyne.io/systray"

	"tgcontrol/internal/paths"
)

type Options struct {
	Port           int
	RelayStatus    func() (configured, connected bool) // состояние облака для пункта статуса (nil → «Нет облака»)
	SetupMode      bool
	BindError      string
	OnQuit         func()
	OnOpen         func() // open the native window (nil → tray opens a browser)
	OnTerminals    func() // open the local client (/miniapp) — terminals, files, screen
	OnHostTerminal func() // open a host terminal window attached to a new session
}

// Run shows the tray icon and blocks until the user picks "Выйти"
// or the context is cancelled. Must be called from the goroutine
// that should host the systray message loop (typically main).
func Run(ctx context.Context, opts Options) {
	onReady := func() {
		systray.SetIcon(IconICO())
		systray.SetTitle("Remotai")
		systray.SetTooltip(fmt.Sprintf("Remotai запущен на :%d", opts.Port))

		// Статус отражает реальное состояние облака (relay), а не просто
		// «процесс жив»: телефон видит ПК только при connected.
		statusText := func() string {
			if opts.BindError != "" {
				return fmt.Sprintf("⛔ Порт занят  •  :%d", opts.Port)
			}
			if opts.SetupMode {
				return fmt.Sprintf("🟡 Требуется настройка  •  :%d", opts.Port)
			}
			if opts.RelayStatus != nil {
				configured, connected := opts.RelayStatus()
				if configured && connected {
					return fmt.Sprintf("🟢 В сети  •  :%d", opts.Port)
				}
				if configured {
					return fmt.Sprintf("🟡 Нет связи с облаком  •  :%d", opts.Port)
				}
			}
			return fmt.Sprintf("🟡 Нет облака  •  :%d", opts.Port)
		}

		mStatus := systray.AddMenuItem(statusText(), "")
		mStatus.Disable()
		systray.AddSeparator()
		mOpen := systray.AddMenuItem("Открыть окно", "Открыть Remotai")
		mTerm := systray.AddMenuItem("Терминалы", "Терминалы, файлы и экран этого компьютера")
		mHostTerm := systray.AddMenuItem("Новый терминал на ПК", "Окно терминала, сразу видное с телефона")
		mFolder := systray.AddMenuItem("Папка проекта", "Открыть в проводнике")
		mLog := systray.AddMenuItem("Открыть лог", "remotai.log")
		systray.AddSeparator()
		mRestart := systray.AddMenuItem("Перезапустить", "")
		mQuit := systray.AddMenuItem("Выйти", "Завершить Remotai")
		if opts.SetupMode {
			mTerm.Disable()
			mHostTerm.Disable()
		}

		dir := projectDir()
		url := fmt.Sprintf("http://localhost:%d/", opts.Port)
		// Реальный лог — в папке данных приложения (см. setupLogging в main.go),
		// а не рядом с exe.
		logPath := filepath.Join(paths.Base(), "remotai.log")

		// Периодически обновляем строку статуса — облако может отвалиться
		// и подняться без участия пользователя.
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					mStatus.SetTitle(statusText())
				}
			}
		}()

		go func() {
			for {
				select {
				case <-ctx.Done():
					systray.Quit()
					return
				case <-mOpen.ClickedCh:
					if opts.OnOpen != nil {
						opts.OnOpen()
					} else {
						shellOpen(url)
					}
				case <-mTerm.ClickedCh:
					if opts.OnTerminals != nil {
						opts.OnTerminals()
					} else {
						shellOpen(fmt.Sprintf("http://localhost:%d/miniapp", opts.Port))
					}
				case <-mHostTerm.ClickedCh:
					if opts.OnHostTerminal != nil {
						opts.OnHostTerminal()
					}
				case <-mFolder.ClickedCh:
					shellOpen(dir)
				case <-mLog.ClickedCh:
					shellOpen(logPath)
				case <-mRestart.ClickedCh:
					if err := spawnSelfDelayed(); err != nil {
						log.Printf("tray: restart spawn failed: %v", err)
					}
					systray.Quit()
				case <-mQuit.ClickedCh:
					if opts.OnQuit != nil {
						opts.OnQuit()
					}
					systray.Quit()
				}
			}
		}()
	}

	onExit := func() {}

	systray.Run(onReady, onExit)
}

func projectDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func shellOpen(target string) {
	cmd := exec.Command("cmd", "/C", "start", "", target)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	_ = cmd.Start()
}

func spawnSelfDelayed() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Wait 2s for current process to exit, then relaunch the same binary detached.
	cmd := exec.Command("cmd", "/C", "timeout", "/T", "2", "/NOBREAK", ">", "NUL", "&", "start", "", exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd.Start()
}
