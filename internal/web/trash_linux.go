//go:build linux

package web

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Корзина freedesktop.org (Trash spec 1.0): файл переезжает в
// $XDG_DATA_HOME/Trash/files, а рядом в info/ ложится <имя>.trashinfo с
// исходным путём и датой. Именно это читают «Корзина» GNOME/KDE и
// gio trash — своя папка «удалённых» им не видна, поэтому спецификацию
// соблюдаем буквально.

// homeTrashDir — корзина домашнего каталога. Пусто, если дома нет вовсе
// (headless-сервис без HOME) — тогда удаляем без корзины и говорим правду.
func homeTrashDir() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(dataHome) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "Trash")
}

// mountPointOf — корень тома, на котором лежит путь: поднимаемся вверх, пока
// не сменится устройство. Нужен для корзины «чужого» диска ($top/.Trash-uid):
// перенести файл в домашнюю корзину через границу ФС нельзя, а копировать
// гигабайты ради удаления — не то, чего человек просил.
func mountPointOf(p string) string {
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return ""
	}
	dev := st.Dev
	cur := p
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur
		}
		var pst syscall.Stat_t
		if err := syscall.Lstat(parent, &pst); err != nil || pst.Dev != dev {
			return cur
		}
		cur = parent
	}
}

func moveToTrash(path string) (trashResult, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return trashResult{}, err
	}
	home := homeTrashDir()
	if home != "" {
		res, err := trashInto(home, abs, "")
		if err == nil {
			return res, nil
		}
		// Не «другой том» — значит корзина недоступна как таковая (нет прав,
		// нет места): выше это станет честным «удалено безвозвратно».
		if !errors.Is(err, syscall.EXDEV) {
			return trashResult{}, err
		}
	}
	top := mountPointOf(abs)
	if top == "" {
		return trashResult{}, errTrashUnsupported
	}
	// Путь внутри такой корзины спецификация требует писать ОТНОСИТЕЛЬНО
	// корня тома — иначе после перемонтирования восстанавливать некуда.
	return trashInto(filepath.Join(top, ".Trash-"+strconv.Itoa(os.Getuid())), abs, top)
}

// trashInto кладёт файл в конкретную корзину. Имя внутри files/ занимаем
// созданием .trashinfo с O_EXCL: спецификация требует, чтобы два процесса,
// удаляющих однофамильцев одновременно, не затёрли друг друга.
func trashInto(trashDir, abs, topdir string) (trashResult, error) {
	filesDir := filepath.Join(trashDir, "files")
	infoDir := filepath.Join(trashDir, "info")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return trashResult{}, err
	}
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		return trashResult{}, err
	}

	recorded := abs
	if topdir != "" {
		if rel, err := filepath.Rel(topdir, abs); err == nil {
			recorded = rel
		}
	}
	base := filepath.Base(abs)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	for n := 1; n < 1000; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s.%d%s", stem, n, ext)
		}
		infoPath := filepath.Join(infoDir, name+".trashinfo")
		f, err := os.OpenFile(infoPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return trashResult{}, err
		}
		// Path — percent-encoded, но со СЛЕШАМИ: url.PathEscape съел бы и их.
		_, werr := fmt.Fprintf(f, "[Trash Info]\nPath=%s\nDeletionDate=%s\n",
			(&url.URL{Path: recorded}).EscapedPath(),
			time.Now().Format("2006-01-02T15:04:05"))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		dst := filepath.Join(filesDir, name)
		if werr == nil {
			// files/ может содержать имя без info (битая корзина) — перезаписать
			// его значило бы уничтожить чужое удалённое.
			if _, serr := os.Lstat(dst); serr == nil {
				werr = fs.ErrExist
			}
		}
		if werr == nil {
			werr = os.Rename(abs, dst)
		}
		if werr != nil {
			_ = os.Remove(infoPath)
			if errors.Is(werr, fs.ErrExist) {
				continue
			}
			return trashResult{}, werr
		}
		return trashResult{Kind: "freedesktop", RestorePath: dst}, nil
	}
	return trashResult{}, errTrashUnsupported
}

// forgetTrashEntry вызывается после того, как файл ВЕРНУЛИ из корзины обычным
// перемещением («Отменить» в тосте). Без удаления .trashinfo корзина рабочего
// стола показывала бы призрак — запись без файла.
func forgetTrashEntry(path string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	dir := filepath.Dir(abs)
	if filepath.Base(dir) != "files" {
		return
	}
	trashDir := filepath.Dir(dir)
	if base := filepath.Base(trashDir); base != "Trash" && !strings.HasPrefix(base, ".Trash") {
		return
	}
	_ = os.Remove(filepath.Join(trashDir, "info", filepath.Base(abs)+".trashinfo"))
}
