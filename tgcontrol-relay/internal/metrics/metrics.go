// Package metrics — лёгкие in-process счётчики событий relay в Prometheus-формате.
//
// Здесь живут только КУМУЛЯТИВНЫЕ счётчики (counters), которые накапливаются за
// время жизни процесса: HTTP-запросы по классам статуса, пейринги ok/fail,
// логины, коннекты агентов. Текущие срезы-gauge (online-устройства, статистика
// из БД, размер файла) собираются в server.handleMetrics, потому что им нужен
// доступ к Hub/БД — держать их здесь означало бы тащить сюда лишние зависимости.
//
// Внешних зависимостей нет специально: пакет импортируют и server, и bot, а они
// уже тянут половину дерева — лишний цикл импорта тут ни к чему.
package metrics

import (
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

var (
	// httpByClass[c] — число ответов со статусом из класса c (c = первая цифра, 1..5).
	httpByClass  [6]atomic.Uint64
	httpDurNanos atomic.Uint64 // суммарная длительность всех запросов, нс
	httpDurCount atomic.Uint64 // число запросов, попавших в сумму

	pairOK        atomic.Uint64
	pairFail      atomic.Uint64
	logins        atomic.Uint64
	agentConnects atomic.Uint64

	// Релейный notifier «агент ждёт ответа» (см. server/notifier.go). Карты
	// заполнены на старте и больше не мутируются — конкурентное чтение
	// безопасно, счётчики атомарные.
	agentNotices = map[string]*atomic.Uint64{
		"sent":      {}, // ушло в Telegram
		"present":   {}, // человек и так в приложении
		"stale":     {}, // за время задержки на вопрос уже ответили
		"muted":     {}, // /notify off
		"no_tg":     {}, // анонимный аккаунт: chat_id нет
		"ratelimit": {}, // потолок на пользователя
		"failed":    {}, // TG API не принял
	}
	agentReplies = map[string]*atomic.Uint64{
		"ok":          {},
		"denied":      {}, // нет доступа к этому ПК
		"offline":     {}, // ПК не в сети
		"unsupported": {}, // старая версия агента / терминал закрыт
		"error":       {},
	}
	// Уведомления «ПК обновился» (server/notifier.go, fireAgentUpdated).
	agentUpdates = map[string]*atomic.Uint64{
		"sent":   {}, // ушло в Telegram
		"dedup":  {}, // про эту версию уже писали
		"muted":  {}, // /notify off
		"no_tg":  {}, // анонимный аккаунт: chat_id нет
		"failed": {}, // TG API не принял
	}
)

// AgentNotice — исход попытки уведомить пользователя о вопросе агента.
// Неизвестный result молча игнорируется (метрика не должна ронять боевой путь).
func AgentNotice(result string) {
	if c, ok := agentNotices[result]; ok {
		c.Add(1)
	}
}

// AgentReply — исход нажатия инлайн-кнопки ответа агенту.
func AgentReply(result string) {
	if c, ok := agentReplies[result]; ok {
		c.Add(1)
	}
}

// AgentUpdate — исход попытки уведомить пользователя об обновлении агента.
func AgentUpdate(result string) {
	if c, ok := agentUpdates[result]; ok {
		c.Add(1)
	}
}

// ObserveHTTP регистрирует один обслуженный HTTP-запрос: его класс статуса и
// длительность. Вызывается из logging-middleware на каждый ответ.
func ObserveHTTP(status int, durNanos int64) {
	if c := status / 100; c >= 1 && c <= 5 {
		httpByClass[c].Add(1)
	}
	if durNanos > 0 {
		httpDurNanos.Add(uint64(durNanos))
		httpDurCount.Add(1)
	}
}

// PairOK / PairFail — успешный / неуспешный ввод пейринг-кода (бот или REST).
func PairOK()   { pairOK.Add(1) }
func PairFail() { pairFail.Add(1) }

// Login — подтверждённый вход через Telegram (бот или REST-poll).
func Login() { logins.Add(1) }

// AgentConnect — десктоп-агент поднял WS-соединение с relay.
func AgentConnect() { agentConnects.Add(1) }

// WriteTo дописывает все счётчики этого пакета в Prometheus-text формате.
func WriteTo(b *strings.Builder) {
	b.WriteString("# HELP tgcontrol_relay_http_requests_total Total HTTP responses by status class\n")
	b.WriteString("# TYPE tgcontrol_relay_http_requests_total counter\n")
	for c := 1; c <= 5; c++ {
		b.WriteString(`tgcontrol_relay_http_requests_total{class="`)
		b.WriteByte(byte('0' + c))
		b.WriteString(`xx"} `)
		b.WriteString(strconv.FormatUint(httpByClass[c].Load(), 10))
		b.WriteByte('\n')
	}

	b.WriteString("# HELP tgcontrol_relay_http_request_duration_seconds_sum Cumulative request duration\n")
	b.WriteString("# TYPE tgcontrol_relay_http_request_duration_seconds_sum counter\n")
	b.WriteString("tgcontrol_relay_http_request_duration_seconds_sum ")
	b.WriteString(strconv.FormatFloat(float64(httpDurNanos.Load())/1e9, 'f', 6, 64))
	b.WriteByte('\n')
	b.WriteString("# HELP tgcontrol_relay_http_request_duration_seconds_count Requests counted into the duration sum\n")
	b.WriteString("# TYPE tgcontrol_relay_http_request_duration_seconds_count counter\n")
	b.WriteString("tgcontrol_relay_http_request_duration_seconds_count ")
	b.WriteString(strconv.FormatUint(httpDurCount.Load(), 10))
	b.WriteByte('\n')

	b.WriteString("# HELP tgcontrol_relay_pairings_total Pairing-code submissions by result\n")
	b.WriteString("# TYPE tgcontrol_relay_pairings_total counter\n")
	b.WriteString(`tgcontrol_relay_pairings_total{result="ok"} `)
	b.WriteString(strconv.FormatUint(pairOK.Load(), 10))
	b.WriteByte('\n')
	b.WriteString(`tgcontrol_relay_pairings_total{result="fail"} `)
	b.WriteString(strconv.FormatUint(pairFail.Load(), 10))
	b.WriteByte('\n')

	writeCounter(b, "tgcontrol_relay_logins_total", "Confirmed Telegram logins", logins.Load())
	writeCounter(b, "tgcontrol_relay_agent_connects_total", "Desktop agent WS connections accepted", agentConnects.Load())

	writeLabelled(b, "tgcontrol_relay_agent_notices_total",
		"Telegram notices about a waiting agent, by outcome", agentNotices)
	writeLabelled(b, "tgcontrol_relay_agent_replies_total",
		"Inline replies sent to a PTY from Telegram, by outcome", agentReplies)
	writeLabelled(b, "tgcontrol_relay_agent_updates_total",
		"Telegram notices about an agent self-update, by outcome", agentUpdates)
}

// writeLabelled печатает семейство счётчиков с меткой result. Порядок
// отсортирован, чтобы вывод /metrics был стабильным между scrape'ами.
func writeLabelled(b *strings.Builder, name, help string, m map[string]*atomic.Uint64) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(help)
	b.WriteString("\n# TYPE ")
	b.WriteString(name)
	b.WriteString(" counter\n")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(name)
		b.WriteString(`{result="`)
		b.WriteString(k)
		b.WriteString(`"} `)
		b.WriteString(strconv.FormatUint(m[k].Load(), 10))
		b.WriteByte('\n')
	}
}

func writeCounter(b *strings.Builder, name, help string, v uint64) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(help)
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(name)
	b.WriteString(" counter\n")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(v, 10))
	b.WriteByte('\n')
}
