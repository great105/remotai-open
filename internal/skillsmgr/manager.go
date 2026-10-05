package skillsmgr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Коды ошибок: по ним интерфейс выбирает фразу, а сервер — HTTP-статус.
const (
	CodeBadURL        = "bad_url"
	CodeNotFound      = "not_found"
	CodeDownload      = "download_failed"
	CodeTooLarge      = "too_large"
	CodeBadArchive    = "bad_archive"
	CodeUnsafeArchive = "unsafe_archive"
	CodeNoSkills      = "no_skills"
	CodeBadName       = "bad_name"
	CodeExists        = "exists"
	CodeReadOnly      = "readonly"
	CodeNoLocation    = "unknown_location"
	CodeNoSkill       = "unknown_skill"
	CodeNoBackup      = "unknown_backup"
	CodeSame          = "same_place"
)

// Error — ошибка с кодом; текст уже человеческий.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func errf(code, format string, args ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// ErrorCode — код ошибки пакета или "".
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Root — одно место со скиллами, как его видит вызывающий (internal/web).
type Root struct {
	// ID — стабильный адрес места: "claude", "codex", "claude:acc-…".
	ID        string
	Agent     string
	AgentName string
	// Label — подпись аккаунта; "" у основного.
	Label string
	// Dir — сам каталог skills (может ещё не существовать).
	Dir string
	// Kind — "main", "account" или "plugins".
	Kind     string
	ReadOnly bool
	// Sources — для Kind == "plugins": по каталогу skills на каждый плагин.
	Sources []Source
}

// Source — каталог скиллов одного плагина.
type Source struct {
	Name string
	Dir  string
}

// Location — место со скиллами в ответе списка.
type Location struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	AgentName string `json:"agent_name"`
	Label     string `json:"label,omitempty"`
	Kind      string `json:"kind"`
	Dir       string `json:"dir"`
	Exists    bool   `json:"exists"`
	ReadOnly  bool   `json:"readonly,omitempty"`
	// SharedWith — аккаунты, чья папка skills связана с этой: скиллы у них
	// те же самые, и удаление отсюда убирает скилл и у них.
	SharedWith []string `json:"shared_with,omitempty"`
	Skills     []Skill  `json:"skills"`
}

// Manager — операции над скиллами. Нулевые лимиты = значения по умолчанию.
type Manager struct {
	// Roots — места; зовётся на каждую операцию (аккаунты меняются на ходу).
	Roots func() []Root
	// BackupDir — куда уезжают удалённые и заменённые скиллы.
	BackupDir string
	// StagingDir — где распаковываем архив до раскладки ("" = системный TEMP).
	StagingDir string
	Client     *http.Client
	// CodeloadBase — адрес архивов GitHub (подменяется в тестах).
	CodeloadBase string
	MaxArchive   int64
	MaxUnpacked  int64
	MaxFiles     int
	Now          func() time.Time

	mu sync.Mutex
}

func (m *Manager) codeloadBase() string {
	if m.CodeloadBase != "" {
		return m.CodeloadBase
	}
	return "https://codeload.github.com"
}

func (m *Manager) httpClient() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return &http.Client{Timeout: 90 * time.Second}
}

func (m *Manager) maxArchive() int64 {
	if m.MaxArchive > 0 {
		return m.MaxArchive
	}
	return 20 << 20
}

func (m *Manager) maxUnpacked() int64 {
	if m.MaxUnpacked > 0 {
		return m.MaxUnpacked
	}
	return 100 << 20
}

func (m *Manager) maxFiles() int {
	if m.MaxFiles > 0 {
		return m.MaxFiles
	}
	return 5000
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// resolved — место после свёртки связанных каталогов.
type resolved struct {
	Root
	exists bool
	// alias → канонический ID (для свёрнутых мест).
	sharedWith []string
}

// resolve сворачивает места, чьи каталоги skills — одна и та же папка
// (аккаунт со связью на основной). Порядок Roots важен: первое место с
// данной папкой — каноническое, поэтому основной профиль идёт первым.
func (m *Manager) resolve() (list []*resolved, byID map[string]*resolved) {
	byID = map[string]*resolved{}
	var roots []Root
	if m.Roots != nil {
		roots = m.Roots()
	}
	type seen struct {
		info os.FileInfo
		r    *resolved
	}
	var seenDirs []seen
	for _, root := range roots {
		r := &resolved{Root: root}
		if root.Kind == "plugins" {
			r.exists = len(root.Sources) > 0
			list = append(list, r)
			byID[root.ID] = r
			continue
		}
		st, err := os.Stat(root.Dir) // по ссылке — ради SameFile
		if err == nil && st.IsDir() {
			r.exists = true
			var canon *resolved
			for _, s := range seenDirs {
				if os.SameFile(s.info, st) {
					canon = s.r
					break
				}
			}
			if canon != nil {
				label := root.Label
				if label == "" {
					label = root.ID
				}
				canon.sharedWith = append(canon.sharedWith, label)
				byID[root.ID] = canon
				continue
			}
			seenDirs = append(seenDirs, seen{info: st, r: r})
		}
		list = append(list, r)
		byID[root.ID] = r
	}
	return list, byID
}

// List — все места и скиллы в них.
func (m *Manager) List() []Location {
	list, _ := m.resolve()
	out := make([]Location, 0, len(list))
	for _, r := range list {
		loc := Location{ID: r.ID, Agent: r.Agent, AgentName: r.AgentName, Label: r.Label, Kind: r.Kind,
			Dir: r.Dir, Exists: r.exists, ReadOnly: r.ReadOnly, SharedWith: r.sharedWith, Skills: []Skill{}}
		if r.Kind == "plugins" {
			for _, s := range r.Sources {
				loc.Skills = append(loc.Skills, listDir(s.Dir, true, s.Name)...)
			}
		} else if r.exists {
			loc.Skills = append(loc.Skills, listDir(r.Dir, r.ReadOnly, "")...)
		}
		sort.SliceStable(loc.Skills, func(i, j int) bool {
			a, b := loc.Skills[i], loc.Skills[j]
			if a.ReadOnly != b.ReadOnly {
				return !a.ReadOnly
			}
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		})
		out = append(out, loc)
	}
	return out
}

// writable — место, куда можно ставить и откуда удалять.
func (m *Manager) writable(byID map[string]*resolved, id string) (*resolved, error) {
	r, ok := byID[id]
	if !ok {
		return nil, errf(CodeNoLocation, "Такого места для скиллов нет: %s", id)
	}
	if r.ReadOnly || r.Kind == "plugins" {
		return nil, errf(CodeReadOnly, "Эти скиллы только для чтения")
	}
	return r, nil
}

// OpResult — итог операции над одним скиллом в одном месте.
type OpResult struct {
	Location string `json:"location"`
	Name     string `json:"name"`
	// Status — "installed", "replaced", "exists", "same", "error".
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Backup — ID резервной копии, куда уехала прежняя версия.
	Backup string `json:"backup,omitempty"`
}

// InstallOptions — куда и как ставить.
type InstallOptions struct {
	Targets []string
	// Replace — заменить уже стоящий (старый уедет в резервную копию).
	Replace bool
	// DryRun — только посмотреть, что лежит в источнике.
	DryRun bool
	// Only — какие из найденных ставить ("" = все).
	Only []string
}

// InstallReport — что нашли и что сделали.
type InstallReport struct {
	Found   []*Found   `json:"found"`
	Results []OpResult `json:"results,omitempty"`
}

// InstallGitHub ставит скилл(ы) по ссылке на GitHub.
func (m *Manager) InstallGitHub(ctx context.Context, rawURL string, opt InstallOptions) (*InstallReport, error) {
	src, err := ParseGitHubURL(rawURL)
	if err != nil {
		return nil, err
	}
	if !opt.DryRun {
		if err := m.checkTargets(opt.Targets); err != nil {
			return nil, err
		}
	}
	data, err := m.download(ctx, src)
	if err != nil {
		return nil, err
	}
	fallback := src.Repo
	if src.Path != "" {
		fallback = filepath.Base(filepath.FromSlash(src.Path))
	}
	return m.installArchive(data, src.Path, fallback, opt)
}

// InstallZip ставит скилл(ы) из загруженного архива.
func (m *Manager) InstallZip(data []byte, fileName string, opt InstallOptions) (*InstallReport, error) {
	if int64(len(data)) > m.maxArchive() {
		return nil, errf(CodeTooLarge, "Архив больше %d МБ", m.maxArchive()>>20)
	}
	if !opt.DryRun {
		if err := m.checkTargets(opt.Targets); err != nil {
			return nil, err
		}
	}
	fallback := strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
	return m.installArchive(data, "", fallback, opt)
}

func (m *Manager) checkTargets(targets []string) error {
	if len(targets) == 0 {
		return errf(CodeNoLocation, "Не выбрано, какому агенту ставить")
	}
	_, byID := m.resolve()
	for _, id := range targets {
		if _, err := m.writable(byID, id); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) installArchive(data []byte, sub, fallback string, opt InstallOptions) (*InstallReport, error) {
	found, err := m.scanArchive(data, sub, fallback)
	if err != nil {
		return nil, err
	}
	report := &InstallReport{Found: found}
	if opt.DryRun {
		return report, nil
	}
	only := map[string]bool{}
	for _, n := range opt.Only {
		if n != "" {
			only[n] = true
		}
	}
	staging, err := os.MkdirTemp(m.StagingDir, "remotai-skill-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging) // свой временный каталог: ссылок в нём нет
	budget := m.maxUnpacked()
	var picked []*Found
	for _, fd := range found {
		if fd.Skip != "" || (len(only) > 0 && !only[fd.Name]) {
			continue
		}
		if err := m.extract(fd, filepath.Join(staging, fd.Name), &budget); err != nil {
			return nil, err
		}
		picked = append(picked, fd)
	}
	if len(picked) == 0 {
		return nil, errf(CodeNoSkills, "Нечего ставить: подходящих скиллов не нашлось")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	_, byID := m.resolve()
	done := map[*resolved]bool{}
	for _, id := range opt.Targets {
		r, err := m.writable(byID, id)
		if err != nil {
			return nil, err
		}
		if done[r] { // два аккаунта с общей папкой — ставим один раз
			continue
		}
		done[r] = true
		for _, fd := range picked {
			report.Results = append(report.Results, m.place(r, fd.Name, filepath.Join(staging, fd.Name), opt.Replace))
		}
	}
	return report, nil
}

// place кладёт готовую папку src в место r под именем name. Сначала копия
// во временную папку рядом, потом переименование: агент не увидит
// полускопированный скилл.
func (m *Manager) place(r *resolved, name, src string, replace bool) OpResult {
	res := OpResult{Location: r.ID, Name: name}
	fail := func(err error) OpResult {
		res.Status, res.Error = "error", err.Error()
		return res
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return fail(err)
	}
	dest := filepath.Join(r.Dir, name)
	if same, _ := sameDir(src, dest); same {
		res.Status = "same"
		return res
	}
	tmp := filepath.Join(r.Dir, "."+name+".remotai-"+randHex())
	budget := m.maxUnpacked()
	if _, err := copyTree(src, tmp, &budget); err != nil {
		os.RemoveAll(tmp)
		if budget < 0 {
			return fail(errf(CodeTooLarge, "Скилл больше %d МБ", m.maxUnpacked()>>20))
		}
		return fail(err)
	}
	res.Status = "installed"
	if _, err := os.Lstat(dest); err == nil {
		if !replace {
			os.RemoveAll(tmp)
			res.Status = "exists"
			return res
		}
		id, err := m.backup(r, name, "replace")
		if err != nil {
			os.RemoveAll(tmp)
			return fail(err)
		}
		res.Status, res.Backup = "replaced", id
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.RemoveAll(tmp)
		return fail(err)
	}
	return res
}

func sameDir(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(sa, sb), nil
}

// copyTree копирует папку. Корень может быть ссылкой (идём по ней), а
// вложенные ссылки ПРОПУСКАЕМ: скилл не должен утащить за собой чужой
// каталог. Возвращает число пропущенных ссылок.
func copyTree(src, dst string, budget *int64) (skipped int, err error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, err
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		switch {
		case isLink(e.Type()):
			skipped++
		case e.IsDir():
			n, err := copyTree(s, d, budget)
			skipped += n
			if err != nil {
				return skipped, err
			}
		case e.Type().IsRegular():
			if err := copyFile(s, d, budget); err != nil {
				return skipped, err
			}
		}
	}
	return skipped, nil
}

func copyFile(src, dst string, budget *int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, *budget+1))
	cerr := out.Close()
	*budget -= n
	if *budget < 0 {
		return errf(CodeTooLarge, "Слишком большой скилл")
	}
	if err != nil {
		return err
	}
	return cerr
}

func randHex() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Copy копирует скилл из одного места в другие.
func (m *Manager) Copy(name, from string, to []string, replace bool) ([]OpResult, error) {
	if !validExistingName(name) {
		return nil, errf(CodeBadName, "Странное имя скилла")
	}
	if len(to) == 0 {
		return nil, errf(CodeNoLocation, "Не выбрано, куда копировать")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, byID := m.resolve()
	src, ok := byID[from]
	if !ok || src.Kind == "plugins" {
		return nil, errf(CodeNoLocation, "Такого места для скиллов нет: %s", from)
	}
	srcDir := filepath.Join(src.Dir, name)
	if _, ok := ReadMeta(srcDir); !ok {
		return nil, errf(CodeNoSkill, "Скилла %s там нет", name)
	}
	var out []OpResult
	for _, id := range to {
		r, err := m.writable(byID, id)
		if err != nil {
			return nil, err
		}
		if r == src {
			out = append(out, OpResult{Location: id, Name: name, Status: "same"})
			continue
		}
		out = append(out, m.place(r, name, srcDir, replace))
	}
	return out, nil
}

// BackupEntry — одна запись резервной копии.
type BackupEntry struct {
	// ID — папка резервной копии (время), общая для записей одной операции.
	ID       string `json:"id"`
	Location string `json:"location"`
	Agent    string `json:"agent"`
	Label    string `json:"label,omitempty"`
	Name     string `json:"name"`
	// Reason — "delete" или "replace".
	Reason string `json:"reason"`
	// From — где скилл лежал.
	From        string `json:"from"`
	At          int64  `json:"at"`
	Link        bool   `json:"link,omitempty"`
	Description string `json:"description,omitempty"`
}

const manifestName = "manifest.json"

func safeSegment(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ':' || r == '/' || r == '\\' || r < 32 {
			return '_'
		}
		return r
	}, s)
}

// backup переносит папку скилла в резервную копию. Только перенос: копия
// с удалением оригинала могла бы пройти по вложенной связи и стереть чужое.
func (m *Manager) backup(r *resolved, name, reason string) (string, error) {
	if m.BackupDir == "" {
		return "", errors.New("резервная копия не настроена")
	}
	src := filepath.Join(r.Dir, name)
	li, err := os.Lstat(src)
	if err != nil {
		return "", errf(CodeNoSkill, "Скилла %s там нет", name)
	}
	meta, _ := ReadMeta(src)
	now := m.now()
	id := now.Format("20060102-150405") + "-" + randHex()
	dir := filepath.Join(m.BackupDir, id, safeSegment(r.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(src, filepath.Join(dir, name)); err != nil {
		os.RemoveAll(filepath.Join(m.BackupDir, id))
		return "", fmt.Errorf("не удалось перенести в резервную копию: %w", err)
	}
	entry := BackupEntry{ID: id, Location: r.ID, Agent: r.Agent, Label: r.Label, Name: name, Reason: reason,
		From: src, At: now.Unix(), Link: isLink(li.Mode()), Description: meta.Description}
	if err := writeManifest(filepath.Join(m.BackupDir, id), []BackupEntry{entry}); err != nil {
		return id, nil // перенос уже сделан; без описи вернуть можно руками
	}
	return id, nil
}

func writeManifest(dir string, entries []BackupEntry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, manifestName), data, 0o644)
}

func readManifest(dir string) ([]BackupEntry, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	var entries []BackupEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// Delete переносит скилл в резервную копию.
func (m *Manager) Delete(location, name string) (*BackupEntry, error) {
	if !validExistingName(name) {
		return nil, errf(CodeBadName, "Странное имя скилла")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, byID := m.resolve()
	r, err := m.writable(byID, location)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(r.Dir, name)); err != nil {
		return nil, errf(CodeNoSkill, "Скилла %s там нет", name)
	}
	id, err := m.backup(r, name, "delete")
	if err != nil {
		return nil, err
	}
	entries, _ := readManifest(filepath.Join(m.BackupDir, id))
	if len(entries) > 0 {
		return &entries[0], nil
	}
	return &BackupEntry{ID: id, Location: r.ID, Name: name, Reason: "delete"}, nil
}

// Backups — резервные копии, свежие первыми.
func (m *Manager) Backups() []BackupEntry {
	out := []BackupEntry{}
	if m.BackupDir == "" {
		return out
	}
	dirs, err := os.ReadDir(m.BackupDir)
	if err != nil {
		return out
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		entries, err := readManifest(filepath.Join(m.BackupDir, d.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			e.ID = d.Name()
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// Restore возвращает скилл из резервной копии туда, где он был. Если там уже
// стоит скилл с тем же именем (например, поставили новую версию), без
// replace ничего не трогаем; с replace стоящий уезжает в резервную копию.
func (m *Manager) Restore(id, location, name string, replace bool) (OpResult, error) {
	res := OpResult{Location: location, Name: name}
	if !validExistingName(name) || !validExistingName(id) {
		return res, errf(CodeBadName, "Странное имя скилла")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	dir := filepath.Join(m.BackupDir, id)
	entries, err := readManifest(dir)
	if err != nil {
		return res, errf(CodeNoBackup, "Такой резервной копии нет")
	}
	idx := -1
	for i, e := range entries {
		if e.Location == location && e.Name == name {
			idx = i
		}
	}
	if idx < 0 {
		return res, errf(CodeNoBackup, "Такой резервной копии нет")
	}
	_, byID := m.resolve()
	r, err := m.writable(byID, location)
	if err != nil {
		return res, err
	}
	src := filepath.Join(dir, safeSegment(location), name)
	if _, err := os.Lstat(src); err != nil {
		return res, errf(CodeNoBackup, "Папка резервной копии пропала")
	}
	dest := filepath.Join(r.Dir, name)
	res.Status = "installed"
	if _, err := os.Lstat(dest); err == nil {
		if !replace {
			return res, errf(CodeExists, "Скилл %s уже стоит — сначала удалите его или замените", name)
		}
		bid, err := m.backup(r, name, "replace")
		if err != nil {
			return res, err
		}
		res.Status, res.Backup = "replaced", bid
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return res, err
	}
	if err := os.Rename(src, dest); err != nil {
		return res, fmt.Errorf("не удалось вернуть: %w", err)
	}
	entries = append(entries[:idx], entries[idx+1:]...)
	if len(entries) == 0 {
		// Опись пуста — папка копии больше не нужна. Удаляем по одному
		// уровню (os.Remove), а не RemoveAll: вдруг внутри что-то осталось.
		os.Remove(filepath.Join(dir, safeSegment(location)))
		os.Remove(filepath.Join(dir, manifestName))
		os.Remove(dir)
	} else {
		_ = writeManifest(dir, entries)
	}
	return res, nil
}
