package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/skillsmgr"
)

// Только временные каталоги: настоящий ~/.claude владельца не участвует.

func writeTestSkill(t *testing.T, dir, name string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, name), 0o755)
	os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: d\n---\n"), 0o644)
}

func TestBuildSkillRoots(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	codex := filepath.Join(home, ".codex")
	os.MkdirAll(filepath.Join(claude, "skills"), 0o755)
	os.MkdirAll(codex, 0o755)
	own := filepath.Join(home, "acc-own")
	writeTestSkill(t, filepath.Join(own, "skills"), "x")
	empty := filepath.Join(home, "acc-empty") // своего skills нет — места нет
	os.MkdirAll(empty, 0o755)

	// Плагин: две версии в кеше, берётся свежая; выключенный не виден.
	fresh := filepath.Join(claude, "plugins", "cache", "p", "new")
	writeTestSkill(t, filepath.Join(fresh, "skills"), "doc")
	writeTestSkill(t, filepath.Join(claude, "plugins", "cache", "off", "1", "skills"), "hidden")
	installed := map[string]any{"version": 2, "plugins": map[string]any{
		"doc-skills@market": []map[string]string{
			{"installPath": filepath.Join(claude, "plugins", "cache", "p", "old"), "lastUpdated": "2026-01-01T00:00:00Z"},
			{"installPath": fresh, "lastUpdated": "2026-09-01T00:00:00Z"},
		},
		"off@market": []map[string]string{{"installPath": filepath.Join(claude, "plugins", "cache", "off", "1"), "lastUpdated": "2026-09-01"}},
	}}
	b, _ := json.Marshal(installed)
	os.WriteFile(filepath.Join(claude, "plugins", "installed_plugins.json"), b, 0o644)
	os.WriteFile(filepath.Join(claude, "settings.json"), []byte(`{"enabledPlugins":{"off@market":false}}`), 0o644)

	f := accountsFile{Accounts: []AgentAccount{
		{ID: "acc-1", AgentID: "claude", Label: "рабочий", Dir: own},
		{ID: "acc-2", AgentID: "claude", Label: "пустой", Dir: empty},
	}}
	mainDir := func(d *agents.AgentDescriptor) string {
		switch d.ID {
		case "claude":
			return claude
		case "codex":
			return codex
		}
		return filepath.Join(home, ".nothing-"+d.ID)
	}
	roots := buildSkillRoots(f, mainDir, claude)
	ids := []string{}
	for _, r := range roots {
		ids = append(ids, r.ID)
	}
	got := strings.Join(ids, ",")
	// gemini попадёт, только если он установлен на машине, где идут тесты.
	got = strings.Replace(got, ",gemini", "", 1)
	if got != "claude,claude:acc-1,claude:plugins,codex" {
		t.Fatalf("roots: %s", got)
	}
	for _, r := range roots {
		if r.ID == "claude:plugins" {
			if len(r.Sources) != 1 || r.Sources[0].Name != "doc-skills" || r.Sources[0].Dir != filepath.Join(fresh, "skills") {
				t.Fatalf("plugin sources: %+v", r.Sources)
			}
		}
		if r.ID == "claude:acc-1" && (r.Label != "рабочий" || r.Kind != "account") {
			t.Fatalf("account root: %+v", r)
		}
	}
}

func useTestSkillsManager(t *testing.T) (claude, codex string) {
	t.Helper()
	base := t.TempDir()
	claude = filepath.Join(base, "claude", "skills")
	codex = filepath.Join(base, "codex", "skills")
	skillsOnce.Do(func() {})
	prev := skillsMgr
	skillsMgr = &skillsmgr.Manager{
		BackupDir:  filepath.Join(base, "backup"),
		StagingDir: t.TempDir(),
		Roots: func() []skillsmgr.Root {
			return []skillsmgr.Root{
				{ID: "claude", Agent: "claude", AgentName: "Claude Code", Kind: "main", Dir: claude},
				{ID: "codex", Agent: "codex", AgentName: "Codex CLI", Kind: "main", Dir: codex},
			}
		},
	}
	t.Cleanup(func() { skillsMgr = prev })
	return claude, codex
}

func TestSkillsHTTPFlow(t *testing.T) {
	claude, codex := useTestSkillsManager(t)
	s := &Server{}

	// ZIP multipart, параметры в query — как шлёт клиент.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w1, _ := zw.Create("pack/alpha/SKILL.md")
	w1.Write([]byte("---\nname: alpha\ndescription: первый\n---\n"))
	zw.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "alpha.zip")
	fw.Write(zbuf.Bytes())
	mw.Close()
	req := httptest.NewRequest("POST", "/api/skills/install?targets=claude&dry_run=0", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.apiSkillsInstall(rec, req, 1)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"installed"`) {
		t.Fatalf("zip install: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(claude, "alpha", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	// Копия.
	rec = httptest.NewRecorder()
	s.apiSkillsCopy(rec, httptest.NewRequest("POST", "/api/skills/copy", strings.NewReader(`{"name":"alpha","from":"claude","to":["codex"]}`)), 1)
	if rec.Code != 200 {
		t.Fatalf("copy: %d %s", rec.Code, rec.Body)
	}

	// Удаление → список резервных копий → возврат.
	rec = httptest.NewRecorder()
	s.apiSkillsDelete(rec, httptest.NewRequest("DELETE", "/api/skills?location=codex&name=alpha", nil), 1)
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	var del struct {
		Backup skillsmgr.BackupEntry `json:"backup"`
	}
	json.Unmarshal(rec.Body.Bytes(), &del)
	if _, err := os.Stat(filepath.Join(codex, "alpha")); !os.IsNotExist(err) {
		t.Fatal("skill must be moved to backup")
	}
	rec = httptest.NewRecorder()
	s.apiSkillsList(rec, httptest.NewRequest("GET", "/api/skills", nil), 1)
	var list struct {
		Locations []skillsmgr.Location    `json:"locations"`
		Backups   []skillsmgr.BackupEntry `json:"backups"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Locations) != 2 || len(list.Backups) != 1 || list.Backups[0].ID != del.Backup.ID {
		t.Fatalf("list: %s", rec.Body)
	}
	rec = httptest.NewRecorder()
	payload, _ := json.Marshal(map[string]string{"id": del.Backup.ID, "location": "codex", "name": "alpha"})
	s.apiSkillsRestore(rec, httptest.NewRequest("POST", "/api/skills/restore", bytes.NewReader(payload)), 1)
	if rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(codex, "alpha", "SKILL.md")); err != nil {
		t.Fatal("restored skill missing")
	}

	// Ошибки — с кодом и статусом.
	cases := []struct {
		req   *http.Request
		code  int
		ecode string
	}{
		{httptest.NewRequest("POST", "/api/skills/install", strings.NewReader(`{"url":"https://gitlab.com/a/b","dry_run":true}`)), 400, "bad_url"},
		{httptest.NewRequest("DELETE", "/api/skills?location=codex&name=..", nil), 400, "bad_name"},
		{httptest.NewRequest("DELETE", "/api/skills?location=nope&name=alpha", nil), 400, "unknown_location"},
		{httptest.NewRequest("DELETE", "/api/skills?location=codex&name=ghost", nil), 404, "unknown_skill"},
	}
	for _, c := range cases {
		rec = httptest.NewRecorder()
		if c.req.Method == "DELETE" {
			s.apiSkillsDelete(rec, c.req, 1)
		} else {
			s.apiSkillsInstall(rec, c.req, 1)
		}
		var e struct{ Code string }
		json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != c.code || e.Code != c.ecode {
			t.Fatalf("%s %s: %d %s", c.req.Method, c.req.URL, rec.Code, rec.Body)
		}
	}
}
