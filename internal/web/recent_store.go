package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tgcontrol/internal/paths"
)

// recentPerUserMax — сколько папок помним на пользователя. Список короткий
// намеренно: «недавние» перестают быть недавними, если их сорок экранов.
const recentPerUserMax = 30

// recentEntry — папка, в которой человек открывал терминал, и когда это было
// в последний раз.
type recentEntry struct {
	Name string  `json:"name"`
	Path string  `json:"path"`
	Time float64 `json:"time"`
}

// recentStore — настоящая история папок, в которых открывали терминалы.
//
// Жалоба владельца 01.09.2026: «недавнее не всегда недавние». И он был прав:
// вкладка «Недавние» вообще не была историей — она перечисляла папки ЖИВЫХ
// сессий (`store.List`). Отсюда обе странности, которые он видел:
//
//   - закрыл терминал — папка мгновенно исчезла из «Недавних», хотя работал в
//     ней минуту назад;
//   - терминал, открытый неделю назад и до сих пор живой, стоял в «Недавних»
//     как свежий.
//
// Теперь папка попадает сюда в момент создания терминала и живёт на диске,
// переживая и закрытие терминала, и перезапуск remotai.exe (то же решение и по
// той же причине, что у bookmarkStore).
type recentStore struct {
	mu   sync.RWMutex
	path string
	data map[int64][]recentEntry
}

// newRecentStore открывает файл истории. Ошибка чтения не фатальна — стор
// стартует пустым: потерянная история хуже пустой, но обе лучше отказа старта.
func newRecentStore() *recentStore {
	return newRecentStoreAt(paths.StateFile("recent-folders.json"))
}

// newRecentStoreAt — newRecentStore с явным путём (тесты).
func newRecentStoreAt(path string) *recentStore {
	s := &recentStore{path: path, data: make(map[int64][]recentEntry)}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	if s.data == nil {
		s.data = make(map[int64][]recentEntry)
	}
	return s
}

// Note отмечает, что в папке открыли терминал. Повторное открытие той же папки
// не плодит запись, а поднимает её наверх — иначе история из десяти строк
// состояла бы из одной папки, открытой десять раз.
func (s *recentStore) Note(uid int64, path string) {
	if path == "" || path == "." {
		return
	}
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) {
		name = path
	}
	now := float64(time.Now().Unix())

	s.mu.Lock()
	list := s.data[uid]
	for i, e := range list {
		if e.Path == path {
			list = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	list = append([]recentEntry{{Name: name, Path: path, Time: now}}, list...)
	if len(list) > recentPerUserMax {
		list = list[:recentPerUserMax]
	}
	s.data[uid] = list
	s.mu.Unlock()
	_ = s.save()
}

// List возвращает историю пользователя, свежее — первым (никогда не nil).
func (s *recentStore) List(uid int64) []recentEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]recentEntry, len(s.data[uid]))
	copy(out, s.data[uid])
	return out
}

func (s *recentStore) save() error {
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
