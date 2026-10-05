package main

// `remotai update` — обновление агента на сервере одной командой.
//
// Зачем отдельная команда: на сервере нет ни окна с кнопкой «Обновить», ни
// панели. Единственным путём была переустановка через install.sh — то есть
// «скачайте скрипт из интернета и выполните», причём после неё служба всё
// равно работала со старым бинарём в памяти, пока её не перезапустят руками
// (та же грабля, что и с привязкой: см. pair.go). Здесь весь путь целиком:
// проверить манифест → скачать бинарь под ЭТУ платформу → заменить →
// перезапустить службу.

import (
	"fmt"
	"os"
	"runtime"

	"tgcontrol/internal/update"
	"tgcontrol/internal/version"
)

func runUpdate(args []string) int {
	// `remotai update --check` — только посмотреть, ничего не трогая.
	checkOnly := false
	for _, a := range args {
		if a == "--check" || a == "-c" {
			checkOnly = true
		}
	}

	fmt.Printf("Установлена версия %s, смотрю обновления…\n", version.Version)
	info, err := version.CheckForUpdate("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai: не удалось проверить обновления: %v\n", err)
		return 1
	}
	if !info.Available {
		fmt.Printf("✅ Это последняя версия (%s на сервере обновлений).\n", info.Version)
		return 0
	}
	fmt.Printf("Доступна %s.\n", info.Version)
	if checkOnly {
		return 0
	}
	// Пустой URL здесь означает ровно одно: под эту ОС и архитектуру сборки в
	// манифесте нет (см. version.CheckForUpdate — чужой бинарь не подставляем).
	if info.DownloadURL == "" {
		fmt.Fprintln(os.Stderr, "remotai: для этой системы сборки пока нет — обновите вручную.")
		return 1
	}

	fmt.Println("Скачиваю и заменяю программу…")
	if err := update.Apply(info.DownloadURL, info.SHA256); err != nil {
		fmt.Fprintf(os.Stderr, "remotai: обновление не применилось: %v\n", err)
		// Права — самая частая причина на сервере: бинарь лежит в /usr/local/bin.
		if os.Geteuid() != 0 {
			fmt.Fprintln(os.Stderr, "   Похоже, не хватило прав — повторите: sudo remotai update")
		}
		return 1
	}
	fmt.Printf("✅ Обновлено до %s.\n", info.Version)

	// Новый бинарь на диске, а в памяти по-прежнему работает старый: без
	// перезапуска службы версия в приложении не изменится, и человек решит,
	// что обновление не сработало.
	switch restartServiceIfRunning() {
	case serviceRestarted:
		fmt.Println("   Служба перезапущена — новая версия уже работает.")
	case serviceRestartFailed:
		fmt.Println("   Осталось перезапустить службу:")
		fmt.Println("     " + serviceRestartHint())
	default:
		// Совет даём тот, что работает НА ЭТОЙ системе: на маке про systemctl
		// говорить нельзя (его там нет), а на Windows службой управляет само
		// приложение — см. serviceStartHint.
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			fmt.Println("   Служба не запущена. Запустить: " + serviceStartHint())
		} else {
			fmt.Println("   Перезапустите Remotai, чтобы новая версия заработала.")
		}
	}
	return 0
}
