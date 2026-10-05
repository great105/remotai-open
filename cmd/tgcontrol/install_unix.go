//go:build !windows

package main

import (
	"log"
	"os"
	"tgcontrol/internal/localize"

	"tgcontrol/internal/bundle"
)

// ensureInstalled — самоустановка бинаря актуальна только на Windows, а папка
// ~/Remotai (files + встроенные скиллы) нужна везде: создаём и на Linux при
// каждом старте агента (у серверов это обычно и есть единственный «install»).
func ensureInstalled() {
	if err := bundle.Ensure(); err != nil {
		log.Printf(localize.Text("[INSTALL] папка ~/Remotai: %v"), err)
	}
}

// isRoot — euid 0 (выбор system/user-режима в `remotai install`).
func isRoot() bool { return os.Geteuid() == 0 }
