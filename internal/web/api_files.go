package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

// realUserHome returns os.UserHomeDir() unless we're running under a Windows
// service profile (LocalSystem → C:\Windows\System32\config\systemprofile).
// In that case it picks the first real user under C:\Users that looks active
// (has .claude or npm install). This keeps UX (Quick Paths, Recent Folders,
// project scans) pointed at what the user actually has on their machine.
func realUserHome() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS != "windows" {
		return home
	}
	lc := strings.ToLower(home)
	if home != "" && !strings.Contains(lc, `\systemprofile`) && !strings.HasPrefix(lc, `c:\windows\`) {
		return home
	}
	entries, err := os.ReadDir(`C:\Users`)
	if err != nil {
		return home
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		switch name {
		case "Public", "Default", "Default User", "All Users":
			continue
		}
		userDir := filepath.Join(`C:\Users`, name)
		for _, marker := range []string{
			filepath.Join(userDir, ".claude"),
			filepath.Join(userDir, "AppData", "Roaming", "npm"),
		} {
			if info, err := os.Stat(marker); err == nil && info.IsDir() {
				return userDir
			}
		}
	}
	return home
}

// isServiceProfilePath reports whether path sits inside Windows system dirs
// that should never surface to the user (systemprofile, C:\Windows\*).
func isServiceProfilePath(p string) bool {
	if p == "" {
		return false
	}
	lc := strings.ToLower(p)
	return strings.Contains(lc, `\systemprofile`) || strings.HasPrefix(lc, `c:\windows\`)
}

// allowedRoots returns the set of root directories the file manager may access.
// On Windows every existing drive letter root is included; on Linux / macOS the
// filesystem root "/" is used.  The user's home directory is always allowed.
func allowedRoots() []string {
	roots := make([]string, 0, 4)
	home, _ := os.UserHomeDir()
	if home != "" {
		roots = append(roots, filepath.Clean(home))
	}
	if runtime.GOOS == "windows" {
		for c := 'A'; c <= 'Z'; c++ {
			drive := fmt.Sprintf("%c:\\", c)
			if _, err := os.Stat(drive); err == nil {
				roots = append(roots, filepath.Clean(drive))
			}
		}
	} else {
		roots = append(roots, "/")
	}
	cwd, _ := os.Getwd()
	if cwd != "" {
		roots = append(roots, filepath.Clean(cwd))
	}
	return roots
}

// Сентинелы validatePath: по ним validPathOrFail отличает «путь вне корней»
// (403 outside_roots) от «путь не передали / кривой» (400 bad_path). Тексты
// сохранены прежними — их видят логи и старые клиенты.
var (
	errPathRequired = errors.New("path required")
	errPathInvalid  = errors.New("invalid path")
	errPathOutside  = errors.New("access denied: path outside allowed directories")
)

// validatePath sanitises a user-supplied path and ensures it resides under one
// of the allowed roots.  Returns the cleaned absolute path or an error.
func validatePath(raw string) (string, error) {
	if raw == "" {
		return "", errPathRequired
	}
	cleaned := filepath.Clean(raw)
	if !filepath.IsAbs(cleaned) {
		abs, err := filepath.Abs(cleaned)
		if err != nil {
			return "", errPathInvalid
		}
		cleaned = abs
	}
	// Resolve symlinks where the target exists so a link can't smuggle a path
	// outside an allowed root past the containment check.
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		cleaned = resolved
	}
	for _, root := range allowedRoots() {
		if pathWithinRoot(cleaned, root) {
			return cleaned, nil
		}
	}
	return "", errPathOutside
}

// pathWithinRoot reports whether path is root itself or a descendant of it,
// using a boundary-aware comparison (so "/home/bob" does not match
// "/home/bobby" the way a raw HasPrefix would). Case-insensitive on Windows.
func pathWithinRoot(path, root string) bool {
	p, r := path, root
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
		r = strings.ToLower(r)
	}
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

var textExts = map[string]bool{
	".txt": true, ".md": true, ".py": true, ".js": true, ".ts": true,
	".tsx": true, ".jsx": true, ".json": true, ".yml": true, ".yaml": true,
	".toml": true, ".cfg": true, ".ini": true, ".csv": true, ".xml": true,
	".html": true, ".css": true, ".sh": true, ".bat": true, ".cmd": true,
	".ps1": true, ".env": true, ".gitignore": true, ".log": true, ".sql": true,
	".rs": true, ".go": true, ".java": true, ".c": true, ".cpp": true,
	".h": true, ".hpp": true, ".rb": true, ".php": true, ".lua": true,
	".r": true, ".m": true, ".swift": true, ".kt": true, ".scala": true,
	".dockerfile": true, ".makefile": true, ".conf": true, ".properties": true,
	".mod": true, ".sum": true,
}

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".bmp": true, ".svg": true, ".ico": true,
}

const (
	// filesListLimit — потолок числа записей в одном ответе (находка #40).
	// node_modules/System32 раньше уезжали в JSON целиком: в облаке этот JSON
	// живёт в памяти агента, релея и вкладки одновременно.
	filesListLimit    = 2000
	filesListMaxLimit = 5000
)

// isJunkEntry — реальный мусор ФС, который не показываем НИКОГДА, даже при
// hidden=1: служебные контейнеры, куда пользователь всё равно не войдёт.
func isJunkEntry(name string) bool {
	switch strings.ToLower(name) {
	case "system volume information", "$recycle.bin", "recycler",
		"$winreagent", "$sysreset", "$getcurrent":
		return true
	}
	return false
}

// isHiddenEntry — «скрытая» запись: точка в начале, служебный $-префикс
// Windows, системные папки корня диска. Показывается при hidden=1.
// ГРАБЛЯ, которую это чинит (находка #13): раньше фильтр был безусловным, и
// .env / .ssh / .gitignore были НЕДОСТИЖИМЫ в файловом менеджере, хотя
// SFTP-браузер соседним экраном их спокойно показывает.
func isHiddenEntry(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
		return true
	}
	switch strings.ToLower(name) {
	case "recovery", "thumbs.db", "desktop.ini":
		return true
	}
	return false
}

// queryFlag — «1/true/yes/on» в query как булев флаг.
func queryFlag(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// queryLimit — необязательный ?limit= с потолком; при кривом значении молча
// остаётся дефолт (лимит — защита сервера, а не часть контракта запроса).
func queryLimit(v string, def, max int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// filesSortMode — порядок записей каталога (?sort=name|date|size).
//
// ЗАЧЕМ (находка N20). Сервер сортировал ВСЕГДА по алфавиту и лишь потом резал
// лимит в 2000 записей, а сортировку «По дате»/«По размеру» делал клиент —
// поверх обрезка. В папке на 3000 файлов кнопка «По дате» обещала показать
// вчерашнее, а сортировала произвольную алфавитную выборку, в которую вчерашний
// «фото.jpg» не попадал вовсе. Теперь порядок задаёт запрос, сортируются ВСЕ
// записи, и лимит режет уже правильный порядок — «первые 2000» становятся
// осмысленными.
type filesSortMode int

const (
	filesSortName filesSortMode = iota
	filesSortDate
	filesSortSize
)

func (m filesSortMode) String() string {
	switch m {
	case filesSortDate:
		return "date"
	case filesSortSize:
		return "size"
	}
	return "name"
}

// parseFilesSort — режим из query. Неизвестное значение молча становится
// «по имени»: порядок — не то, из-за чего стоит отказывать в листинге.
func parseFilesSort(v string) filesSortMode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "date", "modified", "mtime":
		return filesSortDate
	case "size":
		return filesSortSize
	}
	return filesSortName
}

// fileRow — запись каталога с ленивым stat'ом. Info() на Linux — сисколл на
// каждый файл, поэтому при сортировке по имени статим только то, что реально
// уедет клиенту, а при сортировке по дате/размеру — всё (иначе сортировать
// нечем).
type fileRow struct {
	entry   fs.DirEntry
	name    string
	isDir   bool
	size    int64
	mtime   int64
	statted bool
	hasInfo bool
}

func (r *fileRow) stat() {
	if r.statted {
		return
	}
	r.statted = true
	info, err := r.entry.Info()
	if err != nil {
		return
	}
	r.hasInfo = true
	if !r.isDir {
		r.size = info.Size()
	}
	r.mtime = info.ModTime().Unix()
}

// sortFileRows — папки всегда первыми (как рисует клиент), внутри группы —
// выбранный порядок: имя по алфавиту, дата и размер — по убыванию (свежее и
// крупное сверху — именно за этим их и включают).
func sortFileRows(rows []fileRow, mode filesSortMode) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if a.isDir != b.isDir {
			return a.isDir
		}
		switch mode {
		case filesSortDate:
			if a.mtime != b.mtime {
				return a.mtime > b.mtime
			}
		case filesSortSize:
			if a.size != b.size {
				return a.size > b.size
			}
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
}

func (s *Server) apiFilesList(w http.ResponseWriter, r *http.Request, uid int64) {
	q := r.URL.Query()
	path, ok := validPathOrFail(w, q.Get("path"))
	if !ok {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		jsonFSError(w, err, "open directory")
		return
	}
	if !info.IsDir() {
		jsonErrorCode(w, fsStatus(fsCodeNotDir), fsCodeNotDir, "not a directory", nil)
		return
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		jsonFSError(w, err, "read directory")
		return
	}

	showHidden := queryFlag(q.Get("hidden"))
	limit := queryLimit(q.Get("limit"), filesListLimit, filesListMaxLimit)
	mode := parseFilesSort(q.Get("sort"))

	// Фильтруем ДО сортировки и обрезки: total — это число видимых записей.
	rows := make([]fileRow, 0, len(entries))
	hiddenSkipped := 0
	for _, e := range entries {
		name := e.Name()
		if isJunkEntry(name) {
			continue
		}
		if isHiddenEntry(name) && !showHidden {
			hiddenSkipped++
			continue
		}
		rows = append(rows, fileRow{entry: e, name: name, isDir: e.IsDir()})
	}
	total := len(rows)
	if mode != filesSortName {
		for i := range rows {
			rows[i].stat()
		}
	}
	sortFileRows(rows, mode)
	// Обрезаем ПОСЛЕ сортировки — иначе пользователь получил бы 2000
	// произвольных имён и решил, что файлы пропали.
	if len(rows) > limit {
		rows = rows[:limit]
	}

	items := make([]map[string]any, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		row.stat()
		item := map[string]any{
			"name":   row.name,
			"is_dir": row.isDir,
			"path":   filepath.Join(path, row.name),
		}
		if row.hasInfo {
			if !row.isDir {
				item["size"] = row.size
			}
			item["modified"] = float64(row.mtime)
		}
		if isHiddenEntry(row.name) {
			item["hidden"] = true
		}
		items = append(items, item)
	}

	parent := filepath.Dir(path)
	jsonResp(w, map[string]any{
		"path": path, "items": items, "parent": parent,
		"total": total, "truncated": total > len(items),
		"hidden": showHidden, "hidden_skipped": hiddenSkipped,
		// sort — эхо применённого порядка: по нему клиент знает, что обрезка
		// «первые 2000» уже согласована с выбранной кнопкой сортировки.
		"sort": mode.String(),
	})
}

func (s *Server) apiQuickPaths(w http.ResponseWriter, r *http.Request, uid int64) {
	home := realUserHome()
	paths := make([]map[string]string, 0)

	// Use the exe directory as CWD instead of the service's Getwd() (which
	// points at C:\Windows\System32 under LocalSystem and is useless to users).
	var cwd string
	if exe, err := os.Executable(); err == nil {
		cwd = filepath.Dir(exe)
	} else {
		cwd, _ = os.Getwd()
	}

	for _, sub := range []struct{ name, dir string }{
		{"Desktop", "Desktop"}, {"Downloads", "Downloads"},
		{"Documents", "Documents"}, {"Home", ""},
	} {
		p := home
		if sub.dir != "" {
			p = filepath.Join(home, sub.dir)
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			paths = append(paths, map[string]string{"name": sub.name, "path": p})
		}
	}

	// Drive roots on Windows
	if runtime.GOOS == "windows" {
		for c := 'A'; c <= 'Z'; c++ {
			drive := fmt.Sprintf("%c:\\", c)
			if _, err := os.Stat(drive); err == nil {
				paths = append(paths, map[string]string{
					"name": fmt.Sprintf("%c:", c), "path": drive,
				})
			}
		}
	} else {
		seen := make(map[string]bool, len(paths))
		for _, item := range paths {
			seen[item["path"]] = true
		}
		add := func(name, path string) {
			if seen[path] {
				return
			}
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				seen[path] = true
				paths = append(paths, map[string]string{"name": name, "path": path})
			}
		}
		for _, item := range []struct{ name, path string }{
			{"Корень", "/"}, {"opt", "/opt"}, {"srv", "/srv"},
			{"var/www", "/var/www"}, {"etc", "/etc"},
		} {
			add(item.name, item.path)
		}
		if users, err := os.ReadDir("/home"); err == nil {
			for _, user := range users {
				if user.IsDir() {
					add(user.Name(), filepath.Join("/home", user.Name()))
				}
			}
		}
	}

	// Папка программы — ПОСЛЕДНЕЙ. Раньше она стояла первой, и первый заход в
	// «Файлы» открывал каталог установки Remotai (remotai.exe, wintun.dll,
	// logs): человек, поставивший программу «чтобы дотянуться до своих файлов»,
	// видел служебное содержимое вместо «Рабочего стола» и дисков.
	if cwd != "" && !isServiceProfilePath(cwd) {
		dup := false
		for _, item := range paths {
			if item["path"] == cwd {
				dup = true
				break
			}
		}
		if !dup {
			paths = append(paths, map[string]string{"name": "CWD", "path": cwd})
		}
	}

	jsonResp(w, map[string]any{"paths": paths})
}

// apiFileDownload — скачивание файла. Без offset/len поведение прежнее
// (http.ServeFile: полный файл, Range/206 и If-Modified-Since как раньше — на
// этом держатся LAN-путь <a download> и внешние потребители Range). С
// ?offset=&len= отдаёт один кусок (находка #10, подробности в
// download_chunks.go).
func (s *Server) apiFileDownload(w http.ResponseWriter, r *http.Request, uid int64) {
	q := r.URL.Query()
	path, ok := validPathOrFail(w, q.Get("path"))
	if !ok {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		jsonFSError(w, err, "open file")
		return
	}
	if info.IsDir() {
		jsonErrorCode(w, fsStatus(fsCodeIsDir), fsCodeIsDir, "path is a directory", nil)
		return
	}
	rng, chunked, err := parseDownloadRange(q, info.Size())
	if err != nil {
		jsonErrorCode(w, fsStatus(fsCodeBadRange), fsCodeBadRange, err.Error(), nil)
		return
	}
	// Файл дописали/усекли между кусками — честная ошибка вместо молча битого
	// файла на выходе.
	if !checkFileUnchanged(q, info) {
		jsonErrorCode(w, fsStatus(fsCodeChanged), fsCodeChanged,
			"file changed while downloading", fileChangedExtra(info))
		return
	}

	setFileMetaHeaders(w, info)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	if !chunked {
		http.ServeFile(w, r, path)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		jsonFSError(w, err, "open file")
		return
	}
	defer f.Close()
	if rng.offset > 0 {
		if _, err := f.Seek(rng.offset, io.SeekStart); err != nil {
			jsonFSError(w, err, "seek file")
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(rng.length, 10))
	w.Header().Set("X-Chunk-Offset", strconv.FormatInt(rng.offset, 10))
	w.Header().Set("X-Chunk-Len", strconv.FormatInt(rng.length, 10))
	// Кусок нельзя класть в кеш как файл целиком.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if rng.length == 0 {
		return
	}
	if _, err := io.CopyN(w, f, rng.length); err != nil {
		// Заголовки уже ушли — сказать клиенту нечем, он поймёт по короткому
		// телу (Content-Length не сошёлся).
		log.Printf("[FILES] download chunk %s@%d: %v", filepath.Base(path), rng.offset, err)
	}
}

// uploadOverwriteQuery — режим перезаписи загрузки (?overwrite=1).
//
// ЗАЧЕМ (находка N107). Раньше загрузка молча затирала одноимённый файл на ПК:
// «отчет.xlsx» с телефона уничтожал «отчет.xlsx», над которым человек работал,
// и рапортовал «Загружено 1 файл(ов)». Корзины нет, вернуть нельзя. Без
// overwrite=1 существующий файл теперь не трогаем и отвечаем already_exists —
// клиент спрашивает («Заменить?» / «Сохранить копию»). Клиентская сверка со
// списком папки для этого негодна: список обрезан лимитом и без «Скрытых» не
// содержит части имён, поэтому единственный надёжный предохранитель — O_EXCL
// на самой записи.
func uploadOverwriteQuery(q url.Values) bool {
	return queryFlag(q.Get("overwrite"))
}

// createUploadFile открывает целевой файл загрузки. 0o666 — как у os.Create
// (реальные права дорежет umask).
func createUploadFile(path string, overwrite bool) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	return os.OpenFile(path, flags, 0o666)
}

func (s *Server) apiFileUpload(w http.ResponseWriter, r *http.Request, uid int64) {
	maybeCleanupChunkUploads(time.Now())
	target, ok := validPathOrFail(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	overwrite := uploadOverwriteQuery(r.URL.Query())
	cp, chunked, err := parseChunkParams(r.URL.Query())
	if err != nil {
		// bad_upload_id — уже переводимый клиентом код «начните загрузку заново»;
		// сюда попадают все кривые параметры чанкования.
		jsonErrorCode(w, http.StatusBadRequest, "bad_upload_id", err.Error(), nil)
		return
	}

	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100 MB max in RAM, дальше диск
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "invalid upload", nil)
		return
	}
	if r.MultipartForm == nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "no files", nil)
		return
	}

	// Чанкованная загрузка большого файла (по одному файлу за запрос).
	if chunked {
		fhs := r.MultipartForm.File["file"]
		if len(fhs) == 0 {
			jsonErrorCode(w, http.StatusBadRequest, "bad_request", "file required", nil)
			return
		}
		fh := fhs[0]
		safeName := filepath.Base(fh.Filename)
		if safeName == "." || safeName == ".." || safeName == "" {
			jsonErrorCode(w, fsStatus(fsCodeBadPath), fsCodeBadPath, "bad filename", nil)
			return
		}
		// Спрашиваем про перезапись на ПЕРВОМ куске, а не после того, как человек
		// потратит мобильный трафик на все 500 МБ (N107). На резюме прерванной
		// загрузки проверка безвредна: итогового файла ещё нет, есть только .part.
		if !overwrite && cp.index == 0 {
			if _, statErr := os.Stat(filepath.Join(target, safeName)); statErr == nil {
				jsonErrorCode(w, fsStatus(fsCodeExists), fsCodeExists, "file already exists", nil)
				return
			}
		}
		src, err := fh.Open()
		if err != nil {
			jsonErrorCode(w, fsStatus(fsCodeIO), fsCodeIO, "failed to read chunk", nil)
			return
		}
		defer src.Close()
		part, final, err := appendChunk(cp, src)
		if err != nil {
			var offsetErr *chunkOffsetError
			if errors.As(err, &offsetErr) {
				jsonErrorCode(w, http.StatusConflict, "upload_offset", err.Error(), map[string]string{
					"want": strconv.FormatInt(offsetErr.Want, 10),
					"have": strconv.FormatInt(offsetErr.Have, 10),
				})
				return
			}
			// Причина отказа (нет места / нет прав) должна доехать машинным
			// кодом — иначе клиент показывает «Ошибка сервера» на полном диске.
			jsonFSError(w, err, "save chunk")
			return
		}
		if !final {
			jsonResp(w, map[string]any{"ok": true, "chunk_ack": true})
			return
		}
		if err := finishChunkUpload(part, filepath.Join(target, safeName), overwrite); err != nil {
			// already_exists здесь не потеря куска: .part остаётся на месте, и
			// повтор последнего куска с overwrite=1 достроит файл заново.
			jsonFSError(w, err, "assemble file")
			return
		}
		jsonResp(w, map[string]any{"ok": true, "files": []string{safeName}, "chunk_ack": true})
		return
	}

	saved := make([]string, 0)
	// failed — что именно не долетело и почему. Раньше ошибки молча пропускались
	// через continue, а файл с оборванным io.Copy всё равно попадал в saved:
	// клиент рапортовал «Загружено 5 файлов», когда на диске лежали два, причём
	// один из них — обрезанный.
	failed := make([]map[string]string, 0)
	firstStatus := 500

	for _, files := range r.MultipartForm.File {
		for _, fh := range files {
			// Sanitize filename to prevent path traversal (e.g. "../../etc/passwd")
			safeName := filepath.Base(fh.Filename)
			if safeName == "." || safeName == ".." || safeName == "" {
				failed = append(failed, map[string]string{"name": fh.Filename, "code": fsCodeBadPath})
				if len(failed) == 1 {
					firstStatus = fsStatus(fsCodeBadPath)
				}
				continue
			}
			src, err := fh.Open()
			if err != nil {
				log.Printf("[FILES] upload open %q: %v", safeName, err)
				failed = append(failed, map[string]string{"name": safeName, "code": fsCodeIO})
				continue
			}
			full := filepath.Join(target, safeName)
			dst, err := createUploadFile(full, overwrite)
			if err != nil {
				src.Close()
				code, status := fsErrorCode(err)
				if len(failed) == 0 {
					firstStatus = status
				}
				failed = append(failed, map[string]string{"name": safeName, "code": code})
				continue
			}
			_, cerr := io.Copy(dst, src)
			closeErr := dst.Close()
			src.Close()
			if cerr == nil {
				cerr = closeErr
			}
			if cerr != nil {
				// Обрывок хуже отсутствия: его легко принять за целый файл.
				log.Printf("[FILES] upload copy %q: %v", safeName, cerr)
				_ = os.Remove(full)
				code, status := fsErrorCode(cerr)
				if len(failed) == 0 {
					firstStatus = status
				}
				failed = append(failed, map[string]string{"name": safeName, "code": code})
				continue
			}
			saved = append(saved, safeName)
		}
	}

	if len(saved) == 0 && len(failed) > 0 {
		// Полный отказ отдаём ошибкой: 200 с пустым files старый клиент показал
		// бы как «Загружено».
		jsonErrorCode(w, firstStatus, failed[0]["code"], "upload failed", nil)
		return
	}
	jsonResp(w, map[string]any{"ok": len(failed) == 0, "files": saved, "failed": failed})
}

func (s *Server) apiFileUploadStatus(w http.ResponseWriter, r *http.Request, uid int64) {
	maybeCleanupChunkUploads(time.Now())
	id := r.URL.Query().Get("upload_id")
	size, updated, exists, err := chunkUploadStatus(id)
	if err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_upload_id", err.Error(), nil)
		return
	}
	resp := map[string]any{"upload_id": id, "exists": exists, "offset": size}
	if exists {
		resp["updated_at"] = updated.Unix()
	}
	jsonResp(w, resp)
}

func (s *Server) apiFileUploadAbort(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		UploadID string `json:"upload_id"`
	}
	if err := readJSON(r, &body); err != nil || !chunkUploadIDRe.MatchString(body.UploadID) {
		jsonErrorCode(w, http.StatusBadRequest, "bad_upload_id", "invalid upload_id", nil)
		return
	}
	abortChunkUpload(body.UploadID)
	jsonResp(w, map[string]any{"ok": true, "upload_id": body.UploadID})
}

func (s *Server) apiFileDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &body); err != nil || body.Path == "" {
		jsonErrorCode(w, fsStatus(fsCodeBadPath), fsCodeBadPath, "path required", nil)
		return
	}
	safe, ok := validPathOrFail(w, body.Path)
	if !ok {
		return
	}
	info, err := os.Stat(safe)
	if err != nil {
		jsonFSError(w, err, "delete file")
		return
	}
	if info.IsDir() {
		jsonErrorCode(w, fsStatus(fsCodeIsDir), fsCodeIsDir, "path is a directory", nil)
		return
	}
	trashDelete(w, safe, func() error { return os.Remove(safe) })
}

// trashDelete — общий финал удаления файла и папки: сначала корзина, и только
// если её нет — прежнее необратимое удаление. Ответ РАЗЛИЧАЕТ эти два исхода
// (trashed), потому что тост «удалено» после безвозвратного удаления — обещание
// возврата, которого не будет.
func trashDelete(w http.ResponseWriter, safe string, remove func() error) {
	res, terr := moveToTrash(safe)
	if terr == nil {
		jsonResp(w, map[string]any{
			"ok": true, "trashed": true, "trash": res.Kind,
			// restore_path непуст только там, где вернуть файл можно обычным
			// перемещением — клиент по нему рисует «Отменить».
			"restore_path": res.RestorePath,
		})
		return
	}
	if !errors.Is(terr, errTrashUnsupported) {
		// Корзина есть, но не сработала (нет прав, полный диск в ней): знать об
		// этом важнее, чем молча удалить мимо неё.
		log.Printf("[FILES] trash %s: %v — удаляю безвозвратно", safe, terr)
	}
	if err := remove(); err != nil {
		jsonFSError(w, err, "delete")
		return
	}
	jsonResp(w, map[string]any{"ok": true, "trashed": false})
}

func (s *Server) apiFileMkdir(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &body); err != nil || body.Path == "" {
		jsonErrorCode(w, fsStatus(fsCodeBadPath), fsCodeBadPath, "path required", nil)
		return
	}
	safe, ok := validPathOrFail(w, body.Path)
	if !ok {
		return
	}
	if _, err := os.Stat(safe); err == nil {
		jsonErrorCode(w, fsStatus(fsCodeExists), fsCodeExists, "already exists", nil)
		return
	}
	if err := os.MkdirAll(safe, 0755); err != nil {
		jsonFSError(w, err, "mkdir")
		return
	}
	jsonResp(w, map[string]any{"ok": true, "path": safe})
}

// apiFileRename — переименование И перемещение (клиент делает «Переместить»
// именно им: new_path = другая папка + то же имя).
//
// МЕЖДУ ДИСКАМИ (находка N109). os.Rename на Windows — это MoveFileEx без
// MOVEFILE_COPY_ALLOWED, на POSIX — rename(2): перенос C:\…\Downloads → D:\ оба
// отдают «другое устройство», и человек получал тупиковое «Ошибка чтения или
// записи файла», хотя плитку D: ему предложил сам агент. Теперь делаем то, что
// делает проводник: копируем, а затем удаляем источник. Клиент может попросить
// сначала спросить человека, прислав allow_copy:false — тогда ответ 409
// cross_device.
func (s *Server) apiFileRename(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		OldPath   string `json:"old_path"`
		NewPath   string `json:"new_path"`
		Overwrite bool   `json:"overwrite"`
		// AllowCopy: поля нет (nil) — переносим копированием молча (старый
		// клиент иначе остаётся в тупике); false — клиент хочет спросить
		// человека и ждёт код cross_device; true — то же, что nil.
		AllowCopy *bool `json:"allow_copy"`
	}
	if err := readJSON(r, &body); err != nil || body.OldPath == "" || body.NewPath == "" {
		jsonErrorCode(w, fsStatus(fsCodeBadPath), fsCodeBadPath, "old_path and new_path required", nil)
		return
	}
	safeOld, ok := validPathOrFail(w, body.OldPath)
	if !ok {
		return
	}
	safeNew, ok := validPathOrFail(w, body.NewPath)
	if !ok {
		return
	}
	// На POSIX os.Rename молча ЗАТИРАЕТ цель — при перемещении это потеря
	// файла. Спрашиваем клиента явным кодом. EqualFold — чтобы не мешать
	// переименованию a→A на регистронезависимой ФС.
	if !body.Overwrite && !strings.EqualFold(safeOld, safeNew) {
		if _, err := os.Stat(safeNew); err == nil {
			jsonErrorCode(w, fsStatus(fsCodeExists), fsCodeExists, "destination already exists", nil)
			return
		}
	}
	if err := os.Rename(safeOld, safeNew); err != nil {
		if !isCrossDeviceErr(err) {
			jsonFSError(w, err, "rename")
			return
		}
		if body.AllowCopy != nil && !*body.AllowCopy {
			jsonErrorCode(w, http.StatusConflict, "cross_device",
				"cannot move across volumes without copying", nil)
			return
		}
		s.moveAcrossVolumes(w, safeOld, safeNew, body.Overwrite)
		return
	}
	// «Отменить» в тосте после удаления возвращает файл из корзины этим же
	// перемещением. Запись .trashinfo о нём должна уйти вместе с ним — иначе
	// корзина рабочего стола покажет призрак (для «Корзины» Windows — no-op).
	forgetTrashEntry(safeOld)
	jsonResp(w, map[string]any{"ok": true, "path": safeNew})
}

// isCrossDeviceErr — «источник и цель на разных томах»: os.Rename такого не
// умеет. Windows: ERROR_NOT_SAME_DEVICE(17) от MoveFileEx без
// MOVEFILE_COPY_ALLOWED. POSIX: EXDEV.
//
// ГРАБЛЯ: число 17 сравнивать можно ТОЛЬКО на Windows — на Linux errno 17 это
// EEXIST. Обратно syscall.EXDEV на Windows объявлен синтетическим значением
// (APPLICATION_ERROR+iota) и ОС его не возвращает никогда, поэтому одной ветвью
// обе платформы не покрыть. Разбор живёт здесь, а не в platformFSCode: код
// нужен не для текста ошибки, а для развилки «перенести копированием».
func isCrossDeviceErr(err error) bool {
	var e syscall.Errno
	if err == nil || !errors.As(err, &e) {
		return false
	}
	if runtime.GOOS == "windows" {
		return e == syscall.Errno(17)
	}
	return e == syscall.EXDEV
}

// moveAcrossVolumes переносит файл или дерево между томами: копия → удаление
// источника. Источник удаляем ТОЛЬКО после успешной копии, а неудавшуюся копию
// убираем сами — огрызок под финальным именем легко принять за перенесённый
// файл.
//
// ОГРАНИЧЕНИЕ: операция синхронная, как и обычный rename. Гигабайтные деревья
// упрутся в клиентский таймаут (30 с) раньше, чем закончат копирование — для
// них есть менеджер переносов (ssh_transfers.go), это отдельная тема N108.
func (s *Server) moveAcrossVolumes(w http.ResponseWriter, src, dst string, overwrite bool) {
	srcInfo, err := os.Stat(src)
	if err != nil {
		jsonFSError(w, err, "move source")
		return
	}
	if dstInfo, statErr := os.Stat(dst); statErr == nil {
		// Папку не сливаем и не «перезаписываем» никогда (та же политика, что в
		// apiFileCopy): слияние деревьев молча уничтожает данные. Файл заменяем
		// только по явному overwrite — без него вызывающий сюда и не дошёл бы.
		if dstInfo.IsDir() || srcInfo.IsDir() || !overwrite {
			jsonErrorCode(w, fsStatus(fsCodeExists), fsCodeExists, "destination already exists", nil)
			return
		}
	}

	st := &copyStats{}
	if srcInfo.IsDir() {
		err = copyTree(src, dst, st)
	} else {
		var n int64
		n, err = copyFileContents(src, dst, srcInfo.Mode())
		if err == nil {
			st.Files++
			st.Bytes += n
			_ = os.Chtimes(dst, time.Now(), srcInfo.ModTime())
		}
	}
	if err != nil {
		if srcInfo.IsDir() {
			_ = os.RemoveAll(dst)
		} else {
			_ = os.Remove(dst)
		}
		jsonFSErrorExtra(w, err, "move copy", map[string]string{
			"files": strconv.FormatInt(st.Files, 10),
			"dirs":  strconv.FormatInt(st.Dirs, 10),
			"bytes": strconv.FormatInt(st.Bytes, 10),
		})
		return
	}

	resp := map[string]any{
		"ok": true, "path": dst, "copied": true,
		"files": st.Files, "dirs": st.Dirs, "bytes": st.Bytes,
	}
	if st.Skipped > 0 {
		// copyTree не копирует симлинки и спецфайлы — удалить после этого
		// источник значило бы уничтожить их безвозвратно. Оставляем оригинал и
		// говорим, почему: перенос стал копированием.
		resp["skipped"] = st.Skipped
		resp["source_removed"] = false
		resp["source_code"] = "skipped_special"
		log.Printf("[FILES] move %s: %d special entries not copied, source kept", src, st.Skipped)
		jsonResp(w, resp)
		return
	}

	rmErr := os.Remove(src)
	if srcInfo.IsDir() {
		rmErr = os.RemoveAll(src)
	}
	if rmErr != nil {
		// Данные уже на новом диске — это НЕ провал переноса. Ошибкой отвечать
		// нельзя (клиент решит, что ничего не произошло, и повторит), поэтому
		// говорим машинным кодом, что оригинал остался на месте.
		code, _ := fsErrorCode(rmErr)
		log.Printf("[FILES] move: source %s kept: %v (code=%s)", src, rmErr, code)
		resp["source_removed"] = false
		resp["source_code"] = code
	}
	jsonResp(w, resp)
}

// apiFileCopy — POST /api/files/copy (находка #12). Копирование файла или
// папки на самом ПК: раньше единственным способом «скопировать» было скачать
// файл на телефон и залить обратно (через релей, в память трёх процессов).
// Перемещение клиент делает существующим rename.
func (s *Server) apiFileCopy(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Src       string `json:"src"`
		Dst       string `json:"dst"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := readJSON(r, &body); err != nil || body.Src == "" || body.Dst == "" {
		jsonErrorCode(w, fsStatus(fsCodeBadPath), fsCodeBadPath, "src and dst required", nil)
		return
	}
	src, ok := validPathOrFail(w, body.Src)
	if !ok {
		return
	}
	dst, ok := validPathOrFail(w, body.Dst)
	if !ok {
		return
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		jsonFSError(w, err, "copy source")
		return
	}

	// «Скопировать в папку»: если dst — существующая папка, кладём внутрь под
	// именем источника (обычная семантика файлового менеджера).
	final := dst
	if dstInfo, err := os.Stat(dst); err == nil && dstInfo.IsDir() {
		final = filepath.Join(dst, filepath.Base(src))
	}
	// Копирование папки внутрь себя — бесконечная рекурсия, забивающая диск.
	if srcInfo.IsDir() && pathWithinRoot(final, src) {
		jsonErrorCode(w, 400, "dst_inside_src", "cannot copy a directory into itself", nil)
		return
	}
	if finalInfo, err := os.Stat(final); err == nil {
		if finalInfo.IsDir() || !body.Overwrite {
			// Папку не «перезаписываем» никогда: слияние деревьев молча
			// уничтожило бы данные.
			jsonErrorCode(w, fsStatus(fsCodeExists), fsCodeExists, "destination already exists", nil)
			return
		}
	}

	st := &copyStats{}
	if srcInfo.IsDir() {
		err = copyTree(src, final, st)
	} else {
		var n int64
		n, err = copyFileContents(src, final, srcInfo.Mode())
		if err == nil {
			st.Files++
			st.Bytes += n
			_ = os.Chtimes(final, time.Now(), srcInfo.ModTime())
		}
	}
	if err != nil {
		// Дерево могло скопироваться частично — говорим об этом честно.
		jsonFSErrorExtra(w, err, "copy", map[string]string{
			"files": strconv.FormatInt(st.Files, 10),
			"dirs":  strconv.FormatInt(st.Dirs, 10),
			"bytes": strconv.FormatInt(st.Bytes, 10),
		})
		return
	}
	jsonResp(w, map[string]any{
		"ok": true, "path": final,
		"files": st.Files, "dirs": st.Dirs, "bytes": st.Bytes,
		// skipped — симлинки/спецфайлы, которые не копируются: копия дерева не
		// «полная», и клиент имеет право об этом сказать.
		"skipped": st.Skipped,
	})
}

type copyStats struct {
	Files int64
	Dirs  int64
	Bytes int64
	// Skipped — симлинки и спецфайлы, которые copyTree не копирует. Считаем их,
	// потому что «перенести» такую папку между томами (копия + удаление
	// источника) без этого счёта уничтожило бы ссылки молча.
	Skipped int64
}

// copyFileContents копирует один файл. При любой ошибке недописанный файл
// удаляется: огрызок под ФИНАЛЬНЫМ именем — худший исход (пользователь считает
// его целым).
func copyFileContents(src, dst string, mode os.FileMode) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	// Ошибку Close возвращаем: именно там всплывает ENOSPC при буферизации.
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return n, err
	}
	return n, nil
}

// copyTree рекурсивно копирует папку. Симлинки и спецфайлы пропускаются
// (копировать их «по содержимому» опаснее, чем не копировать).
func copyTree(src, dst string, st *copyStats) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel) // rel == "." → target == dst
		if d.IsDir() {
			mode := fs.FileMode(0o755)
			if info, err := d.Info(); err == nil {
				mode = info.Mode().Perm()
			}
			if err := os.MkdirAll(target, mode); err != nil {
				return err
			}
			if rel != "." {
				st.Dirs++
			}
			return nil
		}
		if !d.Type().IsRegular() {
			st.Skipped++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		n, err := copyFileContents(p, target, info.Mode())
		if err != nil {
			return err
		}
		st.Files++
		st.Bytes += n
		_ = os.Chtimes(target, time.Now(), info.ModTime())
		return nil
	})
}

func (s *Server) apiFilePreview(w http.ResponseWriter, r *http.Request, uid int64) {
	path, ok := validPathOrFail(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		jsonFSError(w, err, "preview")
		return
	}
	if info.IsDir() {
		jsonErrorCode(w, fsStatus(fsCodeIsDir), fsCodeIsDir, "path is a directory", nil)
		return
	}

	ext := strings.ToLower(filepath.Ext(path))
	size := info.Size()

	if imageExts[ext] {
		if size > 10*1024*1024 {
			jsonErrorCode(w, fsStatus(fsCodeTooLarge), fsCodeTooLarge, "file too large for preview", nil)
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			jsonFSError(w, err, "preview read")
			return
		}
		mime := "image/" + strings.TrimPrefix(ext, ".")
		if ext == ".svg" {
			mime = "image/svg+xml"
		}
		jsonResp(w, map[string]any{
			"type": "image", "mime": mime,
			"data": base64.StdEncoding.EncodeToString(data),
		})
		return
	}

	if textExts[ext] || ext == "" || size < 500_000 {
		if size > 500_000 {
			jsonErrorCode(w, fsStatus(fsCodeTooLarge), fsCodeTooLarge, "file too large for preview", nil)
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			jsonFSError(w, err, "preview read")
			return
		}
		content := string(data)
		if len(content) > 100000 {
			content = content[:100000]
		}
		lang := strings.TrimPrefix(ext, ".")
		if lang == "" {
			lang = "txt"
		}
		jsonResp(w, map[string]any{
			"type": "text", "content": content, "language": lang,
		})
		return
	}

	jsonResp(w, map[string]any{"type": "binary", "size": size})
}

const (
	searchMaxResults = 50
	searchMaxLimit   = 500
	// searchBudget — потолок времени обхода (находка #41). Раньше обход C:\ шёл
	// до конца даже когда клиент давно отвалился, а LAN-клиент при таймауте
	// повторял запрос дважды — три обхода диска на один поиск.
	searchBudget = 5 * time.Second
	// ctxCheckMask — как часто сверяться с часами и контекстом (раз в 64
	// записи): time.Now() на каждый файл заметен на больших деревьях.
	ctxCheckMask = 0x3F
)

func (s *Server) apiFilesSearch(w http.ResponseWriter, r *http.Request, uid int64) {
	q := r.URL.Query()
	rawQuery := q.Get("q")
	searchPath := q.Get("path")
	limitRaw := q.Get("limit")
	showHidden := queryFlag(q.Get("hidden"))
	if r.Method == http.MethodPost {
		var body struct {
			Query  string `json:"query"`
			Path   string `json:"path"`
			Limit  int    `json:"limit"`
			Hidden bool   `json:"hidden"`
		}
		if err := readJSON(r, &body); err != nil {
			jsonErrorCode(w, 400, "bad_request", "invalid search request", nil)
			return
		}
		rawQuery = body.Query
		searchPath = body.Path
		if body.Limit > 0 {
			limitRaw = strconv.Itoa(body.Limit)
		}
		showHidden = body.Hidden
	}
	query := strings.ToLower(strings.TrimSpace(rawQuery))
	if searchPath == "" {
		searchPath, _ = os.Getwd()
	}
	searchPath, ok := validPathOrFail(w, searchPath)
	if !ok {
		return
	}
	if len([]rune(query)) < 2 {
		jsonErrorCode(w, 400, "query_too_short", "query too short", nil)
		return
	}
	maxResults := queryLimit(limitRaw, searchMaxResults, searchMaxLimit)

	results := make([]map[string]any, 0, 16)

	// Каталоги-шумелки пропускаем всегда, в том числе при hidden=1: .git на
	// десятки тысяч объектов съел бы весь бюджет поиска.
	skipDirs := map[string]bool{
		"node_modules": true, "__pycache__": true, ".git": true,
	}

	ctx := r.Context()
	start := time.Now()
	deadline := start.Add(searchBudget)
	scanned := 0
	truncated, timedOut := false, false

	filepath.WalkDir(searchPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}
		if path == searchPath {
			return nil // корень поиска сам может быть скрытым (.ssh) — не отсекаем его
		}
		scanned++
		// ГРАБЛЯ: в облаке r.Context() НЕ отменяется никогда (relay/client.go
		// гоняет проксированный запрос через httptest.Recorder с
		// context.Background), поэтому дедлайн по часам обязателен — на одной
		// проверке ctx обход снова уехал бы в бесконечность.
		if scanned&ctxCheckMask == 0 && (ctx.Err() != nil || !time.Now().Before(deadline)) {
			timedOut = true
			return filepath.SkipAll
		}
		name := d.Name()
		if isJunkEntry(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if isHiddenEntry(name) && !showHidden {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && skipDirs[strings.ToLower(name)] {
			return filepath.SkipDir
		}
		if strings.Contains(strings.ToLower(name), query) {
			item := map[string]any{
				"name": name, "is_dir": d.IsDir(), "path": path,
			}
			if info, err := d.Info(); err == nil {
				if !d.IsDir() {
					item["size"] = info.Size()
				}
				item["modified"] = float64(info.ModTime().Unix())
			}
			results = append(results, item)
			if len(results) >= maxResults {
				truncated = true
				return filepath.SkipAll
			}
		}
		return nil
	})

	jsonResp(w, map[string]any{
		"results": results, "query": query,
		"truncated": truncated, "timed_out": timedOut,
		"elapsed_ms": time.Since(start).Milliseconds(), "scanned": scanned,
		"limit": maxResults, "hidden": showHidden,
	})
}

func (s *Server) apiDiskInfo(w http.ResponseWriter, r *http.Request, uid int64) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path, _ = os.UserHomeDir()
		if path == "" {
			path = "."
		}
	}
	diskInfo, err := disk.Usage(path)
	if err != nil || diskInfo == nil {
		jsonResp(w, map[string]any{"total": 0, "used": 0, "free": 0})
		return
	}
	jsonResp(w, map[string]any{
		"total": diskInfo.Total,
		"used":  diskInfo.Used,
		"free":  diskInfo.Free,
	})
}

func (s *Server) apiFileDirDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &body); err != nil || body.Path == "" {
		jsonErrorCode(w, fsStatus(fsCodeBadPath), fsCodeBadPath, "path required", nil)
		return
	}
	safe, ok := validPathOrFail(w, body.Path)
	if !ok {
		return
	}
	info, err := os.Stat(safe)
	if err != nil {
		jsonFSError(w, err, "delete dir")
		return
	}
	if !info.IsDir() {
		jsonErrorCode(w, fsStatus(fsCodeNotDir), fsCodeNotDir, "not a directory", nil)
		return
	}
	// Дерево тоже уезжает в корзину: рекурсивный RemoveAll по одному удержанию
	// кнопки был самой необратимой операцией продукта.
	trashDelete(w, safe, func() error { return os.RemoveAll(safe) })
}

const (
	// dirStatBudget/dirStatMaxWalk — счёт содержимого папки не должен держать
	// облачный запрос до 60-секундного потолка релея: node_modules считается
	// «не до конца» и честно помечается truncated.
	dirStatBudget  = 2 * time.Second
	dirStatMaxWalk = 200_000
)

// apiFileDirStat — GET /api/files/dir-stat?path=… (находка #43).
// Отвечает {files,dirs,bytes,truncated,elapsed_ms}: сколько всего удалится.
// Раньше между удержанием кнопки 900 мс и рекурсивным os.RemoveAll не было
// ничего — ни счёта, ни масштаба.
func (s *Server) apiFileDirStat(w http.ResponseWriter, r *http.Request, uid int64) {
	path, ok := validPathOrFail(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		jsonFSError(w, err, "dir stat")
		return
	}
	if !info.IsDir() {
		jsonErrorCode(w, fsStatus(fsCodeNotDir), fsCodeNotDir, "not a directory", nil)
		return
	}

	var files, dirs, bytes int64
	n, truncated := 0, false
	ctx := r.Context()
	start := time.Now()
	deadline := start.Add(dirStatBudget)
	filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir // нет прав в поддереве — счёт не роняем
		}
		if p == path {
			return nil
		}
		n++
		if n > dirStatMaxWalk {
			truncated = true
			return filepath.SkipAll
		}
		// Тот же дедлайн по часам, что и в поиске: в облаке ctx не отменяется.
		if n&ctxCheckMask == 0 && (ctx.Err() != nil || !time.Now().Before(deadline)) {
			truncated = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			dirs++
			return nil
		}
		files++
		if fi, e := d.Info(); e == nil {
			bytes += fi.Size()
		}
		return nil
	})
	jsonResp(w, map[string]any{
		"files": files, "dirs": dirs, "bytes": bytes,
		"truncated": truncated, "elapsed_ms": time.Since(start).Milliseconds(),
	})
}

func (s *Server) apiFileSendToTelegram(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &body); err != nil || body.Path == "" {
		jsonErrorCode(w, fsStatus(fsCodeBadPath), fsCodeBadPath, "path required", nil)
		return
	}
	safe, ok := validPathOrFail(w, body.Path)
	if !ok {
		return
	}
	info, err := os.Stat(safe)
	if err != nil {
		jsonFSError(w, err, "send to telegram")
		return
	}
	if info.IsDir() {
		jsonErrorCode(w, fsStatus(fsCodeIsDir), fsCodeIsDir, "path is a directory", nil)
		return
	}
	if s.sendFileToTelegram == nil {
		jsonErrorCode(w, http.StatusServiceUnavailable, "telegram_unavailable",
			"Telegram file delivery is not configured", nil)
		return
	}
	if err := s.sendFileToTelegram(r.Context(), uid, safe); err != nil {
		log.Printf("[FILES] send to telegram error: %v", err)
		jsonErrorCode(w, http.StatusInternalServerError, "telegram_send_failed",
			"failed to send file", nil)
		return
	}
	resp := map[string]any{"ok": true, "message": "File sent to Telegram"}
	// Трасса «Зафиксировать проблему», присланная файлом в Telegram, попадала
	// на компьютер только транспортом телефон → бот (pty-upload), но оставалась
	// в ~/Remotai/files навсегда — вместе с записанным выводом терминала, хотя
	// клиент говорит «вывод и события стёрты». Доставлена — стираем. Только
	// после УСПЕШНОЙ отправки (при сбое человек повторит) и только эту копию.
	if isSentTraceUpload(safe) {
		if err := os.Remove(safe); err != nil {
			log.Printf("[FILES] trace upload not removed after send: %v", err)
		} else {
			resp["removed"] = true
		}
	}
	jsonResp(w, resp)
}

// isSentTraceUpload — транспортная копия трассы «Зафиксировать проблему»:
// прямой потомок каталога загрузок (uploadDestDir), обычный файл с именем
// pty-upload-<ms>-remotai-trace-*.json (имя трассы даёт traceFileName в
// apk/src/ptyTerm/traceExport.ts). Прочие загрузки и файлы человека —
// в том числе remotai-trace-*.json вне каталога загрузок — не трогаются.
func isSentTraceUpload(path string) bool {
	base := filepath.Base(path)
	rest, ok := strings.CutPrefix(base, "pty-upload-")
	if !ok {
		return false
	}
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return false
	}
	name, ok := strings.CutPrefix(rest[digits:], "-remotai-trace-")
	if !ok || !strings.HasSuffix(name, ".json") || len(name) <= len(".json") {
		return false
	}
	dir := uploadDestDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if !sameBrowserPath(filepath.Dir(path), dir) {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// ── Bookmarks ────────────────────────────────────────────────────

func (s *Server) apiBookmarksList(w http.ResponseWriter, r *http.Request, uid int64) {
	jsonResp(w, map[string]any{"bookmarks": s.bookmarks.List(uid)})
}

func (s *Server) apiBookmarkAdd(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := readJSON(r, &body); err != nil || body.Path == "" {
		jsonErrorCode(w, fsStatus(fsCodeBadPath), fsCodeBadPath, "path required", nil)
		return
	}
	if body.Name == "" {
		body.Name = filepath.Base(body.Path)
	}

	if err := s.bookmarks.Add(uid, bookmark{Name: body.Name, Path: body.Path}); err != nil {
		// Переполнение лимита закладок клиент раньше видел как отказ без кода и
		// показывал молчание — кнопка «📌 Закрепить» выглядела нерабочей.
		if errors.Is(err, errTooManyBookmarks) {
			jsonErrorCode(w, http.StatusBadRequest, "bookmark_limit", "too many bookmarks", nil)
			return
		}
		jsonErrorCode(w, http.StatusInternalServerError, fsCodeIO, err.Error(), nil)
		return
	}
	jsonResp(w, map[string]bool{"ok": true})
}

func (s *Server) apiBookmarkRemove(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &body); err != nil || body.Path == "" {
		jsonErrorCode(w, fsStatus(fsCodeBadPath), fsCodeBadPath, "path required", nil)
		return
	}

	if err := s.bookmarks.Remove(uid, body.Path); err != nil {
		jsonErrorCode(w, http.StatusInternalServerError, fsCodeIO, err.Error(), nil)
		return
	}
	jsonResp(w, map[string]bool{"ok": true})
}

// ── Recent Folders ────────────────────────────────────────────────

func (s *Server) apiRecentFolders(w http.ResponseWriter, r *http.Request, uid int64) {
	sessList := s.store.List(int(uid))

	type recentFolder struct {
		Name string  `json:"name"`
		Path string  `json:"path"`
		Time float64 `json:"time"`
	}

	seen := map[string]bool{}
	var folders []recentFolder

	// Сперва — НАСТОЯЩАЯ история: папки, в которых открывали терминалы. Она
	// лежит на диске и переживает и закрытие терминала, и перезапуск агента.
	// Живые сессии ниже лишь ДОПОЛНЯЮТ её: терминалы, открытые до появления
	// истории, иначе пропали бы из «Недавних» на ровном месте.
	if s.recent != nil {
		for _, e := range s.recent.List(uid) {
			if e.Path == "" || e.Path == "." || seen[e.Path] || isServiceProfilePath(e.Path) {
				continue
			}
			seen[e.Path] = true
			folders = append(folders, recentFolder{Name: e.Name, Path: e.Path, Time: e.Time})
		}
	}

	// Collect unique CWDs from sessions, sorted by last_active_at desc
	sort.Slice(sessList, func(i, j int) bool {
		return sessList[i].LastActiveAt > sessList[j].LastActiveAt
	})

	for _, sess := range sessList {
		cwd := sess.Cwd
		if cwd == "" || cwd == "." || seen[cwd] || isServiceProfilePath(cwd) {
			continue
		}
		seen[cwd] = true
		name := filepath.Base(cwd)
		if name == "." || name == string(filepath.Separator) {
			name = cwd
		}
		folders = append(folders, recentFolder{
			Name: name,
			Path: cwd,
			Time: sess.LastActiveAt,
		})
		if len(folders) >= 10 {
			break
		}
	}

	// Also collect from PTY sessions
	for _, ps := range s.ptyManager.List() {
		cwd := ps.CWD
		if cwd == "" || cwd == "." || seen[cwd] || isServiceProfilePath(cwd) {
			continue
		}
		seen[cwd] = true
		name := filepath.Base(cwd)
		if name == "." || name == string(filepath.Separator) {
			name = cwd
		}
		folders = append(folders, recentFolder{
			Name: name,
			Path: cwd,
			Time: float64(ps.Created) / 1000,
		})
		if len(folders) >= 15 {
			break
		}
	}

	// Порядок обязан быть по времени, а не по источнику: иначе живая сессия,
	// открытая неделю назад, встала бы выше папки, где работали пять минут
	// назад, — ровно та странность, на которую жаловался владелец.
	sort.SliceStable(folders, func(i, j int) bool { return folders[i].Time > folders[j].Time })
	if len(folders) > 15 {
		folders = folders[:15]
	}
	if folders == nil {
		folders = []recentFolder{}
	}
	jsonResp(w, map[string]any{"folders": folders})
}
