// Package bundle — видимая пользователю папка Remotai в домашней директории.
//
// Зачем: человеку и агентам нужно предсказуемое место на диске — куда падают
// загрузки с телефона (files/) и где лежат встроенные скиллы для AI-агентов
// (skills/). Папка создаётся при установке и при каждом старте агента, поэтому
// она появляется и у давних пользователей после автообновления, а удалённую
// можно просто не восстанавливать руками — Ensure идемпотентен.
//
// Скиллы встроены в бинарь (go:embed) и перезаписываются при обновлении:
// это «прошивка», а не пользовательские данные (README папки честно об этом
// предупреждает). Файлы пользователя в files/ не трогаем никогда.
package bundle

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed skills
var embedded embed.FS

// Dir — корень пользовательской папки: ~/Remotai.
func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "Remotai")
}

// FilesDir — куда падают загрузки из приложения и файлы для агентов.
func FilesDir() string {
	d := Dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "files")
}

// Ensure создаёт дерево ~/Remotai и раскладывает встроенные скиллы.
// Скилл перезаписывается, если его содержимое отличается от встроенного —
// так обновления доезжают до существующих установок.
func Ensure() error {
	root := Dir()
	if root == "" {
		return nil // без домашней директории просто живём без папки
	}
	if err := os.MkdirAll(FilesDir(), 0o755); err != nil {
		return err
	}
	return fs.WalkDir(embedded, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := embedded.ReadFile(path)
		if err != nil {
			return err
		}
		if existing, err := os.ReadFile(target); err == nil && string(existing) == string(content) {
			return nil // уже актуальный — не трогаем (и mtime цел)
		}
		return os.WriteFile(target, content, 0o644)
	})
}
