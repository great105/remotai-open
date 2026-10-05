//go:build darwin

package main

import "tgcontrol/internal/service"

// restartOwnService — перезапуск LaunchAgent после привязки.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ: `service.Restart()` объявлен только в
// `service_darwin.go` — на Linux и Windows такой функции нет, и общий вызов
// ломал бы сборку под них. Тег сборки решает это честнее, чем заводить пустые
// заглушки в чужом пакете.
//
// ЗАЧЕМ ВООБЩЕ: `remotai pair` пишет привязку в конфиг, а на связь с облаком
// выходит СЛУЖБА — другой процесс, прочитавший конфиг при старте. На маке
// install.sh поднимает её ДО привязки, поэтому без перезапуска компьютер так и
// остаётся «не в сети». Ровно это увидел первый живой мак 09.08.2026: «✅
// Сервер привязан!» в терминале и «не в сети» на телефоне.
func restartOwnService() (running bool, err error) {
	if !service.IsRunning() {
		return false, nil
	}
	return true, service.Restart()
}
