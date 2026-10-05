package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Сортировка должна применяться ко ВСЕМ записям каталога и только потом резать
// лимит (находка N20): иначе «По дате» упорядочивает алфавитную выборку, и
// самый свежий файл с именем на «я» в ответ не попадает вовсе.
func TestFilesListSortsBeforeTruncating(t *testing.T) {
	dir := t.TempDir()
	// mtime растёт вместе с алфавитом наоборот: самый новый — последний по имени.
	names := []string{"a.txt", "b.txt", "c.txt", "d.txt"}
	for i, name := range names {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), (i+1)*10), 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := time.Now().Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{}

	listNames := func(query string) []string {
		req := httptest.NewRequest("GET", "/api/files?path="+url.QueryEscape(dir)+query, nil)
		rec := httptest.NewRecorder()
		s.apiFilesList(rec, req, 1)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			Sort      string `json:"sort"`
			Total     int    `json:"total"`
			Truncated bool   `json:"truncated"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Total != len(names) {
			t.Fatalf("total=%d, want %d", resp.Total, len(names))
		}
		out := make([]string, 0, len(resp.Items))
		for _, it := range resp.Items {
			out = append(out, it.Name)
		}
		return out
	}

	if got := listNames(""); got[0] != "a.txt" {
		t.Fatalf("default sort: %v", got)
	}
	// Лимит 2 + сортировка по дате: наверху обязаны быть d.txt и c.txt, хотя по
	// алфавиту в первую двойку попали бы a.txt и b.txt.
	if got := listNames("&sort=date&limit=2"); len(got) != 2 || got[0] != "d.txt" || got[1] != "c.txt" {
		t.Fatalf("sort=date&limit=2: %v", got)
	}
	if got := listNames("&sort=size&limit=2"); len(got) != 2 || got[0] != "d.txt" || got[1] != "c.txt" {
		t.Fatalf("sort=size&limit=2: %v", got)
	}
	// Неизвестный режим не ломает листинг — молча «по имени».
	if got := listNames("&sort=zzz&limit=2"); got[0] != "a.txt" {
		t.Fatalf("sort=zzz: %v", got)
	}
}

func TestSortFileRowsFoldersFirst(t *testing.T) {
	rows := []fileRow{
		{name: "old-file", mtime: 100, size: 9, statted: true, hasInfo: true},
		{name: "new-file", mtime: 900, size: 1, statted: true, hasInfo: true},
		{name: "zeta-dir", isDir: true, mtime: 1, statted: true, hasInfo: true},
	}
	sortFileRows(rows, filesSortDate)
	if !rows[0].isDir || rows[1].name != "new-file" {
		t.Fatalf("date order: %+v", rows)
	}
	sortFileRows(rows, filesSortSize)
	if !rows[0].isDir || rows[1].name != "old-file" {
		t.Fatalf("size order: %+v", rows)
	}
	sortFileRows(rows, filesSortName)
	if !rows[0].isDir || rows[1].name != "new-file" || rows[2].name != "old-file" {
		t.Fatalf("name order: %+v", rows)
	}
}

func TestSortSFTPEntriesMatchesLocalOrder(t *testing.T) {
	entries := []sftpEntryJSON{
		{Name: "small.log", Size: 10, mtime: 500},
		{Name: "big.log", Size: 1000, mtime: 100},
		{Name: "dir", IsDir: true, mtime: 1},
	}
	sortSFTPEntries(entries, filesSortSize)
	if !entries[0].IsDir || entries[1].Name != "big.log" {
		t.Fatalf("size order: %+v", entries)
	}
	sortSFTPEntries(entries, filesSortDate)
	if !entries[0].IsDir || entries[1].Name != "small.log" {
		t.Fatalf("date order: %+v", entries)
	}
}

// Загрузка не имеет права молча затирать одноимённый файл на ПК (N107): без
// overwrite=1 ответ — already_exists, и старый файл остаётся целым.
func TestUploadRefusesToOverwriteWithoutFlag(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(existing, []byte("работа человека"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}

	upload := func(query string) *httptest.ResponseRecorder {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		part, err := mw.CreateFormFile("file", "report.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("новое содержимое")); err != nil {
			t.Fatal(err)
		}
		mw.Close()
		req := httptest.NewRequest("POST", "/api/files/upload?path="+url.QueryEscape(dir)+query, body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		s.apiFileUpload(rec, req, 1)
		return rec
	}

	rec := upload("")
	if rec.Code != 409 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var errResp struct{ Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.Code != fsCodeExists {
		t.Fatalf("code=%q, want %q", errResp.Code, fsCodeExists)
	}
	if data, _ := os.ReadFile(existing); string(data) != "работа человека" {
		t.Fatalf("файл затёрт: %q", data)
	}

	if rec := upload("&overwrite=1"); rec.Code != 200 {
		t.Fatalf("overwrite status %d: %s", rec.Code, rec.Body.String())
	}
	if data, _ := os.ReadFile(existing); string(data) != "новое содержимое" {
		t.Fatalf("overwrite не сработал: %q", data)
	}
}

func TestFinishChunkUploadRespectsExistingFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "movie.mp4")
	if err := os.WriteFile(dst, []byte("старый файл"), 0o600); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, "assembled.part")
	if err := os.WriteFile(part, []byte("новый файл"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := finishChunkUpload(part, dst, false)
	if err == nil {
		t.Fatal("ожидался отказ по already_exists")
	}
	if code, status := fsErrorCode(err); code != fsCodeExists || status != 409 {
		t.Fatalf("code=%q status=%d", code, status)
	}
	if data, _ := os.ReadFile(dst); string(data) != "старый файл" {
		t.Fatalf("файл затёрт: %q", data)
	}
	// .part обязан остаться: повтор с overwrite=1 не должен гнать файл заново.
	if _, err := os.Stat(part); err != nil {
		t.Fatalf(".part потерян: %v", err)
	}

	if err := finishChunkUpload(part, dst, true); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if data, _ := os.ReadFile(dst); string(data) != "новый файл" {
		t.Fatalf("overwrite не сработал: %q", data)
	}
}

// Перенос между дисками (N109) выполняется копированием и удалением источника:
// раньше он падал тупиковым io_error. Сам EXDEV в тесте не воспроизвести (нужны
// два тома), поэтому проверяем ветку, в которую он ведёт.
func TestMoveAcrossVolumesCopiesThenRemovesSource(t *testing.T) {
	dir := t.TempDir()
	s := &Server{}

	src := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(src, []byte("байты"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "moved.mp4")
	rec := httptest.NewRecorder()
	s.moveAcrossVolumes(rec, src, dst, false)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Path   string `json:"path"`
		Copied bool   `json:"copied"`
		Files  int64  `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Copied || resp.Path != dst || resp.Files != 1 {
		t.Fatalf("resp %+v", resp)
	}
	if data, _ := os.ReadFile(dst); string(data) != "байты" {
		t.Fatalf("копия неверна: %q", data)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("источник не удалён: %v", err)
	}

	// Папку-одноимёнку не сливаем никогда — честный already_exists.
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "inner", "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	busy := filepath.Join(dir, "busy")
	if err := os.MkdirAll(busy, 0o755); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.moveAcrossVolumes(rec, tree, busy, true)
	if rec.Code != 409 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	// Дерево на свободное имя переносится целиком.
	rec = httptest.NewRecorder()
	s.moveAcrossVolumes(rec, tree, filepath.Join(dir, "tree2"), false)
	if rec.Code != 200 {
		t.Fatalf("tree status %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "tree2", "inner", "a.txt")); err != nil {
		t.Fatalf("дерево не скопировано: %v", err)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("источник дерева не удалён: %v", err)
	}
}

// isCrossDeviceErr не имеет права принимать за «другой том» обычные отказы —
// иначе перенос молча превратится в копирование там, где надо честно ошибиться.
func TestIsCrossDeviceErrIgnoresOrdinaryFailures(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "missing.txt"), filepath.Join(dir, "b.txt")); err == nil {
		t.Fatal("ожидалась ошибка")
	} else if isCrossDeviceErr(err) {
		t.Fatalf("ENOENT принят за cross-device: %v", err)
	}
	if err := os.Rename(src, filepath.Join(dir, "no-such-dir", "b.txt")); err == nil {
		t.Fatal("ожидалась ошибка")
	} else if isCrossDeviceErr(err) {
		t.Fatalf("отсутствие каталога принято за cross-device: %v", err)
	}
	if isCrossDeviceErr(nil) || isCrossDeviceErr(fs.ErrPermission) {
		t.Fatal("не-errno ошибка принята за cross-device")
	}
}

// «Прислать файлом в Telegram» трассы «Зафиксировать проблему» кладёт трассу
// на компьютер загрузкой (pty-upload-<ms>-remotai-trace-*.json) и отдаёт боту.
// Раньше копия оставалась в ~/Remotai/files навсегда — с записанным выводом
// терминала. Теперь её стирает сервер, но только после УСПЕШНОЙ отправки и
// только её: прочие загрузки и файлы человека не трогаются.
func TestSendToTelegramRemovesOnlyDeliveredTraceUpload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	files := filepath.Join(home, "Remotai", "files")
	write := func(dir, name string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(`{"format":"remotai-trace","recording":{"chunks":["secret output"]}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exists := func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}
	trace := write(files, "pty-upload-1789329274003-remotai-trace-20260914-070509.json")
	chunkedTrace := write(files, "pty-upload-1789329274004-remotai-trace-20260914-070510.json")
	otherUpload := write(files, "pty-upload-1789329274005-report.json")
	castUpload := write(files, "pty-upload-1789329274006-remotai-trace-20260914-070511.cast")
	userTrace := write(home, "remotai-trace-20260914-070509.json")
	outsideTrace := write(filepath.Join(home, "Documents"), "pty-upload-1789329274007-remotai-trace-20260914-070512.json")
	nestedTrace := write(filepath.Join(files, "sub"), "pty-upload-1789329274008-remotai-trace-20260914-070513.json")

	fail := false
	var sent []string
	s := &Server{sendFileToTelegram: func(_ context.Context, _ int64, p string) error {
		if fail {
			return errors.New("telegram down")
		}
		// Бот читает файл во время отправки: стирать раньше нельзя.
		if !exists(p) {
			return errors.New("file gone before send")
		}
		sent = append(sent, p)
		return nil
	}}
	post := func(p string) (int, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"path": p})
		rec := httptest.NewRecorder()
		s.apiFileSendToTelegram(rec, httptest.NewRequest("POST", "/api/files/send-to-telegram", bytes.NewReader(body)), 1)
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}

	// Отправка не удалась — трасса остаётся: человек повторит.
	fail = true
	if code, _ := post(trace); code != 500 {
		t.Fatalf("сбой бота: код %d", code)
	}
	if !exists(trace) {
		t.Fatal("трасса стёрта при НЕУДАВШЕЙСЯ отправке")
	}
	fail = false

	for _, p := range []string{trace, chunkedTrace} {
		code, resp := post(p)
		if code != 200 || resp["removed"] != true {
			t.Fatalf("отправка %s: код %d, ответ %v", filepath.Base(p), code, resp)
		}
		if exists(p) {
			t.Fatalf("доставленная трасса осталась на компьютере: %s", p)
		}
	}
	for _, p := range []string{otherUpload, castUpload, userTrace, outsideTrace, nestedTrace} {
		code, resp := post(p)
		if code != 200 || resp["removed"] != nil {
			t.Fatalf("отправка %s: код %d, ответ %v", p, code, resp)
		}
		if !exists(p) {
			t.Fatalf("стёрт файл, который не транспорт трассы: %s", p)
		}
	}
	if len(sent) != 7 {
		t.Fatalf("бот получил %d файлов, ждали 7: %v", len(sent), sent)
	}
}
