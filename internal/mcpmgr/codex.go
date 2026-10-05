package mcpmgr

import (
	"encoding/json"
	"sort"
	"strings"
)

// codexEntry — сервер в выводе `codex mcp list --json` / `get --json`
// (codex-cli 0.158.0, сверено запуском 29.09.2026).
type codexEntry struct {
	Name           string          `json:"name"`
	Enabled        *bool           `json:"enabled"`
	DisabledReason json.RawMessage `json:"disabled_reason"`
	Transport      struct {
		Type              string            `json:"type"` // stdio | streamable_http
		Command           string            `json:"command"`
		Args              []string          `json:"args"`
		Env               map[string]string `json:"env"`
		EnvVars           []string          `json:"env_vars"`
		Cwd               *string           `json:"cwd"`
		URL               string            `json:"url"`
		BearerTokenEnvVar *string           `json:"bearer_token_env_var"`
		HTTPHeaders       map[string]string `json:"http_headers"`
		EnvHTTPHeaders    map[string]string `json:"env_http_headers"`
		HTTPHeadersHelper json.RawMessage   `json:"http_headers_helper"`
	} `json:"transport"`
	EnabledTools      json.RawMessage `json:"enabled_tools"`
	DisabledTools     json.RawMessage `json:"disabled_tools"`
	StartupTimeoutSec json.RawMessage `json:"startup_timeout_sec"`
	ToolTimeoutSec    json.RawMessage `json:"tool_timeout_sec"`
}

func isNullish(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// codexRestorable — вернёт ли `codex mcp add` этот сервер ТАКИМ ЖЕ.
//
// CLI Codex умеет только команду, аргументы, `--env K=V`, `--url` и
// `--bearer-token-env-var`. Всё остальное (рабочая папка, проброс переменных,
// заголовки, списки инструментов, таймауты) при «выключить → включить» молча
// потерялось бы. Такой сервер выключать отсюда не даём — честнее, чем
// испортить настройку.
func codexRestorable(e codexEntry) bool {
	tr := e.Transport
	if (tr.Cwd != nil && *tr.Cwd != "") || len(tr.EnvVars) > 0 {
		return false
	}
	if len(tr.HTTPHeaders) > 0 || len(tr.EnvHTTPHeaders) > 0 || !isNullish(tr.HTTPHeadersHelper) {
		return false
	}
	for _, raw := range []json.RawMessage{e.EnabledTools, e.DisabledTools, e.StartupTimeoutSec, e.ToolTimeoutSec} {
		if !isNullish(raw) {
			return false
		}
	}
	return tr.Type == "stdio" || tr.Type == "streamable_http"
}

func codexServer(e codexEntry) Server {
	tr := e.Transport
	s := Server{
		Name:    e.Name,
		Agent:   "codex",
		Scope:   "user",
		Command: tr.Command,
		Args:    MaskArgs(tr.Args),
		Enabled: e.Enabled == nil || *e.Enabled,
	}
	switch tr.Type {
	case "stdio":
		s.Type = TypeStdio
	case "streamable_http":
		s.Type = TypeHTTP
	default:
		s.Type = tr.Type
	}
	if tr.URL != "" {
		s.URL = MaskURL(tr.URL)
	}
	keys := sortedKeys(tr.Env)
	keys = append(keys, tr.EnvVars...)
	sort.Strings(keys)
	s.EnvKeys = keys
	hk := append(sortedKeys(tr.HTTPHeaders), sortedKeys(tr.EnvHTTPHeaders)...)
	if tr.BearerTokenEnvVar != nil && *tr.BearerTokenEnvVar != "" {
		hk = append(hk, "Authorization")
	}
	sort.Strings(hk)
	s.HeaderKeys = hk
	if len(s.Args) == 0 {
		s.Args = nil
	}
	if len(s.EnvKeys) == 0 {
		s.EnvKeys = nil
	}
	if len(s.HeaderKeys) == 0 {
		s.HeaderKeys = nil
	}
	switch {
	case !s.Enabled:
		// Выключен в самом config.toml (`enabled = false`): это не наше
		// выключение, и включать его отсюда значило бы править TOML руками.
		s.ReadOnly = true
		s.Note = "disabled_by_agent"
	case codexRestorable(e):
		s.CanToggle = true
	default:
		s.Note = "extra_settings"
	}
	return s
}

func parseCodexList(out []byte) ([]codexEntry, error) {
	var list []codexEntry
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// codexAddArgs — аргументы `codex mcp add` из спецификации.
func codexAddArgs(s Spec) []string {
	args := []string{"mcp", "add", s.Name}
	if s.Type == TypeHTTP {
		return append(args, "--url", s.URL)
	}
	for _, k := range sortedKeys(s.Env) {
		args = append(args, "--env", k+"="+s.Env[k])
	}
	args = append(args, "--", s.Command)
	return append(args, s.Args...)
}

// codexRestoreArgs — аргументы `codex mcp add` из сохранённого `get --json`.
func codexRestoreArgs(name string, raw json.RawMessage) ([]string, []string, error) {
	var e codexEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, nil, err
	}
	tr := e.Transport
	secrets := []string{}
	for _, v := range tr.Env {
		secrets = append(secrets, v)
	}
	if tr.Type == "streamable_http" {
		args := []string{"mcp", "add", name, "--url", tr.URL}
		if tr.BearerTokenEnvVar != nil && *tr.BearerTokenEnvVar != "" {
			args = append(args, "--bearer-token-env-var", *tr.BearerTokenEnvVar)
		}
		return args, secrets, nil
	}
	args := []string{"mcp", "add", name}
	for _, k := range sortedKeys(tr.Env) {
		args = append(args, "--env", k+"="+tr.Env[k])
	}
	args = append(args, "--", tr.Command)
	return append(args, tr.Args...), secrets, nil
}
