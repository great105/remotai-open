package web

// Чанкованное скачивание (находка #10) — зеркало upload_chunks.go.
//
// ЗАЧЕМ. В облаке ответ агента едет к релею одним JSON-кадром с телом в base64
// (×1.33). Файл на 200 МБ превращался в ~267 МБ, которые writeJSON пишет ОДНИМ
// кадром под SetWriteDeadline = 10 с (internal/relay/protocol.go). На бытовом
// аплинке это ~100 с ≫ 10 с → i/o timeout посреди кадра → рвётся УПРАВЛЯЮЩИЙ
// relay-сокет агента, и ПК на минуту теряет облако целиком. То есть большое
// скачивание не «медленное», а роняет весь облачный доступ. Плюс потолок
// agent.Send = 60 с на релее: клиент получал 504/pc_timeout («Компьютер не в
// сети») на живом ПК. Куски по ≤32 МБ снимают обе аварии, правок релея не
// требуют (каждый кусок — обычный проксируемый запрос).
//
// ПОЧЕМУ QUERY, А НЕ Range. Заголовок Range не входит в
// Access-Control-Allow-Headers агента (server.go, corsMiddleware), а
// Content-Range без Access-Control-Expose-Headers браузер всё равно не
// прочитает. Поэтому кусок запрашивается простым (simple) GET с ?offset=&len=,
// без preflight, одинаково в LAN и в облаке. Range при этом НЕ ломается: без
// offset/len хендлер по-прежнему отдаёт http.ServeFile, который сам умеет 206.
//
// ЦЕЛОСТНОСТЬ. Файл могут дописать или усечь между кусками — склеенный архив
// оказался бы молча битым. Поэтому в каждом ответе едут X-File-Size и
// X-File-Mtime, а клиент может прислать их обратно как ?expect_size=&expect_mtime=
// — на расхождении агент отвечает 409 file_changed вместо порчи файла.

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

const (
	// maxDownloadChunk — жёсткий потолок одного куска. Без него ?len=10000000000
	// воспроизводит нынешний OOM с валидным токеном, поэтому превышение — это
	// ошибка 416/bad_range, а НЕ молчаливый клампинг.
	maxDownloadChunk = 32 << 20 // 32 МБ

	// fileMetaExposeHeaders — без Access-Control-Expose-Headers кросс-origin
	// клиент (APK на https://localhost → http://192.168.x.x:8080, а в облаке —
	// через релей) не прочитает ни размер, ни mtime, ни признак чанкования.
	// Релей копирует заголовки ответа агента как есть (proxy.go), поэтому
	// выставлять их надо здесь, в самом хендлере: corsMiddleware для
	// relay-проксированных запросов не выполняется вовсе.
	fileMetaExposeHeaders = "X-File-Size, X-File-Mtime, X-Chunk-Offset, X-Chunk-Len"
)

// downloadRange — разобранный кусок файла.
type downloadRange struct {
	offset int64
	length int64
}

// parseDownloadRange разбирает ?offset=&len=.
//
// Возвращает (rng, true, nil) для чанкованного запроса, (zero, false, nil) —
// когда параметров нет вовсе (старое поведение: http.ServeFile целиком) и
// ошибку при кривых параметрах (хендлер отдаёт её как 416 bad_range).
func parseDownloadRange(q url.Values, size int64) (downloadRange, bool, error) {
	rawOff, rawLen := q.Get("offset"), q.Get("len")
	if rawOff == "" && rawLen == "" {
		return downloadRange{}, false, nil
	}
	if rawLen == "" {
		return downloadRange{}, true, fmt.Errorf("len required when offset is set")
	}
	var off int64
	if rawOff != "" {
		v, err := strconv.ParseInt(rawOff, 10, 64)
		if err != nil {
			return downloadRange{}, true, fmt.Errorf("bad offset")
		}
		off = v
	}
	n, err := strconv.ParseInt(rawLen, 10, 64)
	if err != nil {
		return downloadRange{}, true, fmt.Errorf("bad len")
	}
	if off < 0 || off > size {
		return downloadRange{}, true, fmt.Errorf("offset %d out of range (file size %d)", off, size)
	}
	if n <= 0 {
		return downloadRange{}, true, fmt.Errorf("len must be positive")
	}
	if n > maxDownloadChunk {
		return downloadRange{}, true, fmt.Errorf("len %d exceeds max chunk size %d", n, maxDownloadChunk)
	}
	// Клампим только до конца файла — это не «молчаливое урезание запроса», а
	// нормальный последний кусок; клиент сверяет фактическую длину по
	// X-Chunk-Len и Content-Length.
	if rest := size - off; n > rest {
		n = rest
	}
	return downloadRange{offset: off, length: n}, true, nil
}

// setFileMetaHeaders выставляет признаки файла, по которым клиент сверяет
// целостность между кусками и понимает, что агент вообще умеет чанкование.
func setFileMetaHeaders(w http.ResponseWriter, info os.FileInfo) {
	w.Header().Set("X-File-Size", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-File-Mtime", strconv.FormatInt(info.ModTime().Unix(), 10))
	w.Header().Set("Access-Control-Expose-Headers", fileMetaExposeHeaders)
}

// checkFileUnchanged сверяет необязательные ?expect_size=&expect_mtime= с
// текущим состоянием файла. Расхождение → «файл изменился между кусками»:
// лучше честная ошибка, чем молча битый дамп базы на выходе.
func checkFileUnchanged(q url.Values, info os.FileInfo) bool {
	if v := q.Get("expect_size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || n != info.Size() {
			return false
		}
	}
	if v := q.Get("expect_mtime"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || n != info.ModTime().Unix() {
			return false
		}
	}
	return true
}

// fileChangedExtra — поля 409-ответа, чтобы клиент мог перезапустить скачивание
// с новыми признаками, а не гадать.
func fileChangedExtra(info os.FileInfo) map[string]string {
	return map[string]string{
		"size":  strconv.FormatInt(info.Size(), 10),
		"mtime": strconv.FormatInt(info.ModTime().Unix(), 10),
	}
}
