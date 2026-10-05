package pty

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Терминалы, пережившие перезагрузку компьютера.
//
// Персистентный терминал (мини-tmux) переживает перезапуск remotai — но не
// перезагрузку системы: pty-host умирает вместе с ней. До этого запись о такой
// сессии просто удалялась, и человек, включив компьютер, видел пустой список:
// имя, папка, рабочий каталог и всё, что агент успел сделать за ночь, исчезали
// молча. При этом сам продукт обещает обратное — «работа продолжается, пока
// тебя нет», а автообновление специально откладывает перезапуск, чтобы не
// потерять сессии.
//
// Восстановление честное и не притворяется снапшотом: процесс поднимается
// ЗАНОВО (тот же id, имя, папка, каталог и шелл), а последние строки прошлой
// работы показываются как архив с явной чертой. Что именно было запущено,
// человек решает сам — ни одна команда автоматически не повторяется.

// LostSession — терминал, ждущий восстановления, каким его видит клиент.
type LostSession struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	CWD     string `json:"cwd"`
	Shell   string `json:"shell"`
	Group   string `json:"group"`
	Created int64  `json:"created"` // unix ms — когда терминал завели впервые
	LostAt  int64  `json:"lost_at"` // unix ms — когда компьютер выключился
	// Agent — что здесь работало в последний раз («claude»), если известно.
	Agent string `json:"agent,omitempty"`
	// HasScrollback — есть ли сохранённый хвост вывода: карточка обещает
	// «последние строки» только когда они действительно есть.
	HasScrollback bool `json:"has_scrollback"`
	// CWDMissing — папки больше нет (внешний диск, удалённый проект).
	// Восстановить всё равно можно — терминал откроется в ближайшей
	// существующей папке выше, и клиент обязан об этом сказать.
	CWDMissing bool `json:"cwd_missing,omitempty"`
}

// Lost возвращает терминалы, не пережившие перезагрузку, — самые свежие первыми.
func (m *Manager) Lost() []LostSession {
	if m == nil || m.meta == nil {
		return nil
	}
	all := m.meta.AllLost()
	out := make([]LostSession, 0, len(all))
	for id, meta := range all {
		if meta.Lost == nil {
			continue
		}
		// Уже восстановленный терминал в списке потерянных не место: id один и
		// тот же, и карточка задвоилась бы.
		m.mu.RLock()
		_, live := m.sessions[id]
		m.mu.RUnlock()
		if live {
			continue
		}
		out = append(out, LostSession{
			ID:            id,
			Name:          meta.Name,
			CWD:           meta.Lost.CWD,
			Shell:         meta.Lost.Shell,
			Group:         meta.Group,
			Created:       meta.Lost.Created,
			LostAt:        meta.Lost.LostAt,
			Agent:         meta.Agent,
			HasScrollback: m.scrollbacks != nil && m.scrollbacks.has(id),
			CWDMissing:    !dirExists(meta.Lost.CWD),
		})
	}
	// Свежая потеря — сверху: она вероятнее всего и есть та работа, к которой
	// человек возвращается.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].LostAt > out[j-1].LostAt; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ForgetLost убирает потерянный терминал из списка вместе с сохранённым хвостом.
func (m *Manager) ForgetLost(id string) bool {
	if m == nil || m.meta == nil {
		return false
	}
	if m.meta.Get(id).Lost == nil {
		return false
	}
	m.meta.Forget(id)
	if m.scrollbacks != nil {
		m.scrollbacks.forget(id)
	}
	log.Printf("[PTY] lost session forgotten id=%s", id)
	return true
}

// Restore поднимает потерянный терминал заново под тем же id.
//
// Тот же id намеренно: к терминалу ведут ссылки из бота, закладки экрана и
// открытые вкладки — «продолжить работу» не должно означать «получить другой
// терминал». Имя, папка и порядок карточки остаются, потому что лежат в том же
// Meta и потерю пережили.
func (m *Manager) Restore(id string, cols, rows int) (*Session, error) {
	if m == nil || m.meta == nil {
		return nil, fmt.Errorf("восстановление недоступно")
	}
	meta := m.meta.Get(id)
	if meta.Lost == nil {
		return nil, fmt.Errorf("этот терминал не ждёт восстановления")
	}
	m.mu.RLock()
	_, live := m.sessions[id]
	m.mu.RUnlock()
	if live {
		return nil, fmt.Errorf("терминал уже открыт")
	}

	// Папка могла исчезнуть вместе с внешним диском или проектом. Намерение
	// человека однозначно («продолжить эту работу»), поэтому вместо отказа
	// поднимаемся до ближайшей существующей папки — клиент об этом предупредил
	// заранее (LostSession.CWDMissing).
	cwd := nearestExistingDir(meta.Lost.CWD)

	sess, err := m.createWithID(id, meta.Lost.UID, cwd, meta.Lost.Shell, cols, rows)
	if err != nil {
		return nil, err
	}

	// Архив прошлой работы — в буфер новой сессии, ДО живого вывода: человек
	// открывает терминал и видит, на чём всё остановилось.
	if m.scrollbacks != nil {
		if tail := m.scrollbacks.load(id); len(tail) > 0 {
			sess.seedScrollback(append(tail, restoreDivider(meta.Lost.LostAt)...))
		}
		m.scrollbacks.forget(id)
	}
	_ = m.meta.ClearLost(id)
	log.Printf("[PTY] restored id=%s cwd=%s (was %s)", id, cwd, meta.Lost.CWD)
	return sess, nil
}

// restoreDivider — черта между прошлой работой и новой. Пишется прямо в поток
// терминала, чтобы человек не принял архив за живой вывод.
func restoreDivider(lostAt int64) []byte {
	when := ""
	if lostAt > 0 {
		when = time.UnixMilli(lostAt).Local().Format("02.01 15:04")
	}
	line := "\r\n\x1b[2m── выше: работа до выключения компьютера"
	if when != "" {
		line += " (" + when + ")"
	}
	line += " ──\x1b[0m\r\n"
	return []byte(line)
}

// seedScrollback кладёт готовый кусок вывода (архив работы до выключения
// компьютера) в буфер сессии, ещё не подключённой ни к одному клиенту.
//
// totalBytes растёт вместе с буфером — обязательно. Инвариант len(buf) ≤
// totalBytes держит ВСЯ арифметика резюме: и SubscribeResume, и ResyncFrom
// считают начало кольца как `total - len(buf)` в беззнаковой арифметике.
// Пока архив не учитывался, у восстановленного терминала это выражение уходило
// в минус и превращалось в число под 2^64: клиент получал base, который не
// помещается в uint64, обратно его прислать не мог, и КАЖДОЕ переподключение
// приходило полным reset — то есть человек, вернувшийся к терминалу после
// перезагрузки ПК, терял прокрученное. Отсчёт при этом остаётся честным:
// подписчик всё равно получает архив внутри полного снимка (свежая сессия,
// нового epoch у клиента ещё нет), а дельты считаются от позиции ПОСЛЕ него.
func (s *Session) seedScrollback(data []byte) {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	s.buf = append(append(make([]byte, 0, len(data)+len(s.buf)), data...), s.buf...)
	s.totalBytes += uint64(len(data))
	if len(s.buf) > scrollbackSize {
		s.buf = s.buf[len(s.buf)-scrollbackSize:]
	}
}

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// nearestExistingDir поднимается вверх, пока не найдёт существующую папку.
// Пустой результат означает «не нашлось» — вызывающий поднимет терминал там,
// где решит сам (Create подставит домашнюю папку).
func nearestExistingDir(path string) string {
	for dir := path; dir != ""; {
		if dirExists(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
