package pty

import (
	"os"
	"path/filepath"
	"sync"

	"tgcontrol/internal/paths"
)

// Хвост вывода терминала, переживающий перезагрузку компьютера.
//
// Зачем: сессия живёт в pty-host, а тот умирает вместе с системой. Восстановить
// сам процесс невозможно (это не снапшот виртуалки), но человек возвращается не
// к процессу, а к РАБОТЕ: что агент успел сделать, на чём остановился, какую
// команду он запускал. Без последних строк «Продолжить работу» открывало бы
// пустой чёрный экран в нужной папке — формально то же место, фактически потеря
// контекста.
//
// Границы намеренные:
//   - только ХВОСТ (последние scrollbackKeepBytes), а не вся история;
//   - файл на сессию, права 0600, рядом с остальным состоянием агента;
//   - закрыл терминал руками — файл удаляется вместе с записью;
//   - наружу (телефону, облаку) файл не отдаётся: он попадает в терминал только
//     при восстановлении, как обычный вывод.
const scrollbackKeepBytes = 32 * 1024

// scrollbackStore хранит хвосты вывода на диске: <state>/pty-scrollback/<id>.log
type scrollbackStore struct {
	mu  sync.Mutex
	dir string
	// written — сколько всего байт вывела сессия на момент последней записи;
	// по нему flush понимает, что писать нечего.
	written map[string]uint64
}

func newScrollbackStore() *scrollbackStore {
	return newScrollbackStoreAt(paths.StateFile("pty-scrollback"))
}

func newScrollbackStoreAt(dir string) *scrollbackStore {
	return &scrollbackStore{dir: dir, written: make(map[string]uint64)}
}

func (s *scrollbackStore) path(id string) string {
	return filepath.Join(s.dir, id+".log")
}

// save записывает хвост, если сессия что-то вывела с прошлого раза.
func (s *scrollbackStore) save(id string, tail []byte, total uint64) error {
	s.mu.Lock()
	if s.written[id] == total {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	if len(tail) > scrollbackKeepBytes {
		tail = tail[len(tail)-scrollbackKeepBytes:]
	}
	tmp := s.path(id) + ".tmp"
	if err := os.WriteFile(tmp, tail, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path(id)); err != nil {
		return err
	}
	s.mu.Lock()
	s.written[id] = total
	s.mu.Unlock()
	return nil
}

// load возвращает сохранённый хвост (nil, если его нет).
func (s *scrollbackStore) load(id string) []byte {
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil
	}
	return b
}

func (s *scrollbackStore) has(id string) bool {
	st, err := os.Stat(s.path(id))
	return err == nil && st.Size() > 0
}

// forget удаляет хвост: терминал закрыт человеком или снят из списка.
func (s *scrollbackStore) forget(id string) {
	s.mu.Lock()
	delete(s.written, id)
	s.mu.Unlock()
	_ = os.Remove(s.path(id))
	_ = os.Remove(s.path(id) + ".tmp")
}
