package web

// Chunked upload assembly.
//
// Cloud-режим проксирует тело запроса через relay в base64-JSON конверте, то
// есть весь файл раньше жил в памяти релея (и упирался в лимит конверта). При
// многих клиентах это не масштабируется. Поэтому клиент режет большие файлы
// на куски (~12 МБ) и шлёт их последовательно с query upload_id/chunk/chunks.
// Релей в каждый момент держит в памяти только один кусок и остаётся чистым
// передатчиком — файлы на сервере не хранятся.
//
// Куски складываются в .part-файл во временной папке агента, последний кусок
// переименовывает его в итоговое имя. В ответе на КАЖДЫЙ кусок есть
// chunk_ack: по нему клиент понимает, что агент знает протокол (старому
// агенту клиент большие файлы не отправляет — fail fast после первого куска).

import (
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var chunkUploadIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

type chunkParams struct {
	id        string
	index     int
	total     int
	offset    int64
	totalSize int64
	hasOffset bool
}

// parseChunkParams возвращает (params, true, nil) для чанкованного запроса,
// (zero, false, nil) для обычного single-shot и ошибку при кривых параметрах.
func parseChunkParams(q url.Values) (chunkParams, bool, error) {
	id := q.Get("upload_id")
	if id == "" {
		return chunkParams{}, false, nil
	}
	if !chunkUploadIDRe.MatchString(id) {
		return chunkParams{}, false, fmt.Errorf("bad upload_id")
	}
	index, err := strconv.Atoi(q.Get("chunk"))
	if err != nil {
		return chunkParams{}, false, fmt.Errorf("bad chunk")
	}
	total, err := strconv.Atoi(q.Get("chunks"))
	if err != nil {
		return chunkParams{}, false, fmt.Errorf("bad chunks")
	}
	if total < 1 || total > 100000 || index < 0 || index >= total {
		return chunkParams{}, false, fmt.Errorf("chunk out of range")
	}
	p := chunkParams{id: id, index: index, total: total}
	if raw := q.Get("offset"); raw != "" {
		offset, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || offset < 0 {
			return chunkParams{}, false, fmt.Errorf("bad offset")
		}
		p.offset = offset
		p.hasOffset = true
	}
	if raw := q.Get("total_size"); raw != "" {
		totalSize, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || totalSize < 0 {
			return chunkParams{}, false, fmt.Errorf("bad total_size")
		}
		p.totalSize = totalSize
	}
	return p, true, nil
}

func chunkPartPath(id string) string {
	return filepath.Join(os.TempDir(), "tgc-ul-"+id+".part")
}

// appendChunk дописывает src в .part-файл загрузки (первый кусок — с обрезкой,
// чтобы повторная попытка с тем же id начиналась чисто) и сообщает, был ли
// это последний кусок.
type chunkOffsetError struct {
	Want int64
	Have int64
}

func (e *chunkOffsetError) Error() string {
	return fmt.Sprintf("upload offset mismatch: want %d, have %d", e.Want, e.Have)
}

func appendChunk(p chunkParams, src io.Reader) (partPath string, final bool, err error) {
	partPath = chunkPartPath(p.id)
	flag := os.O_WRONLY | os.O_CREATE
	if p.hasOffset {
		flag = os.O_RDWR | os.O_CREATE
	} else if p.index == 0 {
		flag |= os.O_TRUNC
	} else {
		flag |= os.O_APPEND
	}
	f, err := os.OpenFile(partPath, flag, 0o600)
	if err != nil {
		return "", false, err
	}
	if p.hasOffset {
		st, statErr := f.Stat()
		if statErr != nil {
			f.Close()
			return "", false, statErr
		}
		switch {
		case st.Size() < p.offset:
			f.Close()
			return "", false, &chunkOffsetError{Want: p.offset, Have: st.Size()}
		case st.Size() > p.offset:
			// Повтор/резюм с подтверждённого offset: отбрасываем только хвост,
			// а не весь файл (старое O_TRUNC на chunk=0 ломало продолжение).
			if err = f.Truncate(p.offset); err != nil {
				f.Close()
				return "", false, err
			}
		}
		if _, err = f.Seek(p.offset, io.SeekStart); err != nil {
			f.Close()
			return "", false, err
		}
	}
	var written int64
	if written, err = io.Copy(f, src); err != nil {
		f.Close()
		return "", false, err
	}
	if err = f.Close(); err != nil {
		return "", false, err
	}
	final = p.index == p.total-1
	if p.hasOffset && p.totalSize > 0 {
		final = p.offset+written >= p.totalSize
	}
	return partPath, final, nil
}

// finishChunkUpload переименовывает собранный .part в итоговый путь.
//
// overwrite=false — существующий файл НЕ затираем (находка N107): отдаём
// fs.ErrExist, клиент спрашивает человека и повторяет последний кусок с
// overwrite=1. Собранный .part при этом остаётся на диске, поэтому повтор идёт
// с подтверждённого offset, а не с начала файла. Между Stat и Rename есть
// теоретическое окно (кто-то создал файл ровно в этот момент) — на практике оно
// микросекундное, а альтернатива, копирование через O_EXCL, сделала бы дорогим
// нормальный путь.
//
// TEMP агента может лежать на другом томе, чем целевая папка (TEMP на C:,
// загрузка в D:\Видео) — os.Rename между томами не работает, поэтому есть путь
// копированием.
func finishChunkUpload(partPath, dst string, overwrite bool) error {
	if !overwrite {
		if _, err := os.Stat(dst); err == nil {
			return &os.PathError{Op: "create", Path: dst, Err: fs.ErrExist}
		}
	} else {
		_ = os.Remove(dst)
	}
	err := os.Rename(partPath, dst)
	if err == nil {
		return nil
	}
	if !isCrossDeviceErr(err) {
		return err
	}
	if _, err := copyFileContents(partPath, dst, 0o666); err != nil {
		return err
	}
	_ = os.Remove(partPath)
	return nil
}

// abortChunkUpload убирает .part (на будущее: клиент может слать abort).
func abortChunkUpload(id string) {
	if chunkUploadIDRe.MatchString(id) {
		_ = os.Remove(chunkPartPath(id))
	}
}

func chunkUploadStatus(id string) (size int64, updated time.Time, exists bool, err error) {
	if !chunkUploadIDRe.MatchString(id) {
		return 0, time.Time{}, false, fmt.Errorf("bad upload_id")
	}
	st, err := os.Stat(chunkPartPath(id))
	if os.IsNotExist(err) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return st.Size(), st.ModTime(), true, nil
}

// Временные части старше суток не должны жить в TEMP вечно. Отдельная
// горутина здесь не нужна: лёгкая уборка запускается не чаще раза в час при
// следующей операции загрузки.
var lastChunkCleanup atomic.Int64

func maybeCleanupChunkUploads(now time.Time) {
	last := lastChunkCleanup.Load()
	if last != 0 && now.Unix()-last < int64(time.Hour/time.Second) {
		return
	}
	if !lastChunkCleanup.CompareAndSwap(last, now.Unix()) {
		return
	}
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	cutoff := now.Add(-24 * time.Hour)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "tgc-ul-") || !strings.HasSuffix(name, ".part") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "tgc-ul-"), ".part")
		if !chunkUploadIDRe.MatchString(id) {
			continue
		}
		if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(os.TempDir(), name))
		}
	}
}
