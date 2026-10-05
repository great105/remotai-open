package web

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/skillsmgr"
)

// Скиллы агентов: раздел «Скиллы» на экране «Агенты».
//
// Здесь только адаптер: какие места со скиллами есть на ЭТОМ компьютере
// (основные профили, свои каталоги аккаунтов, плагины Claude) и HTTP. Вся
// работа с файлами и архивами — в internal/skillsmgr.
//
// Где лежат скиллы — сверено с живым деревом машины владельца (29.09.2026,
// только чтение): `~/.claude/skills` (63), `~/.codex/skills` (+ встроенные в
// `.system`), `~/.gemini/skills`, плагины Claude — `<installPath>/skills` из
// `~/.claude/plugins/installed_plugins.json`. Папки skills у аккаунтов —
// junction на основной профиль (account_share.go), поэтому менеджер
// сворачивает их в основной, а не показывает второй раз.

// skillAgents — агенты, у которых есть каталог skills, по порядку показа.
var skillAgents = []string{"claude", "codex", "gemini"}

var (
	skillsOnce sync.Once
	skillsMgr  *skillsmgr.Manager
)

func skillsManager() *skillsmgr.Manager {
	skillsOnce.Do(func() {
		skillsMgr = &skillsmgr.Manager{
			Roots:     currentSkillRoots,
			BackupDir: paths.StateFile("skills-backup"),
		}
	})
	return skillsMgr
}

// skillAgentDir — каталог конфига ОСНОВНОГО профиля агента. У Codex его
// может переопределять CODEX_HOME (как в launch_args.go).
func skillAgentDir(d *agents.AgentDescriptor) string {
	if d.ID == "codex" {
		if h := os.Getenv("CODEX_HOME"); h != "" {
			return h
		}
	}
	return mainAccountDir(d)
}

func currentSkillRoots() []skillsmgr.Root {
	accountsMu.Lock()
	f := loadAccountsFile()
	accountsMu.Unlock()
	return buildSkillRoots(f, skillAgentDir, filepath.Join(realUserHome(), ".claude"))
}

// buildSkillRoots собирает места. Основной профиль идёт первым: менеджер
// считает каноническим первое место с данной папкой, и связанный аккаунт
// должен свернуться в основной, а не наоборот.
func buildSkillRoots(f accountsFile, mainDir func(*agents.AgentDescriptor) string, claudeDir string) []skillsmgr.Root {
	var roots []skillsmgr.Root
	for _, id := range skillAgents {
		d := agents.GetDescriptor(id)
		if d == nil {
			continue
		}
		cfg := mainDir(d)
		if cfg == "" {
			continue
		}
		// Агента нет и его каталога нет — места нет: ставить скилл агенту,
		// которого на компьютере нет, бессмысленно.
		if st, err := os.Stat(cfg); (err != nil || !st.IsDir()) && !d.IsDetected() {
			continue
		}
		roots = append(roots, skillsmgr.Root{ID: id, Agent: id, AgentName: d.Name, Kind: "main", Dir: filepath.Join(cfg, "skills")})
		for _, a := range f.Accounts {
			if a.AgentID != id || a.Dir == "" {
				continue
			}
			dir := filepath.Join(d.AccountCredentialsDir(a.Dir), "skills")
			// Своего каталога skills у аккаунта нет — места нет: создавать его
			// нельзя, иначе связь с основным уже не поставится.
			if _, err := os.Lstat(dir); err != nil {
				continue
			}
			label := a.Label
			if label == "" {
				label = a.ID
			}
			roots = append(roots, skillsmgr.Root{ID: id + ":" + a.ID, Agent: id, AgentName: d.Name, Label: label, Kind: "account", Dir: dir})
		}
		if id == "claude" && claudeDir != "" {
			if src := claudePluginSkills(claudeDir); len(src) > 0 {
				roots = append(roots, skillsmgr.Root{ID: "claude:plugins", Agent: id, AgentName: d.Name, Kind: "plugins", ReadOnly: true, Sources: src})
			}
		}
	}
	return roots
}

// claudePluginSkills — каталоги skills установленных плагинов Claude Code.
// У плагина бывает несколько версий в кеше; берём самую свежую по
// lastUpdated. Выключенные в settings.json (`enabledPlugins: false`) не
// показываем: агент их не видит.
func claudePluginSkills(claudeDir string) []skillsmgr.Source {
	raw, err := os.ReadFile(filepath.Join(claudeDir, "plugins", "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var file struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
			LastUpdated string `json:"lastUpdated"`
		} `json:"plugins"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return nil
	}
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if b, err := os.ReadFile(filepath.Join(claudeDir, "settings.json")); err == nil {
		_ = json.Unmarshal(b, &settings)
	}
	var out []skillsmgr.Source
	for key, versions := range file.Plugins {
		if on, ok := settings.EnabledPlugins[key]; ok && !on {
			continue
		}
		best := -1
		for i, v := range versions {
			if best < 0 || v.LastUpdated > versions[best].LastUpdated {
				best = i
			}
		}
		if best < 0 || versions[best].InstallPath == "" {
			continue
		}
		dir := filepath.Join(versions[best].InstallPath, "skills")
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		name := key
		if i := strings.Index(key, "@"); i > 0 {
			name = key[:i]
		}
		out = append(out, skillsmgr.Source{Name: name, Dir: dir})
	}
	return out
}

func skillsError(w http.ResponseWriter, err error) {
	code := skillsmgr.ErrorCode(err)
	status := http.StatusInternalServerError
	switch code {
	case skillsmgr.CodeBadURL, skillsmgr.CodeBadArchive, skillsmgr.CodeUnsafeArchive, skillsmgr.CodeNoSkills,
		skillsmgr.CodeBadName, skillsmgr.CodeNoLocation:
		status = http.StatusBadRequest
	case skillsmgr.CodeNotFound, skillsmgr.CodeNoSkill, skillsmgr.CodeNoBackup:
		status = http.StatusNotFound
	case skillsmgr.CodeExists:
		status = http.StatusConflict
	case skillsmgr.CodeReadOnly:
		status = http.StatusForbidden
	case skillsmgr.CodeTooLarge:
		status = http.StatusRequestEntityTooLarge
	case skillsmgr.CodeDownload:
		status = http.StatusBadGateway
	}
	if code == "" {
		code = "skills_failed"
	}
	jsonErrorCode(w, status, code, err.Error(), nil)
}

// GET /api/skills — места, скиллы в них и резервные копии.
func (s *Server) apiSkillsList(w http.ResponseWriter, r *http.Request, uid int64) {
	m := skillsManager()
	jsonResp(w, map[string]any{"locations": m.List(), "backups": m.Backups()})
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// POST /api/skills/install
//
// JSON `{url, targets, replace, dry_run, only}` — ссылка на GitHub;
// multipart с частью `file` — загруженный ZIP, параметры в query
// (`targets=a,b&replace=1&dry_run=1&only=x,y`): так устроены все загрузки
// клиента, и облачный путь через релей передаёт query как есть.
func (s *Server) apiSkillsInstall(w http.ResponseWriter, r *http.Request, uid int64) {
	m := skillsManager()
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		const limit = 20 << 20
		r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
		mr, err := r.MultipartReader()
		if err != nil {
			jsonErrorCode(w, http.StatusBadRequest, skillsmgr.CodeBadArchive, "Не получилось прочитать загрузку", nil)
			return
		}
		var data []byte
		var name string
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				var tooBig *http.MaxBytesError
				if errors.As(err, &tooBig) {
					jsonErrorCode(w, http.StatusRequestEntityTooLarge, skillsmgr.CodeTooLarge, "Архив больше 20 МБ", nil)
					return
				}
				jsonErrorCode(w, http.StatusBadRequest, skillsmgr.CodeBadArchive, "Загрузка оборвалась", nil)
				return
			}
			if part.FormName() != "file" || data != nil {
				part.Close()
				continue
			}
			name = part.FileName()
			data, err = io.ReadAll(io.LimitReader(part, limit+1))
			part.Close()
			if err != nil {
				jsonErrorCode(w, http.StatusBadRequest, skillsmgr.CodeBadArchive, "Загрузка оборвалась", nil)
				return
			}
			if len(data) > limit {
				jsonErrorCode(w, http.StatusRequestEntityTooLarge, skillsmgr.CodeTooLarge, "Архив больше 20 МБ", nil)
				return
			}
		}
		if data == nil {
			jsonErrorCode(w, http.StatusBadRequest, skillsmgr.CodeBadArchive, "Файл не пришёл", nil)
			return
		}
		q := r.URL.Query()
		opt := skillsmgr.InstallOptions{Targets: splitList(q.Get("targets")), Replace: q.Get("replace") == "1",
			DryRun: q.Get("dry_run") == "1", Only: splitList(q.Get("only"))}
		rep, err := m.InstallZip(data, name, opt)
		if err != nil {
			skillsError(w, err)
			return
		}
		jsonResp(w, rep)
		return
	}
	var req struct {
		URL     string   `json:"url"`
		Targets []string `json:"targets"`
		Replace bool     `json:"replace"`
		DryRun  bool     `json:"dry_run"`
		Only    []string `json:"only"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Некорректный запрос", nil)
		return
	}
	rep, err := m.InstallGitHub(r.Context(), req.URL, skillsmgr.InstallOptions{Targets: req.Targets, Replace: req.Replace, DryRun: req.DryRun, Only: req.Only})
	if err != nil {
		skillsError(w, err)
		return
	}
	jsonResp(w, rep)
}

// POST /api/skills/copy {name, from, to, replace}
func (s *Server) apiSkillsCopy(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		Name    string   `json:"name"`
		From    string   `json:"from"`
		To      []string `json:"to"`
		Replace bool     `json:"replace"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Некорректный запрос", nil)
		return
	}
	res, err := skillsManager().Copy(req.Name, req.From, req.To, req.Replace)
	if err != nil {
		skillsError(w, err)
		return
	}
	jsonResp(w, map[string]any{"results": res})
}

// DELETE /api/skills?location=…&name=… — не удаляет, а переносит в
// резервную копию. Параметры в query: тело у DELETE теряют прокси.
func (s *Server) apiSkillsDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	q := r.URL.Query()
	entry, err := skillsManager().Delete(q.Get("location"), q.Get("name"))
	if err != nil {
		skillsError(w, err)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "backup": entry})
}

// POST /api/skills/restore {id, location, name, replace}
func (s *Server) apiSkillsRestore(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		ID       string `json:"id"`
		Location string `json:"location"`
		Name     string `json:"name"`
		Replace  bool   `json:"replace"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "Некорректный запрос", nil)
		return
	}
	res, err := skillsManager().Restore(req.ID, req.Location, req.Name, req.Replace)
	if err != nil {
		skillsError(w, err)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "result": res})
}

// GET /api/skills/backups — только резервные копии.
func (s *Server) apiSkillsBackups(w http.ResponseWriter, r *http.Request, uid int64) {
	jsonResp(w, map[string]any{"backups": skillsManager().Backups()})
}
