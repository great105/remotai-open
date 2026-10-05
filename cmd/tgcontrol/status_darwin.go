//go:build darwin

package main

import (
	"tgcontrol/internal/localize"
	"tgcontrol/internal/service"
)

// cliAutostartState на macOS — это LaunchAgent, а не systemd.
//
// ЖИВОЙ СЛУЧАЙ 10.08.2026. `remotai doctor` на маке уверенно печатал
// «⚠️ Автозапуск выключен — после перезагрузки компьютер не появится сам».
// Проверка лежала в `status_other.go` с тегом `!windows`, то есть отвечала и за
// macOS, а искала строго два файла:
//
//	/etc/systemd/system/remotai.service
//	~/.config/systemd/user/remotai.service
//
// На маке их не бывает НИКОГДА, поэтому ответ был всегда один и тот же —
// «выключен, не настроен». Диагностика, которая врёт про собственную установку,
// хуже её отсутствия: человек идёт чинить то, что не сломано, а настоящая
// причина остаётся ненайденной.
//
// service.IsInstalled() смотрит ровно туда, куда мы сами кладём plist
// (`~/Library/LaunchAgents/ru.remotai.agent.plist`), а IsRunning() спрашивает
// launchd о живой службе — этого и достаточно.
func cliAutostartState() (bool, string) {
	if !service.IsInstalled() {
		return false, localize.Text("не настроен")
	}
	if service.IsRunning() {
		return true, "LaunchAgent ru.remotai.agent"
	}
	// plist есть, но launchd службу не держит: для человека это всё ещё
	// «автозапуск настроен», просто сейчас не работает — про сам процесс
	// doctor говорит отдельной строкой.
	return true, localize.Text("LaunchAgent ru.remotai.agent (сейчас не запущен)")
}

// cleanupInstalledArtifacts — на маке чистить нечего: бинарь лежит в
// ~/.local/bin, а plist снимает `service.Uninstall()`.
func cleanupInstalledArtifacts() error { return nil }
