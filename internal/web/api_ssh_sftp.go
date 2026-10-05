package web

// SFTP к SSH-серверам через агента (тот же транспорт, что у SSH-терминала).
//
// Соединения кешируются в памяти агента (pty.SFTPPool, idle 10 мин). Пароль
// приходит параметром password в каждом запросе (в query для legacy-GET, в теле
// для POST) и используется только при (пере)подключении — не сохраняется и не
// логируется. known_hosts/TOFU — общая логика dialSSH: неизвестный ключ
// отдаём как 409 host_key_unknown + fingerprint, дальше trust_host=true.
//
// Upload поддерживает чанкование как у /api/files/upload (upload_chunks.go):
// куски собираются в .part на агенте, последний кусок заливается на сервер.
//
// Download умеет отдавать КУСОК файла (offset/len). Транспорт кусков — query
// для GET и поля тела для POST, а НЕ заголовок Range: Range не входит в
// Access-Control-Allow-Headers агента (server.go), а Content-Range без
// Access-Control-Expose-Headers клиент всё равно не прочитает.
// POST /api/ssh/sftp/download — предпочтительный путь: пароль уезжает в теле,
// а не в query, где при 25 кусках он 25 раз попадёт в access-логи.
//
// Ошибки — 4xx/5xx с машинным полем code (клиент ветвится по коду, а не по
// тексту и не по HTTP-статусу).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	urlpath "path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pkg/sftp"

	"tgcontrol/internal/pty"
)

// maxSFTPDownloadChunk — потолок одного куска скачивания. Без него запрос
// ?len=10000000000 с валидным токеном воспроизводит OOM, ради устранения
// которого чанкование и делалось.
const maxSFTPDownloadChunk = 32 << 20
const maxSFTPListEntries = 2000
const maxSFTPPreviewBytes = 128 << 10

// errDirNotEmpty — удаление непустого каталога без recursive.
// Отдельная ошибка, а не разбор текста SSH_FX_FAILURE: разные серверы кладут
// в этот код что угодно, а «папка не пуста» мы знаем точно из ReadDir.
var errDirNotEmpty = errors.New("directory not empty")

// errRefuseRemoteRoot — рекурсивное удаление корня/системного каталога.
var errRefuseRemoteRoot = errors.New("refusing to delete a root directory")

// sftpConnParams — общие параметры подключения для всех SFTP-эндпоинтов.
type sftpConnParams struct {
	host          string
	hostID        string
	port          int
	user          string
	password      string // только в память хендшейка; не логируем и не храним
	identityFile  string
	keyPassphrase string
	proxyJump     string
	proxyPassword string
	trustHost     bool
	// privateKeyPEM — ключ, назначенный серверу в хранилище приложения.
	// Заполняется только на сервере (resolveSFTPParams) и никогда не приходит
	// от клиента: приватный ключ не ездит через сеть даже к своему телефону.
	privateKeyPEM string
}

// sftpEntryJSON — элемент ответа /api/ssh/sftp/list.
type sftpEntryJSON struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	IsDir       bool   `json:"is_dir"`
	ModTime     string `json:"mod_time"`
	Permissions string `json:"permissions"`
	// Hidden — имя начинается с точки. SFTP-браузер такие файлы показывал
	// всегда (и продолжает), но локальный экран прячет их за тумблером —
	// признак нужен, чтобы оба экрана вели себя одинаково (#13).
	Hidden bool `json:"hidden,omitempty"`
	// mtime — unix-секунды для сортировки по дате. Не сериализуется: клиент
	// читает читаемый ModTime (RFC3339), а числом сортирует сервер.
	mtime int64
}

func (p sftpConnParams) sshConfig() pty.SSHConfig {
	return pty.SSHConfig{
		Host:          p.host,
		Port:          p.port,
		User:          p.user,
		Password:      p.password,
		IdentityFile:  p.identityFile,
		KeyPassphrase: p.keyPassphrase,
		ProxyJump:     p.proxyJump,
		ProxyPassword: p.proxyPassword,
		TrustHost:     p.trustHost,
		PrivateKeyPEM: p.privateKeyPEM,
	}
}

func (p *sftpConnParams) validate() error {
	if p.hostID == "" && (p.host == "" || p.user == "") {
		return fmt.Errorf("host and user are required")
	}
	return nil
}

// label — подпись сервера для UI переносов («user@host» / «user@host:2222»).
func (p sftpConnParams) label() string {
	if p.port > 0 && p.port != 22 {
		return fmt.Sprintf("%s@%s:%d", p.user, p.host, p.port)
	}
	return fmt.Sprintf("%s@%s", p.user, p.host)
}

// sftpParamsFromQuery — GET-эндпоинты (list/legacy download) и query-часть
// upload: host, port, user, password, trust_host в query.
func sftpParamsFromQuery(r *http.Request) (sftpConnParams, error) {
	q := r.URL.Query()
	p := sftpConnParams{
		host:          strings.TrimSpace(q.Get("host")),
		hostID:        strings.TrimSpace(q.Get("host_id")),
		user:          strings.TrimSpace(q.Get("user")),
		password:      q.Get("password"),
		identityFile:  strings.TrimSpace(q.Get("identity_file")),
		keyPassphrase: q.Get("key_passphrase"),
		proxyJump:     strings.TrimSpace(q.Get("proxy_jump")),
		proxyPassword: q.Get("proxy_password"),
		trustHost:     q.Get("trust_host") == "true" || q.Get("trust_host") == "1",
	}
	if s := q.Get("port"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 65535 {
			return p, fmt.Errorf("invalid port")
		}
		p.port = n
	}
	return p, p.validate()
}

// sftpBody — JSON-тело POST-эндпоинтов (mkdir/delete/rename/download). Пароль
// в теле, не в query — GET-строки попадают в access-логи, тела нет.
type sftpBody struct {
	Host          string `json:"host"`
	HostID        string `json:"host_id"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password"`
	IdentityFile  string `json:"identity_file"`
	KeyPassphrase string `json:"key_passphrase"`
	ProxyJump     string `json:"proxy_jump"`
	ProxyPassword string `json:"proxy_password"`
	TrustHost     bool   `json:"trust_host"`
	Path          string `json:"path"`
	From          string `json:"from"`
	To            string `json:"to"`
	// Recursive — удалять непустой каталог со всем содержимым (#43).
	// Без него непустая папка отдаёт честный код not_empty, а не 500.
	Recursive bool `json:"recursive"`
}

func (b sftpBody) params() sftpConnParams {
	return sftpConnParams{
		host:          strings.TrimSpace(b.Host),
		hostID:        strings.TrimSpace(b.HostID),
		port:          b.Port,
		user:          strings.TrimSpace(b.User),
		password:      b.Password,
		identityFile:  strings.TrimSpace(b.IdentityFile),
		keyPassphrase: b.KeyPassphrase,
		proxyJump:     strings.TrimSpace(b.ProxyJump),
		proxyPassword: b.ProxyPassword,
		trustHost:     b.TrustHost,
	}
}

func (s *Server) resolveSFTPParams(p sftpConnParams) (sftpConnParams, error) {
	if p.hostID == "" {
		return p, p.validate()
	}
	h, ok := s.resolveSSHHost(p.hostID)
	if !ok {
		return p, fmt.Errorf("host not found")
	}
	if p.host == "" {
		p.host = h.Host
	}
	if p.port == 0 {
		p.port = h.Port
	}
	if p.user == "" {
		p.user = h.User
	}
	if p.identityFile == "" {
		p.identityFile = h.IdentityFile
	}
	if p.proxyJump == "" {
		p.proxyJump = h.ProxyJump
	}
	if secret, ok := s.sshSecretFor(p.hostID); ok {
		if p.password == "" {
			p.password = secret.password
		}
		if p.keyPassphrase == "" {
			p.keyPassphrase = secret.keyPassphrase
		}
		if p.proxyPassword == "" {
			p.proxyPassword = secret.proxyPassword
		}
	}
	// Ключ хранилища — тот же, что открывает терминал этого сервера.
	pemData, keyPass, keyErr := s.sshHostKeyMaterial(h.KeyID)
	if keyErr != nil {
		return p, keyErr
	}
	p.privateKeyPEM = pemData
	if p.keyPassphrase == "" {
		p.keyPassphrase = keyPass
	}
	return p, p.validate()
}

// sftpClient достаёт клиент из пула; при ошибке сам пишет SSH-код-ответ
// (host_key_unknown/auth_failed/unreachable). ok=false — ответ уже записан.
func (s *Server) sftpClient(w http.ResponseWriter, p sftpConnParams) (c *sftp.Client, ok bool) {
	var err error
	p, err = s.resolveSFTPParams(p)
	if err != nil {
		// «Ключ недоступен» — не кривой запрос: параметры верные, а вот ключ
		// удалили или он зашифрован другой учётной записью. bad_request здесь
		// увёл бы человека проверять адрес сервера.
		if errors.Is(err, pty.ErrSSHKeyLocked) || errors.Is(err, pty.ErrSSHKeyNotFound) {
			writeSSHKeyError(w, err)
			return nil, false
		}
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return nil, false
	}
	c, err = s.sftpPool.Get(p.sshConfig())
	if err != nil {
		writeSFTPConnErr(w, err)
		return nil, false
	}
	return c, true
}

// writeSFTPConnErr — отказ подключения к серверу. Всё, что распознаёт
// writeSSHError (host_key/auth/unreachable/key_encrypted), уходит через него;
// отдельно ловим «на сервере нет SFTP-подсистемы» — без своего кода этот отказ
// выглядел как «Ошибка сервера», хотя SSH-терминал к тому же серверу работает
// (N111). Статус 400, а не 5xx: 5xx клиент показывает как поломку Remotai.
func writeSFTPConnErr(w http.ResponseWriter, err error) {
	if errors.Is(err, pty.ErrSFTPSubsystem) {
		jsonErrorCode(w, 400, "sftp_unavailable", err.Error(), nil)
		return
	}
	writeSSHError(w, err)
}

// sftpLease — то же, но с лизом: пока release не вызван, janitor пула не
// закроет соединение по idle. Нужно долгим переносам (#9).
func (s *Server) sftpLease(w http.ResponseWriter, p sftpConnParams) (*sftp.Client, func(), bool) {
	p, err := s.resolveSFTPParams(p)
	if err != nil {
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return nil, nil, false
	}
	c, release, err := s.sftpPool.Lease(p.sshConfig())
	if err != nil {
		writeSFTPConnErr(w, err)
		return nil, nil, false
	}
	return c, release, true
}

// ── Машинные коды ошибок ───────────────────────────────────────────

// sftpErrCode превращает ошибку SFTP-операции в (машинный код, HTTP-статус).
// pkg/sftp нормализует часть статусов в os.ErrNotExist/os.ErrPermission,
// остальное приходит как *sftp.StatusError.
func sftpErrCode(err error) (string, int) {
	switch {
	case err == nil:
		return "", 200
	case errors.Is(err, errDirNotEmpty):
		return "not_empty", 409
	case errors.Is(err, errRefuseRemoteRoot):
		return "refused_root", 400
	case errors.Is(err, fs.ErrNotExist):
		return "not_found", 404
	case errors.Is(err, fs.ErrPermission):
		return "no_permission", 403
	case errors.Is(err, fs.ErrExist):
		return "already_exists", 409
	}
	var se *sftp.StatusError
	if errors.As(err, &se) {
		switch se.FxCode() {
		case sftp.ErrSSHFxPermissionDenied:
			return "no_permission", 403
		case sftp.ErrSSHFxNoSuchFile:
			return "not_found", 404
		case sftp.ErrSSHFxOpUnsupported:
			return "unsupported", 400
		case sftp.ErrSSHFxFailure:
			// SSH_FX_FAILURE — мусорная корзина протокола: серверы кладут в неё
			// всё подряд. Разбираем текст, но только как последнюю попытку.
			low := strings.ToLower(se.Error())
			switch {
			case strings.Contains(low, "not empty"):
				return "not_empty", 409
			case strings.Contains(low, "no space"), strings.Contains(low, "quota"):
				return "disk_full", 507
			case strings.Contains(low, "read-only"), strings.Contains(low, "read only"):
				return "read_only", 409
			case strings.Contains(low, "exists"):
				return "already_exists", 409
			}
		}
	}
	return "io_error", 500
}

// localFSErrCode — то же для локальной стороны переноса (файл на ПК).
// Платформенный разбор (ERROR_SHARING_VIOLATION, ENOSPC) сюда не тащим — это
// отдельная тема #42; здесь достаточно основных трёх случаев.
func localFSErrCode(err error) (string, int) {
	switch {
	case err == nil:
		return "", 200
	case errors.Is(err, fs.ErrNotExist):
		return "not_found", 404
	case errors.Is(err, fs.ErrPermission):
		return "no_permission", 403
	case errors.Is(err, fs.ErrExist):
		return "already_exists", 409
	}
	return "io_error", 500
}

// transferErrCode — код для исхода переноса: сначала SFTP-разбор, он же
// покрывает и локальные os-ошибки (обе ветки смотрят на fs.Err*).
func transferErrCode(err error) (string, int) {
	code, status := sftpErrCode(err)
	if code == "io_error" {
		if c, st := localFSErrCode(err); c != "io_error" {
			return c, st
		}
	}
	return code, status
}

// writeSFTPErr отвечает машинным кодом по ошибке SFTP-операции.
func writeSFTPErr(w http.ResponseWriter, err error, what string) {
	code, status := sftpErrCode(err)
	if status >= 500 {
		log.Printf("[SFTP] %s: %v", what, err)
	}
	jsonErrorCode(w, status, code, what+": "+err.Error(), nil)
}

// sshJSONStatus — jsonResp с явным статусом (202 у переносов, 4xx у частичных
// отказов загрузки, где тело содержит список failed).
func sshJSONStatus(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// ── list ───────────────────────────────────────────────────────────

// GET /api/ssh/sftp/list?host&port&user&path[&hidden=0|1][&sort=name|date|size][&remember=1]
// — список каталога: сначала папки, потом файлы, внутри — выбранный порядок.
//
// Скрытые (dot-)файлы SFTP-браузер показывает ИСТОРИЧЕСКИ ВСЕГДА, и это
// поведение сохранено по умолчанию (старый клиент параметра не шлёт и ничего
// не теряет). Новый клиент управляет им явно: hidden=0 прячет dot-файлы,
// чтобы экран сервера вёл себя так же, как локальные «Файлы» (#13).
//
// sort — тот же контракт, что у локальных «Файлов» (N20): сортируем ВСЕ записи
// каталога и только потом режем лимит в 2000, иначе «По дате» упорядочивает
// произвольную алфавитную выборку.
//
// remember=1 — «Помнить пароль до перезапуска ПК» (N147). Секрет кладём в
// память агента только ПОСЛЕ успешного листинга (помнить неверный пароль
// незачем), а в ответе всегда есть secret_remembered — по нему экран видит
// правду, а не надежду: раньше единственным сигналом был отдельный POST
// /unlock, ответ которого клиент терял.
func (s *Server) apiSFTPList(w http.ResponseWriter, r *http.Request, uid int64) {
	p, err := sftpParamsFromQuery(r)
	if err != nil {
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return
	}
	q := r.URL.Query()
	dir := q.Get("path")
	if dir == "" {
		dir = "."
	}
	showHidden := true
	if v := q.Get("hidden"); v != "" {
		showHidden = v == "1" || v == "true"
	}
	mode := parseFilesSort(q.Get("sort"))
	c, ok := s.sftpClient(w, p)
	if !ok {
		return
	}
	infos, err := c.ReadDir(dir)
	if err != nil {
		s.sftpPool.Drop(p.sshConfig()) // возможно, соединение мёртвое — не кешируем
		writeSFTPErr(w, err, "sftp list")
		return
	}
	total := 0
	entries := make([]sftpEntryJSON, 0, len(infos))
	for _, fi := range infos {
		hidden := strings.HasPrefix(fi.Name(), ".")
		if hidden && !showHidden {
			continue
		}
		total++
		isDir := fi.IsDir()
		if fi.Mode()&os.ModeSymlink != 0 {
			if target, statErr := c.Stat(urlpath.Join(dir, fi.Name())); statErr == nil {
				isDir = target.IsDir()
			}
		}
		entries = append(entries, sftpEntryJSON{
			Name:        fi.Name(),
			Size:        fi.Size(),
			IsDir:       isDir,
			ModTime:     fi.ModTime().UTC().Format(time.RFC3339),
			Permissions: fi.Mode().String(),
			Hidden:      hidden,
			mtime:       fi.ModTime().Unix(),
		})
	}
	// ReadDir уже отсортировал по имени, поэтому стабильная сортировка сохраняет
	// алфавит внутри групп при любом режиме.
	sortSFTPEntries(entries, mode)
	if len(entries) > maxSFTPListEntries {
		entries = entries[:maxSFTPListEntries]
	}
	cwd := dir
	if dir == "." {
		if remoteCWD, cwdErr := c.Getwd(); cwdErr == nil && remoteCWD != "" {
			cwd = remoteCWD
		}
	}
	resp := map[string]any{
		"entries": entries, "hidden": showHidden, "cwd": cwd,
		"total": total, "truncated": total > len(entries),
		"sort": mode.String(),
	}
	if remembered, applicable := s.rememberSFTPSecret(p, queryFlag(q.Get("remember"))); applicable {
		resp["secret_remembered"] = remembered
	}
	jsonResp(w, resp)
}

// sortSFTPEntries — папки первыми (как рисует клиент), внутри группы: имя по
// алфавиту, дата и размер — по убыванию.
func sortSFTPEntries(entries []sftpEntryJSON, mode filesSortMode) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := &entries[i], &entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		switch mode {
		case filesSortDate:
			if a.mtime != b.mtime {
				return a.mtime > b.mtime
			}
		case filesSortSize:
			if a.Size != b.Size {
				return a.Size > b.Size
			}
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
}

// rememberSFTPSecret — «помнить до перезапуска ПК» для сохранённого хоста.
// Возвращает (лежит ли секрет в памяти агента, применим ли признак вообще).
// Для хоста «на один раз» (без host_id) хранить нечего — applicable=false, и
// клиент не рисует несуществующее состояние.
func (s *Server) rememberSFTPSecret(p sftpConnParams, remember bool) (remembered, applicable bool) {
	if p.hostID == "" || s == nil || s.sshSecrets == nil {
		return false, false
	}
	if _, ok := s.resolveSSHHost(p.hostID); !ok {
		return false, false
	}
	if remember && (p.password != "" || p.keyPassphrase != "" || p.proxyPassword != "") {
		cur, _ := s.sshSecrets.get(p.hostID)
		if p.password != "" {
			cur.password = p.password
		}
		if p.keyPassphrase != "" {
			cur.keyPassphrase = p.keyPassphrase
		}
		if p.proxyPassword != "" {
			cur.proxyPassword = p.proxyPassword
		}
		// «Помнить» из файлов сервера значит то же, что и из терминала:
		// пароль переживает перезапуск. Иначе одна и та же галочка в двух
		// местах продукта означала бы разное.
		cur.persist = true
		s.sshSecrets.set(p.hostID, cur)
	}
	_, ok := s.sshSecretFor(p.hostID)
	return ok, true
}

// GET /api/ssh/sftp/preview — bounded text preview for the server file card.
// Binary files return metadata only; images are downloaded through the regular
// chunked endpoint so this handler never buffers a large file.
func (s *Server) apiSFTPPreview(w http.ResponseWriter, r *http.Request, uid int64) {
	p, err := sftpParamsFromQuery(r)
	if err != nil {
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return
	}
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		jsonErrorCode(w, 400, "bad_request", "path is required", nil)
		return
	}
	c, ok := s.sftpClient(w, p)
	if !ok {
		return
	}
	f, err := c.Open(filePath)
	if err != nil {
		writeSFTPErr(w, err, "sftp preview")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeSFTPErr(w, err, "sftp stat")
		return
	}
	if st.IsDir() {
		jsonErrorCode(w, 400, "is_dir", "path is a directory", nil)
		return
	}
	limit := int64(maxSFTPPreviewBytes)
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		writeSFTPErr(w, err, "sftp preview")
		return
	}
	truncated := len(data) > maxSFTPPreviewBytes
	if truncated {
		data = data[:maxSFTPPreviewBytes]
	}
	contentType := http.DetectContentType(data)
	text := utf8.Valid(data) && !bytes.Contains(data, []byte{0})
	out := map[string]any{
		"kind": "binary", "content_type": contentType, "size": st.Size(),
		"truncated": truncated,
	}
	if text {
		out["kind"] = "text"
		out["content"] = string(data)
	}
	jsonResp(w, out)
}

// ── download ───────────────────────────────────────────────────────

// sftpDownloadReq — разобранный запрос скачивания (общий для GET и POST).
type sftpDownloadReq struct {
	params sftpConnParams
	path   string
	// chunked=true — отдать кусок [offset, offset+length).
	chunked bool
	offset  int64
	length  int64
	// expectSize/expectMtime — то, что клиент видел в листинге. Ноль = не
	// проверять. Нужны для целостности между кусками: файл могут дописать или
	// усечь, и без сверки склеится МОЛЧА битый архив.
	expectSize  int64
	expectMtime int64
}

// clampSFTPRange проверяет и нормализует границы куска.
func clampSFTPRange(offset, length, size int64) (int64, int64, error) {
	if offset < 0 || offset > size {
		return 0, 0, fmt.Errorf("bad offset")
	}
	if length <= 0 {
		return 0, 0, fmt.Errorf("bad len")
	}
	if length > maxSFTPDownloadChunk {
		return 0, 0, fmt.Errorf("chunk too large (max %d bytes)", maxSFTPDownloadChunk)
	}
	if rest := size - offset; length > rest {
		length = rest
	}
	return offset, length, nil
}

// GET /api/ssh/sftp/download?host&port&user&path[&offset&len&expect_size&expect_mtime]
// — legacy-путь: пароль в query. Оставлен ради старых клиентов и прямых ссылок;
// новый клиент ходит POST'ом (см. apiSFTPDownloadPost).
func (s *Server) apiSFTPDownload(w http.ResponseWriter, r *http.Request, uid int64) {
	p, err := sftpParamsFromQuery(r)
	if err != nil {
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return
	}
	q := r.URL.Query()
	filePath := q.Get("path")
	if filePath == "" {
		jsonErrorCode(w, 400, "bad_request", "path is required", nil)
		return
	}
	req := sftpDownloadReq{params: p, path: filePath}
	if q.Get("offset") != "" || q.Get("len") != "" {
		// offset можно опустить (значит 0) — ровно как у локального
		// /api/files/download: клиент чанкования один и тот же.
		var off int64
		if raw := q.Get("offset"); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				jsonErrorCode(w, 416, "bad_range", "bad offset", nil)
				return
			}
			off = v
		}
		n, err := strconv.ParseInt(q.Get("len"), 10, 64)
		if err != nil {
			jsonErrorCode(w, 416, "bad_range", "len required when offset is set", nil)
			return
		}
		req.chunked, req.offset, req.length = true, off, n
	}
	req.expectSize, _ = strconv.ParseInt(q.Get("expect_size"), 10, 64)
	req.expectMtime, _ = strconv.ParseInt(q.Get("expect_mtime"), 10, 64)
	s.serveSFTPDownload(w, r, req)
}

// POST /api/ssh/sftp/download
// {host,port,user,password,trust_host,path,offset?,len?,expect_size?,expect_mtime?}
// — байты файла (или его куска). Пароль в теле: при 25 кусках query-вариант
// оставил бы его в access-логах 25 раз.
func (s *Server) apiSFTPDownloadPost(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		sftpBody
		Offset      int64 `json:"offset"`
		Len         int64 `json:"len"`
		ExpectSize  int64 `json:"expect_size"`
		ExpectMtime int64 `json:"expect_mtime"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	p := body.params()
	if err := p.validate(); err != nil {
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return
	}
	if body.Path == "" {
		jsonErrorCode(w, 400, "bad_request", "path is required", nil)
		return
	}
	s.serveSFTPDownload(w, r, sftpDownloadReq{
		params:      p,
		path:        body.Path,
		chunked:     body.Len > 0,
		offset:      body.Offset,
		length:      body.Len,
		expectSize:  body.ExpectSize,
		expectMtime: body.ExpectMtime,
	})
}

func (s *Server) serveSFTPDownload(w http.ResponseWriter, r *http.Request, req sftpDownloadReq) {
	c, ok := s.sftpClient(w, req.params)
	if !ok {
		return
	}
	f, err := c.Open(req.path)
	if err != nil {
		writeSFTPErr(w, err, "sftp open")
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		writeSFTPErr(w, err, "sftp stat")
		return
	}
	if st.IsDir() {
		jsonErrorCode(w, 400, "is_dir", "path is a directory", nil)
		return
	}
	size := st.Size()
	mtime := st.ModTime().Unix()

	// Целостность между кусками: клиент передаёт то, что видел в листинге.
	if (req.expectSize > 0 && req.expectSize != size) || (req.expectMtime > 0 && req.expectMtime != mtime) {
		jsonErrorCode(w, 409, "file_changed", "file changed during download", map[string]string{
			"size":  strconv.FormatInt(size, 10),
			"mtime": strconv.FormatInt(mtime, 10),
		})
		return
	}

	// Заголовки размера/времени полезны и в LAN кросс-origin — без
	// Expose-Headers браузер их не отдаст скрипту. X-Chunk-* — те же, что у
	// локального /api/files/download: по ним клиент сверяет границы куска.
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, "+fileMetaExposeHeaders)
	w.Header().Set("X-File-Size", strconv.FormatInt(size, 10))
	w.Header().Set("X-File-Mtime", strconv.FormatInt(mtime, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", urlpath.Base(req.path)))
	w.Header().Set("Content-Type", "application/octet-stream")

	if !req.chunked {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		// io.Copy выберет (*sftp.File).WriteTo — конкурентное чтение окнами.
		if _, err := io.Copy(w, f); err != nil {
			// Заголовки уже ушли — клиент увидит обрыв; соединение могло
			// умереть, его не кешируем. Пароля в err нет.
			s.dropSFTPUnlessClientGone(r, req.params)
		}
		return
	}

	off, n, err := clampSFTPRange(req.offset, req.length, size)
	if err != nil {
		jsonErrorCode(w, 416, "bad_range", err.Error(), nil)
		return
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		writeSFTPErr(w, err, "sftp seek")
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	w.Header().Set("X-Chunk-Offset", strconv.FormatInt(off, 10))
	w.Header().Set("X-Chunk-Len", strconv.FormatInt(n, 10))
	// Кусок нельзя класть в кеш как файл целиком.
	w.Header().Set("Cache-Control", "no-store")
	// Буфер 512 КБ: (*sftp.File).Read дробит его на конкурентные пакеты, иначе
	// кусок поедет по 32 КБ за круговую задержку. Обёртка struct{io.Writer}
	// прячет ReadFrom у http.ResponseWriter, чтобы буфер точно применился.
	buf := make([]byte, 512<<10)
	if _, err := io.CopyBuffer(struct{ io.Writer }{w}, io.LimitReader(f, n), buf); err != nil {
		s.dropSFTPUnlessClientGone(r, req.params)
	}
}

// dropSFTPUnlessClientGone выбрасывает соединение из пула только если оборвался
// НЕ клиент. Отмена скачивания (кнопка «Отмена» → AbortController) рвёт запись в
// ResponseWriter точно так же, как мёртвый SSH, и без этой проверки каждая
// отмена стоила бы нового SSH-хендшейка на следующем куске (N106).
func (s *Server) dropSFTPUnlessClientGone(r *http.Request, p sftpConnParams) {
	if r != nil && r.Context().Err() != nil {
		return
	}
	s.sftpPool.Drop(p.sshConfig())
}

// ── upload ─────────────────────────────────────────────────────────

// sizedReader сообщает (*sftp.File).ReadFrom размер данных: без него remain
// остаётся нулём и конкурентная запись не включается (см. ssh_transfers.go).
type sizedReader struct {
	r    io.Reader
	size int64
}

func (s sizedReader) Read(b []byte) (int, error) { return s.r.Read(b) }
func (s sizedReader) Size() int64                { return s.size }

// sftpFormFiles собирает ВСЕ файлы формы (а не только первый): поле "file"
// первым, остальные — по алфавиту полей, чтобы порядок был воспроизводимым.
func sftpFormFiles(form *multipart.Form) []*multipart.FileHeader {
	if form == nil {
		return nil
	}
	keys := make([]string, 0, len(form.File))
	for k := range form.File {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if (keys[i] == "file") != (keys[j] == "file") {
			return keys[i] == "file"
		}
		return keys[i] < keys[j]
	})
	out := make([]*multipart.FileHeader, 0, len(keys))
	for _, k := range keys {
		out = append(out, form.File[k]...)
	}
	return out
}

// sanitizeRemoteName — только базовое имя (никаких ../ в пути на сервере).
func sanitizeRemoteName(name string) string {
	base := urlpath.Base(strings.ReplaceAll(name, `\`, "/"))
	switch base {
	case ".", "..", "/", "":
		return ""
	}
	return base
}

// POST /api/ssh/sftp/upload?host&port&user&path[&overwrite=1] — multipart file
// в каталог path. Поддерживает чанкование (upload_id/chunk/chunks) как
// /api/files/upload: куски складываются в .part на агенте, последний кусок
// заливает файл на сервер; ответ на каждый кусок — с chunk_ack.
//
// Обычная (не чанкованная) загрузка обходит ВСЕ файлы формы. Раньше грузился
// только первый, а UI рапортовал «Загружено 5 файлов» (#11); теперь ответ
// честный: {"ok":…, "files":[…], "failed":[{"name","code"}]}.
//
// overwrite (N107): без него одноимённый файл на сервере НЕ затирается —
// c.Create молча обрезает цель, а корзины на сервере нет. Отдаём
// already_exists, клиент спрашивает человека.
func (s *Server) apiSFTPUpload(w http.ResponseWriter, r *http.Request, uid int64) {
	p, err := sftpParamsFromQuery(r)
	if err != nil {
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		jsonErrorCode(w, 400, "bad_request", "path is required", nil)
		return
	}
	overwrite := uploadOverwriteQuery(r.URL.Query())
	cp, chunked, err := parseChunkParams(r.URL.Query())
	if err != nil {
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return
	}
	if err := r.ParseMultipartForm(100 << 20); err != nil || r.MultipartForm == nil {
		jsonErrorCode(w, 400, "bad_request", "invalid upload", nil)
		return
	}
	fhs := sftpFormFiles(r.MultipartForm)
	if len(fhs) == 0 {
		jsonErrorCode(w, 400, "bad_request", "file required", nil)
		return
	}

	// Чанкованная загрузка большого файла — строго по одному файлу за запрос
	// (так её и шлёт клиент): копим .part на агенте, заливаем на последнем куске.
	if chunked {
		fh := fhs[0]
		safeName := sanitizeRemoteName(fh.Filename)
		if safeName == "" {
			jsonErrorCode(w, 400, "bad_filename", "bad filename", nil)
			return
		}
		src, err := fh.Open()
		if err != nil {
			jsonErrorCode(w, 500, "io_error", "failed to read chunk", nil)
			return
		}
		defer src.Close()
		// Спрашиваем про перезапись на ПЕРВОМ куске, а не после заливки
		// гигабайта: пул соединений уже прогрет листингом, проверка бесплатная.
		if !overwrite && cp.index == 0 {
			c, ok := s.sftpClient(w, p)
			if !ok {
				return
			}
			if sftpNameTaken(c, dir, safeName) {
				jsonErrorCode(w, 409, "already_exists", "remote file already exists", nil)
				return
			}
		}
		part, final, err := appendChunk(cp, src)
		if err != nil {
			code, status := localFSErrCode(err)
			jsonErrorCode(w, status, code, "failed to save chunk: "+err.Error(), nil)
			return
		}
		if !final {
			jsonResp(w, map[string]any{"ok": true, "chunk_ack": true})
			return
		}
		// Повторная проверка перед самой записью: между первым и последним куском
		// файл на сервере мог появиться. .part оставляем на месте (уборка через
		// сутки), чтобы повтор с overwrite=1 не гнал файл заново.
		if !overwrite {
			c, ok := s.sftpClient(w, p)
			if !ok {
				return
			}
			if sftpNameTaken(c, dir, safeName) {
				jsonErrorCode(w, 409, "already_exists", "remote file already exists", nil)
				return
			}
		}
		defer abortChunkUpload(cp.id) // .part на агенте больше не нужен
		partFile, err := os.Open(part)
		if err != nil {
			code, status := localFSErrCode(err)
			jsonErrorCode(w, status, code, "failed to assemble file: "+err.Error(), nil)
			return
		}
		defer partFile.Close()
		partSize := int64(0)
		if st, err := partFile.Stat(); err == nil {
			partSize = st.Size()
		}
		c, ok := s.sftpClient(w, p)
		if !ok {
			return
		}
		if err := s.sftpUploadOne(c, p, dir, safeName, partFile, partSize); err != nil {
			writeSFTPErr(w, err, "sftp upload")
			return
		}
		jsonResp(w, map[string]any{"ok": true, "files": []string{safeName}, "failed": []any{}, "chunk_ack": true})
		return
	}

	c, ok := s.sftpClient(w, p)
	if !ok {
		return
	}
	saved := make([]string, 0, len(fhs))
	failed := make([]map[string]string, 0)
	firstStatus := 500
	for _, fh := range fhs {
		safeName := sanitizeRemoteName(fh.Filename)
		if safeName == "" {
			failed = append(failed, map[string]string{"name": fh.Filename, "code": "bad_filename"})
			if len(failed) == 1 {
				firstStatus = 400
			}
			continue
		}
		if !overwrite && sftpNameTaken(c, dir, safeName) {
			// Затирание чужого файла на сервере необратимо — спрашиваем (N107).
			if len(failed) == 0 {
				firstStatus = 409
			}
			failed = append(failed, map[string]string{"name": safeName, "code": "already_exists"})
			continue
		}
		src, err := fh.Open()
		if err != nil {
			log.Printf("[SFTP] upload open %q: %v", safeName, err)
			failed = append(failed, map[string]string{"name": safeName, "code": "io_error"})
			continue
		}
		err = s.sftpUploadOne(c, p, dir, safeName, src, fh.Size)
		src.Close()
		if err != nil {
			code, status := sftpErrCode(err)
			if len(failed) == 0 {
				firstStatus = status
			}
			failed = append(failed, map[string]string{"name": safeName, "code": code})
			continue
		}
		saved = append(saved, safeName)
	}

	if len(saved) == 0 {
		// Полный отказ отдаём ошибкой: 200 с пустым files старый клиент
		// показал бы как «Загружено».
		code := "io_error"
		if len(failed) > 0 {
			code = failed[0]["code"]
		}
		sshJSONStatus(w, firstStatus, map[string]any{
			"error": "sftp upload failed", "code": code,
			"files": saved, "failed": failed,
		})
		return
	}
	jsonResp(w, map[string]any{"ok": len(failed) == 0, "files": saved, "failed": failed})
}

// sftpNameTaken — есть ли уже такой объект в каталоге на сервере. Проверка ДО
// записи: c.Create молча обрезает существующий файл, а корзины на сервере нет
// (N107). Гонка «файл создали ровно между Stat и Create» теоретически есть, но
// SFTP не даёт надёжного O_EXCL на всех серверах, а молчаливое затирание — вред
// гарантированный.
//
// Lstat, а не Stat: имя может быть симлинком (тот же ~/.bashrc или
// /var/www/html на релиз). Stat идёт ПО ссылке и на битой ссылке отвечает
// «свободно» — тогда запись уходит сквозь неё в чужой файл, о котором человека
// никто не спросил. Про симлинк спрашиваем так же, как про обычный файл.
func sftpNameTaken(c *sftp.Client, dir, name string) bool {
	_, err := c.Lstat(urlpath.Join(dir, name))
	return err == nil
}

// sftpUploadOne льёт src в dir/name на сервере. size — размер данных (нужен
// для конкурентной записи). При ошибке недописанный файл удаляется: при
// конкурентной записи он может остаться длиннее записанного и с «дырами».
func (s *Server) sftpUploadOne(c *sftp.Client, p sftpConnParams, dir, name string, src io.Reader, size int64) error {
	remote := urlpath.Join(dir, name)
	dst, err := c.Create(remote)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, sizedReader{r: src, size: size}); err != nil {
		dst.Close()
		_ = c.Remove(remote)
		s.sftpPool.Drop(p.sshConfig())
		return err
	}
	if err := dst.Close(); err != nil {
		_ = c.Remove(remote)
		return err
	}
	return nil
}

// ── mkdir / delete / rename ────────────────────────────────────────

// POST /api/ssh/sftp/mkdir {host,port,user,path} — создать каталог.
// MkdirAll: повторный mkdir существующего — не ошибка (mkdir -p семантика,
// иначе пользователь спотыкается о «уже есть» при каждой загрузке в ту же папку).
func (s *Server) apiSFTPMkdir(w http.ResponseWriter, r *http.Request, uid int64) {
	s.sftpPathOp(w, r, func(c *sftp.Client, body sftpBody) error {
		return c.MkdirAll(body.Path)
	})
}

// POST /api/ssh/sftp/delete {host,port,user,path,recursive?} — удалить файл
// или каталог. Непустой каталог без recursive:true отдаёт машинный код
// not_empty (409), а не 500 с сырым SSH-текстом (#43).
func (s *Server) apiSFTPDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	s.sftpPathOp(w, r, func(c *sftp.Client, body sftpBody) error {
		st, err := c.Stat(body.Path)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return c.Remove(body.Path)
		}
		entries, err := c.ReadDir(body.Path)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return c.RemoveDirectory(body.Path)
		}
		if !body.Recursive {
			return errDirNotEmpty
		}
		if isRemoteRootish(body.Path) {
			return errRefuseRemoteRoot
		}
		return c.RemoveAll(body.Path)
	})
}

// isRemoteRootish — путь, рекурсивное удаление которого почти наверняка
// катастрофа: корень, «~», «.» или каталог первого уровня («/etc», «/home»).
// Файловому менеджеру с телефона такие операции не нужны.
func isRemoteRootish(p string) bool {
	clean := urlpath.Clean(strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"))
	switch clean {
	case "", ".", "..", "/", "~", "~/":
		return true
	}
	if strings.HasPrefix(clean, "/") && !strings.Contains(strings.TrimPrefix(clean, "/"), "/") {
		return true // «/etc», «/home», «/var»
	}
	return false
}

// sftpPathOp — общий каркас для mkdir/delete: тело, валидация, клиент, ошибки.
func (s *Server) sftpPathOp(w http.ResponseWriter, r *http.Request, op func(c *sftp.Client, body sftpBody) error) {
	var body sftpBody
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	p := body.params()
	if err := p.validate(); err != nil {
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return
	}
	if body.Path == "" {
		jsonErrorCode(w, 400, "bad_request", "path is required", nil)
		return
	}
	c, ok := s.sftpClient(w, p)
	if !ok {
		return
	}
	if err := op(c, body); err != nil {
		writeSFTPErr(w, err, "sftp")
		return
	}
	jsonResp(w, map[string]any{"ok": true})
}

// POST /api/ssh/sftp/rename {host,port,user,from,to} — переименовать/переместить.
func (s *Server) apiSFTPRename(w http.ResponseWriter, r *http.Request, uid int64) {
	var body sftpBody
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	p := body.params()
	if err := p.validate(); err != nil {
		jsonErrorCode(w, 400, "bad_request", err.Error(), nil)
		return
	}
	if body.From == "" || body.To == "" {
		jsonErrorCode(w, 400, "bad_request", "from and to are required", nil)
		return
	}
	c, ok := s.sftpClient(w, p)
	if !ok {
		return
	}
	if err := c.Rename(body.From, body.To); err != nil {
		writeSFTPErr(w, err, "sftp rename")
		return
	}
	jsonResp(w, map[string]any{"ok": true})
}
