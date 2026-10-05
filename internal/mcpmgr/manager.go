package mcpmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Manager — операции над MCP-серверами агентов.
type Manager struct {
	Store *Store
	run   runner
	// mu — одна запись за раз: CLI переписывает файл конфига целиком, и две
	// параллельные записи в один `.claude.json` потеряли бы одну из правок.
	mu sync.Mutex
}

// New — менеджер с настоящим запуском CLI.
func New(store *Store) *Manager {
	return &Manager{Store: store, run: runCLI}
}

func (m *Manager) cliError(t Target, stderr, stdout []byte, err error, secrets []string) error {
	if errors.Is(err, errNoCLI) {
		return userErr(ErrUnsupported, "%s не найден на компьютере", agentTitle(t.Agent))
	}
	text := string(stderr)
	if len(text) == 0 {
		text = string(stdout)
	}
	msg := scrub(text, secrets)
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%s: %s", agentTitle(t.Agent), msg)
}

// listLive — серверы, которые сейчас в конфиге агента.
func (m *Manager) listLive(ctx context.Context, t Target) ([]Server, []codexEntry, error) {
	switch t.Agent {
	case "claude":
		list, err := listClaude(t)
		return list, nil, err
	case "codex":
		out, stderr, err := m.run(ctx, t, "mcp", "list", "--json")
		if err != nil {
			return nil, nil, m.cliError(t, stderr, out, err, nil)
		}
		entries, err := parseCodexList(out)
		if err != nil {
			return nil, nil, err
		}
		list := make([]Server, 0, len(entries))
		for _, e := range entries {
			list = append(list, codexServer(e))
		}
		return list, entries, nil
	}
	return nil, nil, userErr(ErrUnsupported, "Этот агент пока не умеет MCP отсюда")
}

// List — серверы агента в аккаунте, включая выключенные нами (они идут с
// Enabled=false и без секретов).
func (m *Manager) List(ctx context.Context, t Target) ([]Server, error) {
	live, _, err := m.listLive(ctx, t)
	if err != nil {
		return nil, err
	}
	disabled, err := m.Store.List(t.Agent, t.AccountID)
	if err != nil {
		return nil, err
	}
	for _, d := range disabled {
		s := disabledServer(t.Agent, d)
		live = append(live, s)
	}
	sort.SliceStable(live, func(i, j int) bool {
		if live[i].Scope != live[j].Scope {
			return live[i].Scope == "user"
		}
		return live[i].Name < live[j].Name
	})
	return live, nil
}

func disabledServer(agent string, d Disabled) Server {
	var s Server
	switch agent {
	case "claude":
		s = claudeServer(d.Name, d.Def, "user", "")
	default:
		var e codexEntry
		_ = json.Unmarshal(d.Def, &e)
		e.Name = d.Name
		s = codexServer(e)
		s.ReadOnly = false
		s.Note = ""
	}
	s.Enabled = false
	s.CanToggle = true
	return s
}

func (m *Manager) exists(ctx context.Context, t Target, name string) (bool, error) {
	live, _, err := m.listLive(ctx, t)
	if err != nil {
		return false, err
	}
	for _, s := range live {
		if s.Name == name && s.Scope == "user" {
			return true, nil
		}
	}
	d, err := m.Store.Get(t.Agent, t.AccountID, name)
	if err != nil {
		return false, err
	}
	return d != nil, nil
}

// Add добавляет сервер через CLI агента.
func (m *Manager) Add(ctx context.Context, t Target, spec Spec) error {
	spec, err := spec.Normalize()
	if err != nil {
		return err
	}
	if err := spec.CheckCaps(t.Agent); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Codex на повтор имени отвечает УСПЕХОМ и молча перезаписывает сервер
	// (проверено запуском) — чужие ключи пропали бы без вопроса. Поэтому
	// проверяем сами, и выключенные тоже: иначе «включить» потом упрётся в
	// занятое имя.
	ok, err := m.exists(ctx, t, spec.Name)
	if err != nil {
		return err
	}
	if ok {
		return userErr(ErrExists, "Сервер «%s» уже есть у %s", spec.Name, agentTitle(t.Agent))
	}
	var args []string
	switch t.Agent {
	case "claude":
		js, err := claudeAddJSON(spec)
		if err != nil {
			return err
		}
		args = []string{"mcp", "add-json", "-s", "user", spec.Name, js}
	case "codex":
		args = codexAddArgs(spec)
	}
	out, stderr, err := m.run(ctx, t, args...)
	if err != nil {
		return m.cliError(t, stderr, out, err, spec.secrets())
	}
	return nil
}

// Remove удаляет сервер. Выключенный удаляется из нашего хранилища — в
// конфиге агента его и так нет.
func (m *Manager) Remove(ctx context.Context, t Target, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.Store.Get(t.Agent, t.AccountID, name)
	if err != nil {
		return err
	}
	if d != nil {
		return m.Store.Delete(t.Agent, t.AccountID, name)
	}
	if err := m.requireUserLive(ctx, t, name); err != nil {
		return err
	}
	return m.removeLive(ctx, t, name)
}

func (m *Manager) requireUserLive(ctx context.Context, t Target, name string) error {
	live, _, err := m.listLive(ctx, t)
	if err != nil {
		return err
	}
	for _, s := range live {
		if s.Name != name {
			continue
		}
		if s.Scope == "user" {
			return nil
		}
	}
	for _, s := range live {
		if s.Name == name {
			return userErr(ErrReadOnly, "Сервер «%s» подключён только в папке проекта — меняйте его там", name)
		}
	}
	return userErr(ErrNotFound, "Сервера «%s» нет", name)
}

func (m *Manager) removeLive(ctx context.Context, t Target, name string) error {
	args := []string{"mcp", "remove", name}
	if t.Agent == "claude" {
		args = []string{"mcp", "remove", "-s", "user", name}
	}
	out, stderr, err := m.run(ctx, t, args...)
	if err != nil {
		return m.cliError(t, stderr, out, err, nil)
	}
	return nil
}

// SetEnabled выключает или включает сервер.
//
// Выключить: сначала определение (с секретами) — в хранилище, потом удалить
// из конфига. Именно в таком порядке: если CLI упадёт после записи, у нас
// лишняя копия, а не потерянный ключ. Упал — копию убираем.
// Включить: вернуть тем же CLI, потом забыть копию.
func (m *Manager) SetEnabled(ctx context.Context, t Target, name string, enabled bool) error {
	if err := ValidName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if enabled {
		return m.enable(ctx, t, name)
	}
	return m.disable(ctx, t, name)
}

func (m *Manager) disable(ctx context.Context, t Target, name string) error {
	if d, err := m.Store.Get(t.Agent, t.AccountID, name); err != nil {
		return err
	} else if d != nil {
		return nil // уже выключен
	}
	if err := m.requireUserLive(ctx, t, name); err != nil {
		return err
	}
	var def json.RawMessage
	switch t.Agent {
	case "claude":
		raw, err := claudeUserDef(t, name)
		if err != nil {
			return err
		}
		if raw == nil {
			return userErr(ErrNotFound, "Сервера «%s» нет", name)
		}
		def = raw
	case "codex":
		out, stderr, err := m.run(ctx, t, "mcp", "get", name, "--json")
		if err != nil {
			return m.cliError(t, stderr, out, err, nil)
		}
		var e codexEntry
		if err := json.Unmarshal(out, &e); err != nil {
			return err
		}
		if !codexRestorable(e) {
			return userErr(ErrUnsupported, "У сервера «%s» есть настройки, которые Codex не вернёт после включения — выключать его отсюда нельзя", name)
		}
		def = json.RawMessage(out)
	default:
		return userErr(ErrUnsupported, "Этот агент пока не умеет MCP отсюда")
	}
	if err := m.Store.Put(Disabled{Agent: t.Agent, AccountID: t.AccountID, Name: name, Def: def}); err != nil {
		return err
	}
	if err := m.removeLive(ctx, t, name); err != nil {
		_ = m.Store.Delete(t.Agent, t.AccountID, name)
		return err
	}
	return nil
}

func (m *Manager) enable(ctx context.Context, t Target, name string) error {
	d, err := m.Store.Get(t.Agent, t.AccountID, name)
	if err != nil {
		return err
	}
	if d == nil {
		// Нечего возвращать: либо уже включён, либо его нет вовсе.
		if err := m.requireUserLive(ctx, t, name); err != nil {
			return err
		}
		return nil
	}
	live, _, err := m.listLive(ctx, t)
	if err != nil {
		return err
	}
	for _, s := range live {
		if s.Name == name && s.Scope == "user" {
			return userErr(ErrExists, "Пока сервер был выключен, у %s появился другой «%s». Удалите один из них", agentTitle(t.Agent), name)
		}
	}
	var args, secrets []string
	switch t.Agent {
	case "claude":
		// В одну строку: в `.claude.json` определение лежит с отступами, а
		// аргумент командной строки с переводами строк — лишний риск.
		var compact bytes.Buffer
		if err := json.Compact(&compact, d.Def); err != nil {
			return err
		}
		args = []string{"mcp", "add-json", "-s", "user", name, compact.String()}
		secrets = claudeDefSecrets(d.Def)
	case "codex":
		args, secrets, err = codexRestoreArgs(name, d.Def)
		if err != nil {
			return err
		}
	default:
		return userErr(ErrUnsupported, "Этот агент пока не умеет MCP отсюда")
	}
	out, stderr, err := m.run(ctx, t, args...)
	if err != nil {
		return m.cliError(t, stderr, out, err, secrets)
	}
	return m.Store.Delete(t.Agent, t.AccountID, name)
}
