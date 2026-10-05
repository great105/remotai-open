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

	"tgcontrol/internal/desktopentry"
	"tgcontrol/internal/service"
)

func installDarwin(opts installOptions) int {
	if os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "remotai install: на macOS не запускайте под sudo —")
		fmt.Fprintln(os.Stderr, "  агент ставится в вашу сессию (LaunchAgent), иначе он не увидит")
		fmt.Fprintln(os.Stderr, "  ваши терминалы, ключи и установленные инструменты.")
		return 1
	}

	if err := service.Install(); err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: не удалось настроить автозапуск: %v\n", err)
		return 1
	}
	fmt.Println("✓ Автозапуск настроен: LaunchAgent ru.remotai.agent")
	fmt.Println("  запускается при входе в систему, поднимается сам после сбоя")
	fmt.Println("  логи: ~/Library/Logs/Remotai/")
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	entry, err := desktopentry.Install(exe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai install: не удалось добавить приложение: %v\n", err)
		return 1
	}
	fmt.Printf("✓ Приложение: %s\n", entry)
	fmt.Println("  Откройте Remotai в папке «Программы» своего пользователя. Интерфейс откроется в браузере.")

	if opts.noPair {
		fmt.Println()
		fmt.Println("Привязка пропущена (--no-pair). Когда будете готовы:")
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
	fmt.Println("✓ Автозапуск снят (LaunchAgent удалён)")
	if err := desktopentry.Remove(); err != nil {
		fmt.Fprintf(os.Stderr, "remotai uninstall: ярлык приложения: %v\n", err)
		return 1
	}
	if purge {
		_ = runUnpair(nil)
		fmt.Println("✓ Привязка к аккаунту удалена")
	}
	return 0
}
