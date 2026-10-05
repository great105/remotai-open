package pty

import (
	"encoding/json"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/paths"
)

// Meta holds per-session metadata that survives app restarts: the user-set
// name and (for persistent terminals) the host record needed to re-attach to
// the live shell process after remotai restarts.
type Meta struct {
	Name string      `json:"name,omitempty"`
	Host *hostRecord `json:"host,omitempty"`
	// Group — имя пользовательской «папки» в списке терминалов ("" = без папки),
	// Sort — ручной порядок карточки (0 = не задан, клиент сортирует по -created).
	Group string  `json:"group,omitempty"`
	Sort  float64 `json:"sort,omitempty"`
	// Modes — активные DEC private-режимы (alt-screen/mouse/…) на момент
	// последнего изменения. Сканер живёт в процессе сервера и умирает вместе с
	// ним; без персиста рестарт (каждое автообновление!) забывал ?1049h живых
	// TUI-сессий → клиент после reset оставался в обычном буфере и скролл
	// пальцем молча ломался. Восстанавливается в reattachHosts.
	Modes []int `json:"modes,omitempty"`
	// ViewCols/ViewRows — последний ПРИМЕНЁННЫЙ к PTY размер. Живёт в памяти
	// агента (viewCols) и умирал с ним: после автообновления зеркало экрана
	// рождалось в born-геометрии (80x24) и переваривало реплей кольца, писанный
	// под живой размер (48xN), — история приезжала на телефон рваной и с
	// дублями (жалоба 13.08.2026 «кодекс и кими дублируют и рвут»; в логе —
	// кадры 80x24 при PTY 48x31). Восстанавливается в reattachHosts.
	ViewCols int `json:"view_cols,omitempty"`
	ViewRows int `json:"view_rows,omitempty"`
	// Folders is used only on the reserved metaFoldersKey entry. Keeping the
	// registry in the same atomic pty.json store makes empty folders consistent
	// across phone, Telegram and the desktop panel without changing the legacy
	// top-level map format.
	Folders []string `json:"folders,omitempty"`
	// Lost — терминал, чей pty-host не пережил перезагрузку компьютера.
	//
	// Раньше такая запись просто удалялась (reattachHosts → Forget), и человек,
	// включив компьютер, видел пустой список: работа, которая шла «пока меня
	// нет», исчезала молча — вместе с папкой, именем и рабочим каталогом.
	// Теперь запись живёт дальше в этом поле, и терминал можно продолжить.
	Lost *lostRecord `json:"lost,omitempty"`
	// Agent — какой AI-агент работал в терминале в последний раз («claude»).
	// Обновляется вместе с хвостом вывода; нужен, чтобы карточка потерянного
	// терминала называла работу, а не просто «сессия».
	Agent string `json:"agent,omitempty"`
	// AccountID/AccountLabel — под каким аккаунтом нейросети запущен агент
	// ИМЕННО В ЭТОМ терминале.
	//
	// Аккаунт выдаётся процессу окружением в момент запуска, поэтому дальше он
	// уже не меняется: в одном терминале может работать рабочая подписка, в
	// соседнем — личная, одновременно. Без этой записи человек, вернувшись к
	// терминалу через час, не может узнать, чьи лимиты он сейчас тратит, — а
	// это ровно тот вопрос, ради которого второй аккаунт и заводят.
	AccountID    string `json:"account_id,omitempty"`
	AccountLabel string `json:"account_label,omitempty"`
	// Sleep — агент этого терминала усыплён человеком (см. agent_sleep.go):
	// процесс снят, беседа ждёт продолжения по номеру. Лежит на диске, потому
	// что спят часами, а автообновление перезапускает Remotai в любой момент.
	Sleep *SleepRecord `json:"sleep,omitempty"`
}

// lostRecord — всё, что нужно, чтобы поднять терминал заново тем же, чем он
// был: тот же каталог, тот же шелл, тот же владелец. Имя, папка и порядок
// карточки лежат в самом Meta и переживают потерю сами.
type lostRecord struct {
	CWD     string `json:"cwd"`
	Shell   string `json:"shell"`
	UID     int64  `json:"uid"`
	Created int64  `json:"created"` // unix ms — когда терминал завели впервые
	LostAt  int64  `json:"lost_at"` // unix ms — когда обнаружили, что хоста нет
}

// isEmpty — в записи не осталось ничего, что стоило бы пережить перезапуск.
// Терминал, ждущий восстановления (Lost), пустым НЕ считается: удалив такую
// запись при переименовании, мы стёрли бы единственный след работы человека.
func (m Meta) isEmpty() bool {
	return m.Name == "" && m.Host == nil && m.Group == "" && m.Sort == 0 && m.Lost == nil &&
		m.AccountID == "" && m.Sleep == nil
}

const metaFoldersKey = "__folders__"

// MetaStore persists per-session metadata to ~/.tgcontrol-pty.json.
type MetaStore struct {
	mu   sync.RWMutex
	path string
	data map[string]Meta
	// loaded — содержимое файла ДЕЙСТВИТЕЛЬНО прочитано (или файла нет вовсе,
	// что для нового профиля нормально). false означает «файл есть, но мы его
	// не поняли»: битый JSON, занят антивирусом, нет прав. Раньше цена такой
	// беды была «терминалы пропали из списка, но работают»; с появлением уборки
	// сирот пустая карта означала бы «все хосты — сироты», и уборка снесла бы
	// живые терминалы вместе с работающими в них агентами. Поэтому уборка
	// смотрит на этот флаг.
	loaded bool
}

// NewMetaStore opens (and creates if needed) the metadata file. A read error
// is non-fatal — the store starts empty.
func NewMetaStore() *MetaStore {
	return NewMetaStoreAt(paths.StateFile("pty.json"))
}

// NewMetaStoreAt is NewMetaStore with an explicit file path (tests).
func NewMetaStoreAt(path string) *MetaStore {
	s := &MetaStore{path: path, data: make(map[string]Meta)}
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		s.loaded = true // чистый профиль — знать нечего, и это не сбой
	case err != nil:
		log.Printf("[PTY] хранилище %s не прочитано (%v) — работаем с пустым, уборка сирот отключена", path, err)
	default:
		if uerr := json.Unmarshal(b, &s.data); uerr != nil {
			log.Printf("[PTY] хранилище %s повреждено (%v) — работаем с пустым, уборка сирот отключена", path, uerr)
			s.data = make(map[string]Meta)
		} else {
			s.loaded = true
		}
	}
	return s
}

// Loaded — состояние на диске прочитано и ему можно доверять. Отрицательный
// ответ означает «мы не знаем, какие терминалы существуют», и всё, что от
// такого знания зависит (уборка сирот), обязано воздержаться.
func (s *MetaStore) Loaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaded
}

// Get returns the metadata for an id (zero-value if absent).
func (s *MetaStore) Get(id string) Meta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[id]
}

// SetName updates the friendly name (empty string clears it). Persists immediately.
func (s *MetaStore) SetName(id, name string) error {
	s.mu.Lock()
	m := s.data[id]
	m.Name = name
	// Drop the entry only when nothing else is stored for it — keep it alive if
	// a host record or folder placement is present (else clearing one field
	// would lose the rest). Sort matters even when Group is empty: that is a
	// manually ordered card in «Без папки».
	if m.isEmpty() {
		delete(s.data, id)
	} else {
		s.data[id] = m
	}
	s.mu.Unlock()
	return s.save()
}

// SetAccount запоминает, под каким аккаунтом нейросети запущен агент этого
// терминала. Пустой id стирает запись — терминал вернулся к основному аккаунту.
func (s *MetaStore) SetAccount(id, accountID, label string) error {
	s.mu.Lock()
	m := s.data[id]
	m.AccountID, m.AccountLabel = accountID, label
	if m.isEmpty() {
		delete(s.data, id)
	} else {
		s.data[id] = m
	}
	s.mu.Unlock()
	return s.save()
}

// SetSleep записывает (nil — стирает) сон агента этого терминала.
func (s *MetaStore) SetSleep(id string, rec *SleepRecord) error {
	s.mu.Lock()
	m := s.data[id]
	m.Sleep = rec
	if m.isEmpty() {
		delete(s.data, id)
	} else {
		s.data[id] = m
	}
	s.mu.Unlock()
	return s.save()
}

// SetPlacement updates the folder (group) and manual sort order of a session
// in the terminal list. Persists immediately.
func (s *MetaStore) SetPlacement(id, group string, sort float64) error {
	s.mu.Lock()
	m := s.data[id]
	m.Group = group
	m.Sort = sort
	if m.isEmpty() {
		delete(s.data, id)
	} else {
		s.data[id] = m
	}
	s.mu.Unlock()
	return s.save()
}

func (s *MetaStore) Folders() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.data[metaFoldersKey].Folders)
}

func (s *MetaStore) SetFolders(folders []string) error {
	clean := make([]string, 0, len(folders))
	seen := make(map[string]struct{}, len(folders))
	for _, folder := range folders {
		if folder == "" {
			continue
		}
		if _, ok := seen[folder]; ok {
			continue
		}
		seen[folder] = struct{}{}
		clean = append(clean, folder)
	}
	folders = clean
	s.mu.Lock()
	if len(folders) == 0 {
		delete(s.data, metaFoldersKey)
	} else {
		m := s.data[metaFoldersKey]
		m.Folders = folders
		s.data[metaFoldersKey] = m
	}
	s.mu.Unlock()
	return s.save()
}

// PutHost stores (or replaces) the host record for a session, preserving any
// existing name. Persists immediately.
func (s *MetaStore) PutHost(id string, h hostRecord) error {
	s.mu.Lock()
	m := s.data[id]
	hc := h
	m.Host = &hc
	s.data[id] = m
	s.mu.Unlock()
	return s.save()
}

// SetModes persists the session's active DEC modes. No-op for sessions without
// a host record (they don't survive restarts, nothing to restore) and when the
// set hasn't changed (modes flip rarely, but the caller can't cheaply tell).
func (s *MetaStore) SetModes(id string, modes []int) error {
	if len(modes) == 0 {
		modes = nil
	}
	s.mu.Lock()
	m, ok := s.data[id]
	if !ok || m.Host == nil || slices.Equal(m.Modes, modes) {
		s.mu.Unlock()
		return nil
	}
	m.Modes = modes
	s.data[id] = m
	s.mu.Unlock()
	return s.save()
}

// SetViewSize persists the last size actually applied to the PTY. Тот же
// контракт, что у SetModes: только для сессий с host-записью и только при
// реальном изменении. Восстанавливается в reattachHosts — зеркало экрана
// после рестарта агента обязано родиться в геометрии, в которой печатался
// поток, а не в born (см. Meta.ViewCols).
func (s *MetaStore) SetViewSize(id string, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	s.mu.Lock()
	m, ok := s.data[id]
	if !ok || m.Host == nil || (m.ViewCols == cols && m.ViewRows == rows) {
		s.mu.Unlock()
		return nil
	}
	m.ViewCols, m.ViewRows = cols, rows
	s.data[id] = m
	s.mu.Unlock()
	return s.save()
}

// MarkLost переводит запись из «живой хост» в «терминал ждёт восстановления».
//
// Зовётся, когда pty-host не отозвался на старте: компьютер перезагрузили, и
// процесс шелла умер вместе с ним. Удалять запись здесь нельзя — это и есть
// всё, что осталось от работы человека; имя, папка и порядок карточки лежат в
// том же Meta и переживают потерю.
func (s *MetaStore) MarkLost(id string, at time.Time) error {
	s.mu.Lock()
	m, ok := s.data[id]
	if !ok || m.Host == nil {
		s.mu.Unlock()
		return nil
	}
	m.Lost = &lostRecord{
		CWD:     m.Host.CWD,
		Shell:   m.Host.Shell,
		UID:     m.Host.UID,
		Created: m.Host.Created,
		LostAt:  at.UnixMilli(),
	}
	m.Host = nil
	// Режимы и размер принадлежали умершему процессу — новый терминал начинает
	// с чистого экрана, и старый alt-screen ему только помешает.
	m.Modes = nil
	m.ViewCols, m.ViewRows = 0, 0
	s.data[id] = m
	s.mu.Unlock()
	return s.save()
}

// SetAgent запоминает, какой агент работал в терминале последним.
func (s *MetaStore) SetAgent(id, kind string) error {
	s.mu.Lock()
	m, ok := s.data[id]
	if !ok || m.Agent == kind {
		s.mu.Unlock()
		return nil
	}
	m.Agent = kind
	s.data[id] = m
	s.mu.Unlock()
	return s.save()
}

// AllLost returns every session waiting to be restored after a reboot.
func (s *MetaStore) AllLost() map[string]Meta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Meta, len(s.data))
	for id, m := range s.data {
		if m.Lost != nil {
			out[id] = m
		}
	}
	return out
}

// ClearLost снимает пометку «ждёт восстановления», оставляя имя и папку:
// зовётся, когда терминал подняли заново под тем же id.
func (s *MetaStore) ClearLost(id string) error {
	s.mu.Lock()
	m, ok := s.data[id]
	if !ok || m.Lost == nil {
		s.mu.Unlock()
		return nil
	}
	m.Lost = nil
	s.data[id] = m
	s.mu.Unlock()
	return s.save()
}

// AllHosts returns a snapshot of every session that has a persistent host
// record — used by Manager.Reattach at startup.
func (s *MetaStore) AllHosts() map[string]hostRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]hostRecord, len(s.data))
	for id, m := range s.data {
		if m.Host != nil {
			out[id] = *m.Host
		}
	}
	return out
}

// KnownIDs — идентификаторы, о которых хранилище хоть что-то знает: живые
// терминалы, потерянные (ждущие восстановления) и просто переименованные.
// Нужен уборке сирот: хост, чьего id здесь нет, вернуть в интерфейс уже нечем.
func (s *MetaStore) KnownIDs() map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]struct{}, len(s.data))
	for id := range s.data {
		if id == metaFoldersKey {
			continue
		}
		out[strings.ToLower(id)] = struct{}{}
	}
	return out
}

// Forget removes any metadata for the id. Used when a session is closed.
func (s *MetaStore) Forget(id string) {
	s.mu.Lock()
	delete(s.data, id)
	s.mu.Unlock()
	_ = s.save()
}

func (s *MetaStore) save() error {
	s.mu.RLock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
