package web

import (
	"encoding/json"
	"errors"
	"os"
	"sync"

	"tgcontrol/internal/paths"
)

// bookmarksPerUserMax — потолок на пользователя. До персиста список жил в RAM и
// обнулялся сам; теперь он растёт на диске вечно, так что нужен предел.
const bookmarksPerUserMax = 100

var errTooManyBookmarks = errors.New("too many bookmarks")

// bookmarkStore хранит закладки папок в ~/.tgcontrol-bookmarks.json.
//
// До этого закладки лежали в обычной map в памяти сервера и умирали на каждом
// рестарте remotai.exe — то есть на каждом автообновлении. Пользователь пинил
// папку, а она молча исчезала; «Быстрый запуск» показывал только недавние.
type bookmarkStore struct {
	mu   sync.RWMutex
	path string
	data map[int64][]bookmark
}

// newBookmarkStore открывает файл закладок. Ошибка чтения не фатальна — стор
// стартует пустым (как MetaStore у PTY).
func newBookmarkStore() *bookmarkStore {
	return newBookmarkStoreAt(paths.StateFile("bookmarks.json"))
}

// newBookmarkStoreAt — newBookmarkStore с явным путём (тесты).
func newBookmarkStoreAt(path string) *bookmarkStore {
	s := &bookmarkStore{path: path, data: make(map[int64][]bookmark)}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	if s.data == nil {
		s.data = make(map[int64][]bookmark)
	}
	return s
}

// List возвращает копию списка закладок пользователя (никогда не nil).
func (s *bookmarkStore) List(uid int64) []bookmark {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]bookmark, len(s.data[uid]))
	copy(out, s.data[uid])
	return out
}

// Has сообщает, запинен ли путь у пользователя.
func (s *bookmarkStore) Has(uid int64, path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.data[uid] {
		if b.Path == path {
			return true
		}
	}
	return false
}

// Add добавляет закладку. Дубль по пути — не ошибка (идемпотентно), просто
// ничего не меняется. Персистит немедленно.
func (s *bookmarkStore) Add(uid int64, b bookmark) error {
	s.mu.Lock()
	for _, ex := range s.data[uid] {
		if ex.Path == b.Path {
			s.mu.Unlock()
			return nil
		}
	}
	if len(s.data[uid]) >= bookmarksPerUserMax {
		s.mu.Unlock()
		return errTooManyBookmarks
	}
	s.data[uid] = append(s.data[uid], b)
	s.mu.Unlock()
	return s.save()
}

// Remove убирает закладку по точному пути. Идемпотентно. Персистит немедленно.
func (s *bookmarkStore) Remove(uid int64, path string) error {
	s.mu.Lock()
	bms := s.data[uid]
	for i, b := range bms {
		if b.Path == path {
			s.data[uid] = append(bms[:i:i], bms[i+1:]...)
			break
		}
	}
	if len(s.data[uid]) == 0 {
		delete(s.data, uid)
	}
	s.mu.Unlock()
	return s.save()
}

func (s *bookmarkStore) save() error {
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
