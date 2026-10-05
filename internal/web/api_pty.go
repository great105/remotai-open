package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/bundle"
	"tgcontrol/internal/connstat"
	"tgcontrol/internal/observability"
	"tgcontrol/internal/pty"
	"tgcontrol/internal/wincli"
	"tgcontrol/internal/wsutil"
)

// ptyLiveCap — максимум одновременных PTY-WS на (uid, session). Легитимно их
// мало: по одному на поверхность (телефон/web/tg/окно exe) плюс запас на
// смену сокета при реконнекте. Всё сверх — патология (см. «зомби-циклы»
// 2.15.12: до ~18 сокетов на сессию), старейший вытесняется.
const ptyLiveCap = 6

// ptyHeartbeatInterval — период application-level heartbeat `{"t":"hb"}` в
// PTY-WS. Protocol-level пинги (wsutil) браузерный JS не видит — их гасит
// сетевой стек, поэтому клиент не может отличить «терминал молчит» от «сокет
// мёртв» (полумёртвые сокеты мобильных WebView: смена сети/VPN без onclose).
// Маркер reset/resumed объявляет интервал полем hb (сек); клиент взводит
// idle-вотчдог ~2.5×hb и при тишине сам пересоздаёт соединение — с resume по
// offset+epoch это почти бесплатно. Старые клиенты игнорируют и hb-поле, и
// hb-сообщения (неизвестный JSON).
const ptyHeartbeatInterval = 20 * time.Second

// screenFrameMinGap — как часто одному клиенту отдаётся кадр экрана. Сборка
// кадра обходит всю сетку зеркала, и делать это на каждое нажатие незачем:
// кадр нужен при открытии терминала и при возврате из фона.
const screenFrameMinGap = time.Second

// splitAtScreenBase — где разрезать пачку вывода, чтобы придержанный кадр встал
// РОВНО на свою базу потока. Ноль — резать не нужно.
//
// ⚠ ЗАЧЕМ. Позиция клиента (`sent`) растёт на ВЕСЬ батч разом, поэтому проверка
// «клиент догнал базу кадра» проходила уже после того, как он её перепрыгнул:
// клиент применял байты ПОСЛЕ базы, а следом получал кадр состояния ДО них.
// Картинка откатывалась, и откаченные изменения не приходили больше никогда —
// один из источников «то продублировалось, то пропало». Дыры в очереди для
// этого не нужны, поэтому и лечится это здесь, а не в защите от дыр
// (повторный аудит 2.57.12, T259-07).
//
// Порядок байт не меняется: клиент получает то же самое, только шов кадра
// оказывается на своём месте — префикс → кадр(база) → суффикс.
func splitAtScreenBase(sent uint64, batch int, base uint64, pending bool) int {
	if !pending || batch <= 0 || base == 0 {
		return 0
	}
	if sent >= base {
		return 0 // база уже позади — кадр отдаст обычный путь
	}
	if sent+uint64(batch) <= base {
		return 0 // пачка целиком до базы — резать нечего
	}
	return int(base - sent)
}

// screenHoldMax — сколько ждать, пока клиент получит вывод, который снятый
// кадр уже содержит (см. sendScreen). Обычно догон занимает один тик пачки
// (~25 мс): байты к этому моменту уже в канале подписчика. Ждать долго нельзя —
// за кадром стоит живой человек, открывающий терминал, и лучше отдать кадр с
// риском дубля, чем показать ему пустой экран.
const screenHoldMax = 750 * time.Millisecond

type screenHoldAction uint8

const (
	screenHoldSend screenHoldAction = iota
	screenHoldCancelAndRetry
)

// screenHoldTimeoutAction — предохранитель от кадра из будущего.
// Timeout ограничивает жизнь снятого кадра, но не отменяет главный
// инвариант: пока sent < base, кадр не отправляется.
func screenHoldTimeoutAction(sent, base uint64) screenHoldAction {
	if sent < base {
		return screenHoldCancelAndRetry
	}
	return screenHoldSend
}

const maxJSONSafeInteger = uint64(1<<53 - 1)

// parseSafeJSONUint — неотрицательное целое, точно представимое JSON-числом
// (≤ 2^53-1). Так проверяются geom_rev и req: строка, дробь, минус, NaN и всё,
// что за пределом, — не число протокола.
func parseSafeJSONUint(raw any) (uint64, bool) {
	v, ok := raw.(float64)
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > float64(maxJSONSafeInteger) || math.Trunc(v) != v {
		return 0, false
	}
	return uint64(v), true
}

type screenFrameRequest struct {
	geomRev uint64
	// req — необязательный номер запроса клиента screen-request-v1 (ST-05).
	// Сервер эхом возвращает его в screen/screen-none, и клиент сопоставляет
	// ответ со СВОИМ запросом, а не угадывает по geom_rev (кадр в ответ на уже
	// вытесненный запрос с той же ревизией раньше принимался). hasReq отличает
	// «не прислан» (старый клиент) от req=0. Очередь latest-only, отложенный
	// повтор и OfferIfEmpty хранят структуру целиком, поэтому req переживает
	// любой внутренний повтор.
	req    uint64
	hasReq bool
}

func parseScreenFrameRequest(ctrl map[string]any) (screenFrameRequest, bool) {
	rev, ok := parseSafeJSONUint(ctrl["geom_rev"])
	if !ok {
		return screenFrameRequest{}, false
	}
	out := screenFrameRequest{geomRev: rev}
	if raw, present := ctrl["req"]; present {
		// Присланный, но битый req — ошибка клиента: ответ, который нельзя
		// сопоставить с запросом, хуже честного отказа (как и с geom_rev).
		id, ok := parseSafeJSONUint(raw)
		if !ok {
			return screenFrameRequest{}, false
		}
		out.req, out.hasReq = id, true
	}
	return out, true
}

// screenRequestCapability — способность, которую клиент объявляет в
// terminal-capabilities, чтобы получать screen-capability, screen-none и эхо
// req (ST-05, T-36). Без неё сервер ведёт себя байт в байт как раньше: молчит
// на пустой кадр и на запрос без geom_rev.
const screenRequestCapability = "screen-request-v1"

// screenReasonInvalidRequest — причина screen-none на запрос без корректного
// geom_rev (или с битым req). Остальные причины — pty.ScreenReason*.
const screenReasonInvalidRequest = "invalid-request"

// marshalScreenFrame — ответ `screen` на запрос кадра.
//
// Поля и их порядок прежние: старый клиент читает screen/history/... и
// base_offset, неизвестные ключи игнорирует. req стоит последним и есть только
// если клиент его прислал, поэтому старый клиент получает прежние байты (T-36).
//
// СЕМАНТИКА OFFSET — один контракт для сервера, клиента, логов и тестов:
// offset = число байт исходного потока эпохи, уже учтённых = 0-based позиция
// следующего байта; служебные сообщения (screen, history, modes, gap notice) в
// шкалу не входят. base_offset кадра — позиция, которую кадр УЖЕ учитывает:
// байты с позициями < base_offset в нём нарисованы, байт base_offset — первый,
// которого в нём ещё нет. Offset маркера reset/resumed — позиция первого байта
// его payload, то есть та же шкала (см. wsPtyHandler).
func marshalScreenFrame(frame, history string, histLines, cols, rows int, baseOff uint64, req screenFrameRequest) ([]byte, error) {
	var reqID *uint64
	if req.hasReq {
		id := req.req
		reqID = &id
	}
	return json.Marshal(struct {
		T       string  `json:"t"`
		Data    string  `json:"screen"`
		History string  `json:"history"`
		HistN   int     `json:"hist_lines"`
		Cols    int     `json:"screen_cols"`
		Rows    int     `json:"screen_rows"`
		BaseOff uint64  `json:"base_offset"`
		GeomRev uint64  `json:"geom_rev"`
		Req     *uint64 `json:"req,omitempty"`
	}{T: "screen", Data: frame, History: history, HistN: histLines, Cols: cols, Rows: rows, BaseOff: baseOff, GeomRev: req.geomRev, Req: reqID})
}

// screenNoneMessage — отрицательный ответ на запрос кадра:
// {"t":"screen-none","req":N,"geom_rev":N,"reason":"…"}. geom_rev — эхо токена
// запроса, а не ревизия сервера; у запроса без корректного geom_rev поля нет,
// как и req у запроса без req.
func screenNoneMessage(req screenFrameRequest, haveGeom bool, reason string) ([]byte, error) {
	var reqID, geomRev *uint64
	if req.hasReq {
		id := req.req
		reqID = &id
	}
	if haveGeom {
		rev := req.geomRev
		geomRev = &rev
	}
	return json.Marshal(struct {
		T       string  `json:"t"`
		Req     *uint64 `json:"req,omitempty"`
		GeomRev *uint64 `json:"geom_rev,omitempty"`
		Reason  string  `json:"reason"`
	}{T: "screen-none", Req: reqID, GeomRev: geomRev, Reason: reason})
}

// screenNoneReply — что ответить на пустой кадр. Клиенту без
// screen-request-v1 — ничего (nil): он ждёт прежнего молчания и остаётся на
// сыром потоке. Согласовавшему — screen-none с причиной, чтобы он не ждал
// ответа, которого не будет (ST-05, «молчаливый отказ»).
func screenNoneReply(capable bool, req screenFrameRequest, reason string) []byte {
	if !capable {
		return nil
	}
	if reason == "" {
		// Пустой кадр без причины по построению CaptureScreenFrame невозможен;
		// если всё же случится — «повтори позже» безопаснее, чем «не будет»
		// (unavailable уводит клиента на полный reset).
		reason = pty.ScreenReasonNotReady
	}
	msg, err := screenNoneMessage(req, true, reason)
	if err != nil {
		return nil
	}
	return msg
}

// screenCapabilityMessage — подтверждение screen-request-v1 (ST-05, T-36).
var screenCapabilityMessage = []byte(`{"t":"screen-capability","v":1}`)

// screenPeer — согласование screen-request-v1 одного соединения. Методы
// возвращают сообщения в том порядке, в каком их надо записать; писатель
// сокета — основной цикл wsPtyHandler, он же единственный, кто зовёт методы.
// Исключение — capable: его ставит читающая горутина до того, как отдаст в
// очередь следующие запросы того же клиента.
type screenPeer struct {
	capable atomic.Bool
	acked   bool
	// lastNone — последний отправленный отказ: внутренние повторы сервера
	// (resize, отмена кадра) по тому же запросу не должны засыпать клиента
	// одинаковыми screen-none. Сбрасывается, как только кадр снят.
	lastNone string
}

// ack — подтверждение способности, ровно один раз и только согласовавшему.
func (p *screenPeer) ack() [][]byte {
	if p.acked || !p.capable.Load() {
		return nil
	}
	p.acked = true
	return [][]byte{screenCapabilityMessage}
}

// emptyFrame — ответ на пустой кадр. Клиенту без способности — ничего
// (прежнее молчание). Согласовавшему — подтверждение, если его ещё не было
// (клиент не должен увидеть отказ раньше, чем узнает, что сервер их умеет), и
// screen-none, если это не повтор предыдущего.
func (p *screenPeer) emptyFrame(req screenFrameRequest, reason string) [][]byte {
	msg := screenNoneReply(p.capable.Load(), req, reason)
	if msg == nil {
		return nil
	}
	out := p.ack()
	if string(msg) == p.lastNone {
		return out
	}
	p.lastNone = string(msg)
	return append(out, msg)
}

// rejected — ответ на запрос без корректного geom_rev (или с битым req).
func (p *screenPeer) rejected(req screenFrameRequest) [][]byte {
	if !p.capable.Load() {
		return nil
	}
	msg, err := screenNoneMessage(req, false, screenReasonInvalidRequest)
	if err != nil {
		return nil
	}
	return append(p.ack(), msg)
}

// frameCaptured — кадр снят: следующий отказ уже не повтор.
func (p *screenPeer) frameCaptured() { p.lastNone = "" }

// screenRequestQueue is latest-only. A geometry change can request a fresh
// frame while an older request is still buffered; keeping the old token would
// guarantee that the client rejects the response and can loop forever.
type screenRequestQueue struct {
	mu     sync.Mutex
	latest screenFrameRequest
	have   bool
	wake   chan struct{}
}

func newScreenRequestQueue() *screenRequestQueue {
	return &screenRequestQueue{wake: make(chan struct{}, 1)}
}

func (q *screenRequestQueue) Offer(req screenFrameRequest) {
	q.mu.Lock()
	q.latest = req
	q.have = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// OfferIfEmpty is for an internal retry of the last served request. It must
// never overwrite a newer client geometry revision already waiting in q.
func (q *screenRequestQueue) OfferIfEmpty(req screenFrameRequest) {
	q.mu.Lock()
	if q.have {
		q.mu.Unlock()
		return
	}
	q.latest = req
	q.have = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *screenRequestQueue) Take() (screenFrameRequest, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.have {
		return screenFrameRequest{}, false
	}
	req := q.latest
	q.have = false
	return req, true
}

// deferScreenRequest — просьба кадра пришла раньше screenFrameMinGap: не
// отбрасываем, а возвращаем её в очередь, когда пауза истечёт. OfferIfEmpty —
// чтобы отложенная просьба не перекрыла более новую геометрию клиента, которая
// успела встать в очередь за это время. Таймер после закрытия соединения
// безвреден: очередь никто не читает, канал wake буферизован.
func deferScreenRequest(q *screenRequestQueue, req screenFrameRequest, wait time.Duration) *time.Timer {
	return time.AfterFunc(wait, func() { q.OfferIfEmpty(req) })
}

type screenFrameRevisionLease interface {
	WithCurrentScreenFrameRevision(uint64, func() error) (bool, error)
}

func writeScreenFrameIfCurrent(lease screenFrameRevisionLease, revision uint64, write func() error) (bool, error) {
	return lease.WithCurrentScreenFrameRevision(revision, write)
}

type resyncMarkerDecision struct {
	send   bool
	action string
	gap    bool
}

// decideResyncMarker keeps the client-side terminal continuity honest.
// A changed epoch is never appendable, even when the new ring is still empty:
// the marker itself must reset the old xterm before any new-host bytes arrive.
// wsPayloadChunk — потолок одного бинарного сообщения для крупных записей
// писателя: начальный хвост (до clientReplayLimit, 512 КиБ) и досылка
// отставшему (до liveResyncLimit, 2 МиБ). У каждого куска свой дедлайн
// wsutil.WriteWait. Одним сообщением 2 МиБ на медленном плече не успевали
// уйти за 10 с: write-error рвал соединение, переподключение снова получало
// хвост одним сообщением, и медленный зритель ходил по кругу (ST-09 B1, замер
// TestSlowViewerResyncLive). Реле пересылает сообщение целиком с тем же
// дедлайном 10 с на запись — куски режут и его плечо. Клиент считает offset
// по длине бинарных кадров, поэтому нарезка для него прозрачна (I-04).
// 0 — одним сообщением (прежнее поведение; только для замера «до»).
var wsPayloadChunk = 256 << 10

// payloadChunks режет payload на куски не длиннее max; max <= 0 — целиком.
// Куски — срезы того же массива, без копирования.
func payloadChunks(payload []byte, max int) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	if max <= 0 || len(payload) <= max {
		return [][]byte{payload}
	}
	parts := make([][]byte, 0, (len(payload)+max-1)/max)
	for len(payload) > max {
		parts = append(parts, payload[:max])
		payload = payload[max:]
	}
	return append(parts, payload)
}

// writeBinaryChunks пишет payload бинарными сообщениями payloadChunks, у
// каждого свой дедлайн записи.
func writeBinaryChunks(conn *websocket.Conn, payload []byte, max int) error {
	for _, part := range payloadChunks(payload, max) {
		_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
		if err := conn.WriteMessage(websocket.BinaryMessage, part); err != nil {
			return err
		}
	}
	return nil
}

func decideResyncMarker(previousEpoch, currentEpoch string, payloadLen int, gap bool) resyncMarkerDecision {
	if currentEpoch != previousEpoch {
		return resyncMarkerDecision{send: true, action: "reset"}
	}
	if payloadLen == 0 {
		return resyncMarkerDecision{}
	}
	return resyncMarkerDecision{send: true, action: "resumed", gap: gap}
}

// ptyLiveSlot — живой PTY-WS в реестре Server.ptyLive. evicted выставляется
// вытеснителем ПЕРЕД Close, чтобы хендлер жертвы отчитался в connstat причиной
// "evicted:cap", а не безликой ошибкой чтения.
type ptyLiveSlot struct {
	conn    *websocket.Conn
	evicted atomic.Bool
}

// registerPtyConn ставит слот в реестр и возвращает вытесняемых (старейших
// сверх капа). Закрывает их вызывающий — вне ptyLiveMu.
func (s *Server) registerPtyConn(key string, slot *ptyLiveSlot) []*ptyLiveSlot {
	s.ptyLiveMu.Lock()
	defer s.ptyLiveMu.Unlock()
	live := append(s.ptyLive[key], slot)
	var evict []*ptyLiveSlot
	if n := len(live) - ptyLiveCap; n > 0 {
		evict, live = live[:n], live[n:]
	}
	s.ptyLive[key] = live
	return evict
}

// unregisterPtyConn убирает слот из реестра (idempotent; слот мог быть уже
// вытеснен из головы списка).
func (s *Server) unregisterPtyConn(key string, slot *ptyLiveSlot) {
	s.ptyLiveMu.Lock()
	defer s.ptyLiveMu.Unlock()
	live := s.ptyLive[key]
	for i, sl := range live {
		if sl == slot {
			live = append(live[:i], live[i+1:]...)
			break
		}
	}
	if len(live) == 0 {
		delete(s.ptyLive, key)
	} else {
		s.ptyLive[key] = live
	}
}

// broadcastAdapter exposes Server.Broadcast as a pty.Broadcaster — used by
// the PTY heuristic detector to deliver waiting/finished/error events into
// the per-user event ring buffer (so APK push and ?since= replay work).
type broadcastAdapter struct {
	s *Server
}

func (b broadcastAdapter) Broadcast(uid int64, event any) {
	if b.s == nil {
		return
	}
	b.s.Broadcast(uid, event)
}

// ── REST endpoints ───────────────────────────────────────────────

// POST /api/pty — create a new PTY session.
func (s *Server) apiPtyCreate(w http.ResponseWriter, r *http.Request, uid int64) {
	// Лимит терминалов — только для облачных подключений: дома без ограничений.
	if s.licenseManager != nil {
		limits := s.limitsFor(r)
		if limits.MaxPTYTerminals > 0 {
			active := len(s.ptyManager.List())
			if active >= limits.MaxPTYTerminals {
				log.Printf("[LICENSE] PTY limit reached (%d/%d, tier: %s)", active, limits.MaxPTYTerminals, s.licenseManager.GetTier())
				jsonErrorCodeAny(w, 403, "pty_limit", "Достигнут лимит терминалов. Закройте ненужный терминал и повторите.", map[string]any{
					"count": active, "limit": limits.MaxPTYTerminals,
				})
				return
			}
		}
	}

	var req struct {
		CWD   string `json:"cwd"`
		Shell string `json:"shell"`
		Cols  int    `json:"cols"`
		Rows  int    `json:"rows"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "bad request", 400)
		return
	}
	if req.Cols == 0 {
		req.Cols = 80
	}
	if req.Rows == 0 {
		req.Rows = 24
	}

	// Validate CWD against allowed roots to prevent directory traversal
	if req.CWD != "" {
		cleaned, err := validatePath(req.CWD)
		if err != nil {
			jsonErrorCode(w, http.StatusForbidden, "invalid_cwd", "Invalid working directory", nil)
			return
		}
		req.CWD = cleaned
	}

	// Empty / "." from client → start in the real user's home, not the
	// service's C:\Windows\System32.
	if req.CWD == "" || req.CWD == "." {
		if h := realUserHome(); h != "" && !isServiceProfilePath(h) {
			req.CWD = h
		}
	}

	sess, err := s.ptyManager.Create(uid, req.CWD, req.Shell, req.Cols, req.Rows)
	if err != nil {
		log.Printf("[PTY-API] create error: %v", err)
		if strings.Contains(err.Error(), "не найден на этом компьютере") {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonError(w, err.Error(), 500)
		return
	}
	// Папка попадает в историю «Недавних» ЗДЕСЬ, а не выводится из живых
	// сессий: закрытый терминал не должен стирать память о том, где работали
	// (жалоба владельца 01.09.2026 «недавнее не всегда недавние»).
	if s.recent != nil {
		s.recent.Note(uid, sess.CWD)
	}
	s.broadcastPtyListChanged(uid, "created", sess.ID)
	jsonResp(w, map[string]string{"id": sess.ID})
}

// broadcastPtyListChanged — список терминалов изменился (создан, закрыт,
// переименован): открытые экраны списка обновляются по событию, а не ждут
// очередного такта опроса. До 2.61.17 телефон опрашивал `/api/pty` каждые
// 6 с (боевой лог 01.09.2026: 4655 запросов за 8 часов через релей), потому
// что о создании и закрытии ему никто не сообщал — статусы агента уходили
// событием `pty_event`, а сама жизнь списка нет. Событие идёт тем же каналом,
// что и `pty_event`, поэтому старый клиент его просто не знает и опрашивает
// как раньше.
func (s *Server) broadcastPtyListChanged(uid int64, reason, id string) {
	s.Broadcast(uid, map[string]any{"type": "pty_list_changed", "reason": reason, "pty_id": id})
}

// GET /api/pty?sort=last_active&limit=N&alive=1 — list PTY sessions.
// Supports query params:
//   - sort=last_active|created (default: created desc)
//   - limit=N — cap result size
//   - alive=1 — only alive sessions (for "resume last terminal" tile)
func (s *Server) apiPtyList(w http.ResponseWriter, r *http.Request, uid int64) {
	list := s.ptyManager.List()

	if r.URL.Query().Get("alive") == "1" {
		filtered := make([]pty.SessionInfo, 0, len(list))
		for _, ses := range list {
			if ses.Alive {
				filtered = append(filtered, ses)
			}
		}
		list = filtered
	}

	switch r.URL.Query().Get("sort") {
	case "last_active":
		sortByLastActiveDesc(list)
	case "created":
		sortByCreatedDesc(list)
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		var lim int
		fmt.Sscanf(limitStr, "%d", &lim)
		if lim > 0 && lim < len(list) {
			list = list[:lim]
		}
	}

	// outcomes — короткий журнал исходов: чем закончились сессии, которых уже
	// нет (мёртвые удаляются через 5 минут) и в которых упала команда (статус
	// "error" гаснет тоже через 5 минут). Именно из него главная отвечает на
	// вопрос «что случилось без меня». Ключ присутствует всегда (пустой массив):
	// по его наличию клиент отличает нового агента от старого. Ни alive=1, ни
	// limit журнал не режут — он про то, чего в списке уже НЕТ.
	// lost — терминалы, не пережившие ПЕРЕЗАГРУЗКУ компьютера: pty-host умирает
	// вместе с системой, и раньше запись о таком терминале просто удалялась —
	// человек включал компьютер и видел пустой список. Теперь они ждут здесь и
	// продолжаются одним нажатием (POST /api/pty/{id}/restore). Ключ всегда
	// присутствует: по нему клиент отличает нового агента от старого.
	jsonResp(w, map[string]any{
		"sessions": labelDefaultAccounts(list),
		"folders":  s.ptyManager.MetaFolders(),
		"outcomes": s.ptyManager.Outcomes(),
		"lost":     s.ptyManager.Lost(),
	})
}

// defaultAccountName — подпись основного аккаунта в списке терминалов.
// У основного аккаунта Label пуст по конструкции (api_accounts.go), а человеку
// нужно отличать его от именованных: «дом» и «работа» — это два Codex.
const defaultAccountName = "Основной"

// labelDefaultAccounts подписывает сессии, запущенные на ОСНОВНОМ аккаунте, —
// но только если у этого агента аккаунтов несколько.
//
// Зачем: два терминала с одним и тем же Codex в списке были неразличимы.
// У именованного аккаунта метка есть, у основного она пустая, и карточки
// выглядели одинаково, хотя лимиты и история у аккаунтов разные.
//
// Почему только при нескольких: у того, кто аккаунтами не пользуется, подпись
// «Основной» на каждой карточке — чистый шум. Экран показывает ровно то, что
// у человека ЕСТЬ.
func labelDefaultAccounts(list []pty.SessionInfo) []pty.SessionInfo {
	f := loadAccountsFile()
	multi := map[string]bool{}
	for i := range list {
		s := &list[i]
		if s.AccountLabel != "" || s.AgentKind == "" || s.AgentKind == "shell" {
			continue
		}
		has, known := multi[s.AgentKind]
		if !known {
			has = len(accountsForAgent(f, s.AgentKind)) > 1
			multi[s.AgentKind] = has
		}
		if has {
			s.AccountLabel = defaultAccountName
		}
	}
	return list
}

// POST /api/pty/{id}/restore — продолжить работу в терминале, который не пережил
// перезагрузку компьютера. Поднимает процесс заново под ТЕМ ЖЕ id, в том же
// каталоге и с тем же именем; последние строки прошлой работы показываются как
// архив с чертой. Ни одна команда автоматически не повторяется — что запускать,
// решает человек.
func (s *Server) apiPtyRestore(w http.ResponseWriter, r *http.Request, uid int64) {
	if s.licenseManager != nil {
		limits := s.limitsFor(r)
		if limits.MaxPTYTerminals > 0 && len(s.ptyManager.List()) >= limits.MaxPTYTerminals {
			jsonErrorCodeAny(w, 403, "pty_limit", "Достигнут лимит терминалов. Закройте ненужный терминал и повторите.", map[string]any{
				"count": len(s.ptyManager.List()), "limit": limits.MaxPTYTerminals,
			})
			return
		}
	}
	var req struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	_ = readJSON(r, &req) // тело необязательно: размер терминала клиент выставит сам
	if req.Cols == 0 {
		req.Cols = 80
	}
	if req.Rows == 0 {
		req.Rows = 24
	}
	sess, err := s.ptyManager.Restore(r.PathValue("id"), req.Cols, req.Rows)
	if err != nil {
		log.Printf("[PTY-API] restore error: %v", err)
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResp(w, map[string]string{"id": sess.ID})
}

func sortByLastActiveDesc(list []pty.SessionInfo) {
	sortSlice(list, func(a, b pty.SessionInfo) bool { return a.LastActive > b.LastActive })
}
func sortByCreatedDesc(list []pty.SessionInfo) {
	sortSlice(list, func(a, b pty.SessionInfo) bool { return a.Created > b.Created })
}
func sortSlice[T any](list []T, less func(a, b T) bool) {
	// Insertion sort — list is small (< 50 typical).
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && less(list[j], list[j-1]); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// DELETE /api/pty/dead — close all dead PTY sessions.
func (s *Server) apiPtyCloseDead(w http.ResponseWriter, r *http.Request, uid int64) {
	n := s.ptyManager.CloseDead()
	jsonResp(w, map[string]int{"closed": n})
}

// POST /api/pty/{id}/reattach — вернуть связь с живым процессом терминала.
//
// Терминал числится завершённым, но его процесс жив: оборвался только канал до
// него (см. internal/pty/reattach_live.go). Тогда «перезапустить» — неверное
// действие: получится второй терминал в той же папке, пока первый продолжает
// работать. Здесь мы подключаемся к тому же процессу под тем же id, и человек
// возвращается ровно к своей работе.
func (s *Server) apiPtyReattach(w http.ResponseWriter, r *http.Request, uid int64) {
	sess, err := s.ptyManager.ReattachSession(r.PathValue("id"))
	if err != nil {
		log.Printf("[PTY-API] reattach error: %v", err)
		// «Занят» — это НЕ «не найден». 404 здесь превращался у человека в тост
		// «Не найдено.» (безкодовая ветка 404 в packages/shared/src/api-core.ts)
		// на терминале, который жив и работает: боевой случай 17.08.2026,
		// 11:58:07. 409 + host_busy говорят правду и предлагают повторить.
		if pty.IsHostBusy(err) {
			jsonErrorCode(w, http.StatusConflict, "host_busy", err.Error(), nil)
			return
		}
		jsonErrorCode(w, http.StatusNotFound, "host_gone", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]string{"id": sess.ID})
}

// DELETE /api/pty/{id} — close a PTY session.
func (s *Server) apiPtyClose(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	if err := s.ptyManager.Close(id); err != nil {
		// Живой сессии нет — возможно, это карточка терминала, ждущего
		// восстановления после перезагрузки. «Убрать» на ней — та же кнопка
		// закрытия, и вести она обязана туда же, а не в «Терминал не найден».
		if s.ptyManager.ForgetLost(id) {
			s.broadcastPtyListChanged(uid, "closed", id)
			jsonResp(w, map[string]bool{"ok": true})
			return
		}
		jsonError(w, err.Error(), 404)
		return
	}
	s.broadcastPtyListChanged(uid, "closed", id)
	jsonResp(w, map[string]bool{"ok": true})
}

// GET /api/pty/{id}/cwd — return the live working directory of a PTY session.
// Kept as a deprecated alias for /state — old clients still poll this.
func (s *Server) apiPtyCWD(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	sess := s.ptyManager.Get(id)
	if sess == nil {
		jsonError(w, "session not found", 404)
		return
	}
	jsonResp(w, map[string]any{
		"cwd":   sess.CurrentCWD(),
		"alive": sess.IsAlive(),
	})
}

// GET /api/pty/{id}/state — full session state (cwd + foreground process).
func (s *Server) apiPtyState(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	sess := s.ptyManager.Get(id)
	if sess == nil {
		jsonError(w, "session not found", 404)
		return
	}

	resp := map[string]any{
		"cwd":   sess.CurrentCWD(),
		"alive": sess.IsAlive(),
		"name":  s.ptyManager.MetaName(id), // empty if no rename set
		"shell": sess.Shell,                // "ssh" у SSH-сессии — экран это учитывает
	}

	// ЛОГИЧЕСКАЯ геометрия терминала — размер, применённый к PTY. Экрану она
	// нужна отдельно от его собственной видимой области: при поднятой
	// клавиатуре видно 9–11 строк, а терминал остаётся тридцатистрочным, и
	// клиенту, открытому уже с клавиатурой, эту высоту взять больше неоткуда.
	// Без неё он ужимал себя до видимой и складывал кадр в последнюю строку.
	if c, r := sess.AppliedSize(); c > 0 && r > 0 {
		resp["cols"] = c
		resp["rows"] = r
	}

	// Статус того же расчёта, что в списке терминалов: без него экран терминала
	// не знал, что агент ждёт ответа, и не мог показать ни вопрос, ни кнопки.
	// hint_kind присутствует всегда (пустой = тип не распознан) — по наличию
	// ключа клиент понимает, что агент новый.
	if info, ok := s.ptyManager.Info(id); ok {
		resp["status"] = info.Status
		resp["status_at"] = info.StatusAt
		resp["hint"] = info.Hint
		resp["hint_kind"] = info.HintKind
		// hint_options — подписи пунктов меню («Yes» / «Yes, and don't ask
		// again» / «No»): экран ставит их на кнопки вместо голых цифр.
		if len(info.HintOptions) > 0 {
			resp["hint_options"] = info.HintOptions
		}
		// viewers — сколько экранов смотрит в этот терминал. Экран показывает
		// это в шапке: иначе перерисовка TUI под чужую ширину выглядит поломкой.
		resp["viewers"] = info.Viewers
		// sleep — агент усыплён человеком: экран показывает «спит» и «Разбудить».
		if info.Sleep != nil {
			resp["sleep"] = info.Sleep
		}
		// kind/ssh_*: экран должен знать, что он в SSH-сессии — иначе «Открыть
		// папку» и «Открыть на ПК» действуют на бастион, а не на сервер.
		resp["kind"] = info.Kind
		if info.Kind == "ssh" {
			resp["ssh_host"] = info.SSHHost
			resp["ssh_host_id"] = info.SSHHostID
			resp["ssh_user"] = info.SSHUser
			resp["ssh_port"] = info.SSHPort
			resp["ssh_proxy_jump"] = info.SSHProxyJump
			resp["remote"] = true
		}
	}

	if sess.IsAlive() {
		fg := sess.ForegroundProcess()
		// fg.Name is empty when only the shell itself is running.
		fgName := fg.Name
		if fgName == "" {
			fgName = baseName(sess.Shell)
		}
		resp["fg_process"] = fgName
		resp["fg_pid"] = fg.PID
		// fg_started — поколение переднего процесса (ST-02): время его создания,
		// unix мс. PID без поколения не является достаточным ключом: номер
		// переиспользуется, и вердикт, измеренный у вышедшего агента, переезжал
		// на новый процесс с тем же PID. Нет — поля нет (старый агент так же).
		if fg.StartMs > 0 {
			resp["fg_started"] = fg.StartMs
		}
		agentKind := ""
		if info, ok := s.ptyManager.Info(id); ok && info.AgentKind != "" {
			agentKind = info.AgentKind
		} else {
			agentKind = pty.AgentKind(fgName)
		}
		resp["agent_kind"] = agentKind
		// Направление прокрутки — свойство агента из канонического реестра, а не
		// клиентский список известных id. Старый/неизвестный агент поля не имеет
		// и остаётся на безопасном «Авто».
		addAgentScrollModeDefault(resp, agentKind)
		addAgentHistoryRetention(resp, agentKind)
	}

	jsonResp(w, resp)
}

func addAgentScrollModeDefault(resp map[string]any, agentKind string) {
	if desc := agents.GetDescriptor(agentKind); desc != nil {
		resp["scroll_mode_default"] = desc.ScrollModeDefault()
	}
}

// addAgentHistoryRetention — политика хранения истории профиля (ST-04) рядом с
// направлением прокрутки, но НЕЗАВИСИМО от него. Приходит на каждый терминал и
// не зависит от кэша реестра на клиенте (его наполняют только списки агентов и
// терминалов). Поле есть, только если реестр его объявил по доказательству;
// у старого и неизвестного агента его нет — клиент берёт своё по умолчанию.
func addAgentHistoryRetention(resp map[string]any, agentKind string) {
	if desc := agents.GetDescriptor(agentKind); desc != nil && desc.HistoryRetention != "" {
		resp["history_retention"] = desc.HistoryRetention
	}
}

// baseName extracts the executable name from a shell path (without extension).
func baseName(p string) string {
	name := filepath.Base(p)
	if i := lastIndex(name, '.'); i > 0 {
		name = name[:i]
	}
	return name
}

// POST /api/pty/{id}/handoff — open a desktop terminal on the host PC at
// the same CWD as this PTY session. Body: { "command": "optional inline cmd" }.
func (s *Server) apiPtyHandoff(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	sess := s.ptyManager.Get(id)
	if sess == nil {
		jsonError(w, "session not found", 404)
		return
	}

	var req struct {
		Command string `json:"command"`
	}
	if r.ContentLength > 0 {
		_ = readJSON(r, &req)
	}

	cwd := sess.CurrentCWD()
	if cwd == "" {
		cwd = sess.CWD
	}

	// Без явной команды терминал на ПК открывается уже ПОДКЛЮЧЁННЫМ к этой
	// же сессии (remotai attach <id>) — продолжение работы бесшовное, после
	// Ctrl+] остаётся обычный shell в той же папке.
	if req.Command == "" {
		if exe, err := os.Executable(); err == nil {
			req.Command = fmt.Sprintf("& '%s' attach %s", strings.ReplaceAll(wincli.Executable(exe), "'", "''"), id)
		}
	}

	if err := pty.OpenOnHost(cwd, req.Command); err != nil {
		log.Printf("[PTY-API] handoff failed: %v", err)
		if strings.Contains(strings.ToLower(err.Error()), "terminal emulator") {
			jsonErrorCode(w, http.StatusServiceUnavailable, "no_terminal_emulator",
				"no supported terminal emulator found", nil)
			return
		}
		jsonError(w, err.Error(), 500)
		return
	}

	log.Printf("[PTY-API] handoff opened: id=%s cwd=%s cmd=%q", id, cwd, req.Command)
	jsonResp(w, map[string]bool{"ok": true})
}

// ── POST /api/pty/{id}/input — ответить агенту, не открывая терминал ──

// ptyInputMaxBytes — это канал ОТВЕТА агенту, а не канал передачи данных.
// Больше — только через WS (paste) или /api/pty/upload.
const ptyInputMaxBytes = 4096

// ptyKeyBytes — байты быстрых ответов. Значения ОБЯЗАНЫ совпадать с sendRaw(…)
// в apk/src/pages/PtyTermView.tsx (ряд клавиш терминала): один и тот же вопрос
// агенту должен отвечаться одинаково с экрана терминала, с карточки главной,
// из уведомления и из inline-кнопки бота. Меняешь здесь — меняй там и в
// pty_input_test.go (он дублирует таблицу явно, чтобы правка в одном месте не
// уехала молча).
var ptyKeyBytes = map[string]string{
	"enter":  "\r",     // PtyTermView: голый Enter (выбор пункта меню)
	"y":      "y\r",    // y/n отвечаются С Enter
	"n":      "n\r",    //
	"1":      "1",      // цифры нумерованного меню — БЕЗ Enter
	"2":      "2",      //
	"3":      "3",      //
	"esc":    "\x1b",   //
	"tab":    "\t",     //
	"up":     "\x1b[A", // стрелки — CSI
	"down":   "\x1b[B", //
	"left":   "\x1b[D", //
	"right":  "\x1b[C", //
	"ctrl-c": "\x03",   // прервать работу агента
	"ctrl-d": "\x04",   //
	"ctrl-z": "\x1a",   //
}

// ptyKeyList — отсортированный список принимаемых клавиш для текста ошибки 400.
// Вызывающий (бот, скрипт) должен видеть, что вообще принимается, а не гадать.
func ptyKeyList() string {
	keys := make([]string, 0, len(ptyKeyBytes))
	for k := range ptyKeyBytes {
		keys = append(keys, k)
	}
	sortSlice(keys, func(a, b string) bool { return a < b })
	return strings.Join(keys, ",")
}

// writePtyInput пишет ввод в PTY тем же способом, что ветка "paste"
// WS-обработчика: чанками по 512 байт с паузой 10 мс между ними (ConPTY при
// заливке одним куском теряет данные) и не разрывая UTF-8-последовательности.
// Принимает io.Writer, а не *pty.Session, чтобы чанкование проверялось тестом
// без живого ConPTY.
func writePtyInput(w io.Writer, b []byte) error {
	if session, ok := w.(interface{ WritePaste([]byte) error }); ok {
		return session.WritePaste(b)
	}
	return pty.WriteInputChunks(w, b)
}

// POST /api/pty/{id}/input — ответить агенту в терминале, НЕ открывая экран
// терминала. Второй (после /ws/pty/{id}) путь записи в PTY: на нём стоят
// кнопки быстрого ответа на карточке «Требует внимания», действия уведомления
// и inline-кнопки Telegram-бота — релей проксирует его как обычный
// client-request, отдельной ветки в релее не нужно.
//
// Тело (ровно одно из двух):
//
//	{ "key": "enter" | "y" | "n" | "1" | "2" | "3" | "esc" | "tab" |
//	         "up" | "down" | "left" | "right" | "ctrl-c" | "ctrl-d" | "ctrl-z" }
//	{ "data": "текст ответа", "enter": true }
//
// Необязательно: { "expect_status_at": <unix ms> } — status_at вопроса, который
// пользователь ВИДЕЛ. Если агент за это время спросил о другом, ответ не
// применяется (409 prompt_changed) — иначе «Да» из ночного уведомления
// подтвердило бы совсем не то действие. Эпизод ожидания ключуется вопросом И
// отпечатком видимого хвоста (waitEpisodeKey в pty/events.go), поэтому два
// подряд ОДИНАКОВЫХ по типу вопроса («Allow tool …?» про разные инструменты)
// дают разный status_at и старая кнопка честно получает 409. Остаточное окно —
// такт детектора (≤2 с между появлением нового вопроса и сменой штампа).
//
// Успех: { "ok": true, "bytes": N } — вызывающий видит, что ввод доставлен.
// Отказы (все с машинным code, чтобы клиент ветвился по коду, а не по тексту):
// 404 pty_not_found · 410 pty_dead · 400 bad_request/unknown_key/bad_encoding ·
// 413 too_large · 409 prompt_changed · 429 rate_limited · 500 write_failed.
//
// Владение сессией здесь, как и во всех PTY-хендлерах, не проверяется: доступ
// к ПК даётся целиком (multi-grant), а тот же токен уже позволяет писать в
// терминал через /ws/pty/{id}. Новых прав эндпоинт не даёт — новое только то,
// что пишут, НЕ видя экрана. Отсюда компенсации: allowlist клавиш (произвольную
// escape-последовательность через key не отправить), лимит на data, Enter
// только по явному флагу, expect_status_at и лог БЕЗ содержимого ввода — в
// терминал вводят пароли и токены.
// apiPtyResize задаёт размер терминала БЕЗ подключения по WebSocket.
//
// ЗАЧЕМ. Размер приходил только от живого зрителя (ctrl «resize» в /ws/pty), и
// у терминала, открытого через API, он оставался тем, что задали при создании.
// Для сценария «агент работает в чужом терминале через REST» это тупик: ни
// подогнать ширину под вывод, ни исправить размер после того, как зритель ушёл
// и оставил свои 48 колонок. Живой мак 10.08.2026: POST /api/pty/{id}/resize
// отвечал 404, а зрителей у сессии не было вовсе.
func (s *Server) apiPtyResize(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	sess := s.ptyManager.Get(id)
	if sess == nil {
		jsonErrorCode(w, 404, "pty_not_found", "session not found", nil)
		return
	}
	if !sess.IsAlive() {
		jsonErrorCode(w, 410, "pty_dead", "session is not running", nil)
		return
	}
	var req struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	if req.Cols <= 0 || req.Rows <= 0 {
		jsonErrorCode(w, 400, "bad_request", "cols and rows must be positive", nil)
		return
	}
	if err := sess.Resize(req.Cols, req.Rows); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	log.Printf("[PTY-API] resize id=%s %dx%d", id, req.Cols, req.Rows)
	jsonResp(w, map[string]any{"ok": true, "cols": req.Cols, "rows": req.Rows})
}

func (s *Server) apiPtyInput(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	sess := s.ptyManager.Get(id)
	if sess == nil {
		jsonErrorCode(w, 404, "pty_not_found", "session not found", nil)
		return
	}
	// Мёртвая сессия остаётся в реестре до Close/CloseDead — «нет сессии» и
	// «сессия закрылась» это разные ответы, иначе UI врёт про причину.
	if !sess.IsAlive() {
		jsonErrorCode(w, 410, "pty_dead", "session is not running", nil)
		return
	}

	var req struct {
		Key            string `json:"key"`
		Data           string `json:"data"`
		Enter          bool   `json:"enter"`
		ExpectStatusAt int64  `json:"expect_status_at"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}

	// Ровно одно из key/data: «и то, и другое» — почти всегда ошибка вызывающего,
	// а угадывать порядок применения нельзя.
	key := strings.ToLower(strings.TrimSpace(req.Key))
	if (key == "") == (req.Data == "") {
		jsonErrorCode(w, 400, "bad_request",
			`exactly one of key/data required (bare Enter is key:"enter")`, nil)
		return
	}

	var payload string
	if key != "" {
		b, ok := ptyKeyBytes[key]
		if !ok {
			jsonErrorCode(w, 400, "unknown_key", "unknown key", map[string]string{"keys": ptyKeyList()})
			return
		}
		payload = b
	} else {
		if len(req.Data) > ptyInputMaxBytes {
			jsonErrorCode(w, 413, "too_large", "input too large", nil)
			return
		}
		// Битый UTF-8 в терминале превращается в мусор и может испортить строку
		// ввода агента — лучше честный отказ.
		if !utf8.ValidString(req.Data) {
			jsonErrorCode(w, 400, "bad_encoding", "data must be valid UTF-8", nil)
			return
		}
		payload = req.Data
		if req.Enter { // как строка ввода терминала: sendRaw(inputText + "\r")
			payload += "\r"
		}
	}

	// Guard «отвечаю на тот вопрос, который видел»: status_at — начало эпизода
	// ожидания, при новом вопросе он другой.
	if req.ExpectStatusAt != 0 {
		info, ok := s.ptyManager.Info(id)
		if !ok || info.StatusAt != req.ExpectStatusAt {
			// hint/hint_kind — фиксированные строки из таблицы шаблонов
			// (pty/events.go), содержимого терминала в них нет: вызывающий может
			// показать «агент теперь спрашивает о другом» с новым вопросом.
			jsonErrorCode(w, 409, "prompt_changed", "the agent is asking about something else now",
				map[string]string{"hint": info.Hint, "hint_kind": info.HintKind})
			return
		}
	}

	if err := writePtyInput(sess, []byte(payload)); err != nil {
		// Сессия могла умереть между IsAlive() и записью — это 410, а не 500:
		// иначе пользователь видит «Ошибка сервера» вместо «терминал закрылся».
		if !sess.IsAlive() {
			jsonErrorCode(w, 410, "pty_dead", "session is not running", nil)
			return
		}
		log.Printf("[PTY-API] input write failed id=%s: %v", id, err)
		jsonErrorCode(w, 500, "write_failed", err.Error(), nil)
		return
	}

	// СОДЕРЖИМОЕ data не логируем: в терминал вводят пароли и токены.
	log.Printf("[PTY-API] input id=%s uid=%d key=%q bytes=%d", id, uid, key, len(payload))
	jsonResp(w, map[string]any{"ok": true, "bytes": len(payload)})
}

// ansiStripRe matches CSI / OSC escape sequences for export cleanup.
var ansiStripRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z@-~]|\x1b\][^\x07]*\x07|\x1b[=>78]|\r`)

// GET /api/pty/{id}/export?format=txt|md — download scrollback as a file,
// stripped of ANSI escape codes.
func (s *Server) apiPtyExport(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	sess := s.ptyManager.Get(id)
	if sess == nil {
		jsonError(w, "session not found", 404)
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "txt"
	}

	clean := ansiStripRe.ReplaceAll(sess.Scrollback(), nil)

	name := s.ptyManager.MetaName(id)
	if name == "" {
		name = baseName(sess.Shell)
	}
	cwd := sess.CurrentCWD()
	if cwd == "" {
		cwd = sess.CWD
	}

	var body []byte
	var ext, mime string
	switch format {
	case "md":
		header := fmt.Sprintf("# Terminal session — %s\n\n- **Shell:** `%s`\n- **CWD:** `%s`\n- **Exported:** %s\n\n```\n",
			name, sess.Shell, cwd, time.Now().Format(time.RFC3339))
		body = append([]byte(header), clean...)
		body = append(body, []byte("\n```\n")...)
		ext, mime = "md", "text/markdown; charset=utf-8"
	default:
		header := fmt.Sprintf("Terminal session — %s\nShell: %s\nCWD: %s\nExported: %s\n%s\n",
			name, sess.Shell, cwd, time.Now().Format(time.RFC3339),
			"================================================")
		body = append([]byte(header), clean...)
		ext, mime = "txt", "text/plain; charset=utf-8"
	}

	filename := fmt.Sprintf("pty-%s-%s.%s", safeFilename(name), time.Now().Format("20060102-150405"), ext)
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.Write(body)
}

// safeFilename strips characters that don't belong in a download filename.
func safeFilename(s string) string {
	if s == "" {
		return "session"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
			out = append(out, c)
		case c == ' ':
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "session"
	}
	return string(out)
}

// safeUploadFilename keeps a readable Unicode filename while removing path
// components, controls and characters forbidden by Windows. PTY uploads land
// in the agent's temp directory, but the original multipart name is still
// untrusted and may come from another OS.
func safeUploadFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	runes := []rune(name)
	if len(runes) > 120 {
		name = string(runes[:120])
	}
	if name == "" || name == "." || name == ".." {
		return "upload"
	}
	return name
}

// ptyNameMaxRunes — предел длины имени терминала в СИМВОЛАХ, не байтах.
const ptyNameMaxRunes = 80

// sanitizePtyName чистит пользовательское имя перед записью в pty.json:
// выкидывает управляющие символы (переводы строк, ANSI-escape) и битые байты,
// затем режет по рунам. Обрезка по байтам рвала кириллицу посреди UTF-8.
func sanitizePtyName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	cleaned = strings.TrimSpace(cleaned)
	if utf8.RuneCountInString(cleaned) > ptyNameMaxRunes {
		cleaned = strings.TrimSpace(string([]rune(cleaned)[:ptyNameMaxRunes]))
	}
	return cleaned
}

// PATCH /api/pty/{id} — rename a PTY session. Body: { "name": "..." }.
func (s *Server) apiPtyRename(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	sess := s.ptyManager.Get(id)
	if sess == nil {
		jsonError(w, "session not found", 404)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "bad request", 400)
		return
	}
	name := sanitizePtyName(req.Name)
	if err := s.ptyManager.SetMetaName(id, name); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	s.broadcastPtyListChanged(uid, "renamed", id)
	jsonResp(w, map[string]string{"name": name})
}

// PATCH /api/pty/meta — bulk-update folder (group) and manual sort order of
// PTY sessions. Body: { "items": [{ "id": "...", "group": "...", "sort": 10 }] }.
// Used by the terminal list UI after drag-and-drop, folder moves and folder
// renames — one request instead of a call per session.
func (s *Server) apiPtySetMeta(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		Items []struct {
			ID    string  `json:"id"`
			Group string  `json:"group"`
			Sort  float64 `json:"sort"`
		} `json:"items"`
		Folders *[]string `json:"folders"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "bad request", 400)
		return
	}
	if len(req.Items) == 0 && req.Folders == nil {
		jsonError(w, "items or folders required", 400)
		return
	}
	for _, it := range req.Items {
		if it.ID == "" {
			jsonError(w, "id required", 400)
			return
		}
		if s.ptyManager.Get(it.ID) == nil {
			jsonError(w, "session not found: "+it.ID, 404)
			return
		}
	}
	for _, it := range req.Items {
		if err := s.ptyManager.SetMetaPlacement(it.ID, sanitizePtyName(it.Group), it.Sort); err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
	}
	if req.Folders != nil {
		if len(*req.Folders) > 100 {
			jsonError(w, "too many folders", 400)
			return
		}
		folders := make([]string, 0, len(*req.Folders))
		seen := make(map[string]bool)
		for _, raw := range *req.Folders {
			name := sanitizePtyName(raw)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			folders = append(folders, name)
		}
		if err := s.ptyManager.SetMetaFolders(folders); err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
	}
	jsonResp(w, map[string]bool{"ok": true})
}

func lastIndex(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// uploadDestDir — куда падают загрузки из приложения. Раньше это был
// os.TempDir(): файл «где-то там», а чистильщики TEMP его подтирали до того,
// как человек добрался. Теперь — видимая папка ~/Remotai/files (создаётся
// bundle.Ensure при старте; на всякий случай досоздаём здесь), TEMP — только
// запасной выход, если домашняя директория недоступна.
func uploadDestDir() string {
	if dir := bundle.FilesDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			return dir
		}
	}
	return os.TempDir()
}

// POST /api/pty/upload — upload any file (image, document, archive, …) to
// ~/Remotai/files (см. uploadDestDir) and return its path (typed into the
// terminal by the client).
// Большие файлы приходят чанками (upload_id/chunk/chunks, см. upload_chunks.go).
func (s *Server) apiPtyUpload(w http.ResponseWriter, r *http.Request, uid int64) {
	cp, chunked, err := parseChunkParams(r.URL.Query())
	if err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	if err := r.ParseMultipartForm(20 << 20); err != nil { // 20 MB max in RAM, дальше диск
		jsonError(w, "invalid upload", 400)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "file required", 400)
		return
	}
	defer file.Close()

	// Build unique filename: pty-upload-<timestamp>-<original>
	safeName := safeUploadFilename(header.Filename)

	if chunked {
		part, final, err := appendChunk(cp, file)
		if err != nil {
			jsonError(w, "failed to save chunk", 500)
			return
		}
		if !final {
			jsonResp(w, map[string]any{"ok": true, "chunk_ack": true})
			return
		}
		dst := filepath.Join(uploadDestDir(), fmt.Sprintf("pty-upload-%d-%s", time.Now().UnixMilli(), safeName))
		// overwrite=true: имя уникально по метке времени, конфликта имён здесь
		// не бывает (защита от затирания нужна файловому менеджеру — N107).
		if err := finishChunkUpload(part, dst, true); err != nil {
			jsonError(w, "failed to assemble file", 500)
			return
		}
		log.Printf("[PTY-API] uploaded chunked file (%d chunks): %s", cp.total, dst)
		jsonResp(w, map[string]any{"path": dst, "chunk_ack": true})
		return
	}

	name := fmt.Sprintf("pty-upload-%d-%s", time.Now().UnixMilli(), safeName)
	dst := filepath.Join(uploadDestDir(), name)

	out, err := os.Create(dst)
	if err != nil {
		jsonError(w, "failed to create file", 500)
		return
	}
	defer out.Close()

	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.Remove(dst) // don't leave a half-written file behind
		jsonError(w, "failed to save file", 500)
		return
	}
	log.Printf("[PTY-API] uploaded file: %s", dst)
	jsonResp(w, map[string]string{"path": dst})
}

// ── WebSocket /ws/pty/{id} — bidirectional terminal I/O ─────────

func (s *Server) wsPtyHandler(w http.ResponseWriter, r *http.Request) {
	// Auth — uses shared authenticateWS helper.
	initData := r.URL.Query().Get("initData")
	uid := s.authenticateWS(initData)
	if uid == 0 {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if len(s.allowed) > 0 && uid != -1 && !s.allowed[uid] {
		http.Error(w, "Forbidden", 403)
		return
	}

	id := r.PathValue("id")
	sess := s.ptyManager.Get(id)
	if sess == nil {
		http.Error(w, "session not found", 404)
		return
	}

	serverSession := s.isServerSession(id)
	if serverSession && !s.requireServerAccess(w, r) {
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	if serverSession {
		accessCtx, cancelAccess := context.WithCancel(r.Context())
		defer cancelAccess()
		go s.watchServerSocket(accessCtx, conn, 30*time.Second)
	}

	// Allow large paste messages (default is 32KB which truncates big pastes).
	conn.SetReadLimit(1 << 20) // 1 MB

	// Keepalive: server pings + read deadline so an idle terminal is not silently
	// dropped by the Cloudflare tunnel / mobile NAT (the root cause of the PTY
	// "reconnect every 2-4s" storm).
	ka := wsutil.Start(conn)
	defer ka.Stop()

	log.Printf("[PTY-WS] connected uid=%d id=%s", uid, id)

	// Кап сокетов на (uid, session): регистрируем себя, старейших сверх капа
	// вытесняем. Их ReadMessage упадёт на закрытом conn, а флаг evicted даст
	// хендлеру-жертве правильную причину закрытия.
	liveKey := fmt.Sprintf("%d|%s", uid, id)
	slot := &ptyLiveSlot{conn: conn}
	for _, old := range s.registerPtyConn(liveKey, slot) {
		old.evicted.Store(true)
		_ = old.conn.Close()
		log.Printf("[PTY-WS] evicted oldest uid=%d id=%s (over cap %d)", uid, id, ptyLiveCap)
	}
	defer s.unregisterPtyConn(liveKey, slot)

	// Учёт стабильности соединения — фиксируем открытие и (с причиной) закрытие.
	cs := connstat.Default.Open(connstat.KindPTY, id, uid)
	closeReason := "client-disconnect"
	defer func() {
		if slot.evicted.Load() {
			closeReason = "evicted:cap"
		}
		log.Printf("[PTY-WS] closed uid=%d id=%s reason=%s", uid, id, closeReason)
		cs.Close(closeReason)
	}()

	// Subscribe to PTY output with offset+epoch resume (SOTA resumable subscription,
	// cf. Centrifugo/Ably). The client sends ?resume=<epoch>:<offset> on reconnect;
	// if the epoch matches and the offset is still in the ring buffer we replay only
	// the tail (marker "resumed", client appends), otherwise we send the whole buffer
	// (marker "reset", client does a clean full redraw). This kills both the
	// scrollback-duplication-on-reconnect bug AND the wasteful full re-send: a
	// reconnect with no new output transfers nothing and redraws nothing. The marker
	// carries the BASE offset (start of payload); the client sets its offset to it,
	// then increments by the byte length of every binary frame (replay + live alike).
	// Old clients ignore the unknown JSON and behave as before — safe either side.
	resumeEpoch, resumeOffset := parseResume(r.URL.Query().Get("resume"))
	ch, payload, offset, epoch, isDelta, gap := sess.SubscribeResume(resumeEpoch, resumeOffset)
	defer sess.Unsubscribe(ch)

	// Этот экран как ЗРИТЕЛЬ терминала: его размер хранится отдельно от чужих
	// (resize применяется по самому узкому), а число зрителей уезжает в
	// SessionInfo — интерфейс наконец может сказать «терминал открыт ещё где-то»
	// вместо необъяснимой перерисовки TUI под чужую ширину.
	viewer := sess.AddViewer()
	sizeControls, sizeControlsSupported := any(sess).(interface {
		EnableSizeControls(*pty.Viewer)
		ChangeSizeControl(*pty.Viewer, string, string, uint64)
		SizeControls(*pty.Viewer) (pty.SizeControlState, bool)
	})
	defer sess.RemoveViewer(viewer)

	action := "reset"
	if isDelta {
		action = "resumed"
	}
	// offset соответствующий началу payload. Шкала — общий контракт offset
	// (см. marshalScreenFrame): число байт исходного потока эпохи, уже учтённых,
	// то есть 0-based позиция следующего байта; маркеры, кадры, история, modes и
	// строка о пропуске в неё не входят.
	base := offset - uint64(len(payload))
	// На полном reset до-сылаем активные DEC-режимы (alt-screen/mouse/bracketed-
	// paste): их включающая последовательность обычно уже вытеснена из 512 КБ
	// буфера, и без этого клиент после term.reset() остаётся в обычном буфере без
	// mouse-tracking → прокрутка full-screen TUI (Claude Code) молча не идёт.
	// json.Marshal экранирует ESC как ; старый клиент игнорирует поле modes.
	markerObj := struct {
		T      string `json:"t"`
		Epoch  string `json:"epoch"`
		Offset uint64 `json:"offset"`
		Modes  string `json:"modes,omitempty"`
		Gap    bool   `json:"gap,omitempty"` // середина потока потеряна — клиент дописывает с пометкой
		HB     int    `json:"hb,omitempty"`  // период heartbeat, сек (см. ptyHeartbeatInterval)
		// Screen — САМОДОСТАТОЧНЫЙ КАДР ЭКРАНА вместо хвоста сырого потока.
		// Едет полем текстового маркера (как modes) намеренно: клиент считает
		// свою позицию по длине БИНАРНЫХ кадров, и синтетический кадр в
		// бинарном канале разъехался бы с серверным счётчиком — каждое
		// следующее открытие приходило бы полным reset, то есть мы бы своими
		// руками получили вечные 512 КБ.
		Screen     string `json:"screen,omitempty"`
		ScreenCols int    `json:"screen_cols,omitempty"`
		ScreenRows int    `json:"screen_rows,omitempty"`
	}{T: action, Epoch: epoch, Offset: base, Gap: gap, HB: int(ptyHeartbeatInterval / time.Second)}

	// ⚠ КАДР ЭКРАНА ЗДЕСЬ НЕ ОТДАЁТСЯ. Клиент просит его отдельным сообщением
	// уже ВНУТРИ соединения (ctrl `screen` ниже), и вот почему.
	//
	// Сначала умение объявлялось флагом в адресе (`?screen=1`). Живой отказ
	// 12.08.2026: релей проверяет параметры пути по закрытому белому списку
	// (streamQueryAllowed, ws_stream.go) и на незнакомый ответил
	// «400 invalid stream path» за 2 мс. Телефон перестал открывать терминалы
	// вообще — 35 отказов подряд, до агента соединение не доходило. На
	// localhost этого не видно: там релея нет.
	//
	// Внутри соединения нет ни релея, ни согласования версий: старый агент
	// неизвестный ctrl молча игнорирует (switch без default), старый клиент
	// ничего не просит. Любая пара версий работает.
	switch {
	case gap:
		// При gap-резюме нужна ПОЛНАЯ синхронизация режимов (SET+RESET):
		// в потерянной середине потока они могли переключиться в любую сторону.
		markerObj.Modes = sess.ModeSyncSeq()
	case !isDelta:
		markerObj.Modes = sess.ModeReassertSeq()
	}
	marker, merr := json.Marshal(markerObj)
	if merr != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
	if err := conn.WriteMessage(websocket.TextMessage, marker); err != nil {
		return
	}
	if len(payload) > 0 {
		// Начальный хвост до 512 КиБ — кусками wsPayloadChunk (ST-09 B1).
		if err := writeBinaryChunks(conn, payload, wsPayloadChunk); err != nil {
			return
		}
	}

	// Read from WebSocket → write to PTY (runs until conn closes). readReason is
	// set by the reader before it closes closeCh (happens-before the main loop's
	// receive), so the main loop can attribute the disconnect cause precisely.
	closeCh := make(chan struct{})
	var readReason string
	// Запрос кадра экрана от клиента. Писать в сокет из читающей горутины
	// нельзя (единственный писатель — основной цикл ниже), поэтому просьба
	// едет latest-only очередью: новый geometry revision обязан заменить ещё
	// не обслуженный старый, иначе клиент гарантированно отвергнет ответ.
	screenReq := newScreenRequestQueue()
	historyCtx, cancelHistory := context.WithCancel(r.Context())
	defer cancelHistory()
	history := newHistoryChannel(historyCtx, sess)
	var historyEnabled bool // reader goroutine only
	historyCapabilities := make(chan struct{}, 1)
	// screen-request-v1 (ST-05). Флаг ставит читающая горутина ДО того, как
	// отдаст в очередь следующие запросы того же клиента, поэтому основной
	// цикл, разбирая запрос, уже видит способность — даже если select выберет
	// очередь раньше канала подтверждения. screen-capability пишет только
	// основной цикл (единственный писатель сокета).
	screenNeg := &screenPeer{}
	screenCapabilities := make(chan struct{}, 1)
	// Отказы по запросам без корректного geom_rev. Писать из читающей горутины
	// нельзя, поэтому ответ едет каналом; буфер 1 и неблокирующая отправка —
	// поток мусорных запросов ничего не копит.
	screenRejects := make(chan screenFrameRequest, 1)
	go func() {
		defer observability.RecoverPanic("pty-ws-read")
		defer close(closeCh)
		defer cancelHistory()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				readReason = classifyWSError(err)
				return
			}
			ka.Touch() // active client → keep the read deadline fresh
			if mt == websocket.TextMessage {
				var ctrl map[string]any
				if json.Unmarshal(msg, &ctrl) == nil {
					t, _ := ctrl["t"].(string)
					switch t {
					case "terminal-capabilities":
						if version, _ := ctrl["v"].(float64); version == 1 {
							if sizeControlsSupported && terminalCapability(ctrl, "size-owner-v1") {
								sizeControls.EnableSizeControls(viewer)
							}
							if terminalCapability(ctrl, "agent-history-v1") {
								historyEnabled = true
								select {
								case historyCapabilities <- struct{}{}:
								default:
								}
							}
							if terminalCapability(ctrl, screenRequestCapability) {
								screenNeg.capable.Store(true)
								select {
								case screenCapabilities <- struct{}{}:
								default:
								}
							}
						}
					case "agent-history-read":
						if historyEnabled {
							request, _ := ctrl["request"].(string)
							cursor, _ := ctrl["cursor"].(string)
							history.request(request, cursor)
						}
					case "size-control":
						if sizeControlsSupported {
							op, _ := ctrl["op"].(string)
							target, _ := ctrl["target"].(string)
							revision, ok := ctrl["revision"].(float64)
							if ok && revision >= 0 && revision <= 9007199254740991 && float64(uint64(revision)) == revision {
								sizeControls.ChangeSizeControl(viewer, op, target, uint64(revision))
							}
						}
					case "diag":
						// Одна строка лога агента на каждое сообщение: формат по
						// виду сообщения — в чистой formatClientDiag (ST-01), чтобы
						// его можно было проверить тестом и чтобы незнакомый вид не
						// печатался чужим форматом из одних `<nil>`.
						log.Print(formatClientDiag(id, ctrl))
					case "resize":
						cols := int(jsonFloat(ctrl, "cols"))
						rows := int(jsonFloat(ctrl, "rows"))
						if cols > 0 && rows > 0 {
							// Размер запоминается ЗА ЭТИМ клиентом, а к общему PTY
							// применяется минимум по всем открытым экранам: раньше
							// последний пришедший resize перекраивал сессию, и
							// телефон ужимал работающий на ПК TUI агента до своих
							// 60 колонок (см. Session.ResizeFor).
							_ = sess.ResizeFor(viewer, cols, rows)
						}
					case "screen":
						// «Пришли готовый кадр экрана вместо обрывков потока».
						// Просьба приходит ВНУТРИ соединения, а не параметром
						// адреса: путь проверяет релей по закрытому белому
						// списку и на незнакомый параметр отвечает 400 —
						// телефон тогда не открывал терминалы вовсе.
						if req, ok := parseScreenFrameRequest(ctrl); ok {
							screenReq.Offer(req)
						} else {
							// Без client geometry generation сервер не может отличить
							// кадр, снятый до локального resize, от актуального. Старые
							// клиенты остаются на lossless raw stream/replay.
							log.Printf("[PTY-WS] кадр экрана id=%s не запрошен: отсутствует корректный geom_rev", id)
							// Согласовавший screen-request-v1 не должен ждать ответа,
							// которого не будет: отказ с причиной (ST-05). Старому
							// клиенту — прежнее молчание.
							if screenNeg.capable.Load() {
								reqID, hasReq := parseSafeJSONUint(ctrl["req"])
								select {
								case screenRejects <- screenFrameRequest{req: reqID, hasReq: hasReq}:
								default:
								}
							}
						}
					case "pause", "resume":
						// Flow control: клиент сообщает, что его xterm не успевает
						// разбирать поток (> 256 КБ отданных, но не распарсенных
						// байт → pause; спад ниже 64 КБ → resume). Пауза — на этого
						// подписчика: PTY читается дальше, кольцо и screen-модель
						// наполняются, остальные зрители не задеты; на resume
						// пропущенное до-сылается из кольца (см. SetFlowPaused).
						// Старые клиенты этих сообщений не шлют — для них всё как
						// было. Heartbeat на паузе НЕ глушим: он не поток (счётчик
						// клиента не растёт), а без него вотчдог тишины на длинной
						// паузе пересоздаст соединение. Уже отобранный из канала
						// хвост докидывается штатным таймером пачки: протокол
						// байтовый, кадры уходят целыми.
						sess.SetFlowPaused(ch, t == "pause")
						log.Printf("[PTY-WS] flow-control id=%s paused=%v", id, t == "pause")
					case "paste":
						text, _ := ctrl["text"].(string)
						if text != "" {
							// Тот же хелпер, что у REST-ввода (POST /api/pty/{id}/input):
							// чанки по 512 байт с паузами, без разрыва UTF-8. Раньше это
							// был инлайн-код здесь — вынесен, чтобы вставка и быстрый
							// ответ не разъехались.
							if werr := writePtyInput(sess, []byte(text)); werr != nil {
								return // PTY pipe broken — tear down so the client reconnects clean
							}
						}
					}
				}
			} else {
				// Binary — raw terminal input.
				// Write in chunks to avoid overwhelming ConPTY pipe buffer.
				const ptyChunk = 4096
				for i := 0; i < len(msg); i += ptyChunk {
					end := min(i+ptyChunk, len(msg))
					if _, werr := sess.Write(msg[i:end]); werr != nil {
						return // PTY pipe broken — tear down so the client reconnects clean
					}
				}
			}
		}
	}()

	// Send PTY output → WebSocket. Every write has a deadline so a stalled client
	// can't wedge this goroutine (and the subscriber channel) forever.
	//
	// sent — позиция в потоке, до которой байты РЕАЛЬНО ушли в сокет. Ведётся
	// ровно так же, как её ведёт клиент (см. PtyTermView: offset из маркера,
	// дальше += длина каждого бинарного кадра), поэтому по ней можно честно
	// до-слать пропущенное из кольца.
	sent := offset
	// wake — сигнал «этот подписчик отстал»: канал переполнился, и readLoop
	// больше в него не пишет (иначе свежие байты обогнали бы до-сылку).
	// Пропущенное лежит в кольце сессии — забираем его оттуда и отдаём с тем же
	// маркером resumed, которым пользуется обычное переподключение. Раньше на
	// этом месте кадр молча выбрасывался: замер 2026-08-04 показал 8 329
	// потерянных строк из 20 000 у клиента, подтормозившего на 4 секунды.
	sub := sess.Handle(ch)
	// Every successful resync marker starts a new delivery generation. A screen
	// captured before that marker cannot be sent afterwards: numeric offsets may
	// have crossed its base (or even changed epoch), but the frame is stale.
	var streamGeneration uint64
	resync := func() bool {
		// payload, offset и epoch снимаются одной critical section
		// Session.bufMu: иначе reset между двумя вызовами маркировал
		// бы payload старой эпохи как новую.
		payload, newOffset, gap, currentEpoch := sess.ResyncFrom(ch, sent, epoch)
		decision := decideResyncMarker(epoch, currentEpoch, len(payload), gap)
		if !decision.send {
			sent = newOffset
			return true
		}
		m, err := json.Marshal(struct {
			T      string `json:"t"`
			Epoch  string `json:"epoch"`
			Offset uint64 `json:"offset"`
			Modes  string `json:"modes,omitempty"`
			Gap    bool   `json:"gap,omitempty"`
			HB     int    `json:"hb,omitempty"`
		}{
			T: decision.action, Epoch: currentEpoch, Offset: newOffset - uint64(len(payload)),
			// При пропуске режимы могли переключиться в любую сторону — как и на
			// обычном gap-резюме, отдаём полную синхронизацию SET+RESET.
			// После epoch reset xterm чист, и ему нужно лишь повторно
			// включить текущие active modes.
			Modes: func() string {
				if decision.action == "reset" {
					return sess.ModeReassertSeq()
				}
				if decision.gap {
					return sess.ModeSyncSeq()
				}
				return ""
			}(),
			Gap: decision.gap, HB: int(ptyHeartbeatInterval / time.Second),
		})
		if err != nil {
			return false
		}
		_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
		if err := conn.WriteMessage(websocket.TextMessage, m); err != nil {
			closeReason = "write-error: " + err.Error()
			return false
		}
		if len(payload) > 0 {
			// Досылка до 2 МиБ — кусками wsPayloadChunk, у каждого свой
			// дедлайн (ST-09 B1): медленное плечо не рвёт соединение.
			if err := writeBinaryChunks(conn, payload, wsPayloadChunk); err != nil {
				closeReason = "write-error: " + err.Error()
				return false
			}
		}
		streamGeneration++
		log.Printf("[PTY-WS] slow-client resync id=%s sent=%d → %d action=%s gap=%v", id, sent, newOffset, decision.action, decision.gap)
		sent = newOffset
		epoch = currentEpoch
		return true
	}

	// Коалесцирование мелких кадров: ConPTY отдаёт репейнты Codex/Kimi чанками
	// по ~72 байта, и отправка каждого отдельным сообщением давала тысячи
	// WS-фреймов на залп (со своим write-дедлайном на каждом плече ПК → релей →
	// телефон). Накапливаем вывод и шлём пачкой: по достижении wsutil.BatchMax
	// или по таймеру wsutil.BatchDelay — интерактивное эхо задерживается не более
	// чем на ~25 мс, а залп уходит десятками сообщений. Порядок байт не меняется.
	batcher := &wsutil.Batcher{}
	// Кадр, снятый, но ещё не отданный: ждёт, пока клиент получит вывод, который
	// этот кадр уже содержит (см. sendScreen ниже). Объявлены ДО flush: пачка
	// вывода обязана уметь разрезаться ровно по базе кадра.
	var pendingScreen []byte
	var pendingScreenBase uint64
	var pendingScreenGeneration uint64
	var pendingScreenRevision uint64
	var pendingScreenGeomRev uint64
	// Когда кадр был снят — для измерения задержки шва (см. лог в sendScreen).
	var pendingScreenAt time.Time
	// Будильник сторожа: живёт только пока кадр придержан.
	var screenHold <-chan time.Time
	// retryScreen — watchdog отменил кадр, который обгонял sent.
	// Запрашиваем новый, но на flow-pause ждём resume, чтобы не
	// переснимать один и тот же недостижимый base по кругу.
	retryScreen := false
	var screenGeometryWake <-chan struct{}
	var lastScreenReq screenFrameRequest
	haveLastScreenReq := false
	// writeScreenNow отдаёт придержанный кадр немедленно и называет шов числами.
	var writeScreenNow func(path string) bool
	var cancelPendingScreen func() uint64

	writeBinary := func(data []byte) bool {
		if len(data) == 0 {
			return true
		}
		_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
		if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
			closeReason = "write-error: " + err.Error()
			return false
		}
		sent += uint64(len(data))
		return true
	}
	// writeText — служебное текстовое сообщение (screen-capability, screen-none,
	// ответы истории). В шкалу sent не входит — см. контракт offset.
	writeText := func(msg []byte) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			closeReason = "write-error: " + err.Error()
			return false
		}
		return true
	}

	// flush сбрасывает накопленный вывод ОДНИМ бинарным сообщением (дедлайн — на
	// пачку целиком). Любой текстовый маркер (resumed/hb/exit) обязан уходить
	// ПОСЛЕ уже принятого вывода, поэтому везде перед маркером стоит flush.
	flush := func() bool {
		data := batcher.Flush()
		if len(data) == 0 {
			return true
		}
		// ⚠ ПАЧКА НЕ ИМЕЕТ ПРАВА ПЕРЕПРЫГНУТЬ БАЗУ ПРИДЕРЖАННОГО КАДРА.
		//
		// `sent` растёт на ВЕСЬ батч разом, и проверка «клиент догнал базу»
		// (sendScreen) проходила в момент, когда он её уже перескочил: клиент
		// применял байты ПОСЛЕ базы, а следом получал кадр состояния ДО них —
		// картинка откатывалась, и те изменения не приходили больше никогда.
		// Дыры в очереди для этого не нужны (повторный аудит, T259-07).
		//
		// Режем ровно по базе: префикс → кадр(база) → суффикс. Порядок байт не
		// меняется, клиент получает то же самое, только со швом на своём месте.
		if cut := splitAtScreenBase(sent, len(data), pendingScreenBase, pendingScreen != nil); cut > 0 {
			if !writeBinary(data[:cut]) {
				return false
			}
			if writeScreenNow != nil && !writeScreenNow("шов") {
				return false
			}
			return writeBinary(data[cut:])
		}
		return writeBinary(data)
	}

	hbTicker := time.NewTicker(ptyHeartbeatInterval)
	defer hbTicker.Stop()
	// Когда последний раз отдавали кадр этому клиенту: сборка сетки стоит
	// процессорного времени, и повторять её чаще незачем.
	var lastScreenAt time.Time
	// sendScreen отдаёт придержанный кадр, если пришло его время.
	//
	// ⚠ Кадр нельзя отправлять раньше байт, которые в нём уже нарисованы.
	// Зеркало и канал подписчика наполняются в одной горутине, но РАЗНЫМИ
	// темпами: снимок мог включить чанк, который ещё лежит в канале и уйдёт
	// клиенту следом. Клиент исполнял бы одни и те же VT-команды дважды —
	// видимый дубль на тексте и произвольный сдвиг экрана на LF, вставке строк
	// и alt-screen (внешний аудит 13.08.2026, находка T-003). Держим кадр, пока
	// sent не догонит base: sent ведётся в той же шкале, что Session.totalBytes.
	//
	// Сторож по времени обязателен: клиент на паузе flow control свой канал не
	// наполняет вовсе, и ждать догона можно было бы вечно. Просрочка НЕ отправляет
	// future frame: он отменяется, а свежий кадр запрашивается после догона/разпаузы.
	writeScreenNow = func(path string) bool {
		if pendingScreen == nil {
			return true
		}
		payload := pendingScreen
		base := pendingScreenBase
		revision := pendingScreenRevision
		generation := pendingScreenGeneration
		heldMs := int64(0)
		if !pendingScreenAt.IsZero() {
			heldMs = time.Since(pendingScreenAt).Milliseconds()
		}
		if generation != streamGeneration {
			cancelPendingScreen()
			retryScreen = haveLastScreenReq
			lastScreenAt = time.Time{}
			log.Printf("[PTY-WS] кадр экрана id=%s отменён после resync: база=%d", id, base)
			return true
		}
		wrote, err := writeScreenFrameIfCurrent(sess, revision, func() error {
			_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
			return conn.WriteMessage(websocket.TextMessage, payload)
		})
		if err != nil {
			closeReason = "write-error: " + err.Error()
			return false
		}
		if !wrote {
			cancelPendingScreen()
			retryScreen = haveLastScreenReq
			lastScreenAt = time.Time{}
			log.Printf("[PTY-WS] кадр экрана id=%s отменён после resize: база=%d revision=%d", id, base, revision)
			return true
		}
		cancelPendingScreen()
		// ⚠ ШОВ КАДРА — ЧИСЛАМИ. Кадр снят на позиции `база`, клиенту отдано
		// `отдано` байт. Совпало — шов точный. `ПЕРЕЛЁТ` больше нуля означал бы,
		// что пачка пересекла базу и клиент применил байты ПОСЛЕ неё, а кадр
		// показал состояние ДО, — с 2.57.16 такого быть не должно: flush режет
		// пачку ровно по базе (путь=шов). Строка оставлена именно как сторож:
		// увидим перелёт — значит появился ещё один путь мимо резки.
		over := int64(sent) - int64(base)
		switch {
		case sent < base:
			log.Printf("[PTY-WS] кадр экрана id=%s путь=%s база=%d отдано=%d отставание=%d Б ждали=%d мс",
				id, path, base, sent, base-sent, heldMs)
		case over > 0:
			log.Printf("[PTY-WS] кадр экрана id=%s путь=%s база=%d отдано=%d ПЕРЕЛЁТ=%d Б ждали=%d мс",
				id, path, base, sent, over, heldMs)
		default:
			log.Printf("[PTY-WS] кадр экрана id=%s путь=%s база=%d отдано=%d ждали=%d мс", id, path, base, sent, heldMs)
		}
		return true
	}
	cancelPendingScreen = func() uint64 {
		base := pendingScreenBase
		pendingScreen = nil
		pendingScreenBase = 0
		pendingScreenGeneration = 0
		pendingScreenRevision = 0
		pendingScreenGeomRev = 0
		pendingScreenAt = time.Time{}
		screenHold = nil
		return base
	}

	sendScreen := func() bool {
		if pendingScreen == nil {
			return true
		}
		if pendingScreenGeneration != streamGeneration {
			base := cancelPendingScreen()
			retryScreen = true
			lastScreenAt = time.Time{}
			log.Printf("[PTY-WS] кадр экрана id=%s отменён после resync: база=%d", id, base)
			return true
		}
		if sent < pendingScreenBase {
			return true // байты кадра ещё в пути к клиенту
		}
		path := "точный"
		if sent > pendingScreenBase {
			path = "догон"
		}
		return writeScreenNow(path)
	}
	// writeTexts — сообщения согласования screen-request-v1 в порядке, который
	// выдал screenPeer (подтверждение всегда раньше первого отказа).
	writeTexts := func(msgs [][]byte) bool {
		for _, msg := range msgs {
			if !writeText(msg) {
				return false
			}
		}
		return true
	}
	// ST-10 B: способность истории — только когда у сессии появился источник.
	// historySourceWake — канал «источник есть» (nil — не ждём), отписка —
	// при выходе из хендлера.
	var historySourceWake <-chan struct{}
	cancelHistoryWait := func() {}
	defer func() { cancelHistoryWait() }()
	historyCapSent := false
	for {
		// Придержанный кадр отдаём, как только клиент догнал его базу: sent
		// меняется только внутри этого цикла, поэтому проверка здесь ловит
		// любое продвижение потока.
		if !sendScreen() {
			return
		}
		if retryScreen && screenGeometryWake == nil && !sub.Paused() && haveLastScreenReq {
			retryScreen = false
			screenReq.OfferIfEmpty(lastScreenReq)
		}
		select {
		case <-viewer.ControlsWake():
			if sizeControlsSupported {
				if state, ok := sizeControls.SizeControls(viewer); ok {
					_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
					if err := conn.WriteJSON(state); err != nil {
						return
					}
				}
			}
		case <-historyCapabilities:
			// Раньше способность уходила сразу, и кнопка «История агента»
			// появлялась в любом терминале, включая голый шелл. Теперь то же
			// сообщение ждёт, пока хук агента впервые назовёт источник
			// (Session.HistorySourceReady): старый клиент получает прежнее,
			// просто позже. Повторное объявление после отправки — как раньше.
			if historyCapSent {
				_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
				if err := conn.WriteJSON(map[string]any{"t": "agent-history-capability", "v": 1}); err != nil {
					return
				}
				break
			}
			if historySourceWake == nil {
				historySourceWake, cancelHistoryWait = sess.HistorySourceReady()
			}
		case <-historySourceWake:
			historySourceWake = nil
			cancelHistoryWait()
			cancelHistoryWait = func() {}
			historyCapSent = true
			_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
			if err := conn.WriteJSON(map[string]any{"t": "agent-history-capability", "v": 1}); err != nil {
				return
			}
		case <-screenCapabilities:
			if !writeTexts(screenNeg.ack()) {
				return
			}
		case bad := <-screenRejects:
			if msgs := screenNeg.rejected(bad); len(msgs) > 0 {
				if !flush() || !writeTexts(msgs) {
					return
				}
			}
		case reply := <-history.replies:
			_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
			if err := conn.WriteJSON(reply); err != nil {
				return
			}
		case <-screenGeometryWake:
			// A resize request/completion changed the capture barrier. If a
			// retry was withheld while ACK was pending, the next loop now
			// re-enqueues it without requiring another client request.
			screenGeometryWake = nil
		case <-screenHold:
			if screenHoldTimeoutAction(sent, pendingScreenBase) == screenHoldCancelAndRetry {
				base := cancelPendingScreen()
				retryScreen = true
				// Новый запрос не должен попасть под min-gap от
				// уже отменённого кадра.
				lastScreenAt = time.Time{}
				log.Printf("[PTY-WS] кадр экрана id=%s отменён: база=%d отдано=%d — запрошен свежий", id, base, sent)
				break
			}
			if !sendScreen() {
				return
			}
		case <-sub.Wake():
			// Клиент на паузе — до-сылка подождёт: будильник взведёт снова сама
			// разпауза (см. Session.SetFlowPaused). Без этого стража сюда
			// пришли бы мы слать до 2 МБ как раз перегруженному клиенту.
			if sub.Paused() {
				break
			}
			// Маркер до-сылки — после накопленного вывода, иначе клиент получит
			// «resumed» раньше байтов, которые ещё лежат в буфере.
			if !flush() || !resync() {
				return
			}
		case <-screenReq.wake:
			req, ok := screenReq.Take()
			if !ok {
				break
			}
			if !writeTexts(screenNeg.ack()) {
				return
			}
			if !haveLastScreenReq || req.geomRev != lastScreenReq.geomRev {
				lastScreenAt = time.Time{}
				if pendingScreen != nil && pendingScreenGeomRev != req.geomRev {
					base := cancelPendingScreen()
					log.Printf("[PTY-WS] кадр экрана id=%s отменён новым geom_rev=%d: база=%d", id, req.geomRev, base)
				}
			}
			lastScreenReq = req
			haveLastScreenReq = true
			// Кадр экрана по просьбе клиента. Отдаём ПОСЛЕ накопленного вывода
			// (flush), иначе кадр лёг бы раньше байтов, которые ещё в буфере, и
			// они дорисовали бы поверх него старое.
			//
			// Кадр НЕ двигает позицию клиента в потоке: он синтетический и в
			// счётчик байт не входит — поэтому уходит текстовым сообщением, как
			// маркеры. Байты, из которых он собран, клиент получит обычным
			// путём; кадр их просто опережает картинкой.
			if !flush() {
				return
			}
			if wait := screenFrameMinGap - time.Since(lastScreenAt); wait > 0 {
				// Частая просьба — не повод её ВЫБРОСИТЬ. До 2.61.16 здесь стоял
				// голый `break`, и ровно этот путь оставлял человека без экрана:
				// клиент, отвергнув кадр как устаревший, просит свежий через
				// полсекунды (SCREEN_REQUEST_DELAY_MS), то есть внутри этой же
				// секунды, — и просьба исчезала без следа в логе. Боевой лог
				// владельца за 8 часов 01.09.2026: 62 вердикта «кадр устарел»,
				// и в 51 из них следующий кадр пришёл только при СЛЕДУЮЩЕМ
				// открытии терминала (через 17 с … 49 мин). У Claude Code, который
				// рисует экран диффом по ячейкам, без кадра остаётся чёрный
				// экран с одной строкой спиннера. Сетку по-прежнему считаем не
				// чаще раза в секунду — просто досчитываем паузу и спрашиваем сами.
				deferScreenRequest(screenReq, req, wait)
				break
			}
			lastScreenAt = time.Now()
			capture := sess.CaptureScreenFrame()
			frame, history, histLines, fcols, frows, baseOff, screenRevision := capture.Frame, capture.History, capture.HistLines, capture.Cols, capture.Rows, capture.BaseOff, capture.Revision
			if frame == "" {
				if capture.Retry {
					retryScreen = true
					screenGeometryWake = capture.Wake
					lastScreenAt = time.Time{}
				}
				// Зеркала нет (старая сессия) или оно отстало — клиенту без
				// screen-request-v1 молчим: он остаётся на потоке, как и до
				// появления кадров.
				if sess.ScreenStale() {
					log.Printf("[PTY-WS] кадр экрана id=%s не отдан: зеркало отстало", id)
				}
				// Согласовавшему — причина (ST-05): not-ready — повторить позже,
				// unavailable — кадров не будет, resize-pending — сервер повторит
				// сам и кадр придёт с тем же req.
				if !writeTexts(screenNeg.emptyFrame(req, capture.Reason)) {
					return
				}
				break
			}
			screenNeg.frameCaptured()
			// history/hist_lines — история для прокрутки назад: scrollback ПЛЮС
			// текущие видимые строки (шов с кадром — см. historyLocked; клиент
			// пишет её в чистый терминал ДО кадра и в геометрии снапшота).
			// Поля присутствуют ВСЕГДА, даже пустые: по их наличию клиент
			// отличает нового агента от старого. Поле screen и его формат не
			// меняются — старый клиент читает только его, а неизвестные ключи
			// JSON игнорирует.
			// base_offset — позиция потока, которую кадр УЖЕ содержит. С ней
			// клиент может сам ответить на вопрос «этот кадр свежее того, что я
			// уже показал, или старее»; раньше он не знал о ней ничего и
			// применял любой пришедший кадр (повторный аудит, T259-07).
			// Старый клиент поле игнорирует — поведение прежнее.
			// req — эхо номера запроса screen-request-v1; у старого клиента его
			// нет, и байты ответа прежние (marshalScreenFrame).
			sm, merr := marshalScreenFrame(frame, history, histLines, fcols, frows, baseOff, req)
			if merr != nil {
				break
			}
			// Не отправляем сразу: кадр ждёт, пока клиент получит покрытый им
			// вывод (см. sendScreen). Обычно это тот же тик — байты уже в канале.
			pendingScreen, pendingScreenBase = sm, baseOff
			pendingScreenGeneration = streamGeneration
			pendingScreenRevision = screenRevision
			pendingScreenGeomRev = req.geomRev
			pendingScreenAt = time.Now()
			screenHold = time.After(screenHoldMax)
			log.Printf("[PTY-WS] кадр экрана id=%s %dx%d %d Б, история %d строк %d Б, база потока %d (отдано %d)", id, fcols, frows, len(frame), histLines, len(history), baseOff, sent)
			if !sendScreen() {
				return
			}
		case <-hbTicker.C:
			if !flush() {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"t":"hb"}`)); err != nil {
				closeReason = "write-error: " + err.Error()
				return
			}
		case data, ok := <-ch:
			if !ok {
				_ = flush() // отдать принятое до конца канала
				closeReason = "session-ended"
				return
			}
			sub.Took(len(data)) // место в запасе подписчика освободилось
			full := batcher.Add(data)
			// Залповый репейнт: добираем уже стоящие в канале кадры не блокируясь —
			// пачка наполняется сразу, без ожидания таймера на каждый чанк.
		fill:
			for !full {
				select {
				case d, ok := <-ch:
					if !ok {
						_ = flush()
						closeReason = "session-ended"
						return
					}
					sub.Took(len(d))
					full = batcher.Add(d)
				default:
					break fill
				}
			}
			if full {
				if !flush() {
					return
				}
			}
		case <-batcher.C():
			// Таймер накопления: пора отдать то, что лежит в буфере.
			if !flush() {
				return
			}
		case <-sess.Done():
			if !drainPTYBeforeExit(ch, sub.Took, batcher.Add, flush, resync, sendScreen) {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(wsutil.WriteWait))
			conn.WriteMessage(websocket.TextMessage, []byte(`{"t":"exit"}`))
			closeReason = "process-exit"
			return
		case <-closeCh:
			closeReason = readReason
			return
		}
	}
}

// drainPTYBeforeExit establishes the final output→exit barrier. Session.Done
// closes after readLoop has appended/fanned out its last n>0+EOF bytes, but a
// select may observe Done while those bytes are still ready in ch. Drain every
// already-queued frame, then resync unconditionally: paused/lagged subscribers
// may have a final tail only in the authoritative Session ring. No producer can
// append after Done, so the resync offset is final. A pending screen is handled
// last against that exact offset/generation, before the terminal exit marker.
func drainPTYBeforeExit(ch <-chan []byte, took func(int), add func([]byte) bool, flush, resync, sendScreen func() bool) bool {
	for {
		select {
		case data, ok := <-ch:
			if !ok {
				goto drained
			}
			took(len(data))
			if add(data) && !flush() {
				return false
			}
		default:
			goto drained
		}
	}

drained:
	if !flush() || !resync() {
		return false
	}
	return sendScreen()
}

func jsonFloat(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// parseResume разбирает значение ?resume=<epoch>:<offset>. epoch — hex (без
// двоеточий), поэтому делим по последнему ':'. Пустая/битая строка → ("",0),
// что трактуется как первый коннект (полный буфер).
func parseResume(s string) (string, uint64) {
	if s == "" {
		return "", 0
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0
	}
	off, err := strconv.ParseUint(s[i+1:], 10, 64)
	if err != nil {
		return "", 0
	}
	return s[:i], off
}

// ── client-diag: строка лога агента для {t:"diag"} клиента ──────────

// Пределы общего формата (ST-01): лог не должен становиться каналом, которым
// клиент выносит на диск произвольный объём или чужой текст.
const (
	diagMaxFields     = 16
	diagMaxValueRunes = 64
	diagMaxKeyLen     = 32
)

// diagDeniedKeys — поля, которые в лог не пишутся НИКОГДА, даже если клиент их
// прислал: это содержимое вывода, ввода, буфера обмена и имена файлов (I-15).
// Список проверяется и в общем, и в явных форматах через diagFields.
var diagDeniedKeys = map[string]bool{
	"data": true, "text": true, "screen": true, "history": true, "name": true,
	"clipboard": true, "paste": true, "input": true, "output": true, "content": true,
	"message": true, "cmd": true, "command": true, "path": true, "file": true,
	"filename": true, "title": true, "cwd": true, "url": true, "prompt": true,
}

// formatClientDiag — строка лога агента для диагностического сообщения клиента.
// Содержимого терминала, ввода и clipboard в строках нет (I-15).
//
// Известные виды печатаются своим форматом; прежние — для обычных значений байт
// в байт как раньше, чтобы старые логи и новые читались одинаково, но каждое
// значение клиента проходит legacyDiagValue (без управляющих символов, с
// пределом длины), а не сырой %v:
//   - alt-scroll — куда ушёл жест прокрутки и ПОЧЕМУ. С 2.57.13 чья это история
//     решает замер потока (строк scrollback на килобайт вывода), а не порог
//     глубины: владелец=terminal — край своей истории, приложению не шлём
//     ничего; владелец=application — история внутри агента, идут PgUp/PgDn и
//     Ctrl+Home/End. Числа замера здесь же, иначе жалобу «то листается, то нет»
//     опять нечем будет разобрать. До ST-01 этим же форматом печатался ЛЮБОЙ
//     незнакомый вид — и snapshot-adopt, snapshot-width, alt-scroll-page-dead
//     выходили в лог одними `<nil>`.
//   - tg-chrome — геометрия окна Telegram при открытии терминала. Заведён
//     04.08.2026: на iPad владельца кнопка «✕ Закрыть» ложилась поверх нашей
//     «←», а изнутри стенда причину воспроизвести не удалось. Экран, размер
//     окна и инсет — не пользовательские данные.
//   - snapshot — решение клиента о том, чьей прокруткой пользоваться: присланной
//     зеркалом или своей (13.08.2026, «почему в Kimi вижу всё, а в Claude только
//     часть»). ⚠ Геометрии называются раздельно (с 2.57.14): прежнее
//     `клиент=48x11` смешивало логический размер с видимой областью и
//     подталкивало к неверному диагнозу. `решение` — apply | restore | resync.
//   - snapshot-stale — клиент ОТКАЗАЛСЯ применять кадр, снятый раньше того, что
//     человек уже видит. Строка обязана быть редкой: с 2.57.16 пачка режется по
//     базе кадра (splitAtScreenBase). `показано` — позиция, разобранная xterm.
//   - snapshot-geometry-stale — кадр прежней ревизии геометрии клиента (лог
//     01.09.2026 состоял из одних `<nil>`).
//   - alt-scroll-verdict-restored — вердикт взят ИЗ ПАМЯТИ сессии, а не измерен
//     (жалоба 08.09 «автоопределение вывода не всегда работает»). page/wheel:
//     false = канал не ответил.
//   - upload — почему НЕ загрузился файл (13.08.2026). С ST-01 имя файла в лог
//     не пишется, только расширение (I-15): имя файла — пользовательские данные.
//
// Новые явные виды (ST-01): snapshot-adopt, snapshot-width, alt-scroll-page-dead
// (changed/shifted), trace-mark («Зафиксировать проблему»), retention-shadow
// (ST-04), flow (ST-09), nav (решение навигации клиента). Всё прочее —
// отсортированные key=value только скалярных полей: не больше 16 полей по 64
// символа, без вложенных объектов и без полей из diagDeniedKeys.
func formatClientDiag(id string, ctrl map[string]any) string {
	what, _ := ctrl["what"].(string)
	prefix := "[PTY-WS] client-diag id=" + id + " what="
	// v — значение клиента для прежних форматов: только через legacyDiagValue,
	// никогда сырым %v (перевод строки в значении подделывал строку журнала).
	v := func(key string) string { return legacyDiagValue(ctrl[key]) }
	switch what {
	case "tg-chrome":
		return fmt.Sprintf("[PTY-WS] client-diag id=%s what=tg-chrome client=%s surface=%s platform=%s ver=%s fullscreen=%s screen=%sx%s win=%sx%s vp=%sx%s reported=%s inset=%s backTop=%s",
			id, v("client"), v("surface"), v("platform"), v("ver"), v("fullscreen"),
			v("screenW"), v("screenH"), v("winW"), v("winH"),
			v("vpW"), v("vpH"), v("reported"), v("inset"), v("backTop"))
	case "upload":
		return prefix + "upload расширение=" + uploadDiagExt(ctrl["name"]) +
			" размер=" + diagValue(ctrl["size"]) + " тип=" + diagValue(ctrl["type"]) +
			" код=" + diagValue(ctrl["code"]) + " статус=" + diagValue(ctrl["status"]) +
			" ошибка=" + diagValue(ctrl["message"])
	case "snapshot-stale":
		return fmt.Sprintf("[PTY-WS] client-diag id=%s what=snapshot-stale база=%s принято=%s показано=%s — кадр устарел, клиент запросил свежий",
			id, v("base"), v("accepted"), v("applied"))
	case "snapshot-geometry-stale":
		return fmt.Sprintf("[PTY-WS] client-diag id=%s what=snapshot-geometry-stale кадр_rev=%s клиент_rev=%s — кадр прежней геометрии отвергнут, клиент запросил свежий",
			id, v("frame_rev"), v("client_rev"))
	case "snapshot":
		return fmt.Sprintf("[PTY-WS] client-diag id=%s what=snapshot зеркало=%s своя=%s заменить=%s снапшот=%s логический=%s видимо=%s клавиатура=%s решение=%s",
			id, v("server"), v("local"), v("replace"), v("snap"),
			v("logical"), v("visible"), v("keyboard"), v("action"))
	case "alt-scroll-verdict-restored":
		return fmt.Sprintf("[PTY-WS] client-diag id=%s what=alt-scroll-verdict-restored процесс=%s страницы=%s колесо=%s — вердикт взят из памяти, а не измерен",
			id, v("process"), v("page"), v("wheel"))
	case "alt-scroll":
		return fmt.Sprintf("[PTY-WS] client-diag id=%s what=alt-scroll alt=%s mouse=%s cols=%s rows=%s владелец=%s строк=%s байт=%s своя=%s",
			id, v("alt"), v("mouse"), v("cols"), v("rows"),
			v("owner"), v("lines"), v("bytes"), v("own"))
	case "snapshot-adopt":
		// Кадр шире/выше своей сетки, и клиент локально принял авторитетную
		// сетку PTY (geometry.ts adopt). Раньше — строка из `<nil>`.
		return prefix + "snapshot-adopt снапшот=" + diagValue(ctrl["snap"]) +
			" было=" + diagValue(ctrl["was"]) + " клиент=" + diagValue(ctrl["client"]) +
			" — клиент принял сетку кадра"
	case "snapshot-width":
		// Кадр снят в чужой ширине и не записан: клиент шлёт свой размер и ждёт
		// свежий кадр. auth — авторитетные колонки PTY, известные клиенту.
		return prefix + "snapshot-width снапшот=" + diagValue(ctrl["snap"]) +
			" клиент=" + diagValue(ctrl["client"]) + " авторитет=" + diagValue(ctrl["auth"]) +
			" — кадр чужой ширины отвергнут"
	case "alt-scroll-page-dead":
		// PgUp/PgDn ушли приложению, а экран не сдвинулся. changed/shifted —
		// сколько строк изменилось и сколько сдвинулось: без них «страницы не
		// ответили» нельзя было отличить от «ответили перерисовкой на месте».
		return prefix + "alt-scroll-page-dead владелец=" + diagValue(ctrl["owner"]) +
			" строк=" + diagValue(ctrl["lines"]) + " байт=" + diagValue(ctrl["bytes"]) +
			" своя=" + diagValue(ctrl["own"]) + " изменилось=" + diagValue(ctrl["changed"]) +
			" сдвинулось=" + diagValue(ctrl["shifted"])
	case "trace-mark":
		// Человек нажал «Зафиксировать проблему» (ST-01): метка связывает
		// сохранённую на телефоне трассу со строками этого лога по seq.
		return prefix + "trace-mark seq=" + diagValue(ctrl["seq"]) +
			" событий=" + diagValue(ctrl["events"]) + " выброшено=" + diagValue(ctrl["dropped"]) +
			" байт=" + diagValue(ctrl["bytes"]) + " — клиент сохранил трассу"
	case "retention-shadow":
		// ST-04, этап shadow: новая политика хранения решила бы иначе, чем
		// прежняя. Считаем, сколько CSI 3 J реально поменяют исход, до
		// переключения.
		return prefix + "retention-shadow legacy=" + diagValue(ctrl["legacy"]) +
			" policy=" + diagValue(ctrl["policy"]) + " чтение=" + diagValue(ctrl["reading"]) +
			" gen=" + diagValue(ctrl["gen"]) + " — политика хранения разошлась с прежним решением"
	case "flow":
		// ST-09: счётчики очереди и паузы клиента. Известные поля — первыми и в
		// постоянном порядке, pauses{hidden,backlog} разворачивается в
		// pauses.hidden/pauses.backlog, прочие скалярные — следом по алфавиту.
		return prefix + "flow " + diagFields(flattenDiag(ctrl, "pauses"), []string{
			"state", "reason", "maxQueued", "maxUnacked", "maxBatch",
			"pauses.hidden", "pauses.backlog", "pausedMs", "drops", "dropBytes", "maxFlushMs",
		})
	case "nav":
		// Решение навигации клиента (ST-02/03): кто исполнил жест, каким каналом
		// и почему. Строки и числа, без содержимого.
		return prefix + "nav " + diagFields(ctrl, []string{"executor", "channel", "mode", "reason", "page", "wheel", "owner"})
	}
	name := "?"
	if what != "" {
		name = diagString(what)
	}
	return prefix + name + " " + diagFields(ctrl, nil)
}

// diagFields — поля сообщения строкой key=value: сначала preferred (в этом
// порядке, если присланы), затем остальные по алфавиту. Только скалярные
// значения (строка, число, bool), не больше diagMaxFields, ключи — короткие
// и из безопасного алфавита, запрещённые (diagDeniedKeys) не печатаются вовсе.
// Сколько полей не вошло — честно пишется в конце.
func diagFields(ctrl map[string]any, preferred []string) string {
	scalar := func(v any) bool {
		switch v.(type) {
		case string, float64, bool:
			return true
		}
		return false
	}
	usable := func(k string) bool {
		if k == "t" || k == "what" || !diagKeyAllowed(k) {
			return false
		}
		v, ok := ctrl[k]
		return ok && scalar(v)
	}
	keys := make([]string, 0, len(ctrl))
	taken := map[string]bool{}
	for _, k := range preferred {
		if !taken[k] && usable(k) {
			keys = append(keys, k)
			taken[k] = true
		}
	}
	rest := make([]string, 0, len(ctrl))
	for k := range ctrl {
		if !taken[k] && usable(k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)
	skipped := 0
	if len(keys) > diagMaxFields {
		skipped = len(keys) - diagMaxFields
		keys = keys[:diagMaxFields]
	}
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(diagValue(ctrl[k]))
	}
	if skipped > 0 {
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString("пропущено_полей=" + strconv.Itoa(skipped))
	}
	return sb.String()
}

// flattenDiag разворачивает вложенный объект key{a,b} в поля key.a и key.b
// (только один уровень и только скалярные значения). Остальные вложенные
// объекты diagFields просто не печатает.
func flattenDiag(ctrl map[string]any, key string) map[string]any {
	nested, ok := ctrl[key].(map[string]any)
	if !ok {
		return ctrl
	}
	out := make(map[string]any, len(ctrl)+len(nested))
	for k, v := range ctrl {
		if k != key {
			out[k] = v
		}
	}
	for k, v := range nested {
		out[key+"."+k] = v
	}
	return out
}

func diagKeyAllowed(k string) bool {
	if k == "" || len(k) > diagMaxKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	// Запрет проверяется и у развёрнутых полей: pauses.text не лучше text.
	last := k
	if i := strings.LastIndexByte(k, '.'); i >= 0 {
		last = k[i+1:]
	}
	return !diagDeniedKeys[strings.ToLower(k)] && !diagDeniedKeys[strings.ToLower(last)]
}

// diagValue — значение поля для лога: целые числа без экспоненты, строки не
// длиннее diagMaxValueRunes и в кавычках, если в них пробел, кавычка, `=` или
// управляющий символ (перевод строки в значении иначе подделал бы следующую
// строку лога). Отсутствующее поле — «?», вложенный объект — «{…}».
func diagValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "?"
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) <= float64(maxJSONSafeInteger) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', 6, 64)
	case string:
		return diagString(x)
	default:
		return "{…}"
	}
}

// legacyDiagValue — значение клиента для ПРЕЖНИХ форматов (tg-chrome, snapshot,
// snapshot-stale, snapshot-geometry-stale, alt-scroll, alt-scroll-verdict-restored).
// Раньше они печатались сырым %v: строка с переводом строки подделывала
// следующую строку журнала агента, длина не ограничивалась, вложенный объект
// выводился целиком. Теперь строка идёт через diagString (управляющие символы
// только экранированными, в кавычках, не длиннее diagMaxValueRunes),
// вложенное — «{…}». Обычные значения печатаются байт в байт как раньше: числа,
// bool и отсутствующее поле (`<nil>`) — тем же %v, пустая строка — пустой,
// строка без пробела, кавычки, `=` и управляющих символов — как есть.
func legacyDiagValue(v any) string {
	switch x := v.(type) {
	case nil, bool, float64:
		return fmt.Sprint(x)
	case string:
		if x == "" {
			return ""
		}
		return diagString(x)
	default:
		return "{…}"
	}
}

func diagString(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if utf8.RuneCountInString(s) > diagMaxValueRunes {
		s = string([]rune(s)[:diagMaxValueRunes-1]) + "…"
	}
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if r <= ' ' || r == '"' || r == '=' || r == 0x7f || !unicode.IsPrint(r) {
			return strconv.Quote(s)
		}
	}
	return s
}

// uploadDiagExt — от имени загружаемого файла в лог попадает только
// расширение (I-15): «.pdf» помогает разобрать отказ, а имя файла — это уже
// пользовательские данные. Нет расширения или оно странное — «-».
func uploadDiagExt(name any) string {
	s, _ := name.(string)
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	dot := strings.LastIndexByte(s, '.')
	if dot <= 0 || dot == len(s)-1 {
		return "-"
	}
	ext := strings.ToLower(s[dot+1:])
	if len(ext) > 10 {
		return "-"
	}
	for i := 0; i < len(ext); i++ {
		if c := ext[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return "-"
		}
	}
	return "." + ext
}
