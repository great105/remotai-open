package mcpmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// claudeConfigFile — где лежит `.claude.json` аккаунта.
//
// ГРАБЛЯ (поймана живым прогоном ещё при переносе MCP в новый аккаунт, см.
// web/account_share.go): у ОСНОВНОГО аккаунта файл в корне домашней папки
// (`~/.claude.json`), а у аккаунта со своим CLAUDE_CONFIG_DIR — внутри этого
// каталога. Перепроверено 29.09.2026: `claude mcp add -s user` с
// CLAUDE_CONFIG_DIR пишет в `<каталог>/.claude.json`.
func claudeConfigFile(t Target) string {
	if t.ConfigDir != "" {
		return filepath.Join(t.ConfigDir, ".claude.json")
	}
	return filepath.Join(t.Home, ".claude.json")
}

// claudeEntry — сервер в `.claude.json`, как его пишет сам Claude Code.
type claudeEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type claudeFile struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	} `json:"projects"`
}

func readClaudeFile(t Target) (claudeFile, error) {
	var f claudeFile
	raw, err := os.ReadFile(claudeConfigFile(t))
	if os.IsNotExist(err) {
		return f, nil // агент ещё ни разу не запускался в этом аккаунте
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return claudeFile{}, err
	}
	return f, nil
}

// claudeServer превращает запись в безопасный для клиента вид.
func claudeServer(name string, raw json.RawMessage, scope, project string) Server {
	var e claudeEntry
	_ = json.Unmarshal(raw, &e)
	typ := e.Type
	if typ == "" {
		// Старые записи без поля type: команда — значит stdio.
		if e.Command != "" {
			typ = TypeStdio
		} else if e.URL != "" {
			typ = TypeHTTP
		}
	}
	s := Server{
		Name:       name,
		Agent:      "claude",
		Scope:      scope,
		Project:    project,
		Type:       typ,
		Command:    e.Command,
		Args:       MaskArgs(e.Args),
		EnvKeys:    sortedKeys(e.Env),
		HeaderKeys: sortedKeys(e.Headers),
		Enabled:    true,
	}
	if e.URL != "" {
		s.URL = MaskURL(e.URL)
	}
	if len(s.Args) == 0 {
		s.Args = nil
	}
	return s
}

// listClaude — серверы аккаунта: общие (user) и проектные (только чтение).
//
// Проектные — это `projects[<папка>].mcpServers` («local» в терминах Claude):
// они принадлежат конкретной папке, и выключать их отсюда значило бы
// переписывать чужой раздел файла. Серверы из `.mcp.json` в репозиториях не
// показываем вовсе: какие папки считать проектами, отсюда не видно.
func listClaude(t Target) ([]Server, error) {
	f, err := readClaudeFile(t)
	if err != nil {
		return nil, err
	}
	out := []Server{}
	for _, name := range sortedKeys(f.MCPServers) {
		s := claudeServer(name, f.MCPServers[name], "user", "")
		s.CanToggle = true
		out = append(out, s)
	}
	projects := sortedKeys(f.Projects)
	for _, p := range projects {
		m := f.Projects[p].MCPServers
		for _, name := range sortedKeys(m) {
			s := claudeServer(name, m[name], "project", p)
			s.ReadOnly = true
			s.Note = "project"
			out = append(out, s)
		}
	}
	return out, nil
}

// claudeUserDef — определение общего сервера целиком (С СЕКРЕТАМИ) для
// хранилища выключенных. nil — такого нет.
func claudeUserDef(t Target, name string) (json.RawMessage, error) {
	f, err := readClaudeFile(t)
	if err != nil {
		return nil, err
	}
	raw, ok := f.MCPServers[name]
	if !ok {
		return nil, nil
	}
	return raw, nil
}

// claudeAddJSON — то, что уйдёт в `claude mcp add-json`. Формат — ровно тот,
// что Claude сам пишет в файл (сверено живым прогоном): type, command, args,
// env | type, url, headers. Пустые поля не кладём.
func claudeAddJSON(s Spec) (string, error) {
	obj := map[string]any{"type": s.Type}
	if s.Type == TypeStdio {
		obj["command"] = s.Command
		args := s.Args
		if args == nil {
			args = []string{}
		}
		obj["args"] = args
		env := s.Env
		if env == nil {
			env = map[string]string{}
		}
		obj["env"] = env
	} else {
		obj["url"] = s.URL
		if len(s.Headers) > 0 {
			obj["headers"] = s.Headers
		}
	}
	b, err := json.Marshal(obj)
	return string(b), err
}

// claudeDefSecrets — значения env и заголовков из сохранённого определения
// (для вычистки из текста ошибок).
func claudeDefSecrets(raw json.RawMessage) []string {
	var e claudeEntry
	_ = json.Unmarshal(raw, &e)
	out := []string{}
	for _, v := range e.Env {
		out = append(out, v)
	}
	for _, v := range e.Headers {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
