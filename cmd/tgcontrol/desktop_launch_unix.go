//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"tgcontrol/internal/localize"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/desktopinstall"
	"tgcontrol/internal/procutil"
	"tgcontrol/internal/version"
)

func launchPackagedDesktop() bool {
	if runtime.GOOS == "darwin" && len(os.Args) == 2 && os.Args[1] == "--desktop-window" {
		result := prepareDesktopWindow()
		// Private pipe to the native window, never a URL in process arguments.
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return true
	}
	exe, err := os.Executable()
	if err != nil || !desktopinstall.IsPackageLaunch(exe, os.Args[1:], runtime.GOOS) {
		return false
	}
	home, err := os.UserHomeDir()
	if err == nil && os.Geteuid() == 0 {
		err = fmt.Errorf("%s", localize.Text("Откройте Remotai из меню приложений обычного пользователя"))
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var installed string
		installed, err = desktopinstall.Install(ctx, exe, home, version.Version)
		if err == nil {
			if pending := desktopinstall.PendingActivation(ctx, installed, config.GetNoSetup().Port()); pending != nil {
				openBrowser(pending.PanelURL)
				message := fmt.Sprintf(localize.Text("Установлена версия %s, но сейчас работает прежняя версия %s.\n\nВ открытой панели прокрутите вниз до строки «Версия», нажмите «проверить» → «Обновить». Remotai обновится и перезапустится сам.\n\nТакже можно нажать «Завершить Remotai на этом компьютере…», затем снова открыть приложение. Закрытие вкладки браузера не завершает Remotai."), pending.InstalledVersion, pending.RunningVersion)
				fmt.Fprintln(os.Stderr, "Remotai:", message)
				showDesktopDialog(localize.Text("Обновление Remotai ещё не запущено"), message, false)
				return true
			}
			err = desktopinstall.Start(installed)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Remotai:", err)
		showDesktopInstallError(err.Error())
	}
	return true
}

func prepareDesktopWindow() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fail := func(err error) map[string]string { return map[string]string{"error": err.Error()} }
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	if os.Geteuid() == 0 {
		return fail(fmt.Errorf("%s", localize.Text("Откройте Remotai из «Программ» обычного пользователя")))
	}
	installed, err := desktopinstall.Install(ctx, exe, home, version.Version)
	if err != nil {
		return fail(err)
	}
	pending := desktopinstall.PendingActivation(ctx, installed, config.GetNoSetup().Port())
	if pending == nil {
		if err := desktopinstall.Start(installed, "--background"); err != nil {
			return fail(err)
		}
	}
	for ctx.Err() == nil {
		// The agent can choose a free port during startup; follow its saved port.
		port := config.Reload().Port()
		url, err := desktopinstall.WindowURL(ctx, port)
		if err == nil {
			result := map[string]string{"url": url}
			if pending != nil {
				result["url"] = pending.PanelURL
				result["notice"] = fmt.Sprintf(localize.Text("Установлена версия %s, но работает %s. Внизу панели нажмите «проверить» → «Обновить». Если обновление не завершится, выберите «Завершить Remotai на этом компьютере…» и снова откройте приложение. Закрытие окна сохраняет фоновую работу Remotai."), pending.InstalledVersion, pending.RunningVersion)
			}
			return result
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fail(fmt.Errorf("%s", localize.Text("Remotai не успел запуститься. Нажмите «Повторить»; работающие терминалы сохранятся")))
}

func showDesktopInstallError(message string) {
	showDesktopDialog(localize.Text("Не удалось открыть Remotai"), message, true)
}

func showDesktopDialog(title, message string, isError bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		style := "informational"
		if isError {
			style = "critical"
		}
		cmd = exec.CommandContext(ctx, "/usr/bin/osascript", "-e", "on run argv", "-e", `display alert (item 1 of argv) message (item 2 of argv) as `+style, "-e", "end run", title, message)
	} else if path, err := exec.LookPath("zenity"); err == nil {
		kind := "--info"
		if isError {
			kind = "--error"
		}
		cmd = exec.CommandContext(ctx, path, kind, "--title="+title, "--text="+message, "--no-markup")
	} else if path, err := exec.LookPath("kdialog"); err == nil {
		kind := "--msgbox"
		if isError {
			kind = "--error"
		}
		cmd = exec.CommandContext(ctx, path, kind, message, "--title", title)
	}
	if cmd != nil {
		procutil.Hidden(cmd)
		_ = cmd.Run()
	}
}
