package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/metrics"
	"tgcontrol-relay/internal/notify"
	"tgcontrol-relay/internal/protocol"
	"tgcontrol-relay/internal/relayhub"
)

// Notifier — «агент ждёт ответа» доходит до человека, когда приложение закрыто.
//
// Почему на релее: событие агента (internal/pty/events.go) сегодня превращается
// в уведомление только в нативном APK и только пока он жив; в Telegram-мини-аппе
// канала уведомлений нет вовсе, и ночной агент простаивает до утра. Релей уже
// видит поток событий каждого ПК (AgentConn.SubscribeEvents) и умеет писать в
// Telegram — этого достаточно, FCM не нужен.
//
// Устройство:
//
//	Attach/consume — подписка и по одной горутине на живое соединение агента:
//	                 читает канал событий и НИЧЕГО больше (см. ниже про дроп).
//	onEvent        — фильтр + латч эпизода + отложенный таймер. Только память.
//	fire           — все проверки и отправка, вне цикла чтения.
//
// Второй поток событий — «агент обновился» (type=agent_updated): onAgentUpdated
// в цикле чтения делает только разбор и реплей-фильтр, а fireAgentUpdated (вне
// цикла, через time.AfterFunc) — дедуп по events и одно спокойное сообщение в
// Telegram на (пользователь, устройство, версия).
//
// ⚠ Главное ограничение: AgentConn.dispatchEvent шлёт подписчикам неблокирующе
// и МОЛЧА ДРОПАЕТ событие, если канал полон. Любой сетевой вызов или запрос в
// БД внутри цикла consume означал бы потерянные вопросы агента, поэтому там
// разрешены только операции с памятью и time.AfterFunc.
//
// Шторма уведомлений не будет — три независимых рубежа: латч эпизода на
// терминал (повтор того же вопроса не шлётся), cooldown на терминал и
// токен-бакет на пользователя. Плюс агент сам шлёт waiting_input один раз на
// эпизод (internal/pty/events.go), а error — с дебаунсом 15 с.
//
// Cooldown при этом ОТКЛАДЫВАЕТ вопрос, а не выбрасывает его: «один раз на
// эпизод» со стороны агента означает, что выброшенный вопрос не придёт больше
// никогда — для фичи, обещающей «ночной вопрос дойдёт», это тихая потеря.
// Частоту это не повышает: отправку по-прежнему гейтит cooldown в fire(), то
// есть на терминал остаётся не больше одного сообщения за ptyCooldown, а
// таймер на терминал всегда ровно один (см. onEvent).
type Notifier struct {
	srv *Server

	// Настройки скопированы из конфига при создании — в тестах их удобно
	// подменять на миллисекунды, не трогая глобальный конфиг.
	holdDelay   time.Duration // сколько ждать перед отправкой
	ptyCooldown time.Duration // не чаще раза в … на один терминал
	maxPerHour  int           // потолок сообщений на пользователя (0 = без лимита)

	mu  sync.Mutex
	eps map[epKey]*episode
	// lastSent — когда по ЭТОМУ ТЕРМИНАЛУ реально ушло сообщение. Живёт ОТДЕЛЬНО
	// от эпизода специально: эпизод умирает при каждой смене вопроса, а кулдаун
	// обязан пережить эту смену (см. onEvent).
	lastSent map[epKey]time.Time
	clocks   map[string]*deviceClock
	rl       map[int64]*userBucket
}

type epKey struct {
	device string
	pty    string
}

// episode — латч на терминал: один эпизод простоя = одно сообщение. Тот же
// принцип, что в detectorState агента (internal/pty/events.go), но здесь он
// защищает ещё и от повторов при переподключении агента, когда его собственный
// латч начинается заново.
//
// Латч держится до конца эпизода (см. notified и sameQuestion), а не до
// отправки: с отложенным кулдауном «уже отправили» и «ещё не отправили» — это
// два РАЗНЫХ ответа на повтор того же вопроса, и первый обязан молчать.
type episode struct {
	kind     string
	hint     string // вопрос из события, которым эпизод заведён
	statusAt int64  // метка эпизода по часам ПК (0 — агент её не прислал)
	// sentHint/sentStatusAt — что реально уехало в сообщение: переспрос ПК перед
	// отправкой мог уточнить и текст вопроса, и метку эпизода. Латч обязан
	// узнавать эпизод по ОБОИМ снимкам — иначе и повтор исходного события, и
	// событие про уточнённый вопрос считались бы новым эпизодом и давали второе
	// сообщение про то же самое.
	sentHint     string
	sentStatusAt int64

	timer     *time.Timer // отложенная отправка; nil — уже сработала
	sending   bool        // fire() прямо сейчас идёт в БД/к ПК/в Telegram
	notified  bool        // про этот вопрос человеку уже написали
	startedAt time.Time   // когда эпизод начался (ограничивает переносы отправки)
	// held — сколько эпизод прождал НЕ из-за присутствия человека, а из-за
	// кулдауна терминала. Вычитается из бюджета переносов (maxHoldFactor):
	// иначе вопрос, честно достоявший 10-минутный кулдаун, приходил бы к rearm
	// с уже исчерпанным бюджетом и пропадал навсегда, стоило человеку в этот
	// момент на минуту открыть приложение.
	held time.Duration
}

// sameQuestion — это тот же самый эпизод ожидания, а не просто похожий текст.
// Сверяем с обоими снимками эпизода (см. sentHint/sentStatusAt).
func (e *episode) sameQuestion(ev ptyEventPayload) bool {
	if e.kind != ev.Event {
		return false
	}
	if sameEpisodeMark(e.statusAt, e.hint, ev) {
		return true
	}
	return e.notified && sameEpisodeMark(e.sentStatusAt, e.sentHint, ev)
}

// sameEpisodeMark — один снимок эпизода против события.
//
// Когда метка эпизода есть с обеих сторон, сравниваем ЕЁ: агент ключует эпизод
// вопросом И отпечатком хвоста (waitEpisodeKey), поэтому два подряд одинаковых
// по тексту вопроса («Allow tool …?» про разные инструменты) дают разные метки
// — второй обязан дойти до человека. Она же гасит дрожание hint: агент
// пересчитывает его по видимому хвосту, и внутри одного эпизода текст может
// шевельнуться, а эпизод остаётся тем же. Без метки (агент старой версии)
// остаётся прежнее сравнение по тексту.
func sameEpisodeMark(statusAt int64, hint string, ev ptyEventPayload) bool {
	if statusAt > 0 && ev.StatusAt > 0 {
		return statusAt == ev.StatusAt
	}
	return hint == ev.Hint
}

// deviceClock — заметка о часах одного ПК. Нужна ровно для того, чтобы про
// уехавшее время написать в лог ОДИН раз, а не на каждое событие.
type deviceClock struct {
	logged bool      // о расхождении уже сообщили
	seen   time.Time // последнее событие с этого ПК (для PruneEpisodes)
}

// userBucket — токен-бакет антиспама на пользователя.
type userBucket struct {
	tokens float64
	last   time.Time
}

// maxHoldFactor — сколько раз подряд отправку можно перенести, если человек в
// этот момент в приложении (переносим на holdDelay). Даёт верхнюю границу
// ожидания holdDelay*maxHoldFactor (по умолчанию 30с*20 = 10 минут), после
// которой эпизод считается «человек и так всё видел» и молчит.
const maxHoldFactor = 20

// cooldownSlack — запас, с которым отложенная отправка целится ЗА конец
// кулдауна. Без него таймер срабатывал бы ровно в границу, перепроверка в fire()
// видела бы «осталось ещё микросекунда» и вопрос терялся бы на ровном месте.
const cooldownSlack = time.Second

func NewNotifier(s *Server) *Notifier {
	n := &Notifier{
		srv:         s,
		holdDelay:   s.Config.NotifyDelay,
		ptyCooldown: s.Config.NotifyPtyCooldown,
		maxPerHour:  s.Config.NotifyMaxPerHour,
		eps:         make(map[epKey]*episode),
		lastSent:    make(map[epKey]time.Time),
		clocks:      make(map[string]*deviceClock),
		rl:          make(map[int64]*userBucket),
	}
	if n.holdDelay <= 0 {
		n.holdDelay = 30 * time.Second
	}
	if n.ptyCooldown <= 0 {
		n.ptyCooldown = 10 * time.Minute
	}
	return n
}

// Attach подписывается на события соединения агента. Подписка делается
// СИНХРОННО, а читается в отдельной горутине: reader-цикл ws_agent стартует
// сразу после регистрации, и подписка «когда-нибудь потом» (go Attach) теряла
// бы первые события — dispatchEvent молча дропает событие, если подписчиков ещё
// нет. Именно этот случай и есть самый важный: агент переподключился, а вопрос
// на экране уже висит.
func (n *Notifier) Attach(ac *relayhub.AgentConn) {
	if n == nil || ac == nil {
		return
	}
	ch := ac.SubscribeEvents(64)
	go n.consume(ac, ch)
}

// consume живёт ровно столько, сколько живёт соединение: AgentConn.Close()
// закрывает каналы всех подписчиков, поэтому `for range ch` завершается сам —
// отдельного стоп-канала не нужно.
//
// В цикле разрешена только работа с памятью (см. комментарий к Notifier).
func (n *Notifier) consume(ac *relayhub.AgentConn, ch chan protocol.Event) {
	defer ac.UnsubscribeEvents(ch) // идемпотентно даже после Close()
	for e := range ch {
		n.onEvent(ac.DeviceID, e)
	}
}

// ptyEventPayload — формат события агента (internal/pty/events.go, buildEvent).
// Единственный формат, который нас интересует.
type ptyEventPayload struct {
	ID        uint64 `json:"id"`
	Type      string `json:"type"`
	Event     string `json:"event"`
	PtyID     string `json:"pty_id"`
	Name      string `json:"name"`
	Agent     string `json:"agent"`
	FgProcess string `json:"fg_process"`
	Hint      string `json:"hint"`
	HintKind  string `json:"hint_kind"`
	TS        int64  `json:"ts"`
	// StatusAt — начало эпизода ожидания по часам ПК (unix ms), тот же штамп,
	// что отдаёт GET /api/pty/{id}/state. Разбирать его ОБЯЗАТЕЛЬНО: без него
	// метка эпизода бралась бы только из переспроса ПК, а переспрос может не
	// удаться (таймаут 5 с) — это «не повод молчать», и сообщение уходило бы с
	// кнопками, но без expect_status_at. Ночное «Да» тогда подтверждало бы тот
	// вопрос, который висит в момент нажатия, а не тот, который человек видел.
	StatusAt int64 `json:"status_at"`
}

// eventReplayMaxAge — событие, пролежавшее у агента дольше этого порога, считаем
// реплеем истории и не уведомляем (тот же порог, что в apk/src/notifications.ts).
const eventReplayMaxAge = 2 * time.Minute

// clockSkewNotable — с какого расхождения часов ПК и релея пишем строчку в лог.
// На решение «уведомлять или нет» расхождение НЕ влияет (см. eventAge).
const clockSkewNotable = 5 * time.Minute

// eventAge — сколько событие пролежало у агента ДО отправки на релей.
//
// Считается в часах САМОГО ПК: и ts (когда событие возникло, pty/events.go), и
// created_at (когда агент отдал его релею, internal/relay/client.go ForwardEvent)
// проставляет одна и та же машина, поэтому сдвиг её часов из разности уходит.
// Раньше здесь стояло time.Since(ev.TS) — то есть возраст по часам РЕЛЕЯ: у ПК с
// уехавшим временем (спящий ноутбук, VM без NTP, WSL) любое событие выглядело
// реплеем, и человек не получал уведомлений вовсе — молча.
//
// Возраст неизвестен (агент не проставил метки) → 0: событие только что пришло
// по живому сокету, а свежесть мы считаем по времени прихода на релей.
func eventAge(createdAt, tsMillis int64) time.Duration {
	if createdAt <= 0 || tsMillis <= 0 {
		return 0
	}
	// created_at — секунды, ts — миллисекунды: округление даёт «отрицательный
	// возраст» до секунды, это тот же ноль.
	if age := time.Unix(createdAt, 0).Sub(time.UnixMilli(tsMillis)); age > 0 {
		return age
	}
	return 0
}

// noteClockSkew — только диагностика: сообщить в лог (один раз на устройство),
// что часы ПК разошлись с часами релея. Само расхождение уведомление не глушит,
// но без этой строчки такие ПК были неотличимы от «уведомления не работают».
func (n *Notifier) noteClockSkew(deviceID string, createdAt int64) {
	if createdAt <= 0 {
		return
	}
	skew := time.Since(time.Unix(createdAt, 0))
	if skew < 0 {
		skew = -skew
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	c := n.clocks[deviceID]
	if c == nil {
		c = &deviceClock{}
		n.clocks[deviceID] = c
	}
	c.seen = time.Now()
	if c.logged || skew < clockSkewNotable {
		return
	}
	c.logged = true
	log.Printf("[NOTIFY] часы ПК device=%s расходятся с релеем на %s — свежесть событий считаем по времени прихода",
		deviceID, skew.Round(time.Second))
}

func (n *Notifier) onEvent(deviceID string, e protocol.Event) {
	if e.Channel != "events" {
		return
	}
	// Payload объявлен как any → после json.Unmarshal это map[string]any;
	// типизированный разбор требует повторной сериализации.
	raw, err := json.Marshal(e.Payload)
	if err != nil {
		return
	}
	var ev ptyEventPayload
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	if ev.Type == "agent_updated" {
		n.onAgentUpdated(deviceID, e, raw)
		return
	}
	if ev.Type != "pty_event" || ev.PtyID == "" {
		return
	}
	switch ev.Event {
	case string(notify.KindWaiting):
		// Пустой hint — это «агент освободился», а не вопрос: такие события
		// агент наружу не шлёт (EventAgentReady), но старые версии слали.
		if strings.TrimSpace(ev.Hint) == "" {
			return
		}
	case string(notify.KindError):
	default:
		return // finished / прочее — молчим
	}
	n.noteClockSkew(deviceID, e.CreatedAt)
	if eventAge(e.CreatedAt, ev.TS) > eventReplayMaxAge {
		return // агент переслал старое событие: реплей истории, а не новый вопрос
	}

	key := epKey{device: deviceID, pty: ev.PtyID}
	n.mu.Lock()
	defer n.mu.Unlock()
	// Кулдаун — на ТЕРМИНАЛ, а не на вопрос, и учитывается ДО постановки таймера,
	// один и тот же это вопрос или уже другой. Так и должно быть: waiting_input и
	// error чередуются на одном терминале, а у старых агентов (без метки эпизода)
	// «дрожит» ещё и hint — каждый раз это НОВЫЙ эпизод. Пока отметка отправки
	// лежала внутри эпизода, она вместе с ним и умирала, и от ночного шторма
	// оставался единственный рубеж — 12 сообщений в час на пользователя, то есть
	// ~96 за ночь.
	//
	// Но кулдаун здесь ОТКЛАДЫВАЕТ вопрос, а не выбрасывает: агент шлёт
	// waiting_input один раз на эпизод, и вернуться к этому вопросу больше
	// некому — выброшенный тут вопрос не дошёл бы никогда (а если человек ответил
	// не из Telegram, кулдаун ещё и не снимается через NoteAnswered). Отправку
	// всё равно гейтит перепроверка кулдауна в fire(), поэтому чаще одного
	// сообщения на терминал за ptyCooldown не станет.
	delay := n.holdDelay
	if left := n.cooldownLeftLocked(key); left > 0 {
		delay = max(delay, left+cooldownSlack)
	}
	if cur := n.eps[key]; cur != nil && cur.sameQuestion(ev) {
		// Уже написали, прямо сейчас пишем или вот-вот напишем — второго
		// сообщения про этот же вопрос быть не должно. Проверка notified тут
		// обязательна именно из-за отложенного кулдауна: без неё реплей события
		// при переподключении агента не отбрасывался бы, а превращался в
		// отложенный дубль в конце кулдауна.
		if cur.timer != nil || cur.sending || cur.notified {
			return
		}
	} else if cur != nil && cur.timer != nil {
		cur.timer.Stop() // новый вопрос вытесняет старый
	}
	// Таймер на терминал всегда ровно один: либо мы вернулись выше (тот же
	// вопрос), либо погасили прежний. Ожидание конца кулдауна бюджет переносов
	// не тратит — см. episode.held.
	ep := &episode{
		kind: ev.Event, hint: ev.Hint, statusAt: ev.StatusAt, startedAt: time.Now(),
		held: delay - n.holdDelay,
	}
	ep.timer = time.AfterFunc(delay, func() { n.fire(key, ev) })
	n.eps[key] = ep
}

// cooldownLeftLocked — сколько ещё молчать по этому терминалу. Вызывать под n.mu.
func (n *Notifier) cooldownLeftLocked(key epKey) time.Duration {
	sent, ok := n.lastSent[key]
	if !ok {
		return 0
	}
	if left := n.ptyCooldown - time.Since(sent); left > 0 {
		return left
	}
	return 0
}

// fire — отложенная отправка: здесь живут ВСЕ проверки и весь ввод-вывод.
func (n *Notifier) fire(key epKey, ev ptyEventPayload) {
	// Отметку «эпизод в работе» ставим ПЕРВЫМ делом. Ниже — чтение устройства и
	// пользователя из БД, поход к ПК с таймаутом 5 с и сама отправка в Telegram;
	// всё это время повтор того же события видел бы «таймера нет, ничего не
	// отправлено» и заводил бы вторую отправку — на один вопрос ушло бы два
	// сообщения.
	n.mu.Lock()
	ep := n.eps[key]
	if ep != nil {
		ep.timer = nil
		ep.sending = true
	}
	// Кулдаун перепроверяем и здесь: между постановкой таймера и этим моментом
	// прошло holdDelay, за которое по этому терминалу могло уйти сообщение по
	// другому вопросу (тот вытеснил бы эпизод, но не отметку отправки).
	cooldown := n.cooldownLeftLocked(key)
	n.mu.Unlock()
	defer n.releaseSending(key, ep)

	if cooldown > 0 {
		// Не выбрасываем — переносим за конец кулдауна: второго waiting_input по
		// этому эпизоду агент не пришлёт. Сюда попадаем, когда сообщение по
		// терминалу ушло уже ПОСЛЕ постановки таймера (вопрос сменился, пока
		// прежний ждал holdDelay).
		n.rearm(key, ep, ev, cooldown+cooldownSlack, true)
		return
	}
	if !n.srv.Config.NotifyTG || n.srv.UserNotify == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dev, err := db.GetDevice(ctx, n.srv.DB, key.device)
	if err != nil || dev.RevokedAt.Valid {
		return
	}

	// 1) Присутствие: человек в приложении — вопрос он и так видит. Проверка идёт
	//    до похода к ПК и до Telegram, чтобы перенос отправки (rearm) стоил
	//    одного чтения из БД и обращения к карте в памяти, а не запроса к агенту.
	if away, active := n.srv.Hub.ClientAwayFor(key.device, dev.UserID); active || away < n.holdDelay {
		if n.rearm(key, ep, ev, n.holdDelay, false) {
			return
		}
		metrics.AgentNotice("present")
		return
	}

	// 2) Кому писать. Анонимный аккаунт (пейринг по QR с телефона) не имеет
	//    chat_id — уведомить его невозможно в принципе.
	u, err := db.GetUserByID(ctx, n.srv.DB, dev.UserID)
	if err != nil {
		return
	}
	// Background events never start a trial or grant free cloud service.
	if n.srv.effectiveTier(u) == "free" {
		return
	}
	if db.IsAnonTelegramID(u.TelegramID) {
		metrics.AgentNotice("no_tg")
		return
	}
	if on, err := db.NotifyEnabled(ctx, n.srv.DB, u.ID); err != nil || !on {
		metrics.AgentNotice("muted")
		return
	}

	// 3) Вопрос ещё висит? За время задержки человек мог ответить прямо с ПК.
	ac := n.srv.Hub.Get(key.device)
	online := ac != nil
	// Метка эпизода: с ней ответ из этого сообщения не применится к СЛЕДУЮЩЕМУ
	// вопросу агента (см. expect_status_at). База — штамп из САМОГО события: до
	// ПК мы можем и не достучаться, а сообщение с кнопками при этом уйдёт, и
	// кнопка без метки бьёт в тот вопрос, который висит в момент нажатия.
	// Устаревший штамп безопасен: ПК сверит его со своим и ответит 409
	// prompt_changed — «агент спрашивает уже о другом», ввод не применится.
	var statusAt int64
	if ev.Event == string(notify.KindWaiting) {
		statusAt = ev.StatusAt
	}
	if online && ev.Event == string(notify.KindWaiting) {
		if st, ok := n.ptyState(ctx, ac, key.pty); ok {
			if st.Status != "" && (st.Status != "waiting" || strings.TrimSpace(st.Hint) == "") {
				metrics.AgentNotice("stale")
				return
			}
			if st.Hint != "" {
				ev.Hint, ev.HintKind = st.Hint, st.HintKind
			}
			if st.Name != "" {
				ev.Name = st.Name
			}
			// Свежий штамп ПК точнее события: за holdDelay вопрос мог смениться, а
			// текст выше мы взяли уже новый — метка обязана описывать его же. Ноль
			// в ответе ПК — это «штампа не дал», а не «метки нет»: затирать им
			// метку события нельзя, иначе дыра возвращается на ровном месте.
			if st.StatusAt > 0 {
				statusAt = st.StatusAt
			}
		}
	}

	// 4) Антиспам на пользователя — последний рубеж против «ночи из 200 сообщений».
	if !n.allowUser(u.ID) {
		metrics.AgentNotice("ratelimit")
		return
	}

	notice := notify.Notice{
		Kind:        notify.Kind(ev.Event),
		DeviceID:    dev.ID,
		DeviceName:  dev.Name,
		PtyID:       key.pty,
		PtyName:     ev.Name,
		Agent:       ev.Agent,
		FgProcess:   ev.FgProcess,
		Hint:        ev.Hint,
		HintKind:    ev.HintKind,
		StatusAt:    statusAt,
		AgentOnline: online,
		CanReply:    online && n.agentSupportsInput(dev.AgentVersion),
	}
	// Отметку «отправлено» ставим ДО похода в Telegram: отправка занимает сотни
	// миллисекунд, и повтор события, пришедший в этот момент, иначе уехал бы
	// вторым сообщением. Отметка привязана к ТЕРМИНАЛУ, а не к эпизоду: вопрос за
	// это время мог смениться, но кулдаун обязан пережить смену вопроса. При
	// ошибке TG отметка тоже остаётся: биться в недоступный API каждые 30 секунд
	// хуже, чем пропустить одно уведомление.
	n.mu.Lock()
	n.lastSent[key] = time.Now()
	// Латч эпизода: помечаем, что про ЭТОТ вопрос человеку уже написали, и
	// приводим эпизод к тому, что реально уехало в сообщение (переспрос ПК мог
	// уточнить и текст, и метку). Иначе событие про тот же вопрос завело бы
	// второй эпизод и второе сообщение в конце кулдауна. Правим только СВОЙ
	// эпизод: пока fire ходил в БД и к ПК, вопрос мог смениться — тогда в карте
	// лежит уже другой, и он про это сообщение ничего не знает.
	if ep != nil && n.eps[key] == ep {
		ep.notified = true
		ep.hint = ev.Hint
		if statusAt > 0 {
			ep.statusAt = statusAt
		}
	}
	n.mu.Unlock()

	if err := n.srv.UserNotify(ctx, u.TelegramID, notice); err != nil {
		log.Printf("[NOTIFY] send user=%d device=%s pty=%s: %v", u.ID, dev.ID, key.pty, err)
		metrics.AgentNotice("failed")
		return
	}
	metrics.AgentNotice("sent")
	log.Printf("[NOTIFY] agent waiting: user=%d device=%s pty=%s kind=%s can_reply=%v",
		u.ID, dev.ID, key.pty, ev.Event, notice.CanReply)
}

// rearm переносит отправку эпизода на d. Возвращает false, когда переносить
// больше некуда (эпизод ждёт дольше отпущенного бюджета) — тогда молчим совсем:
// человек всё это время был в приложении и вопрос видел.
//
// held=true — перенос НЕ из-за присутствия человека, а ради конца кулдауна
// терминала: такое ожидание бюджет переносов не тратит (см. episode.held),
// иначе дождавшийся своей очереди вопрос пропал бы при первом же появлении
// человека в приложении.
//
// Шторма не будет: таймер на терминал всегда ровно один, число переносов
// по присутствию ограничено (maxHoldFactor), перенос по кулдауну на терминал
// бывает не чаще самого кулдауна, а сама проверка присутствия стоит одного
// обращения к карте в памяти.
func (n *Notifier) rearm(key epKey, own *episode, ev ptyEventPayload, d time.Duration, held bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	ep := n.eps[key]
	if ep == nil || ep.timer != nil {
		return true // эпизод вытеснен новым вопросом — этот таймер больше не нужен
	}
	// Переносим ТОЛЬКО свой эпизод. Пока fire ходил в БД и к ПК, вопрос мог
	// смениться, и в карте лежит уже другой эпизод — повесить на него таймер с
	// нашим (старым) событием значит прислать человеку вопрос, которого агент
	// давно не задаёт. Именно так «первый вопрос» всплывал третьим сообщением.
	if own != nil && ep != own {
		return true
	}
	if time.Since(ep.startedAt) > ep.held+n.holdDelay*maxHoldFactor {
		return false
	}
	if held {
		ep.held += d
	}
	ep.timer = time.AfterFunc(d, func() { n.fire(key, ev) })
	return true
}

// releaseSending снимает отметку «в работе» — но только со СВОЕГО эпизода: пока
// fire ходил в БД и к ПК, вопрос мог смениться, и в карте лежит уже другой
// эпизод со своим таймером.
func (n *Notifier) releaseSending(key epKey, ep *episode) {
	if ep == nil {
		return
	}
	n.mu.Lock()
	if n.eps[key] == ep {
		ep.sending = false
	}
	n.mu.Unlock()
}

// ── «Агент обновился» ────────────────────────────────────────────────────────
//
// Отдельный, гораздо более простой поток: обновление — редкое событие без
// эпизодов, кулдаунов и переспроса ПК. Одно спокойное сообщение на
// (пользователь, устройство, версия), дедуп — записью в events (см. db).

// agentUpdatedPayload — контракт с агентом:
// {"type":"agent_updated","version":"2.46.5","ts":<unix ms>}.
type agentUpdatedPayload struct {
	Type    string `json:"type"`
	Version string `json:"version"`
	TS      int64  `json:"ts"`
}

// onAgentUpdated — фильтр в цикле чтения: здесь, как и в onEvent, разрешены
// только память и time.AfterFunc (см. шапку файла). БД и Telegram — в
// fireAgentUpdated.
func (n *Notifier) onAgentUpdated(deviceID string, e protocol.Event, raw []byte) {
	var ev agentUpdatedPayload
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	version := strings.TrimSpace(ev.Version)
	if version == "" {
		return
	}
	n.noteClockSkew(deviceID, e.CreatedAt)
	if eventAge(e.CreatedAt, ev.TS) > eventReplayMaxAge {
		return // реплей истории, как у pty-событий
	}
	// «Немедленно, но вне цикла»: таймер с нулевой задержкой срабатывает в
	// своей горутине, цикл чтения событий не блокируется.
	time.AfterFunc(0, func() { n.fireAgentUpdated(deviceID, version) })
}

// fireAgentUpdated — проверки и отправка «ПК обновился». Все выходы молчаливые:
// это фоновое событие, повторный его прогон бесплатен (агент пришлёт событие
// снова при следующем подключении — если дедуп-отметки ещё нет).
func (n *Notifier) fireAgentUpdated(deviceID, version string) {
	if !n.srv.Config.NotifyTG || n.srv.UserNotifyText == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dev, err := db.GetDevice(ctx, n.srv.DB, deviceID)
	if err != nil || dev.RevokedAt.Valid {
		return
	}
	u, err := db.GetUserByID(ctx, n.srv.DB, dev.UserID)
	if err != nil {
		return
	}
	if n.srv.effectiveTier(u) == "free" {
		return
	}
	if db.IsAnonTelegramID(u.TelegramID) {
		metrics.AgentUpdate("no_tg")
		return
	}
	if on, err := db.NotifyEnabled(ctx, n.srv.DB, u.ID); err != nil || !on {
		metrics.AgentUpdate("muted")
		return
	}

	// Дедуп на (user, device, version): про обновление до конкретной версии
	// пишем один раз, сколько бы раз агент ни переподключался. Проверка и
	// вставка НЕ атомарны — два одновременных коннекта агента могут проскочить
	// оба; худшее последствие — дубль сообщения раз в релиз, ради устранения
	// которого транзакция и уникальный индекс не нужны.
	seen, err := db.HasAgentUpdatedEvent(ctx, n.srv.DB, u.ID, deviceID, version)
	if err != nil {
		// «Не смогли проверить» — молчим: при следующем событии проверим снова,
		// а дубль без отметки гарантированно хуже.
		log.Printf("[NOTIFY] agent_updated dedup user=%d device=%s: %v", u.ID, deviceID, err)
		return
	}
	if seen {
		metrics.AgentUpdate("dedup")
		return
	}
	// Отметку ставим ДО отправки (как lastSent в fire): при ошибке TG не бьёмся
	// в недоступный API на каждом переподключении агента.
	if err := db.MarkAgentUpdatedEvent(ctx, n.srv.DB, u.ID, deviceID, version); err != nil {
		log.Printf("[NOTIFY] agent_updated mark user=%d device=%s: %v", u.ID, deviceID, err)
		return
	}

	if err := n.srv.UserNotifyText(ctx, u.TelegramID, dev.Name, version); err != nil {
		log.Printf("[NOTIFY] agent_updated send user=%d device=%s v%s: %v", u.ID, deviceID, version, err)
		metrics.AgentUpdate("failed")
		return
	}
	metrics.AgentUpdate("sent")
	log.Printf("[NOTIFY] agent updated: user=%d device=%s version=%s", u.ID, deviceID, version)
}

// NoteAnswered снимает латч терминала после успешного ответа из Telegram:
// следующий вопрос (даже дословно такой же) снова дойдёт до человека, а
// висящая отправка по уже отвеченному вопросу отменяется. Кулдаун терминала
// тоже снимаем: человек только что ответил осознанным нажатием, и следующий
// вопрос агента — это продолжение диалога, а не шторм.
func (n *Notifier) NoteAnswered(deviceID, ptyID string) {
	if n == nil {
		return
	}
	key := epKey{device: deviceID, pty: ptyID}
	n.mu.Lock()
	if ep := n.eps[key]; ep != nil {
		if ep.timer != nil {
			ep.timer.Stop()
		}
		delete(n.eps, key)
	}
	delete(n.lastSent, key)
	n.mu.Unlock()
}

// allowUser — токен-бакет maxPerHour сообщений в час на пользователя.
func (n *Notifier) allowUser(userID int64) bool {
	if n.maxPerHour <= 0 {
		return true
	}
	max := float64(n.maxPerHour)
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.rl[userID]
	if b == nil {
		b = &userBucket{tokens: max, last: now}
		n.rl[userID] = b
	}
	if refill := now.Sub(b.last).Hours() * max; refill > 0 {
		b.tokens = min(max, b.tokens+refill)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// PruneEpisodes чистит отработавшие эпизоды, отметки отправки, заметки о часах
// и полные бакеты: карты растут по числу пар (device,pty), устройств и
// пользователей за всё время жизни процесса. Эпизоды с живым таймером и
// эпизоды в работе не трогаем.
//
// Вместе с эпизодом уходит и его латч (notified): вопрос, провисевший дольше
// olderThan (6 ч в server.go) и заново пришедший от агента, снова станет
// сообщением. Это осознанно — за такой срок «напомнить один раз» полезнее, чем
// молчать вечно; частоту всё равно держит кулдаун терминала.
func (n *Notifier) PruneEpisodes(olderThan time.Duration) int {
	if n == nil {
		return 0
	}
	now := time.Now()
	cutoff := now.Add(-olderThan)
	// Отметки отправки обязаны жить дольше кулдауна: стереть их раньше — значит
	// снять рубеж «не чаще раза в … на терминал».
	sentCutoff := now.Add(-max(olderThan, n.ptyCooldown))
	removed := 0
	n.mu.Lock()
	for k, ep := range n.eps {
		if ep.timer == nil && !ep.sending && ep.startedAt.Before(cutoff) {
			delete(n.eps, k)
			removed++
		}
	}
	for k, at := range n.lastSent {
		if at.Before(sentCutoff) {
			delete(n.lastSent, k)
		}
	}
	for dev, c := range n.clocks {
		if c.seen.Before(cutoff) {
			delete(n.clocks, dev)
		}
	}
	for uid, b := range n.rl {
		if b.last.Before(cutoff) {
			delete(n.rl, uid)
		}
	}
	n.mu.Unlock()
	return removed
}

// ptyStateInfo — часть ответа GET /api/pty/{id}/state, которая нам нужна.
type ptyStateInfo struct {
	Status   string `json:"status"`
	StatusAt int64  `json:"status_at"` // начало эпизода ожидания, unix ms
	Hint     string `json:"hint"`
	HintKind string `json:"hint_kind"`
	Name     string `json:"name"`
	Alive    bool   `json:"alive"`
}

// ptyState переспрашивает у ПК актуальное состояние терминала. ok=false, когда
// спросить не удалось — это НЕ повод молчать (агент мог быть занят), а лишь
// повод не уточнять вопрос.
func (n *Notifier) ptyState(ctx context.Context, ac *relayhub.AgentConn, ptyID string) (ptyStateInfo, bool) {
	var st ptyStateInfo
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := ac.Send(cctx, protocol.Cmd{
		Type:      protocol.MsgCmd,
		RequestID: uuid.NewString(),
		Method:    http.MethodGet,
		Path:      "/api/pty/" + ptyID + "/state",
	}, 5*time.Second)
	if err != nil || res == nil || res.StatusCode != http.StatusOK {
		return st, false
	}
	if err := json.Unmarshal(res.Body, &st); err != nil {
		return st, false
	}
	return st, true
}

// agentSupportsInput — умеет ли эта версия агента POST /api/pty/{id}/input.
// Неизвестную/нечисловую версию трактуем как «не умеет»: иначе кнопки ответа
// висели бы и падали в 404.
func (n *Notifier) agentSupportsInput(agentVersion string) bool {
	minVer := strings.TrimSpace(n.srv.Config.NotifyMinAgent)
	if minVer == "" || minVer == "any" || minVer == "0" {
		return true
	}
	return versionAtLeast(agentVersion, minVer)
}

// versionAtLeast — сравнение вида "2.30.1" >= "2.30.0". Префикс "v" снимается,
// нечисловые хвосты ("2.30.0-rc1") отбрасываются, битая версия → false.
func versionAtLeast(have, want string) bool {
	hv, ok := parseVersion(have)
	if !ok {
		return false
	}
	wv, ok := parseVersion(want)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if hv[i] != wv[i] {
			return hv[i] > wv[i]
		}
	}
	return true
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	if v == "" {
		return out, false
	}
	parts := strings.SplitN(v, ".", 4)
	for i := 0; i < 3 && i < len(parts); i++ {
		p := parts[i]
		// Обрезаем всё нецифровое: "0-rc1" → "0".
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 {
			return out, false
		}
		nval, err := strconv.Atoi(p[:end])
		if err != nil {
			return out, false
		}
		out[i] = nval
	}
	return out, true
}

// SendPtyInput пишет одну «клавишу» в терминал ПК от имени уже авторизованного
// пользователя. Внутренний путь: релей отправляет агенту обычный Cmd через Hub,
// а агент сам подставляет свой X-API-Token (internal/relay/client.go —
// «relay-proxied requests are already authorized at the relay»), поэтому ни
// user-JWT, ни лишнего HTTP-хопа не нужно.
//
// Контракт эндпоинта — internal/web/api_pty.go (apiPtyInput): тело
// {key, expect_status_at}, машинные коды ошибок. Разрешён только словарь
// notify (подмножество ptyKeyBytes агента): произвольные байты из callback_data
// в PTY не попадают никогда.
//
// Права проверяются ЗДЕСЬ, а не только у вызывающего: это единственное место,
// где релей пишет в терминал чужого ПК по device_id, и точка принуждения обязана
// совпадать с точкой проверки. В боте проверка остаётся ранним отказом с
// понятным человеку текстом (cbPtyInput), но второй вызывающий про неё знать не
// обязан.
func (s *Server) SendPtyInput(ctx context.Context, req notify.InputRequest) error {
	if !notify.KeyAllowed(req.Key) {
		return notify.ErrKeyNotAllowed
	}
	// Без пользователя писать в терминал нельзя: «не указан» — это отказ, а не
	// «разрешено всё» (вызывающий, забывший проставить UserID, должен получить
	// ошибку, а не запись в чужой ПК).
	if req.UserID <= 0 {
		return notify.ErrNoAccess
	}
	ok, err := db.UserCanAccessDevice(ctx, s.DB, req.DeviceID, req.UserID)
	if err != nil {
		// «Не смогли проверить» ≠ «нет доступа»: занятая SQLite не повод говорить
		// владельцу, что это не его ПК.
		return fmt.Errorf("проверка доступа к %s: %w", req.DeviceID, err)
	}
	if !ok {
		return notify.ErrNoAccess
	}
	if s.Config == nil {
		return notify.ErrSubscriptionUnavailable
	}
	dev, err := db.GetDevice(ctx, s.DB, req.DeviceID)
	if err != nil {
		return notify.ErrSubscriptionUnavailable
	}
	if !s.selfHosted() {
		if err := db.StartTrialIfNeeded(ctx, s.DB, dev.UserID, s.Config.TrialDays); err != nil {
			return notify.ErrSubscriptionUnavailable
		}
	}
	access, err := s.cloudAccess(ctx, dev.UserID)
	if err != nil {
		return notify.ErrSubscriptionUnavailable
	}
	if !access.Allowed {
		return notify.ErrSubscriptionRequired
	}
	ac := s.Hub.Get(req.DeviceID)
	if ac == nil {
		return notify.ErrOffline
	}
	payload := map[string]any{"key": req.Key}
	if req.ExpectStatusAt > 0 {
		// «Отвечаю на тот вопрос, который человек видел»: если агент за это
		// время спросил о другом, ПК ответит 409 и ввод НЕ применит.
		payload["expect_status_at"] = req.ExpectStatusAt
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	res, err := ac.Send(ctx, protocol.Cmd{
		Type:      protocol.MsgCmd,
		RequestID: uuid.NewString(),
		Method:    http.MethodPost,
		Path:      "/api/pty/" + req.PtyID + "/input",
		Body:      body,
		Headers:   map[string]string{"Content-Type": "application/json"},
	}, 8*time.Second)
	if err != nil {
		if errors.Is(err, relayhub.ErrAgentDisconnect) || errors.Is(err, relayhub.ErrAgentOffline) {
			return notify.ErrOffline
		}
		return err
	}
	if res.StatusCode >= 400 {
		return agentInputError(res.StatusCode, res.Body)
	}
	// Ответ ушёл — снимаем латч, чтобы следующий вопрос этого терминала снова
	// дошёл до человека.
	s.Notifier.NoteAnswered(req.DeviceID, req.PtyID)
	return nil
}

// agentInputError переводит отказ ПК в sentinel, понятный боту. Разбираем
// машинный code, а не текст: тексты локализуются и меняются.
func agentInputError(status int, body []byte) error {
	var e struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	switch e.Code {
	case "pty_not_found", "pty_dead":
		return notify.ErrPtyGone
	case "prompt_changed":
		return notify.ErrPromptChanged
	case "unknown_key":
		return notify.ErrKeyNotAllowed
	case "rate_limited":
		// Отдельная причина, а не общее «не получилось»: человеку надо сказать
		// «слишком часто», иначе он будет давить кнопку ещё усерднее.
		return notify.ErrRateLimited
	}
	// Старый агент: роута нет вовсе, net/http отдаёт текстовый «404 page not
	// found» без code — это «обновите Remotai», а не «терминал закрыт».
	if status == http.StatusNotFound {
		return notify.ErrUnsupported
	}
	if status == http.StatusTooManyRequests {
		return notify.ErrRateLimited // лимитер агента без машинного кода
	}
	if status == http.StatusGone || status == http.StatusConflict {
		return notify.ErrPtyGone
	}
	return fmt.Errorf("агент ответил %d: %s", status, agentErrText(e.Error, body))
}

// agentErrText — текст ошибки агента: поле error, иначе первые 120 символов тела.
func agentErrText(errField string, body []byte) string {
	if errField != "" {
		return errField
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
