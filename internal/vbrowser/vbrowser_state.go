package vbrowser

import (
	"os"
	"path/filepath"

	"tgcontrol/internal/paths"
)

// UploadFile — уже загруженный на агент файл, который нужно передать
// перехваченному <input type=file>. Name — имя, которое увидит сайт; Path —
// временный путь агента, никогда не отправляемый в браузерный интерфейс.
type UploadFile struct {
	Path string
	Name string
}

// StateDir — базовый каталог состояния виртуального браузера (профиль,
// загрузки). От root — /var/lib/remotai-vb: браузер идёт под отдельным
// пользователем, а в /root (0700) он даже не пройдёт по пути. Без build-тега:
// каталог загрузок читает и web-пакет на любой ОС.
func StateDir() string {
	if os.Geteuid() == 0 {
		return "/var/lib/remotai-vb"
	}
	return filepath.Dir(paths.StateFile("vbrowser-profile"))
}
