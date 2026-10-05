package pty

import (
	"testing"
	"time"
)

// Строка, которой CLI-агент отчитывается о своей внутренней мелочи, ошибкой не
// считается. Живой случай: Codex печатает «error: hook exited with code 1» на
// каждый неудачный PreToolUse-хук и спокойно работает дальше — в одном терминале
// за сутки набегали десятки таких строк, и каждая давала карточку «упало».
func TestErrorHintSkipsAgentNoise(t *testing.T) {
	noise := "• PreToolUse hook (failed)\r\n  error: hook exited with code 1\r\n"
	if got := errorHintFrom([]byte(noise)); got != "" {
		t.Fatalf("шумовая строка агента принята за ошибку: %q", got)
	}
	// Но настоящую ошибку в том же чанке шум заслонять не должен.
	mixed := noise + "Error: build failed\r\n"
	if got := errorHintFrom([]byte(mixed)); got != "Error: build failed" {
		t.Fatalf("настоящая ошибка потерялась за шумом: %q", got)
	}
}

// Ошибка, после которой терминал ушёл работать дальше, исходом не становится:
// строки уже нет на экране, агент её прочитал и исправился.
func TestSettleErrorIgnoresRecoveredOutput(t *testing.T) {
	m := newOutcomeManager()
	sess := newOutcomeSession("aaa", "pwsh")
	m.sessions[sess.ID] = sess
	state := newDetectorState()

	now := time.Now()
	state.noteError("Error: fetch failed", now)
	tail := []byte("• Ran git status --short\n  ok\n❯ ")

	if m.settleError(sess, state, tail, now.Add(2*time.Second)) {
		t.Fatal("исход записан, хотя строки ошибки на экране уже нет")
	}
	if got := m.Outcomes(); len(got) != 0 {
		t.Fatalf("журнал не должен был пополниться: %+v", got)
	}
}

// Терминал ОСТАНОВИЛСЯ на ошибке — вот это новость: она идёт и в статус, и в
// журнал, и несёт саму строку (без неё карточка умела сказать лишь «мелькнула
// строка, похожая на ошибку», и проверить это было нечем).
func TestSettleErrorRecordsWhenVisible(t *testing.T) {
	m := newOutcomeManager()
	sess := newOutcomeSession("bbb", "pwsh")
	m.sessions[sess.ID] = sess
	state := newDetectorState()

	now := time.Now()
	state.noteError("Error: build failed", now)
	tail := []byte("npm run build\n  Error: build failed\n❯ ")

	if !m.settleError(sess, state, tail, now.Add(3*time.Second)) {
		t.Fatal("остановка на ошибке обязана стать исходом")
	}
	got := m.Outcomes()
	if len(got) != 1 {
		t.Fatalf("ожидали 1 исход, получили %d: %+v", len(got), got)
	}
	if got[0].Status != "error" || got[0].Reason != OutcomeReasonOutputError {
		t.Fatalf("исход искажён: %+v", got[0])
	}
	if got[0].Hint != "Error: build failed" {
		t.Fatalf("строка ошибки не доехала до журнала: %q", got[0].Hint)
	}
	if got[0].At != now.UnixMilli() {
		t.Fatalf("метка исхода %d, ожидали момент САМОЙ ошибки %d", got[0].At, now.UnixMilli())
	}
	// Статус в списке терминалов — из того же события, с той же строкой.
	status, _, hint, _ := statusFrom(
		sessEvent{kind: EventError, at: now, hint: "Error: build failed"},
		true, false, true, false, now.Add(3*time.Second), 0,
	)
	if status != "error" || hint != "Error: build failed" {
		t.Fatalf("статус=%q hint=%q, ожидали error со строкой ошибки", status, hint)
	}
	// Латч не даёт следующему спокойному тику перебить ошибку «свободен».
	if !state.errorHeld(now.Add(time.Minute)) {
		t.Fatal("латч ошибки погас раньше errorStatusTTL")
	}
	if state.errorHeld(now.Add(errorStatusTTL + time.Second)) {
		t.Fatal("латч ошибки держится дольше errorStatusTTL")
	}
}

// Пометка не живёт вечно: ошибка, после которой терминал работал полчаса,
// новостью быть перестала — даже если похожая строка снова оказалась в хвосте.
func TestSettleErrorForgetsStaleMark(t *testing.T) {
	m := newOutcomeManager()
	sess := newOutcomeSession("ccc", "pwsh")
	m.sessions[sess.ID] = sess
	state := newDetectorState()

	now := time.Now()
	state.noteError("Error: build failed", now)
	tail := []byte("  Error: build failed\n❯ ")

	if m.settleError(sess, state, tail, now.Add(errorSettleTTL+time.Second)) {
		t.Fatal("протухшая пометка стала исходом")
	}
	if got := m.Outcomes(); len(got) != 0 {
		t.Fatalf("журнал не должен был пополниться: %+v", got)
	}
}

// Пометка одна на эпизод: многострочный стек приходит пачкой чанков, и метка со
// строкой обязаны остаться от ПЕРВОГО из них — иначе штамп в уведомлении
// разойдётся со штампом из GET /api/pty и латч клиента перестанет гасить повтор.
func TestNoteErrorKeepsFirstLineOfEpisode(t *testing.T) {
	state := newDetectorState()
	first := time.Now()
	state.noteError("Error: cannot find module 'foo'", first)
	state.noteError("    at Module._resolveFilename", first.Add(200*time.Millisecond))

	at, hint := state.takeError()
	if !at.Equal(first) || hint != "Error: cannot find module 'foo'" {
		t.Fatalf("эпизод перезаписан следующим чанком: at=%v hint=%q", at, hint)
	}
	// Снимается пометка ровно один раз.
	if at, _ := state.takeError(); !at.IsZero() {
		t.Fatal("пометка осталась после разбора")
	}
}

// Ошибка в выводе ЖИВОГО агента — не исход терминала.
//
// Живая жалоба владельца (2026-07-27): в терминале Kimi Code висела красная
// карточка «⚠ ошибка · 2м назад» со строкой `ERROR: relation "org_memberships"
// does not exist`, а агент в этот момент вёл три подзадачи и работал дальше.
// Строка была ответом Postgres на его пробный запрос: агент её прочитал и пошёл
// дальше — падением терминала это не было.
//
// Два обстоятельства делали ложное срабатывание неизбежным:
//  1. молчание агента ≠ остановка (Kimi в режиме thinking не рисует ни
//     спиннера, ни таймера — терминал молчит минутами, пока идёт работа);
//  2. интерфейс агента печатает историю заново, поэтому строка снова попадала
//     в хвост, и КАЖДАЯ следующая пауза записывала исход повторно.
//
// Правило: у агентского терминала пометка снимается без исхода. Настоящее
// падение агента — это смерть процесса, её пишет readLoop исходом "dead".
func TestAgentOwnErrorNeverBecomesOutcome(t *testing.T) {
	m := newOutcomeManager()
	sess := newOutcomeSession("kimi-1", "pwsh")
	m.sessions[sess.ID] = sess
	state := newDetectorState()

	now := time.Now()
	sqlLine := `ERROR: relation "org_memberships" does not exist`
	// Строка распознаётся как ошибка — иначе тест ничего бы не проверял.
	if got := errorHintFrom([]byte(sqlLine + "\r\n")); got != sqlLine {
		t.Fatalf("SQL-ошибка не распознана детектором: %q", got)
	}
	state.noteError(sqlLine, now)

	// Так ведёт себя ветка живого агента в tickSession: пометку снимаем.
	if !state.hasError() {
		t.Fatal("пометка не поставилась")
	}
	state.takeError()

	// Дальше терминал молчит, а строка ВСЁ ЕЩЁ в хвосте (агент перерисовал свой
	// интерфейс вместе с историей). Исхода быть не должно ни сейчас, ни потом.
	tail := []byte(sqlLine + "\n│ > цц │\n yolo K3 thinking: max [3 agents running]\n")
	if m.settleError(sess, state, tail, now.Add(3*time.Second)) {
		t.Fatal("исход записан для ошибки, которую агент прочитал сам")
	}
	if got := m.Outcomes(); len(got) != 0 {
		t.Fatalf("журнал пополнился ошибкой живого агента: %+v", got)
	}
	if state.errorHeld(now.Add(time.Second)) {
		t.Fatal("статус «error» залатчен — карточка станет красной без причины")
	}

	// А у ОБЫЧНОГО терминала (не агент) правило прежнее: команда упала, промпт
	// вернулся — это новость.
	shell := newOutcomeSession("shell-1", "pwsh")
	m.sessions[shell.ID] = shell
	shellState := newDetectorState()
	shellState.noteError("Error: build failed", now)
	if !m.settleError(shell, shellState, []byte("npm run build\n  Error: build failed\n❯ "), now.Add(2*time.Second)) {
		t.Fatal("падение команды в шелле перестало быть исходом")
	}
}
