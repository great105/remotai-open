//go:build darwin

// `remotai install` и `remotai uninstall` на macOS.
//
// Отличие от Linux: там выбор между system-юнитом и user-юнитом (и целая
// история с linger), здесь вариант один — пользовательский LaunchAgent. Он
// стартует при входе в систему, работает от имени человека и видит его
// терминалы, ключи и PATH. Root тут не нужен и вреден.
package main

import (
	"fmt"
	"os"
	"tgcontrol/internal/localize"

	"tgcontrol/internal/desktopentry"
	"tgcontrol/internal/service"
)

func installDarwin(opts installOptions) int {
	if os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, localize.Text("remotai install: на macOS не запускайте под sudo —"))
		fmt.Fprintln(os.Stderr, localize.Text("  агент ставится в вашу сессию (LaunchAgent), иначе он не увидит"))
		fmt.Fprintln(os.Stderr, localize.Text("  ваши терминалы, ключи и установленные инструменты."))
		return 1
	}

	if err := service.Install(); err != nil {
		fmt.Fprintf(os.Stderr, localize.Text("remotai install: не удалось настроить автозапуск: %v\n"), err)
		return 1
	}
	fmt.Println(localize.Text("✓ Автозапуск настроен: LaunchAgent ru.remotai.agent"))
	fmt.Println(localize.Text("  запускается при входе в систему, поднимается сам после сбоя"))
	fmt.Println(localize.Text("  логи: ~/Library/Logs/Remotai/"))
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	entry, err := desktopentry.Install(exe)
	if err != nil {
		fmt.Fprintf(os.Stderr, localize.Text("remotai install: не удалось добавить приложение: %v\n"), err)
		return 1
	}
	fmt.Printf(localize.Text("✓ Приложение: %s\n"), entry)
	fmt.Println(localize.Text("  Откройте Remotai в папке «Программы» своего пользователя. Интерфейс откроется в браузере."))

	if opts.noPair {
		fmt.Println()
		fmt.Println(localize.Text("Привязка пропущена (--no-pair). Когда будете готовы:"))
		fmt.Println("  remotai pair")
		return 0
	}
	return runPair(nil)
}

func uninstallDarwin(purge bool) int {
	if err := service.Uninstall(); err != nil {
		fmt.Fprintf(os.Stderr, "remotai uninstall: %v\n", err)
		return 1
	}
	fmt.Println(localize.Text("✓ Автозапуск снят (LaunchAgent удалён)"))
	if err := desktopentry.Remove(); err != nil {
		fmt.Fprintf(os.Stderr, localize.Text("remotai uninstall: ярлык приложения: %v\n"), err)
		return 1
	}
	if purge {
		_ = runUnpair(nil)
		fmt.Println(localize.Text("✓ Привязка к аккаунту удалена"))
	}
	return 0
}
