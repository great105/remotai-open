package pty

import (
	"context"
	"sync"
	"time"

	"tgcontrol/internal/agenthistory"
	"tgcontrol/internal/agenthooks"
	"tgcontrol/internal/agents"
)

func (s *Session) rememberHistorySource(ev agenthooks.Event) {
	if ev.SessionID == "" || (ev.Agent != "claude" && ev.Agent != "codex") {
		return
	}
	source := agenthistory.Source{Agent: ev.Agent, SessionID: ev.SessionID, Transcript: ev.Transcript, ConfigHome: ev.ConfigHome}
	s.historyMu.Lock()
	first := s.historySource == (agenthistory.Source{})
	s.historySource = source
	s.historyAt = time.Now()
	s.historyMu.Unlock()
	if first {
		s.signalHistorySource()
	}
}

// historyWaiters — открытые соединения, которые ждут ПЕРВОГО источника истории
// своей сессии (ST-10 B).
//
// ⚠ ЗАЧЕМ. Способность agent-history-capability сервер присылал сразу в ответ
// на объявление клиента, не спрашивая, есть ли у сессии что читать. Кнопка
// «История агента» поэтому появлялась в любом терминале, включая голый шелл, а
// нажатие давало «недоступно». Теперь соединение ждёт здесь, пока хук агента
// впервые назовёт источник, и только тогда присылает то же самое сообщение:
// старый клиент получает ровно прежнее, просто позже (I-14).
//
// Реестр держит сессию только пока кто-то ждёт: источник появился — запись
// удаляется; соединение закрылось раньше — отписывается само (cancel).
var historyWaiters = struct {
	sync.Mutex
	m map[*Session]map[chan struct{}]struct{}
}{m: map[*Session]map[chan struct{}]struct{}{}}

func (s *Session) signalHistorySource() {
	historyWaiters.Lock()
	for ch := range historyWaiters.m[s] {
		close(ch)
	}
	delete(historyWaiters.m, s)
	historyWaiters.Unlock()
}

// HistorySourceReady возвращает канал, закрытый, как только у сессии есть
// источник истории агента (уже есть — закрыт сразу), и функцию отписки,
// которую вызывающий обязан позвать, когда ждать больше незачем.
//
// Порядок замков historyWaiters → historyMu; rememberHistorySource берёт их
// по очереди, не вложенно, поэтому взаимной блокировки нет, а источник,
// названный между проверкой и подпиской, не теряется: подписка и проверка
// идут под одним historyWaiters, сигнал — тоже.
func (s *Session) HistorySourceReady() (<-chan struct{}, func()) {
	ch := make(chan struct{})
	historyWaiters.Lock()
	defer historyWaiters.Unlock()
	s.historyMu.Lock()
	has := s.historySource != (agenthistory.Source{})
	s.historyMu.Unlock()
	if has {
		close(ch)
		return ch, func() {}
	}
	set := historyWaiters.m[s]
	if set == nil {
		set = map[chan struct{}]struct{}{}
		historyWaiters.m[s] = set
	}
	set[ch] = struct{}{}
	return ch, func() {
		historyWaiters.Lock()
		defer historyWaiters.Unlock()
		if set := historyWaiters.m[s]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(historyWaiters.m, s)
			}
		}
	}
}

func (s *Session) ReadAgentHistory(ctx context.Context, cursor string) (agenthistory.Page, error) {
	s.historyMu.Lock()
	source := s.historySource
	s.historyMu.Unlock()
	binary := ""
	if descriptor := agents.GetDescriptor(source.Agent); descriptor != nil {
		binary = descriptor.Path()
	}
	page, err := agenthistory.Read(ctx, source, cursor, binary)
	s.historyMu.Lock()
	same := s.historySource == source
	s.historyMu.Unlock()
	if !same {
		return agenthistory.Page{}, agenthistory.ErrChanged
	}
	return page, err
}
