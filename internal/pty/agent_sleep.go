package pty

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/procutil"
)

// Усыпление агента — просьба владельца 29.09.2026 после замера памяти: десять
// открытых Claude держали 7,5 ГБ, из них семь-восемь просто ждали человека.
// Процесс агента живёт ради ОДНОЙ беседы, а беседа и так лежит на диске
// (Claude и Codex пишут её по ходу). Значит процесс можно снять вместе с его
// MCP и node, оставив терминал и номер беседы, и поднять ту же беседу
// командой продолжения по номеру, когда человек вернётся.
//
// Решает ЧЕЛОВЕК: таймера нет намеренно («сам хочу решать, кого усыплять»).
// Будит клиент: команду запуска собирает он (аккаунт и прокси агента живут в
// окружении процесса и не должны попадать в текст команды на сервере, см.
// agentLaunch.ts), сервер лишь отдаёт номер беседы и флаги режима.

// SleepRecord — всё, что нужно, чтобы поднять ту же беседу тем же агентом в
// том же режиме. Лежит в Meta (pty.json) и переживает перезапуск Remotai.
type SleepRecord struct {
	Agent     string   `json:"agent"`
	SessionID string   `json:"session_id"`
	Flags     []string `json:"flags,omitempty"`
	At        int64    `json:"at"` // unix ms — когда усыпили
	// WakingAt — клиент забрал запись и набирает команду продолжения. Второй
	// экран (телефон и окно на ПК смотрят в один терминал) в это время
	// получает отказ: две команды подряд — это вторая строка, напечатанная уже
	// в чат проснувшегося агента.
	WakingAt int64 `json:"waking_at,omitempty"`
	// PID/StartMs — какой именно процесс сняли. Пока taskkill/SIGTERM не
	// отработал (и ещё до 500 мс кэша findForegroundProcess), опрос видит
	// этого же агента «живым» — это не пробуждение, и запись стирать нельзя
	// (скептик 29.09: иначе плашка «Разбудить» пропадала сразу после сна).
	PID     uint32 `json:"pid,omitempty"`
	StartMs int64  `json:"start_ms,omitempty"`
}

// isSleptProcess — на переднем плане всё ещё тот процесс, который усыпили.
func (r *SleepRecord) isSleptProcess(fg ProcessInfo) bool {
	if r.PID == 0 || fg.PID != r.PID {
		return false
	}
	return r.StartMs == 0 || fg.StartMs == 0 || fg.StartMs == r.StartMs
}

// SleepError — отказ с кодом для клиента и фразой для человека.
type SleepError struct {
	Code    string
	Message string
}

func (e *SleepError) Error() string { return e.Message }

func sleepErr(code, msg string) error { return &SleepError{Code: code, Message: msg} }

// wakeClaimTTL — сколько ждём, что после выдачи записи агент появится в
// терминале. Не появился (команда не дошла, агент упал на старте) — запись
// снова можно забрать, а не терять беседу навсегда.
// 20 с, а не минута: повтор после неудачной попытки (жалоба 29.09 — вторая
// «Разбудить» упиралась в «конфликт»). Двойную команду в проснувшегося
// агента и так отсекает проверка «на переднем плане шелл» в Wake.
const wakeClaimTTL = 20 * time.Second

// Номер беседы уходит в командную строку шелла — только безопасные символы.
// Claude и Codex используют UUID.
var sleepSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)

// Sleep снимает агента терминала id и запоминает его беседу.
func (m *Manager) Sleep(id string) (SleepRecord, error) {
	s := m.Get(id)
	if s == nil {
		return SleepRecord{}, sleepErr("not_found", "Терминал не найден.")
	}
	if !s.IsAlive() {
		return SleepRecord{}, sleepErr("dead", "Терминал уже завершён.")
	}
	if s.Shell == "ssh" {
		return SleepRecord{}, sleepErr("ssh", "Усыпление пока работает только для агентов на этом компьютере.")
	}
	if m.meta == nil {
		return SleepRecord{}, sleepErr("no_store", "Хранилище терминалов недоступно.")
	}
	now := time.Now()
	info := m.infoOf(s, now)
	desc := agents.GetDescriptor(info.AgentKind)
	if !desc.CanSleep() {
		return SleepRecord{}, sleepErr("not_agent", "Усыпить можно Claude Code или Codex, а здесь его нет.")
	}
	switch info.Status {
	case "working", "stalled":
		return SleepRecord{}, sleepErr("busy", "Агент сейчас работает. Усыпить можно, когда он закончит.")
	case "waiting":
		return SleepRecord{}, sleepErr("busy", "Агент ждёт ответа. Ответьте ему или отмените вопрос, потом усыпляйте.")
	}
	fg := s.ForegroundProcess()
	// Процесс на переднем плане обязан быть тем самым агентом: снимок живёт до
	// 500 мс, и снимать по PID, который успел смениться, — значит снять чужое.
	if fg.PID == 0 || AgentKind(fg.Name) != info.AgentKind {
		return SleepRecord{}, sleepErr("not_agent", "Не удалось найти процесс агента.")
	}
	sid := s.sleepSessionID(info.AgentKind, fg)
	if !sleepSessionIDPattern.MatchString(sid) {
		return SleepRecord{}, sleepErr("no_session",
			"Номер беседы неизвестен: агент запущен до обновления Remotai. Перезапустите его один раз, и усыпление заработает.")
	}
	rec := SleepRecord{
		Agent:     info.AgentKind,
		SessionID: sid,
		Flags:     desc.SleepFlags(strings.Fields(processCmdline(fg.PID))),
		At:        now.UnixMilli(),
		PID:       fg.PID,
		StartMs:   fg.StartMs,
	}
	// Запись ДО снятия процесса: снятый агент без записи — это потерянная
	// беседа. Запись со снятым процессом sleepInfoFor не трогает (PID/StartMs).
	m.sleepMu.Lock()
	err := m.meta.SetSleep(id, &rec)
	m.sleepMu.Unlock()
	if err != nil {
		return SleepRecord{}, fmt.Errorf("сон не записан: %w", err)
	}
	if err := procutil.KillPIDTree(int(fg.PID)); err != nil && processAlive(fg.PID) {
		m.sleepMu.Lock()
		_ = m.meta.SetSleep(id, nil)
		m.sleepMu.Unlock()
		return SleepRecord{}, sleepErr("kill_failed", "Не удалось остановить агента: "+err.Error())
	}
	// Режимы ввода, которые включил снятый агент, больше не нужны: иначе
	// реассерт на переподключении снова включит клиенту мышь и фокус.
	var modes []int
	s.bufMu.Lock()
	if s.dec != nil {
		s.dec.dropInputModes()
		if s.dec.takeDirty() {
			modes = s.dec.snapshot()
		}
	}
	s.bufMu.Unlock()
	if modes != nil {
		_ = m.meta.SetModes(id, modes)
	}
	log.Printf("[PTY] усыплён агент id=%s agent=%s pid=%d flags=%v", id, rec.Agent, fg.PID, rec.Flags)
	return rec, nil
}

// Wake отдаёт запись сна клиенту, который наберёт команду продолжения.
func (m *Manager) Wake(id string) (SleepRecord, error) {
	s := m.Get(id)
	if s == nil {
		return SleepRecord{}, sleepErr("not_found", "Терминал не найден.")
	}
	if !s.IsAlive() {
		return SleepRecord{}, sleepErr("dead", "Терминал уже завершён.")
	}
	if m.meta == nil {
		return SleepRecord{}, sleepErr("no_store", "Хранилище терминалов недоступно.")
	}
	// Команда продолжения печатается в терминал — значит на переднем плане
	// обязан быть сам шелл. Иначе `claude --resume …` ушла бы сообщением в
	// запущенный там Codex или в чужую программу (скептик 29.09).
	fg := s.ForegroundProcess()
	m.sleepMu.Lock()
	defer m.sleepMu.Unlock()
	rec := m.meta.Get(id).Sleep
	if rec == nil {
		return SleepRecord{}, sleepErr("not_sleeping", "Агент в этом терминале не спит.")
	}
	if fg.Name == "" || AgentKind(fg.Name) != "shell" {
		return SleepRecord{}, sleepErr("busy",
			"В терминале сейчас работает другая программа. Завершите её, потом будите агента.")
	}
	now := time.Now()
	if rec.WakingAt != 0 && now.Sub(time.UnixMilli(rec.WakingAt)) < wakeClaimTTL {
		return SleepRecord{}, sleepErr("waking", "Агент уже просыпается.")
	}
	claimed := *rec
	claimed.WakingAt = now.UnixMilli()
	if err := m.meta.SetSleep(id, &claimed); err != nil {
		return SleepRecord{}, fmt.Errorf("сон не обновлён: %w", err)
	}
	return claimed, nil
}

// sleepInfoFor — запись сна для SessionInfo. Агент снова в терминале (его
// разбудили или человек запустил его руками) — сон окончен, запись стирается.
func (m *Manager) sleepInfoFor(s *Session, agentKind string, fg ProcessInfo) *SleepRecord {
	if m.meta == nil {
		return nil
	}
	rec := m.meta.Get(s.ID).Sleep
	if rec == nil {
		return nil
	}
	if agentKind == rec.Agent && !rec.isSleptProcess(fg) {
		m.sleepMu.Lock()
		if cur := m.meta.Get(s.ID).Sleep; cur != nil && cur.At == rec.At {
			_ = m.meta.SetSleep(s.ID, nil)
		}
		m.sleepMu.Unlock()
		return nil
	}
	out := *rec
	out.Flags = append([]string(nil), rec.Flags...)
	return &out
}

// sleepSessionID — номер беседы именно того процесса, что сейчас на переднем
// плане. Claude пишет его сам в ~/.claude/sessions/<pid>.json — это надёжнее
// хука: есть и у агента, запущенного до обновления. Номер из хука берём,
// только если хук пришёл ПОСЛЕ старта этого процесса: иначе это беседа
// прошлого агента в том же терминале, и проснулась бы не та.
func (s *Session) sleepSessionID(kind string, fg ProcessInfo) string {
	if kind == "claude" {
		if rs, ok := s.claudeRuntimeStatus(int(fg.PID)); ok && rs.SessionID != "" {
			return rs.SessionID
		}
	}
	s.historyMu.Lock()
	source, at := s.historySource, s.historyAt
	s.historyMu.Unlock()
	if source.Agent == kind && source.SessionID != "" && (fg.StartMs == 0 || at.UnixMilli() >= fg.StartMs) {
		return source.SessionID
	}
	// Хука ещё не было: Codex сообщает номер только в конце хода. Если агент
	// поднят продолжением по номеру, номер — в его командной строке; иначе
	// «Беседы» позволили бы поднять ту же беседу второй раз (скептик 29.09).
	return resumeIDFromArgs(kind, strings.Fields(processCmdline(fg.PID)))
}

// resumeIDFromArgs — номер беседы из командной строки продолжения:
// `codex resume <id>`, `claude --resume <id>` / `-r <id>` / `--resume=<id>`.
func resumeIDFromArgs(kind string, argv []string) string {
	for i, a := range argv {
		next := ""
		if i+1 < len(argv) {
			next = strings.Trim(argv[i+1], `"`)
		}
		switch {
		case kind == "codex" && a == "resume":
			if sleepSessionIDPattern.MatchString(next) {
				return next
			}
		case kind == "claude" && (a == "--resume" || a == "-r"):
			if sleepSessionIDPattern.MatchString(next) {
				return next
			}
		case kind == "claude" && strings.HasPrefix(a, "--resume="):
			if v := strings.TrimPrefix(a, "--resume="); sleepSessionIDPattern.MatchString(v) {
				return v
			}
		}
	}
	return ""
}
