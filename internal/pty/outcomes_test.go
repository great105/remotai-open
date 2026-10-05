package pty

import (
	"io"
	"testing"
	"time"
)

// newOutcomeManager — менеджер без единого живого PTY: журнал исходов не зависит
// ни от бэкенда сессии, ни от реаппера.
func newOutcomeManager() *Manager {
	return &Manager{sessions: make(map[string]*Session)}
}

func newOutcomeSession(id, shell string) *Session {
	return &Session{ID: id, CWD: "C:\\proj", Shell: shell, Created: time.Now()}
}

// Смысл журнала: он живёт ДОЛЬШЕ самой сессии. Реаппер удаляет мёртвую сессию
// через 5 минут, а запись об исходе обязана остаться — иначе главная не может
// сказать, чем закончилась ночная сборка.
func TestOutcomeSurvivesSessionRemoval(t *testing.T) {
	m := newOutcomeManager()
	sess := newOutcomeSession("aaa", "bash")
	m.sessions[sess.ID] = sess

	died := time.Now()
	m.recordOutcome(sess, "dead", OutcomeReasonExited, died, "")
	delete(m.sessions, sess.ID) // как reapLoop через 5 минут

	got := m.Outcomes()
	if len(got) != 1 {
		t.Fatalf("ожидали 1 исход, получили %d", len(got))
	}
	if got[0].ID != "aaa" || got[0].Status != "dead" || got[0].Reason != OutcomeReasonExited {
		t.Fatalf("исход искажён: %+v", got[0])
	}
	if got[0].At != died.UnixMilli() {
		t.Fatalf("время исхода %d, ожидали %d", got[0].At, died.UnixMilli())
	}
	if got[0].Shell != "bash" || got[0].CWD != "C:\\proj" {
		t.Fatalf("контекст терминала потерян: %+v", got[0])
	}
}

// Повторная ошибка в том же терминале обновляет время, а не плодит строки:
// главной нужно «что случилось», а не полный лог.
func TestOutcomeSameKindCollapses(t *testing.T) {
	m := newOutcomeManager()
	sess := newOutcomeSession("bbb", "pwsh")
	first := time.Now().Add(-time.Hour)
	m.recordOutcome(sess, "error", OutcomeReasonOutputError, first, "Error: build failed")
	second := time.Now()
	m.recordOutcome(sess, "error", OutcomeReasonOutputError, second, "Error: build failed")
	m.recordOutcome(sess, "dead", OutcomeReasonDetached, second, "")

	got := m.Outcomes()
	if len(got) != 2 {
		t.Fatalf("ожидали 2 записи (error + dead), получили %d: %+v", len(got), got)
	}
	for _, o := range got {
		if o.Status == "error" && o.At != second.UnixMilli() {
			t.Fatalf("время ошибки не обновилось: %+v", o)
		}
	}
}

// Записи старше outcomeTTL выкидываются на чтении — журнал короткий по замыслу.
func TestOutcomesDropStaleAndSortNewestFirst(t *testing.T) {
	m := newOutcomeManager()
	old := newOutcomeSession("old", "bash")
	fresh := newOutcomeSession("fresh", "bash")
	m.recordOutcome(old, "dead", OutcomeReasonExited, time.Now().Add(-outcomeTTL-time.Minute), "")
	m.recordOutcome(fresh, "dead", OutcomeReasonExited, time.Now().Add(-time.Minute), "")
	m.recordOutcome(fresh, "error", OutcomeReasonOutputError, time.Now(), "")

	got := m.Outcomes()
	if len(got) != 2 {
		t.Fatalf("ожидали 2 свежих записи, получили %d: %+v", len(got), got)
	}
	if got[0].Status != "error" {
		t.Fatalf("журнал не отсортирован свежими вперёд: %+v", got)
	}
	for _, o := range got {
		if o.ID == "old" {
			t.Fatalf("протухшая запись осталась: %+v", o)
		}
	}
}

// Больше outcomeLimit записей журнал не держит, и выкидывает САМЫЕ СТАРЫЕ.
func TestOutcomesCapKeepsNewest(t *testing.T) {
	m := newOutcomeManager()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < outcomeLimit+7; i++ {
		sess := newOutcomeSession(string(rune('a'+i%26))+string(rune('0'+i/26)), "bash")
		m.recordOutcome(sess, "dead", OutcomeReasonExited, base.Add(time.Duration(i)*time.Minute), "")
	}
	got := m.Outcomes()
	if len(got) != outcomeLimit {
		t.Fatalf("ожидали %d записей, получили %d", outcomeLimit, len(got))
	}
	oldestKept := base.Add(7 * time.Minute).UnixMilli()
	for _, o := range got {
		if o.At < oldestKept {
			t.Fatalf("в журнале осталась запись старше предела: %+v", o)
		}
	}
}

// eofConn — бэкенд, который сразу отдаёт EOF: readLoop доходит до своего
// defer'а, и видно, попадает ли смерть сессии в журнал.
type eofConn struct{}

func (eofConn) Read(p []byte) (int, error)  { return 0, io.EOF }
func (eofConn) Write(p []byte) (int, error) { return len(p), nil }
func (eofConn) Resize(cols, rows int) error { return nil }
func (eofConn) Close() error                { return nil }
func (eofConn) shellPID() uint32            { return 0 }
func (eofConn) currentCWD() (string, error) { return "", nil }

// Смерть терминала сама по себе — исход, и readLoop обязан его записать.
func TestReadLoopRecordsDeath(t *testing.T) {
	m := newOutcomeManager()
	sess := newSession("ddd", "C:\\proj", "bash", 7, eofConn{}, time.Now())
	sess.readLoop(m)

	got := m.Outcomes()
	if len(got) != 1 {
		t.Fatalf("ожидали 1 исход, получили %d: %+v", len(got), got)
	}
	if got[0].Status != "dead" || got[0].Reason != OutcomeReasonExited {
		t.Fatalf("исход искажён: %+v", got[0])
	}
}

// Осознанное закрытие человеком (Manager.Close ставит пометку до kill) исходом
// не считается — иначе главная сообщала бы «терминал завершился» о том, что
// человек закрыл сам.
func TestReadLoopSkipsUserClose(t *testing.T) {
	m := newOutcomeManager()
	sess := newSession("eee", "C:\\proj", "bash", 7, eofConn{}, time.Now())
	sess.closedByUser.Store(true)
	sess.readLoop(m)

	if got := m.Outcomes(); len(got) != 0 {
		t.Fatalf("закрытие руками попало в журнал: %+v", got)
	}
}
