package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestParseDownloadRange(t *testing.T) {
	const size = 1000

	// Без offset/len — старое поведение (ServeFile целиком).
	if _, chunked, err := parseDownloadRange(url.Values{}, size); chunked || err != nil {
		t.Fatalf("no params: want (false, nil), got chunked=%v err=%v", chunked, err)
	}

	q := url.Values{}
	q.Set("offset", "100")
	q.Set("len", "200")
	rng, chunked, err := parseDownloadRange(q, size)
	if err != nil || !chunked {
		t.Fatalf("valid: chunked=%v err=%v", chunked, err)
	}
	if rng.offset != 100 || rng.length != 200 {
		t.Fatalf("valid: %+v", rng)
	}

	// Последний кусок клампится до конца файла.
	q.Set("offset", "900")
	q.Set("len", "500")
	rng, _, err = parseDownloadRange(q, size)
	if err != nil || rng.length != 100 {
		t.Fatalf("tail clamp: %+v err=%v", rng, err)
	}

	// offset == size — легальный «нулевой хвост».
	q.Set("offset", strconv.Itoa(size))
	q.Set("len", "10")
	rng, _, err = parseDownloadRange(q, size)
	if err != nil || rng.length != 0 {
		t.Fatalf("offset==size: %+v err=%v", rng, err)
	}

	bad := []struct {
		name        string
		offset, len string
	}{
		{"offset beyond size", "1001", "10"},
		{"negative offset", "-1", "10"},
		{"zero len", "0", "0"},
		{"negative len", "0", "-5"},
		{"len over max", "0", strconv.Itoa(maxDownloadChunk + 1)},
		{"huge len (OOM-запрос)", "0", "10000000000"},
		{"garbage offset", "abc", "10"},
		{"garbage len", "0", "abc"},
		{"len missing", "0", ""},
	}
	for _, c := range bad {
		q := url.Values{}
		q.Set("offset", c.offset)
		if c.len != "" {
			q.Set("len", c.len)
		}
		if _, chunked, err := parseDownloadRange(q, size); err == nil {
			t.Fatalf("%s: want error, got chunked=%v", c.name, chunked)
		}
	}
}

// downloadTestFile — файл на 3000 байт с предсказуемым содержимым.
func downloadTestFile(t *testing.T) (path string, data []byte) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "big.bin")
	data = make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

// Дефолт (без offset/len) обязан остаться http.ServeFile: на этом держится
// LAN-путь <a download> и поддержка Range.
func TestDownloadWithoutRangeServesWholeFile(t *testing.T) {
	path, data := downloadTestFile(t)
	s := &Server{}

	req := httptest.NewRequest("GET", "/api/files/download?path="+url.QueryEscape(path), nil)
	rec := httptest.NewRecorder()
	s.apiFileDownload(rec, req, 1)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != len(data) {
		t.Fatalf("body %d bytes, want %d", rec.Body.Len(), len(data))
	}
	if got := rec.Header().Get("X-File-Size"); got != strconv.Itoa(len(data)) {
		t.Fatalf("X-File-Size=%q", got)
	}

	// Range по-прежнему обслуживается stdlib (206 + Content-Range).
	req = httptest.NewRequest("GET", "/api/files/download?path="+url.QueryEscape(path), nil)
	req.Header.Set("Range", "bytes=10-19")
	rec = httptest.NewRecorder()
	s.apiFileDownload(rec, req, 1)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range status %d", rec.Code)
	}
	if rec.Body.Len() != 10 || rec.Body.Bytes()[0] != data[10] {
		t.Fatalf("range body %d bytes", rec.Body.Len())
	}
}

func TestDownloadChunked(t *testing.T) {
	path, data := downloadTestFile(t)
	s := &Server{}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Собираем файл кусками по 1000 байт.
	var assembled []byte
	for off := 0; off < len(data); off += 1000 {
		u := fmt.Sprintf("/api/files/download?path=%s&offset=%d&len=1000", url.QueryEscape(path), off)
		rec := httptest.NewRecorder()
		s.apiFileDownload(rec, httptest.NewRequest("GET", u, nil), 1)
		if rec.Code != 200 {
			t.Fatalf("chunk at %d: status %d (%s)", off, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("X-Chunk-Len"); got != "1000" {
			t.Fatalf("chunk at %d: X-Chunk-Len=%q", off, got)
		}
		if got := rec.Header().Get("X-File-Size"); got != strconv.FormatInt(info.Size(), 10) {
			t.Fatalf("chunk at %d: X-File-Size=%q", off, got)
		}
		if rec.Header().Get("Access-Control-Expose-Headers") == "" {
			t.Fatal("Expose-Headers not set: кросс-origin клиент не прочитает признаки файла")
		}
		assembled = append(assembled, rec.Body.Bytes()...)
	}
	if string(assembled) != string(data) {
		t.Fatalf("assembled %d bytes, want %d", len(assembled), len(data))
	}

	// Запрос гигантского куска — ошибка, а не молчаливый клампинг (иначе
	// возвращается нынешний OOM).
	rec := httptest.NewRecorder()
	s.apiFileDownload(rec, httptest.NewRequest("GET",
		"/api/files/download?path="+url.QueryEscape(path)+"&offset=0&len=10000000000", nil), 1)
	if rec.Code != 416 {
		t.Fatalf("huge len: status %d, want 416", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != fsCodeBadRange {
		t.Fatalf("huge len: code=%v", body["code"])
	}

	// Файл изменился между кусками — 409 file_changed, а не молча битый файл.
	rec = httptest.NewRecorder()
	u := fmt.Sprintf("/api/files/download?path=%s&offset=0&len=100&expect_size=%d",
		url.QueryEscape(path), info.Size()+1)
	s.apiFileDownload(rec, httptest.NewRequest("GET", u, nil), 1)
	if rec.Code != 409 {
		t.Fatalf("expect_size mismatch: status %d, want 409", rec.Code)
	}
	body = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != fsCodeChanged {
		t.Fatalf("expect_size mismatch: code=%v", body["code"])
	}

	// Совпадающие признаки проходят.
	rec = httptest.NewRecorder()
	u = fmt.Sprintf("/api/files/download?path=%s&offset=0&len=100&expect_size=%d&expect_mtime=%d",
		url.QueryEscape(path), info.Size(), info.ModTime().Unix())
	s.apiFileDownload(rec, httptest.NewRequest("GET", u, nil), 1)
	if rec.Code != 200 || rec.Body.Len() != 100 {
		t.Fatalf("expect match: status %d len %d", rec.Code, rec.Body.Len())
	}
}
