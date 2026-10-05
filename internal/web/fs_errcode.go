package web

// Машинные коды отказов файловых операций (находка #42).
//
// До этого любой сбой ФС уезжал клиенту как jsonError("Failed to delete file",
// 500) — сырой английский текст без единого признака, по которому клиент мог бы
// ветвиться. Правило проекта: клиент ветвится по машинному code, а не по
// HTTP-статусу и не по тексту. Здесь — один распознаватель ошибок os/io на все
// файловые хендлеры (локальные; SFTP-хендлеры могут переиспользовать fsStatus и
// сами коды, добавив свой разбор *sftp.StatusError).
//
// ГРАБЛЯ Windows: os.IsPermission/fs.ErrPermission покрывает НЕ всё — «файл
// открыт другой программой» приходит как ERROR_SHARING_VIOLATION(32), а не как
// ERROR_ACCESS_DENIED(5). Поэтому платформенный доводчик platformFSCode
// вызывается ПЕРЕД проверкой fs.ErrPermission (иначе file_in_use схлопнется в
// no_permission). Вторая грабля: на Windows Go объявляет syscall.ENOSPC/ETXTBSY
// синтетическими значениями (APPLICATION_ERROR+iota), ОС их не возвращает
// никогда — сравнивать можно только с настоящими кодами Win32 (см.
// fs_errcode_windows.go).

import (
	"errors"
	"io/fs"
	"log"
	"net/http"
)

const (
	fsCodePermission = "no_permission"
	fsCodeInUse      = "file_in_use"
	fsCodeDiskFull   = "disk_full"
	fsCodeNotEmpty   = "not_empty"
	fsCodeReadOnly   = "read_only"
	fsCodeTooLarge   = "too_large"
	fsCodeOutside    = "outside_roots"
	fsCodeBadPath    = "bad_path"
	fsCodeNotFound   = "not_found"
	fsCodeExists     = "already_exists"
	fsCodeIsDir      = "is_dir"
	fsCodeNotDir     = "not_a_dir"
	fsCodeBadRange   = "bad_range"
	fsCodeChanged    = "file_changed"
	fsCodeIO         = "io_error"
)

// fsErrorCode превращает ошибку os/io в машинный код и HTTP-статус.
func fsErrorCode(err error) (code string, status int) {
	switch {
	case err == nil:
		return "", http.StatusOK
	case errors.Is(err, fs.ErrNotExist):
		return fsCodeNotFound, fsStatus(fsCodeNotFound)
	case errors.Is(err, fs.ErrExist):
		return fsCodeExists, fsStatus(fsCodeExists)
	}
	// Платформенный разбор — ДО fs.ErrPermission (см. комментарий в шапке).
	if c := platformFSCode(err); c != "" {
		return c, fsStatus(c)
	}
	if errors.Is(err, fs.ErrPermission) {
		return fsCodePermission, fsStatus(fsCodePermission)
	}
	return fsCodeIO, fsStatus(fsCodeIO)
}

// fsStatus — HTTP-статус для машинного кода. 403/409 выбраны так, чтобы клиент
// НЕ выкидывал пользователя на экран входа: правило v2.29.0 — выход на логин
// только при 401/403 БЕЗ code, а у нас код есть всегда.
func fsStatus(code string) int {
	switch code {
	case fsCodePermission, fsCodeOutside:
		return http.StatusForbidden // 403
	case fsCodeInUse, fsCodeNotEmpty, fsCodeExists, fsCodeReadOnly, fsCodeChanged:
		return http.StatusConflict // 409
	case fsCodeDiskFull:
		return http.StatusInsufficientStorage // 507
	case fsCodeTooLarge:
		return http.StatusRequestEntityTooLarge // 413
	case fsCodeNotFound:
		return http.StatusNotFound // 404
	case fsCodeBadPath, fsCodeIsDir, fsCodeNotDir:
		return http.StatusBadRequest // 400
	case fsCodeBadRange:
		return http.StatusRequestedRangeNotSatisfiable // 416
	}
	return http.StatusInternalServerError
}

// jsonFSError — единая точка ответа на ошибку ФС. msg остаётся английским (он
// для логов и старых клиентов), человеческий текст рисует клиент по code.
func jsonFSError(w http.ResponseWriter, err error, op string) {
	jsonFSErrorExtra(w, err, op, nil)
}

// jsonFSErrorExtra — то же с дополнительными полями (например, сколько успели
// скопировать до отказа).
func jsonFSErrorExtra(w http.ResponseWriter, err error, op string, extra map[string]string) {
	code, status := fsErrorCode(err)
	log.Printf("[FILES] %s: %v (code=%s)", op, err, code)
	msg := op
	if err != nil {
		msg = op + ": " + err.Error()
	}
	jsonErrorCode(w, status, code, msg, extra)
}

// validPathOrFail прогоняет путь через validatePath и, если путь не прошёл,
// сам пишет ответ с машинным кодом (outside_roots / bad_path). Заменяет десяток
// копий jsonError(w, err.Error(), 403) — именно они выкидывали облачного
// пользователя на экран входа (403 без code).
func validPathOrFail(w http.ResponseWriter, raw string) (string, bool) {
	safe, err := validatePath(raw)
	if err == nil {
		return safe, true
	}
	code := fsCodeOutside
	if errors.Is(err, errPathRequired) || errors.Is(err, errPathInvalid) {
		code = fsCodeBadPath
	}
	jsonErrorCode(w, fsStatus(code), code, err.Error(), nil)
	return "", false
}
