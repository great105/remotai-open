package web

// Понятные уведомления в Telegram: «Claude закончил» → «что именно сделал».
//
// ЗАЧЕМ. Уведомление «агент закончил в терминале «работа» · 4 мин» не отвечает
// на единственный вопрос, ради которого его читают: что он там сделал. Человек
// всё равно открывал терминал — то есть уведомление не экономило ничего.
//
// УСТРОЙСТВО. Детектор (internal/pty) отдаёт сюда повод и сырой хвост вывода,
// этот файл решает «звать модель или молчать», а чистая работа с текстом —
// очистка, маскирование секретов, промпт, разбор ответа — живёт в
// internal/agentsummary и проверяется тестами без сети.
//
// ТРИ ПРАВИЛА, КОТОРЫЕ ЗДЕСЬ ВАЖНЕЕ КОДА:
//
//  1. Выключено по умолчанию. Текст терминала уходит в чужой сервис, и человек
//     обязан включить это сам, прочитав, что именно уходит.
//  2. Модель молчит — уведомление всё равно уходит, просто без выжимки. Тишина
//     вместо новости хуже короткой новости: агент закончил, и человек ждёт.
//  3. «Агент ждёт ответа» отправляем ТОЛЬКО когда модель подтвердила вопрос.
//     Ложное «вас ждут» будит ночью зря — ровно из-за таких срабатываний
//     распознавание вопросов по экрану выключено с 2.49.4.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/agentsummary"
	"tgcontrol/internal/config"
	"tgcontrol/internal/openrouter"
	"tgcontrol/internal/pty"
)

const (
	// summaryQueue — сколько поводов ждут очереди. Переполнение означает, что
	// модель отвечает медленнее, чем агенты заканчивают работу; лишнее роняем,
	// потому что устаревшее уведомление хуже отсутствующего.
	summaryQueue = 16
	// summaryTimeout — сколько ждём модель. Уведомление имеет смысл, пока
	// новость свежая; через минуту человек уже посмотрел сам.
	summaryTimeout = 40 * time.Second
)

// summaryEngine — движок понятных уведомлений. Один на процесс агента.
type summaryEngine struct {
	srv    *Server
	budget *agentsummary.Budget
	jobs   chan pty.SummaryEvent

	// noKeyOnce — про отсутствие ключа пишем в лог один раз: повод приходит на
	// каждое завершение работы агента, и без этого лог заполнился бы им.
	noKeyOnce sync.Once
}

// StartAgentSummaries подключает движок к детектору. Зовётся один раз при
// старте web-сервера; сама настройка проверяется на каждом поводе, поэтому
// включение и выключение работают без перезапуска агента.
func (s *Server) StartAgentSummaries() {
	if s.ptyManager == nil {
		return
	}
	e := &summaryEngine{
		srv:    s,
		budget: agentsummary.NewBudget(),
		jobs:   make(chan pty.SummaryEvent, summaryQueue),
	}
	s.ptyManager.SetSummaryHook(e.enqueue)
	go e.run()
}

// enqueue вызывается ИЗ ГОРУТИНЫ ДЕТЕКТОРА — здесь нельзя ждать ничего.
func (e *summaryEngine) enqueue(ev pty.SummaryEvent) {
	select {
	case e.jobs <- ev:
	default:
		log.Printf("[SUMMARY] очередь полна, пропускаю событие %s pty=%s", ev.Kind, ev.PtyID)
	}
}

// run обрабатывает поводы ПО ОДНОМУ. Последовательно намеренно: параллельные
// запросы к модели быстрее сожгли бы дневную квоту, а спешить некуда.
func (e *summaryEngine) run() {
	for ev := range e.jobs {
		e.handle(ev)
	}
}

func (e *summaryEngine) handle(ev pty.SummaryEvent) {
	cfg := config.GetNoSetup()
	if !cfg.AgentSummaries {
		return
	}
	// Человек прямо сейчас смотрит в этот терминал — рассказывать ему то, что у
	// него на экране, незачем (та же проверка присутствия, что у релейного
	// notifier, только здесь она бесплатная).
	if sess := e.srv.ptyManager.Get(ev.PtyID); sess != nil && sess.ViewerCount() > 0 {
		return
	}

	key := ""
	if e.srv.openrouter != nil {
		key = e.srv.openrouter.Key()
	}
	if key == "" {
		e.noKeyOnce.Do(func() {
			log.Printf("[SUMMARY] понятные уведомления включены, но ключ OpenRouter не задан — выжимок не будет")
		})
		// Без ключа уведомление всё равно уходит: «агент закончил» — уже новость.
		e.send(ev, "")
		return
	}

	text, ok := agentsummary.Prepare(ev.Tail)
	if !ok {
		e.send(ev, "") // пересказывать нечего
		return
	}
	if !e.budget.Allow(ev.PtyID, time.Now()) {
		e.send(ev, "") // бюджет исчерпан — новость без выжимки
		return
	}

	kind := agentsummary.KindFinished
	if ev.Kind == pty.SummaryQuestion {
		kind = agentsummary.KindWaiting
	}
	client := openrouter.New(key)
	system := agentsummary.System(kind)
	user := agentsummary.User(ev.AgentKind, projectName(ev.CWD), text)

	// Попыток две — и это не «на всякий случай», а следствие живого замера
	// (12.08.2026): бесплатный роутер выбирает модель под каждый запрос сам, и
	// один и тот же материал то даёт нормальный русский пересказ, то пустой
	// ответ или размышление вслух. Второй запрос почти всегда уходит на ДРУГУЮ
	// модель — то есть повтор здесь дешевле любых уговоров в промпте.
	// Вопросы не повторяем: там пустой ответ означает «агент не ждёт», и это
	// законный ответ, а не неудача.
	attempts := 2
	if ev.Kind == pty.SummaryQuestion {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), summaryTimeout)
		answer, err := client.Complete(ctx,
			// Служебная выжимка идёт бесплатным роутером, а не моделью, выбранной
			// для запуска агента: за уведомления человек платить не подписывался.
			openrouter.RouterModel, system, user)
		cancel()
		switch {
		case errors.Is(err, openrouter.ErrRateLimited):
			e.budget.NoteRateLimited(time.Now())
			log.Printf("[SUMMARY] квота модели исчерпана — час без выжимок")
			e.send(ev, "")
			return
		case err != nil:
			log.Printf("[SUMMARY] модель не ответила (попытка %d): %v", attempt, err)
			continue
		}
		if ev.Kind == pty.SummaryQuestion {
			// Вопрос отправляем, только если модель его подтвердила (правило 3).
			if q, real := agentsummary.Question(answer); real {
				e.send(ev, q)
			}
			return
		}
		if s := agentsummary.Summary(answer); s != "" {
			e.send(ev, s)
			return
		}
	}
	// Все попытки без пригодного пересказа — новость всё равно отправляем.
	e.send(ev, "")
}

// send отправляет готовое сообщение владельцу. Пустая выжимка — не повод
// молчать про завершение работы, но повод молчать про вопрос: «агент о чём-то
// спрашивает, о чём — не знаю» это не новость, а тревога.
func (e *summaryEngine) send(ev pty.SummaryEvent, summary string) {
	if ev.Kind == pty.SummaryQuestion && strings.TrimSpace(summary) == "" {
		return
	}
	SendOwnerText(summaryMessage(ev, summary))
}

// summaryMessage — текст уведомления. Релей допишет сверху имя компьютера
// («📨 Рабочий ПК:»), поэтому имя машины здесь не повторяем.
func summaryMessage(ev pty.SummaryEvent, summary string) string {
	agent := agentTitle(ev.AgentKind)
	var sb strings.Builder
	if ev.Kind == pty.SummaryQuestion {
		fmt.Fprintf(&sb, "❓ %s ждёт ответа", agent)
	} else {
		fmt.Fprintf(&sb, "✅ %s закончил", agent)
		if ev.Duration > 0 {
			fmt.Fprintf(&sb, " · %s", humanDuration(ev.Duration))
		}
	}
	// Вторая строка — «где это». Проект человек спрашивает первым делом, а имя
	// терминала он задавал сам, поэтому узнаёт его быстрее любого id.
	place := projectName(ev.CWD)
	if name := strings.TrimSpace(ev.Name); name != "" && name != place {
		if place != "" {
			place += " · "
		}
		place += "«" + name + "»"
	}
	if place != "" {
		fmt.Fprintf(&sb, "\n📁 %s", place)
	}
	if s := strings.TrimSpace(summary); s != "" {
		fmt.Fprintf(&sb, "\n\n%s", s)
	}
	return sb.String()
}

// projectName — последняя часть пути: «по какому проекту» человек мыслит
// именем папки, а не полным путём (он на телефоне всё равно не поместится).
func projectName(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	if strings.HasPrefix(cwd, "ssh:") {
		return cwd // ярлык удалённой машины — он и есть «где»
	}
	base := filepath.Base(strings.TrimRight(strings.ReplaceAll(cwd, "\\", "/"), "/"))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

// agentTitle — как называть агента в сообщении. Те же подписи, что в клиенте и
// в боте (apk/src/notifications.ts, relay/internal/bot/notify.go).
func agentTitle(kind string) string {
	switch kind {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "gemini":
		return "Gemini"
	case "kimi":
		return "Kimi"
	case "aider":
		return "Aider"
	case "opencode":
		return "OpenCode"
	case "copilot":
		return "Copilot"
	case "cursor-agent":
		return "Cursor"
	case "cline":
		return "Cline"
	case "kilo":
		return "Kilo"
	case "amazon-q":
		return "Amazon Q"
	case "":
		return "Агент"
	}
	return kind
}

// humanDuration — длительность словами. Секунды показываем только у коротких
// заходов: «работал 3 ч 07 мин 12 с» человеку ночью не нужно.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d с", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%d ч", h)
		}
		return fmt.Sprintf("%d ч %d мин", h, m)
	}
}
