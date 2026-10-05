package main

import (
	"os"
	"regexp"
	"testing"
)

// В режим службы Windows (SCM) уходим ТОЛЬКО на Windows.
//
// ЖИВОЙ МАК 10.08.2026 — корень «привязался, но не в сети». `RunAsService()` на
// маке отвечает true законно (launchd выставляет XPC_SERVICE_NAME, и признак
// нужен автообновлению), но здесь он читался как «иди в SCM-режим». А
// `service.Run` на darwin всегда возвращает ошибку, и `log.Fatalf` убивал
// процесс. launchd поднимал его снова — в логе ровный цикл каждые 10 секунд:
//
//	Running as Windows service
//	Service error: на macOS агент запускается launchd как обычный процесс
//
// Проверяем исходник, а не поведение: сам режим воспроизводится только под
// настоящим SCM, а цена ошибки — агент, не работающий на целой платформе.
func TestServiceModeOnlyOnWindows(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	// Ищем условие входа в SCM-ветку: перед RunAsService() обязана стоять
	// проверка платформы.
	guard := regexp.MustCompile(`if runtime\.GOOS == "windows" && service\.RunAsService\(\)`)
	if !guard.Match(src) {
		t.Fatal("вход в режим службы Windows не ограничен платформой: на macOS это бесконечный цикл падений (см. комментарий у теста)")
	}
	bare := regexp.MustCompile(`(?m)^\s*if service\.RunAsService\(\) \{`)
	if bare.Match(src) {
		t.Fatal("остался безусловный вход в режим службы по RunAsService()")
	}
}
