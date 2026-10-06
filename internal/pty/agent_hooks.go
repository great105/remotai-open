package pty

// Структурные сигналы ИИ-агента поверх экрана (см. internal/agenthooks).
//
// Где агент САМ говорит «жду ответа» и «закончил», экран не угадываем. Два
// источника, оба проверены живым прогоном в терминале Remotai 11.09.2026
// (claude 2.1.268, запрос разрешения на Write):
//
//   - хуки: PermissionRequest (инструмент и его вход) пришёл в тот же миг, что
//     и диалог, PostToolUse — после ответа, Stop — в конце хода;
//   - файл статуса Claude (~/.claude/sessions/<pid>.json): пока висит диалог,
//     там "waiting", после ответа "busy", после конца хода "idle". Этот
//     источник работает и без хуков — у Claude, запущенного руками.
//
// А продукт в тот же прогон показывал «работает» и «свободен» вперемешку:
// статус "waiting" мы не разбирали вовсе, а вопросы по экрану выключены с
// 2.49.4 — рассказ о меню от самого меню по тексту не отличить.

import (
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/agenthooks"
)

const (
	// hookWaitGrace — сколько вопрос из хука держится вопреки файлу статуса.
	// Хук и запись файла приходят не одновременно: в первые секунды файл ещё
	// может говорить "busy" про уже открытый диалог.
	hookWaitGrace = 3 * time.Second

	claudeWaitingHint = "Claude ждёт вашего ответа"
)

// hookState — что детектор знает о сессии из сигналов агента. Пишет и читает
// только горутина детектора (как и остальные поля detectorState).
type hookState struct {
	tail         *agenthooks.Tail
	codexRuntime *agenthooks.CodexRuntimeReader

	agent string // чьи хуки слышали: "claude" / "codex"
	seen  bool   // хуки этой сессии работают — эвристике «закончил» молчать

	turnAt   time.Time // UserPromptSubmit — начало хода
	waitAt   time.Time // вопрос из хука открыт; ноль — нет
	waitTool string    // инструмент, разрешения на который ждут
	waitHint string
	// waitOptions — пункты вопроса из хука (AskUserQuestion). У запроса
	// разрешения их нет: там подписи берём с экрана.
	waitOptions []string

	// rtWaitSeen — файл статуса Claude хоть раз сказал "waiting". Только тогда
	// его «уже не жду» закрывает вопрос из хука: у версии, которая "waiting"
	// не пишет вовсе, файл весь диалог говорил бы "busy".
	rtWaitSeen bool
	waiting    bool // вопрос был открыт на прошлом тике
	waitSeq    int  // номер эпизода ожидания (ключ латча уведомлений)
}

// reportsStop — конец хода этого агента приходит хуком, эвристика не нужна.
func (h *hookState) reportsStop(kind string) bool {
	return h.seen && h.agent == kind
}

// apply — чистый переход по одному событию. finished — ход закончился, dur —
// сколько он шёл (от UserPromptSubmit, иначе от начала всплеска вывода).
func (h *hookState) apply(ev agenthooks.Event, activityStart time.Time) (finished bool, dur time.Duration) {
	at := ev.Time()
	switch ev.Kind {
	case agenthooks.KindSessionStart:
		h.seen, h.agent = true, ev.Agent
		h.closeWait()
	case agenthooks.KindPrompt:
		h.seen, h.agent = true, ev.Agent
		h.turnAt = at
		h.closeWait()
	case agenthooks.KindPermission:
		h.seen, h.agent = true, ev.Agent
		h.waitAt, h.waitTool, h.waitHint, h.waitOptions = at, ev.Tool, permissionHint(ev), nil
	case agenthooks.KindPreTool:
		if ev.Tool == "AskUserQuestion" {
			hint := ev.Detail // сам вопрос
			if hint == "" {
				hint = "Claude задаёт вопрос"
			}
			h.waitAt, h.waitTool, h.waitHint, h.waitOptions = at, ev.Tool, hint, ev.Options
		}
	case agenthooks.KindNotification:
		// Запасной путь для версий без PermissionRequest. Живой прогон:
		// на запрос разрешения Notification НЕ пришёл за 7 секунд.
		if h.waitAt.IsZero() && (ev.NotificationType == "permission_prompt" || ev.NotificationType == "elicitation_dialog") {
			hint := ev.Message
			if hint == "" {
				hint = claudeWaitingHint
			}
			h.waitAt, h.waitTool, h.waitHint, h.waitOptions = at, "", hint, nil
		}
	case agenthooks.KindPostTool:
		// Инструмент отработал — значит, на вопрос о НЁМ ответили. Чужой
		// параллельный инструмент открытый вопрос не закрывает.
		if h.waitTool == "" || h.waitTool == ev.Tool {
			h.closeWait()
		}
	case agenthooks.KindStop:
		h.seen, h.agent = true, ev.Agent
		h.closeWait()
		start := h.turnAt
		if start.IsZero() || start.After(at) {
			start = activityStart
		}
		if !start.IsZero() && at.After(start) {
			dur = at.Sub(start)
		}
		h.turnAt = time.Time{}
		return true, dur
	case agenthooks.KindSessionEnd:
		tail := h.tail
		*h = hookState{tail: tail}
	}
	return false, 0
}

func (h *hookState) closeWait() {
	h.waitAt, h.waitTool, h.waitHint, h.waitOptions = time.Time{}, "", "", nil
}

// agentSignal — вывод детектора на этом тике.
type agentSignal struct {
	// structured — у агента есть честный источник «ждёт ответа»; вопрос по
	// тексту экрана для него не ищем.
	structured bool
	waiting    bool
	hint       string
	options    []string // пункты вопроса из хука; пусто — ищем на экране
	seq        int
}

// signal — чистое правило «ждёт ли агент ответа» по хукам и файлу статуса.
// rt/rtOK — статус из файла Claude (нет файла — rtOK=false).
func (h *hookState) signal(kind, rt string, rtOK bool, now time.Time) agentSignal {
	if kind != "claude" {
		// Сигнала «жду ответа» у прочих агентов нет (у Codex notify — только
		// конец хода): для них всё как раньше.
		h.waiting = false
		return agentSignal{}
	}
	rt = strings.ToLower(rt)
	if rtOK && rt == "waiting" {
		h.rtWaitSeen = true
	}
	waiting := false
	switch {
	case rtOK && rt == "waiting":
		waiting = true
	case !h.waitAt.IsZero():
		if rtOK && h.rtWaitSeen && now.Sub(h.waitAt) > hookWaitGrace {
			h.closeWait() // Claude сам говорит, что уже не ждёт (Esc, отказ)
		} else {
			waiting = true
		}
	}
	sig := agentSignal{structured: rtOK || h.reportsStop("claude"), waiting: waiting}
	if waiting {
		if !h.waiting {
			h.waitSeq++
		}
		sig.hint = claudeWaitingHint
		if !h.waitAt.IsZero() && h.waitHint != "" {
			sig.hint = h.waitHint
			sig.options = h.waitOptions
		}
	}
	h.waiting = waiting
	sig.seq = h.waitSeq
	return sig
}

// permissionHint — вопрос словами человека: он уходит и в уведомление.
func permissionHint(ev agenthooks.Event) string {
	what := ev.Detail
	switch ev.Tool {
	case "":
		return claudeWaitingHint
	case "Bash", "PowerShell":
		if what != "" {
			return "Разрешить команду: " + what
		}
		return "Разрешить команду?"
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		if what != "" {
			return "Разрешить изменить файл: " + baseName(what)
		}
		return "Разрешить изменить файл?"
	case "WebFetch", "WebSearch":
		if what != "" {
			return "Разрешить доступ в интернет: " + what
		}
		return "Разрешить доступ в интернет?"
	case "ExitPlanMode":
		return "Claude предлагает план — одобрить?"
	}
	if what != "" {
		return "Разрешить " + ev.Tool + ": " + what
	}
	return "Разрешить " + ev.Tool + "?"
}

// baseName — имя файла из пути любой ОС (путь приходит с машины агента).
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}

// pollAgentHooks забирает новые события из очереди терминала. Конец хода
// становится новостью сразу, без 45–120 секунд тишины.
func (m *Manager) pollAgentHooks(s *Session, d *detectorState, now time.Time, bc Broadcaster) {
	h := &d.hooks
	if h.tail == nil {
		h.tail = &agenthooks.Tail{Path: agenthooks.SpoolPath(s.ID)}
	}
	for _, ev := range h.tail.Read() {
		// notify fires for every Codex child thread too. A child completing a
		// turn does not mean the foreground Codex is ready. Unknown/old events
		// fall back to the screen heuristic instead of proving completion.
		if ev.Agent == "codex" && ev.Kind == agenthooks.KindStop && ev.SessionScope != "root" {
			continue
		}
		s.rememberHistorySource(ev)
		if ev.Agent == "codex" && ev.Kind == agenthooks.KindStop {
			if h.codexRuntime == nil || h.codexRuntime.ConfigHome != ev.ConfigHome || h.codexRuntime.ThreadID != ev.SessionID {
				h.codexRuntime = agenthooks.NewCodexRuntimeReader(ev.ConfigHome, ev.SessionID, ev.Time())
				s.codexActivity.runtime("", time.Time{}, "")
			}
			s.codexActivity.stopTurn(ev.Time(), ev.TurnID)
		}
		if finished, dur := h.apply(ev, d.activityStart); finished {
			m.hookFinished(s, d, ev.Agent, dur, now, bc)
		}
	}
}

func (m *Manager) pollCodexRuntime(s *Session, d *detectorState, now time.Time) {
	if r := d.hooks.codexRuntime; r != nil {
		before, _ := s.codexActivity.status(now)
		status, at := r.Read(now)
		s.codexActivity.runtime(status, at, r.TurnID())
		after, _ := s.codexActivity.status(now)
		if before != after {
			if bc := m.broadcaster(); bc != nil {
				bc.Broadcast(s.UID, map[string]any{"type": "pty_list_changed", "reason": "runtime_status", "pty_id": s.ID})
			}
		}
	}
}

// hookFinished — ход агента закончился (Stop у Claude, notify у Codex).
func (m *Manager) hookFinished(s *Session, d *detectorState, kind string, dur time.Duration, now time.Time, bc Broadcaster) {
	// Этот всплеск вывода уже отзвонил — эвристика его не повторит.
	d.finActivityStart = d.activityStart
	// «Привет» — не работа: порог тот же, что у эвристики (набор текста в поле
	// ввода агента и короткие ответы уведомлением не звучат).
	if dur < agentFinishedMinWork {
		return
	}
	m.fireSummary(SummaryEvent{
		Kind: SummaryFinished, PtyID: s.ID, UID: s.UID,
		Name: m.sessionName(s), CWD: s.CurrentCWD(), AgentKind: kind,
		Duration: dur, Tail: summaryTail(s, min(d.bytesSinceLastPrompt, summaryEpisodeBytes)),
	})
	if bc != nil && d.shouldEmit(EventFinished, now) {
		evt := m.buildEvent(s, kind, EventFinished, time.Time{})
		evt["duration_ms"] = dur.Milliseconds()
		bc.Broadcast(s.UID, evt)
	}
}

// agentSignals — сигнал на этом тике: хуки + файл статуса Claude.
func (m *Manager) agentSignals(s *Session, d *detectorState, kind string, now time.Time) agentSignal {
	rt, rtOK := "", false
	if kind == "claude" {
		if fg := s.ForegroundProcess(); fg.PID > 0 {
			if st, ok := s.claudeRuntimeStatus(int(fg.PID)); ok {
				rt, rtOK = st.Status, true
			}
		}
	}
	return d.hooks.signal(kind, rt, rtOK, now)
}

// holdStructuredWait — вопрос агента открыт: держим статус «ждёт ответа» с
// кнопками и один раз на эпизод сообщаем наружу (релей шлёт это в Telegram).
func (m *Manager) holdStructuredWait(s *Session, d *detectorState, tail []byte, sig agentSignal, now time.Time, bc Broadcaster) {
	// Сам вопрос известен точно. Пункты вопроса Claude (AskUserQuestion) приходят
	// хуком; подписи запроса разрешения (Yes / Yes, allow all edits / No) хук не
	// присылает — их берём с экрана.
	options := sig.options
	if len(options) == 0 {
		options = choiceOptions(tail)
	}
	hintKind := ""
	if len(options) > 0 {
		hintKind = HintKindChoice
	}
	fresh := d.enterEpisode(waitEpisodePrefix+"agent|"+strconv.Itoa(sig.seq), now)
	d.waitSeenAt = now
	s.setEvent(EventWaitingInput, d.episodeAt, sig.hint, hintKind, options...)
	s.syncWaitEpisode(d)
	if fresh && bc != nil {
		evt := m.buildEvent(s, sessionAgentKind(s), EventWaitingInput, d.episodeAt)
		evt["hint"] = sig.hint
		evt["hint_kind"] = hintKind
		if len(options) > 0 {
			evt["hint_options"] = options
		}
		bc.Broadcast(s.UID, evt)
	}
}
