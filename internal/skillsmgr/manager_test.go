package skillsmgr

import (
	"archive/zip"
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"tgcontrol/internal/procutil"
)

// Все проверки — на временных каталогах: настоящие ~/.claude и ~/.codex
// владельца здесь не участвуют.

type zfile struct {
	name string
	body string
	mode fs.FileMode
}

func makeZip(t *testing.T, files ...zfile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		h := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		if f.mode != 0 {
			h.SetMode(f.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func skillMD(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# " + name + "\n"
}

type env struct {
	m      *Manager
	claude string
	codex  string
	backup string
	extra  []Root
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{
		claude: filepath.Join(base, "home", ".claude", "skills"),
		codex:  filepath.Join(base, "home", ".codex", "skills"),
		backup: filepath.Join(base, "state", "skills-backup"),
	}
	e.m = &Manager{
		BackupDir:  e.backup,
		StagingDir: t.TempDir(),
		Roots: func() []Root {
			roots := []Root{
				{ID: "claude", Agent: "claude", AgentName: "Claude Code", Kind: "main", Dir: e.claude},
				{ID: "codex", Agent: "codex", AgentName: "Codex CLI", Kind: "main", Dir: e.codex},
			}
			return append(roots, e.extra...)
		},
	}
	return e
}

func writeSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, SkillFile), []byte(skillMD(name, desc)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func find(locs []Location, id string) *Location {
	for i := range locs {
		if locs[i].ID == id {
			return &locs[i]
		}
	}
	return nil
}

func TestParseFrontmatter(t *testing.T) {
	cases := []struct {
		name, in, wantName, wantDesc string
	}{
		{"plain", "---\nname: foo\ndescription: Делает foo\n---\nbody", "foo", "Делает foo"},
		{"quoted", "---\nname: \"imagegen\"\ndescription: \"Say \\\"hi\\\"\"\n---\n", "imagegen", `Say "hi"`},
		{"single", "---\nname: 'a'\ndescription: 'it''s'\n---\n", "a", "it's"},
		{"folded", "---\nname: x\ndescription: >\n  first line\n  second line\nlicense: MIT\n---\n", "x", "first line second line"},
		{"block", "---\nname: x\ndescription: |\n  one\n  two\n---\n", "x", "one\ntwo"},
		{"continued", "---\nname: x\ndescription: starts here\n  and goes on\n---\n", "x", "starts here and goes on"},
		{"crlf+bom", "\xef\xbb\xbf---\r\nname: w\r\ndescription: d\r\n---\r\n", "w", "d"},
		{"no frontmatter", "# Title\nname: nope\n", "", ""},
		{"unclosed", "---\nname: x\n", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := ParseFrontmatter([]byte(c.in))
			if m.Name != c.wantName || m.Description != c.wantDesc {
				t.Fatalf("got %q/%q, want %q/%q", m.Name, m.Description, c.wantName, c.wantDesc)
			}
		})
	}
}

func TestValidNewName(t *testing.T) {
	for _, ok := range []string{"pdf", "hyperframes-cli", "a.b_c", "X1"} {
		if !ValidNewName(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", ".hidden", "..", "a/b", `a\b`, "con", "NUL.txt", "trail.", "рус", "a b", strings.Repeat("a", 81)} {
		if ValidNewName(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestParseGitHubURL(t *testing.T) {
	good := []struct{ in, owner, repo, ref, path string }{
		{"https://github.com/anthropics/skills", "anthropics", "skills", "", ""},
		{"github.com/anthropics/skills.git", "anthropics", "skills", "", ""},
		{"https://github.com/anthropics/skills/tree/main/skills/pdf", "anthropics", "skills", "main", "skills/pdf"},
		{"https://github.com/o/r/blob/v1.2/x/SKILL.md", "o", "r", "v1.2", "x"},
		{"https://www.github.com/o/r/tree/abc123", "o", "r", "abc123", ""},
	}
	for _, g := range good {
		s, err := ParseGitHubURL(g.in)
		if err != nil {
			t.Fatalf("%s: %v", g.in, err)
		}
		if s.Owner != g.owner || s.Repo != g.repo || s.Ref != g.ref || s.Path != g.path {
			t.Fatalf("%s: got %+v", g.in, s)
		}
	}
	for _, bad := range []string{"", "https://gitlab.com/o/r", "https://github.com/o", "https://github.com/o/r/issues/1",
		"https://github.com/o/r/tree/main/../../etc", "ftp://github.com/o/r", "https://github.com/o$/r"} {
		if _, err := ParseGitHubURL(bad); ErrorCode(err) != CodeBadURL {
			t.Fatalf("%q: want bad_url, got %v", bad, err)
		}
	}
}

func TestInstallZipAndReplace(t *testing.T) {
	e := newEnv(t)
	data := makeZip(t,
		zfile{name: "pack/"},
		zfile{name: "pack/alpha/SKILL.md", body: skillMD("alpha", "первый")},
		zfile{name: "pack/alpha/scripts/run.py", body: "print(1)"},
		zfile{name: "pack/beta/SKILL.md", body: skillMD("beta", "второй")},
		// Вложенный SKILL.md — пример внутри beta, а не отдельный скилл.
		zfile{name: "pack/beta/examples/inner/SKILL.md", body: skillMD("inner", "")},
		zfile{name: "pack/README.md", body: "readme"},
	)
	// Сначала «посмотреть»: ничего не ставится.
	rep, err := e.m.InstallZip(data, "pack.zip", InstallOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Found) != 2 || rep.Found[0].Name != "alpha" || rep.Found[1].Name != "beta" || rep.Found[0].Description != "первый" {
		t.Fatalf("found: %+v", rep.Found)
	}
	if exists(e.claude) {
		t.Fatal("dry run must not create anything")
	}
	rep, err = e.m.InstallZip(data, "pack.zip", InstallOptions{Targets: []string{"claude", "codex"}, Only: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 2 || rep.Results[0].Status != "installed" || rep.Results[1].Status != "installed" {
		t.Fatalf("results: %+v", rep.Results)
	}
	for _, dir := range []string{e.claude, e.codex} {
		if b, err := os.ReadFile(filepath.Join(dir, "alpha", "scripts", "run.py")); err != nil || string(b) != "print(1)" {
			t.Fatalf("alpha not unpacked into %s: %v", dir, err)
		}
		if exists(filepath.Join(dir, "beta")) {
			t.Fatal("beta was not selected")
		}
	}
	// Повтор без replace — не трогаем.
	rep, _ = e.m.InstallZip(data, "pack.zip", InstallOptions{Targets: []string{"claude"}, Only: []string{"alpha"}})
	if rep.Results[0].Status != "exists" {
		t.Fatalf("want exists, got %+v", rep.Results[0])
	}
	// С replace — старая версия в резервной копии.
	os.WriteFile(filepath.Join(e.claude, "alpha", "mine.txt"), []byte("моё"), 0o644)
	rep, _ = e.m.InstallZip(data, "pack.zip", InstallOptions{Targets: []string{"claude"}, Only: []string{"alpha"}, Replace: true})
	if rep.Results[0].Status != "replaced" || rep.Results[0].Backup == "" {
		t.Fatalf("want replaced, got %+v", rep.Results[0])
	}
	if exists(filepath.Join(e.claude, "alpha", "mine.txt")) {
		t.Fatal("old version must be gone from the skills dir")
	}
	b, err := os.ReadFile(filepath.Join(e.backup, rep.Results[0].Backup, "claude", "alpha", "mine.txt"))
	if err != nil || string(b) != "моё" {
		t.Fatalf("old version must be in backup: %v", err)
	}
	if bk := e.m.Backups(); len(bk) != 1 || bk[0].Reason != "replace" || bk[0].Name != "alpha" {
		t.Fatalf("backups: %+v", bk)
	}
	// Временные папки не остались.
	entries, _ := os.ReadDir(e.claude)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".") {
			t.Fatalf("leftover temp dir %s", en.Name())
		}
	}
}

func TestInstallZipSingleSkillAtRoot(t *testing.T) {
	e := newEnv(t)
	// SKILL.md прямо в корне архива, имя из шапки.
	data := makeZip(t, zfile{name: "SKILL.md", body: skillMD("root-skill", "d")}, zfile{name: "ref/a.md", body: "a"})
	rep, err := e.m.InstallZip(data, "whatever.zip", InstallOptions{Targets: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Results[0].Name != "root-skill" || !exists(filepath.Join(e.claude, "root-skill", "ref", "a.md")) {
		t.Fatalf("results: %+v", rep.Results)
	}
	// Шапка без годного имени — имя файла архива.
	data = makeZip(t, zfile{name: "my-tool/SKILL.md", body: "---\nname: Мой инструмент\n---\n"})
	rep, err = e.m.InstallZip(data, "my-tool.zip", InstallOptions{Targets: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Results[0].Name != "my-tool" {
		t.Fatalf("results: %+v", rep.Results)
	}
}

func TestZipSlipRejected(t *testing.T) {
	outside := []string{"../evil/SKILL.md", "pack/../../evil/SKILL.md", "/abs/SKILL.md", "C:/x/SKILL.md", `pack\..\..\evil\SKILL.md`, "a/b:ads/SKILL.md"}
	for _, name := range outside {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			data := makeZip(t, zfile{name: "ok/SKILL.md", body: skillMD("ok", "")}, zfile{name: name, body: skillMD("evil", "")})
			_, err := e.m.InstallZip(data, "x.zip", InstallOptions{Targets: []string{"claude"}})
			if ErrorCode(err) != CodeUnsafeArchive {
				t.Fatalf("want unsafe_archive, got %v", err)
			}
			if exists(e.claude) {
				t.Fatal("nothing must be written for an unsafe archive")
			}
		})
	}
}

func TestZipSymlinkAndLimits(t *testing.T) {
	e := newEnv(t)
	data := makeZip(t,
		zfile{name: "s/SKILL.md", body: skillMD("s", "")},
		zfile{name: "s/link", body: "/etc/passwd", mode: fs.ModeSymlink | 0o777},
	)
	rep, err := e.m.InstallZip(data, "x.zip", InstallOptions{DryRun: true})
	if err != nil || rep.Found[0].Skip == "" {
		t.Fatalf("skill with symlink must be marked skip: %+v %v", rep, err)
	}
	if _, err := e.m.InstallZip(data, "x.zip", InstallOptions{Targets: []string{"claude"}}); ErrorCode(err) != CodeNoSkills {
		t.Fatalf("want no_skills, got %v", err)
	}
	// Распакованное больше лимита.
	e.m.MaxUnpacked = 1000
	big := makeZip(t, zfile{name: "b/SKILL.md", body: skillMD("b", "")}, zfile{name: "b/big.bin", body: strings.Repeat("x", 5000)})
	if _, err := e.m.InstallZip(big, "b.zip", InstallOptions{Targets: []string{"claude"}}); ErrorCode(err) != CodeTooLarge {
		t.Fatalf("want too_large, got %v", err)
	}
	// Архив больше лимита.
	e.m.MaxArchive = 100
	if _, err := e.m.InstallZip(big, "b.zip", InstallOptions{Targets: []string{"claude"}}); ErrorCode(err) != CodeTooLarge {
		t.Fatalf("want too_large for archive, got %v", err)
	}
	// Не zip.
	e.m.MaxArchive = 0
	if _, err := e.m.InstallZip([]byte("hello"), "b.zip", InstallOptions{Targets: []string{"claude"}}); ErrorCode(err) != CodeBadArchive {
		t.Fatalf("want bad_archive, got %v", err)
	}
	// Без скиллов.
	if _, err := e.m.InstallZip(makeZip(t, zfile{name: "a/readme.md", body: "x"}), "b.zip", InstallOptions{DryRun: true}); ErrorCode(err) != CodeNoSkills {
		t.Fatalf("want no_skills, got %v", err)
	}
}

func TestInstallGitHub(t *testing.T) {
	repo := makeZip(t,
		zfile{name: "r-main/README.md", body: "x"},
		zfile{name: "r-main/skills/foo/SKILL.md", body: skillMD("foo", "фу")},
		zfile{name: "r-main/skills/bar/SKILL.md", body: skillMD("bar", "бар")},
	)
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/o/r/zip/main", "/o/r/zip/HEAD":
			w.Write(repo)
		case "/o/huge/zip/HEAD":
			w.Header().Set("Content-Length", strconv.Itoa(30<<20))
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e := newEnv(t)
	e.m.CodeloadBase = srv.URL
	ctx := context.Background()

	rep, err := e.m.InstallGitHub(ctx, "https://github.com/o/r/tree/main/skills/foo", InstallOptions{Targets: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Found) != 1 || rep.Found[0].Name != "foo" || rep.Results[0].Status != "installed" {
		t.Fatalf("report: %+v %+v", rep.Found, rep.Results)
	}
	if !exists(filepath.Join(e.codex, "foo", SkillFile)) || exists(filepath.Join(e.codex, "bar")) {
		t.Fatal("only foo must be installed")
	}
	// Весь репозиторий: находим оба.
	rep, err = e.m.InstallGitHub(ctx, "https://github.com/o/r", InstallOptions{DryRun: true})
	if err != nil || len(rep.Found) != 2 {
		t.Fatalf("whole repo: %+v %v", rep, err)
	}
	if hits[len(hits)-1] != "/o/r/zip/HEAD" {
		t.Fatalf("default branch must be HEAD, hits %v", hits)
	}
	if _, err := e.m.InstallGitHub(ctx, "https://github.com/o/missing", InstallOptions{DryRun: true}); ErrorCode(err) != CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
	if _, err := e.m.InstallGitHub(ctx, "https://github.com/o/huge", InstallOptions{DryRun: true}); ErrorCode(err) != CodeTooLarge {
		t.Fatalf("want too_large, got %v", err)
	}
	if _, err := e.m.InstallGitHub(ctx, "https://github.com/o/r/tree/main/nope", InstallOptions{DryRun: true}); ErrorCode(err) != CodeNoSkills {
		t.Fatalf("want no_skills, got %v", err)
	}
	// Неизвестная цель — отказ ДО скачивания.
	before := len(hits)
	if _, err := e.m.InstallGitHub(ctx, "https://github.com/o/r", InstallOptions{Targets: []string{"nope"}}); ErrorCode(err) != CodeNoLocation {
		t.Fatalf("want unknown_location, got %v", err)
	}
	if len(hits) != before {
		t.Fatal("must not download for a bad target")
	}
}

func TestCopyDeleteRestore(t *testing.T) {
	e := newEnv(t)
	writeSkill(t, e.claude, "alpha", "первый")
	os.WriteFile(filepath.Join(e.claude, "alpha", "extra.txt"), []byte("x"), 0o644)

	res, err := e.m.Copy("alpha", "claude", []string{"codex", "claude"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != "installed" || res[1].Status != "same" {
		t.Fatalf("copy: %+v", res)
	}
	if !exists(filepath.Join(e.codex, "alpha", "extra.txt")) {
		t.Fatal("copied files missing")
	}
	if _, err := e.m.Copy("ghost", "claude", []string{"codex"}, false); ErrorCode(err) != CodeNoSkill {
		t.Fatalf("want unknown_skill, got %v", err)
	}
	if _, err := e.m.Copy("../x", "claude", []string{"codex"}, false); ErrorCode(err) != CodeBadName {
		t.Fatalf("want bad_name, got %v", err)
	}

	entry, err := e.m.Delete("codex", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(e.codex, "alpha")) {
		t.Fatal("deleted skill must leave the skills dir")
	}
	if !exists(filepath.Join(e.claude, "alpha")) {
		t.Fatal("other agent must keep its copy")
	}
	bk := e.m.Backups()
	if len(bk) != 1 || bk[0].ID != entry.ID || bk[0].Location != "codex" || bk[0].Reason != "delete" || bk[0].Description != "первый" {
		t.Fatalf("backups: %+v", bk)
	}
	// Пока стоит новый — без replace не возвращаем.
	writeSkill(t, e.codex, "alpha", "новый")
	if _, err := e.m.Restore(entry.ID, "codex", "alpha", false); ErrorCode(err) != CodeExists {
		t.Fatalf("want exists, got %v", err)
	}
	r, err := e.m.Restore(entry.ID, "codex", "alpha", true)
	if err != nil || r.Status != "replaced" {
		t.Fatalf("restore: %+v %v", r, err)
	}
	if !exists(filepath.Join(e.codex, "alpha", "extra.txt")) {
		t.Fatal("restored copy must be the old one")
	}
	// Старая запись ушла, новая (заменённая «новый») осталась.
	bk = e.m.Backups()
	if len(bk) != 1 || bk[0].Reason != "replace" {
		t.Fatalf("backups after restore: %+v", bk)
	}
	if exists(filepath.Join(e.backup, entry.ID)) {
		t.Fatal("empty backup folder must be removed")
	}
	if _, err := e.m.Restore("nope", "codex", "alpha", false); ErrorCode(err) != CodeNoBackup {
		t.Fatalf("want unknown_backup, got %v", err)
	}
	if _, err := e.m.Delete("codex", "ghost"); ErrorCode(err) != CodeNoSkill {
		t.Fatalf("want unknown_skill, got %v", err)
	}
}

// linkDir — как у настоящих аккаунтов: junction на Windows, симлинк иначе.
func linkDir(t *testing.T, src, dst string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := procutil.Hidden(exec.Command("cmd", "/c", "mklink", "/J", dst, src)).CombinedOutput()
		if err != nil {
			t.Fatalf("mklink: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(src, dst); err != nil {
		t.Fatal(err)
	}
}

func TestLinkedAccountFoldsIntoMain(t *testing.T) {
	e := newEnv(t)
	writeSkill(t, e.claude, "alpha", "")
	accShared := filepath.Join(t.TempDir(), "acc1")
	accOwn := filepath.Join(t.TempDir(), "acc2")
	os.MkdirAll(accShared, 0o755)
	linkDir(t, e.claude, filepath.Join(accShared, "skills"))
	writeSkill(t, filepath.Join(accOwn, "skills"), "mine", "")
	e.extra = []Root{
		{ID: "claude:acc1", Agent: "claude", AgentName: "Claude Code", Label: "рабочий", Kind: "account", Dir: filepath.Join(accShared, "skills")},
		{ID: "claude:acc2", Agent: "claude", AgentName: "Claude Code", Label: "личный", Kind: "account", Dir: filepath.Join(accOwn, "skills")},
	}
	locs := e.m.List()
	if find(locs, "claude:acc1") != nil {
		t.Fatal("linked account must be folded into main")
	}
	main := find(locs, "claude")
	if main == nil || len(main.SharedWith) != 1 || main.SharedWith[0] != "рабочий" || len(main.Skills) != 1 {
		t.Fatalf("main: %+v", main)
	}
	own := find(locs, "claude:acc2")
	if own == nil || len(own.Skills) != 1 || own.Skills[0].Name != "mine" {
		t.Fatalf("own account: %+v", own)
	}

	// Ставим «в основной и в связанный аккаунт» — ставится один раз.
	data := makeZip(t, zfile{name: "beta/SKILL.md", body: skillMD("beta", "")})
	rep, err := e.m.InstallZip(data, "b.zip", InstallOptions{Targets: []string{"claude", "claude:acc1"}})
	if err != nil || len(rep.Results) != 1 {
		t.Fatalf("install via link: %+v %v", rep, err)
	}
	// Копия из основного в связанный — «уже там».
	res, err := e.m.Copy("alpha", "claude", []string{"claude:acc1"}, false)
	if err != nil || res[0].Status != "same" {
		t.Fatalf("copy into linked: %+v %v", res, err)
	}
	// Удаление через адрес связанного аккаунта удаляет скилл, но НЕ связь.
	if _, err := e.m.Delete("claude:acc1", "alpha"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(e.claude, "alpha")) {
		t.Fatal("skill must be moved out")
	}
	li, err := os.Lstat(filepath.Join(accShared, "skills"))
	if err != nil || !isLink(li.Mode()) {
		t.Fatalf("account link must survive: %v", err)
	}
	if !exists(filepath.Join(e.claude, "beta", SkillFile)) {
		t.Fatal("main dir itself must survive")
	}
}

func TestLinkedSkillEntryAndReadOnly(t *testing.T) {
	e := newEnv(t)
	store := filepath.Join(t.TempDir(), "agents-skills")
	writeSkill(t, store, "shared-one", "из общего хранилища")
	os.MkdirAll(e.claude, 0o755)
	linkDir(t, filepath.Join(store, "shared-one"), filepath.Join(e.claude, "shared-one"))
	writeSkill(t, filepath.Join(e.codex, ".system"), "imagegen", "встроенный")
	plug := filepath.Join(t.TempDir(), "plug", "skills")
	writeSkill(t, plug, "doc", "")
	e.extra = []Root{{ID: "claude:plugins", Agent: "claude", AgentName: "Claude Code", Kind: "plugins", ReadOnly: true,
		Sources: []Source{{Name: "document-skills", Dir: plug}}}}

	locs := e.m.List()
	sk := find(locs, "claude").Skills
	if len(sk) != 1 || !sk[0].Link || sk[0].Description != "из общего хранилища" {
		t.Fatalf("linked skill: %+v", sk)
	}
	cx := find(locs, "codex").Skills
	if len(cx) != 1 || !cx[0].ReadOnly || cx[0].Source != "system" {
		t.Fatalf("codex system: %+v", cx)
	}
	pl := find(locs, "claude:plugins")
	if pl == nil || len(pl.Skills) != 1 || pl.Skills[0].Source != "document-skills" || !pl.Skills[0].ReadOnly {
		t.Fatalf("plugins: %+v", pl)
	}
	if _, err := e.m.Delete("claude:plugins", "doc"); ErrorCode(err) != CodeReadOnly {
		t.Fatalf("want readonly, got %v", err)
	}
	if _, err := e.m.Delete("codex", ".system"); ErrorCode(err) != CodeBadName {
		t.Fatalf("want bad_name, got %v", err)
	}

	// Копия связанного скилла — настоящая папка с содержимым.
	if _, err := e.m.Copy("shared-one", "claude", []string{"codex"}, false); err != nil {
		t.Fatal(err)
	}
	if li, _ := os.Lstat(filepath.Join(e.codex, "shared-one")); li == nil || isLink(li.Mode()) {
		t.Fatal("copy must be a real folder")
	}
	// Удаление связанного скилла уносит только ссылку; хранилище цело.
	entry, err := e.m.Delete("claude", "shared-one")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Link || !exists(filepath.Join(store, "shared-one", SkillFile)) {
		t.Fatalf("target must survive, entry %+v", entry)
	}
	if _, err := e.m.Restore(entry.ID, "claude", "shared-one", false); err != nil {
		t.Fatal(err)
	}
	li, err := os.Lstat(filepath.Join(e.claude, "shared-one"))
	if err != nil || !isLink(li.Mode()) {
		t.Fatalf("restored entry must be the link again: %v", err)
	}
}
