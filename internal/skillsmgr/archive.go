package skillsmgr

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GitHubSource — разобранная ссылка на GitHub.
type GitHubSource struct {
	Owner, Repo string
	// Ref — ветка, тег или коммит; пусто = ветка по умолчанию (HEAD).
	Ref string
	// Path — подкаталог внутри репозитория, "" = весь.
	Path string
}

var ghPart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// ParseGitHubURL принимает ссылку, которую человек скопирует из браузера:
//
//	https://github.com/<owner>/<repo>
//	https://github.com/<owner>/<repo>/tree/<ref>/<path>
//	https://github.com/<owner>/<repo>/blob/<ref>/<path>/SKILL.md
//
// Ветку со слешем в имени (`feature/x`) отличить от пути по ссылке нельзя —
// берём первый сегмент как ветку: так устроены почти все репозитории скиллов.
func ParseGitHubURL(raw string) (GitHubSource, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return GitHubSource{}, errf(CodeBadURL, "Не похоже на ссылку GitHub")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" && u.Scheme != "http" || (host != "github.com" && host != "www.github.com") {
		return GitHubSource{}, errf(CodeBadURL, "Нужна ссылка на github.com")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return GitHubSource{}, errf(CodeBadURL, "В ссылке нет владельца и репозитория")
	}
	src := GitHubSource{Owner: parts[0], Repo: strings.TrimSuffix(parts[1], ".git")}
	if !ghPart.MatchString(src.Owner) || !ghPart.MatchString(src.Repo) {
		return GitHubSource{}, errf(CodeBadURL, "Странное имя репозитория в ссылке")
	}
	rest := parts[2:]
	if len(rest) >= 2 && (rest[0] == "tree" || rest[0] == "blob") {
		src.Ref = rest[1]
		sub := rest[2:]
		if rest[0] == "blob" && len(sub) > 0 && strings.EqualFold(sub[len(sub)-1], SkillFile) {
			sub = sub[:len(sub)-1]
		}
		for _, p := range sub {
			if p == "" || p == "." || p == ".." {
				return GitHubSource{}, errf(CodeBadURL, "Странный путь в ссылке")
			}
		}
		src.Path = strings.Join(sub, "/")
		if src.Ref == "" || strings.ContainsAny(src.Ref, " \\?#") {
			return GitHubSource{}, errf(CodeBadURL, "Странная ветка в ссылке")
		}
	} else if len(rest) > 0 && rest[0] != "" {
		return GitHubSource{}, errf(CodeBadURL, "Ссылка должна вести на репозиторий или папку в нём")
	}
	return src, nil
}

// archiveURL — прямой адрес zip-архива на codeload (тот же, что отдаёт кнопка
// «Download ZIP»). `zip/HEAD` = ветка по умолчанию, проверено запросом.
func (s GitHubSource) archiveURL(base string) string {
	ref := s.Ref
	if ref == "" {
		ref = "HEAD"
	}
	return strings.TrimRight(base, "/") + "/" + s.Owner + "/" + s.Repo + "/zip/" + url.PathEscape(ref)
}

// download качает архив не больше лимита. Размер проверяем дважды: по
// заголовку (чтобы не тратить трафик) и по факту (заголовка может не быть).
func (m *Manager) download(ctx context.Context, src GitHubSource) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.archiveURL(m.codeloadBase()), nil)
	if err != nil {
		return nil, errf(CodeBadURL, "Не удалось собрать адрес архива")
	}
	req.Header.Set("User-Agent", "Remotai-skills")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, errf(CodeDownload, "GitHub не ответил: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errf(CodeNotFound, "Репозиторий или ветка не найдены. Закрытые репозитории не поддерживаются.")
	case resp.StatusCode != http.StatusOK:
		return nil, errf(CodeDownload, "GitHub ответил %d", resp.StatusCode)
	}
	limit := m.maxArchive()
	if resp.ContentLength > limit {
		return nil, errf(CodeTooLarge, "Архив больше %d МБ", limit>>20)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errf(CodeDownload, "Архив не скачался: %v", err)
	}
	if int64(len(data)) > limit {
		return nil, errf(CodeTooLarge, "Архив больше %d МБ", limit>>20)
	}
	return data, nil
}

// Found — скилл, найденный в архиве.
type Found struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Files       int    `json:"files"`
	Bytes       int64  `json:"bytes"`
	// Skip — почему этот скилл поставить нельзя (плохое имя, ссылки внутри).
	Skip string `json:"skip,omitempty"`

	files []foundFile
}

// foundFile — файл скилла: путь ВНУТРИ папки скилла и запись архива.
type foundFile struct {
	rel string
	f   *zip.File
}

// cleanEntry проверяет имя записи архива. Любая попытка выйти за пределы
// папки (zip-slip), абсолютный путь или диск — отказ ВСЕГО архива: такой
// архив собран со злым умыслом, и ставить из него что-либо нельзя.
func cleanEntry(name string) (string, error) {
	n := strings.ReplaceAll(name, `\`, "/")
	if n == "" || strings.HasPrefix(n, "/") || strings.ContainsAny(n, ":\x00") {
		return "", fmt.Errorf("unsafe path %q", name)
	}
	for _, p := range strings.Split(strings.TrimSuffix(n, "/"), "/") {
		if p == ".." {
			return "", fmt.Errorf("unsafe path %q", name)
		}
	}
	c := path.Clean(n)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("unsafe path %q", name)
	}
	return c, nil
}

type entry struct {
	name string // очищенный путь
	dir  bool
	link bool
	f    *zip.File
}

// scanArchive разбирает архив и находит в нём скиллы внутри sub.
// fallbackName — имя для скилла, если SKILL.md лежит прямо в корне архива, а
// в шапке имени нет (имя репозитория или загруженного файла).
func (m *Manager) scanArchive(data []byte, sub, fallbackName string) ([]*Found, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errf(CodeBadArchive, "Это не ZIP-архив или он повреждён")
	}
	if len(zr.File) > m.maxFiles() {
		return nil, errf(CodeTooLarge, "В архиве больше %d файлов", m.maxFiles())
	}
	var total uint64
	entries := make([]entry, 0, len(zr.File))
	for _, f := range zr.File {
		name, err := cleanEntry(f.Name)
		if err != nil {
			return nil, errf(CodeUnsafeArchive, "В архиве опасный путь: %s", f.Name)
		}
		total += f.UncompressedSize64
		if total > uint64(m.maxUnpacked()) {
			return nil, errf(CodeTooLarge, "Распакованный архив больше %d МБ", m.maxUnpacked()>>20)
		}
		mode := f.Mode()
		entries = append(entries, entry{name: name, dir: mode.IsDir() || strings.HasSuffix(f.Name, "/"), link: mode&fs.ModeSymlink != 0, f: f})
	}
	// Снимаем общий корень: GitHub кладёт всё в `<repo>-<ref>/`, и так же
	// часто пакуют руками («папка → Сжать»).
	root := commonRoot(entries)
	strip := func(n string) (string, bool) {
		if root == "" {
			return n, true
		}
		if n == root {
			return "", true
		}
		if strings.HasPrefix(n, root+"/") {
			return n[len(root)+1:], true
		}
		return "", false
	}
	if sub = strings.Trim(sub, "/"); sub != "" {
		sub = path.Clean(sub)
	}
	within := func(n string) bool { return sub == "" || n == sub || strings.HasPrefix(n, sub+"/") }

	// Папки со SKILL.md.
	skillDirs := map[string]bool{}
	for _, e := range entries {
		n, ok := strip(e.name)
		if !ok || e.dir || !within(n) {
			continue
		}
		if path.Base(n) == SkillFile {
			d := path.Dir(n)
			if d == "." {
				d = ""
			}
			skillDirs[d] = true
		}
	}
	// Вложенный SKILL.md (примеры внутри скилла) — часть внешнего, а не
	// отдельный скилл.
	var dirs []string
	for d := range skillDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var roots []string
	for _, d := range dirs {
		nested := false
		for _, r := range roots {
			if r == "" || strings.HasPrefix(d, r+"/") {
				nested = true
				break
			}
		}
		if !nested {
			roots = append(roots, d)
		}
	}
	if len(roots) == 0 {
		if sub != "" {
			return nil, errf(CodeNoSkills, "В папке %s нет SKILL.md", sub)
		}
		return nil, errf(CodeNoSkills, "В архиве нет ни одного скилла (папки с SKILL.md)")
	}
	if len(roots) > maxSkillsPerArchive {
		roots = roots[:maxSkillsPerArchive]
	}
	var out []*Found
	for _, r := range roots {
		fd := &Found{}
		for _, e := range entries {
			n, ok := strip(e.name)
			if !ok || n == "" || n == r {
				continue
			}
			rel := n
			if r != "" {
				if !strings.HasPrefix(n, r+"/") {
					continue
				}
				rel = n[len(r)+1:]
			}
			if e.link {
				fd.Skip = "внутри есть ссылки на другие файлы — такое не ставим"
			}
			if e.dir {
				continue
			}
			fd.files = append(fd.files, foundFile{rel: rel, f: e.f})
			fd.Files++
			fd.Bytes += int64(e.f.UncompressedSize64)
		}
		// Шапку читаем из самого архива.
		for _, ff := range fd.files {
			if ff.rel == SkillFile {
				rc, err := ff.f.Open()
				if err == nil {
					head, _ := io.ReadAll(io.LimitReader(rc, 256*1024))
					rc.Close()
					meta := ParseFrontmatter(head)
					fd.Title, fd.Description = meta.Name, meta.Description
				}
			}
		}
		switch {
		case r != "":
			fd.Name = path.Base(r)
		case ValidNewName(fd.Title):
			fd.Name = fd.Title
		default:
			fd.Name = fallbackName
		}
		if fd.Skip == "" && !ValidNewName(fd.Name) {
			fd.Skip = "имя папки не подходит: только латиница, цифры, точка, дефис и подчёркивание"
		}
		out = append(out, fd)
	}
	return out, nil
}

const maxSkillsPerArchive = 200

func commonRoot(entries []entry) string {
	root := ""
	for _, e := range entries {
		if !strings.Contains(e.name, "/") && !e.dir {
			return "" // файл прямо в корне
		}
		first := strings.SplitN(e.name, "/", 2)[0]
		if root == "" {
			root = first
		} else if root != first {
			return ""
		}
	}
	return root
}

// extract распаковывает найденный скилл в dst с проверкой ФАКТИЧЕСКОГО
// размера: объявленный в архиве может врать (zip-бомба). budget — сколько
// байт ещё можно записать на всю установку.
func (m *Manager) extract(fd *Found, dst string, budget *int64) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, ff := range fd.files {
		target := filepath.Join(dst, filepath.FromSlash(ff.rel))
		if r, err := filepath.Rel(dst, target); err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return errf(CodeUnsafeArchive, "В архиве опасный путь: %s", ff.f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := ff.f.Open()
		if err != nil {
			return errf(CodeBadArchive, "Файл %s в архиве повреждён", ff.f.Name)
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		n, err := io.Copy(out, io.LimitReader(rc, *budget+1))
		rc.Close()
		out.Close()
		if err != nil {
			return errf(CodeBadArchive, "Файл %s в архиве повреждён", ff.f.Name)
		}
		*budget -= n
		if *budget < 0 {
			return errf(CodeTooLarge, "Распакованный архив больше %d МБ", m.maxUnpacked()>>20)
		}
	}
	return nil
}
