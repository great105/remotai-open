package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"tgcontrol/internal/pty"
)

// Повторная просьба кадра внутри screenFrameMinGap не пропадает: она
// возвращается в очередь по истечении паузы. До 2.61.16 её глотал голый break,
// и клиент, отвергнувший первый кадр как устаревший, оставался без экрана до
// следующего открытия терминала (51 случай из 62 за 8 часов боевого лога).
func TestDeferredScreenRequestReturnsAfterGap(t *testing.T) {
	q := newScreenRequestQueue()
	deferScreenRequest(q, screenFrameRequest{geomRev: 5}, 20*time.Millisecond)
	if _, ok := q.Take(); ok {
		t.Fatal("отложенная просьба оказалась в очереди раньше срока")
	}
	select {
	case <-q.wake:
	case <-time.After(2 * time.Second):
		t.Fatal("отложенная просьба так и не разбудила писателя")
	}
	got, ok := q.Take()
	if !ok || got.geomRev != 5 {
		t.Fatalf("после паузы в очереди %+v ok=%v, ждали rev 5", got, ok)
	}

	// Более новая просьба клиента, вставшая в очередь за время паузы,
	// не перекрывается отложенным повтором.
	q2 := newScreenRequestQueue()
	deferScreenRequest(q2, screenFrameRequest{geomRev: 5}, 20*time.Millisecond)
	q2.Offer(screenFrameRequest{geomRev: 6})
	time.Sleep(80 * time.Millisecond)
	got, ok = q2.Take()
	if !ok || got.geomRev != 6 {
		t.Fatalf("отложенный повтор перекрыл новую геометрию: %+v ok=%v", got, ok)
	}
}

type fakeScreenRevisionLease struct {
	current bool
	err     error
}

func (f *fakeScreenRevisionLease) WithCurrentScreenFrameRevision(_ uint64, send func() error) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if !f.current {
		return false, nil
	}
	return true, send()
}

// ШОВ КАДРА С ПОТОКОМ: пачка вывода не имеет права перепрыгнуть базу кадра.
//
// Боевая механика до 2.57.16: клиенту отдано 1900 байт, кадр снят на 2000, а
// батчер собрал кусок 1900..2200 и ушёл целиком. Проверка «клиент догнал базу»
// проходила (2200 ≥ 2000), кадр уезжал следом — и откатывал картинку на 200
// байт назад. Эти 200 байт клиент больше не получал никогда.
func TestSplitAtScreenBase(t *testing.T) {
	cases := []struct {
		name    string
		sent    uint64
		batch   int
		base    uint64
		pending bool
		want    int
	}{
		{"пачка пересекает базу — режем ровно по ней", 1900, 300, 2000, true, 100},
		{"пачка кончается ровно на базе — резать нечего", 1900, 100, 2000, true, 0},
		{"пачка целиком до базы", 1000, 500, 2000, true, 0},
		{"база уже позади — кадр отдаст обычный путь", 2500, 300, 2000, true, 0},
		{"кадра нет — режем только ради кадра", 1900, 300, 2000, false, 0},
		{"базы нет (старый путь) — не режем", 1900, 300, 0, true, 0},
		{"пустая пачка", 1900, 0, 2000, true, 0},
		{"первый байт после базы", 1999, 2, 2000, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := splitAtScreenBase(c.sent, c.batch, c.base, c.pending); got != c.want {
				t.Fatalf("splitAtScreenBase(sent=%d batch=%d base=%d pending=%v) = %d, ожидалось %d",
					c.sent, c.batch, c.base, c.pending, got, c.want)
			}
		})
	}
}

// Разрез обязан сохранять поток байт в точности: префикс + суффикс равны
// исходной пачке, и ни один байт не теряется и не дублируется.
func TestSplitAtScreenBaseKeepsBytesIntact(t *testing.T) {
	data := []byte("0123456789")
	cut := splitAtScreenBase(1995, len(data), 2000, true)
	if cut != 5 {
		t.Fatalf("длина префикса %d вместо 5", cut)
	}
	prefix, suffix := data[:cut], data[cut:]
	if string(prefix)+string(suffix) != string(data) {
		t.Fatalf("разрез изменил поток: %q + %q", prefix, suffix)
	}
	if string(prefix) != "01234" || string(suffix) != "56789" {
		t.Fatalf("разрез не по базе: %q | %q", prefix, suffix)
	}
}

// Сторож не имеет права отправить кадр из будущего. По timeout кадр
// отменяется и запрашивается свежий, а не показывается до потока.
func TestScreenHoldTimeoutNeverSendsFutureFrame(t *testing.T) {
	if action := screenHoldTimeoutAction(100, 150); action != screenHoldCancelAndRetry {
		t.Fatalf("sent<base: action=%v, want cancel+retry", action)
	}
	if action := screenHoldTimeoutAction(150, 150); action != screenHoldSend {
		t.Fatalf("sent==base: action=%v, want send", action)
	}
	if action := screenHoldTimeoutAction(175, 150); action != screenHoldSend {
		t.Fatalf("sent>base: action=%v, want send", action)
	}
}

func TestScreenRequestGeometryRevisionIsValidatedAndLatestOnly(t *testing.T) {
	valid, ok := parseScreenFrameRequest(map[string]any{"geom_rev": float64(42)})
	if !ok || valid.geomRev != 42 {
		t.Fatalf("valid geom_rev parsed as %+v ok=%v", valid, ok)
	}
	for _, ctrl := range []map[string]any{
		{},
		{"geom_rev": "42"},
		{"geom_rev": float64(-1)},
		{"geom_rev": 1.5},
		{"geom_rev": float64(maxJSONSafeInteger) + 2},
	} {
		if got, ok := parseScreenFrameRequest(ctrl); ok {
			t.Fatalf("unsafe geom_rev accepted: ctrl=%v got=%+v", ctrl, got)
		}
	}

	q := newScreenRequestQueue()
	q.Offer(screenFrameRequest{geomRev: 7})
	q.Offer(screenFrameRequest{geomRev: 8})
	select {
	case <-q.wake:
	default:
		t.Fatal("latest-only queue did not wake writer")
	}
	got, ok := q.Take()
	if !ok || got.geomRev != 8 {
		t.Fatalf("latest-only queue returned %+v ok=%v, want rev 8", got, ok)
	}

	q.Offer(screenFrameRequest{geomRev: 12})
	q.OfferIfEmpty(screenFrameRequest{geomRev: 11})
	select {
	case <-q.wake:
	default:
		t.Fatal("client request did not wake writer")
	}
	got, ok = q.Take()
	if !ok || got.geomRev != 12 {
		t.Fatalf("internal retry overwrote newer client request: %+v ok=%v", got, ok)
	}
}

// ST-05 screen-request-v1: req — необязательное безопасное целое по тем же
// правилам, что geom_rev. Latest-only очередь, внутренний повтор
// (OfferIfEmpty) и отложенный по min-gap запрос сохраняют req: кадр, снятый
// на повторе, клиент сопоставит со своим запросом.
func TestScreenRequestReqIsValidatedAndSurvivesRetries(t *testing.T) {
	got, ok := parseScreenFrameRequest(map[string]any{"geom_rev": float64(4), "req": float64(9)})
	if !ok || got != (screenFrameRequest{geomRev: 4, req: 9, hasReq: true}) {
		t.Fatalf("req parsed as %+v ok=%v", got, ok)
	}
	if got, ok := parseScreenFrameRequest(map[string]any{"geom_rev": float64(4), "req": float64(0)}); !ok || !got.hasReq || got.req != 0 {
		t.Fatalf("req=0 lost: %+v ok=%v", got, ok)
	}
	if got, ok := parseScreenFrameRequest(map[string]any{"geom_rev": float64(4)}); !ok || got.hasReq {
		t.Fatalf("legacy request without req: %+v ok=%v", got, ok)
	}
	for _, bad := range []any{"9", float64(-1), 1.5, float64(maxJSONSafeInteger) + 2, nil, true} {
		if got, ok := parseScreenFrameRequest(map[string]any{"geom_rev": float64(4), "req": bad}); ok {
			t.Fatalf("unsafe req %#v accepted: %+v", bad, got)
		}
	}

	q := newScreenRequestQueue()
	q.Offer(screenFrameRequest{geomRev: 7, req: 1, hasReq: true})
	q.Offer(screenFrameRequest{geomRev: 7, req: 2, hasReq: true})
	if got, _ := q.Take(); got.req != 2 || !got.hasReq {
		t.Fatalf("latest-only lost newer req: %+v", got)
	}
	q.OfferIfEmpty(screenFrameRequest{geomRev: 7, req: 2, hasReq: true})
	if got, ok := q.Take(); !ok || got.req != 2 || !got.hasReq {
		t.Fatalf("internal retry lost req: %+v ok=%v", got, ok)
	}
	q2 := newScreenRequestQueue()
	deferScreenRequest(q2, screenFrameRequest{geomRev: 5, req: 11, hasReq: true}, 10*time.Millisecond)
	select {
	case <-q2.wake:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred request never woke writer")
	}
	if got, ok := q2.Take(); !ok || got.req != 11 || !got.hasReq {
		t.Fatalf("deferred request lost req: %+v ok=%v", got, ok)
	}
}

// T-36: клиент без req получает ответ screen БАЙТ В БАЙТ как до ST-05 —
// эталон собран той же анонимной структурой, что стояла в wsPtyHandler.
// С req — те же байты плюс хвост ,"req":N.
func TestScreenFrameBytesUnchangedForLegacyClient(t *testing.T) {
	legacy, err := json.Marshal(struct {
		T       string `json:"t"`
		Data    string `json:"screen"`
		History string `json:"history"`
		HistN   int    `json:"hist_lines"`
		Cols    int    `json:"screen_cols"`
		Rows    int    `json:"screen_rows"`
		BaseOff uint64 `json:"base_offset"`
		GeomRev uint64 `json:"geom_rev"`
	}{T: "screen", Data: "\x1b[1;1Hкадр", History: "line\r\n", HistN: 1, Cols: 80, Rows: 24, BaseOff: 1234, GeomRev: 9})
	if err != nil {
		t.Fatal(err)
	}
	got, err := marshalScreenFrame("\x1b[1;1Hкадр", "line\r\n", 1, 80, 24, 1234, screenFrameRequest{geomRev: 9})
	if err != nil || !bytes.Equal(got, legacy) {
		t.Fatalf("legacy screen bytes changed:\n got %s\nwant %s", got, legacy)
	}
	withReq, err := marshalScreenFrame("\x1b[1;1Hкадр", "line\r\n", 1, 80, 24, 1234, screenFrameRequest{geomRev: 9, req: 0, hasReq: true})
	if err != nil || string(withReq) != string(legacy[:len(legacy)-1])+`,"req":0}` {
		t.Fatalf("req echo: %s", withReq)
	}
}

// ST-05 «молчаливый отказ»: клиент без screen-request-v1 не получает ни
// подтверждения, ни screen-none (прежнее молчание); согласовавший получает
// подтверждение раньше первого отказа, эхо req и geom_rev, причину, а
// одинаковые отказы на внутренних повторах сервера не дублируются.
func TestScreenNegotiationLegacySilentV1Explained(t *testing.T) {
	req := screenFrameRequest{geomRev: 7, req: 3, hasReq: true}
	legacy := &screenPeer{}
	if msgs := legacy.emptyFrame(req, pty.ScreenReasonNotReady); len(msgs) != 0 {
		t.Fatalf("legacy client got %q", msgs)
	}
	if msgs := legacy.rejected(screenFrameRequest{}); len(msgs) != 0 {
		t.Fatalf("legacy client got reject %q", msgs)
	}
	if msgs := legacy.ack(); len(msgs) != 0 {
		t.Fatalf("legacy client got ack %q", msgs)
	}

	expect := func(t *testing.T, got [][]byte, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("messages %q, want %q", got, want)
		}
		for i := range want {
			if string(got[i]) != want[i] {
				t.Fatalf("message %d = %s, want %s", i, got[i], want[i])
			}
		}
	}
	v1 := &screenPeer{}
	v1.capable.Store(true)
	expect(t, v1.emptyFrame(req, pty.ScreenReasonNotReady),
		`{"t":"screen-capability","v":1}`,
		`{"t":"screen-none","req":3,"geom_rev":7,"reason":"not-ready"}`)
	expect(t, v1.emptyFrame(req, pty.ScreenReasonNotReady))
	expect(t, v1.emptyFrame(req, pty.ScreenReasonUnavailable),
		`{"t":"screen-none","req":3,"geom_rev":7,"reason":"unavailable"}`)
	v1.frameCaptured()
	expect(t, v1.emptyFrame(req, pty.ScreenReasonUnavailable),
		`{"t":"screen-none","req":3,"geom_rev":7,"reason":"unavailable"}`)
	expect(t, v1.rejected(screenFrameRequest{req: 5, hasReq: true}),
		`{"t":"screen-none","req":5,"reason":"invalid-request"}`)
	expect(t, v1.ack())

	// Настоящая сессия без зеркала (старая сессия / транспорт без кадров)
	// называет причину сама — клиент не ждёт ответа, которого не будет.
	capture := (&pty.Session{}).CaptureScreenFrame()
	expect(t, v1.emptyFrame(screenFrameRequest{geomRev: 1}, capture.Reason),
		`{"t":"screen-none","geom_rev":1,"reason":"unavailable"}`)

	// Отказ по битому запросу раньше любого другого ответа тоже идёт после
	// подтверждения.
	fresh := &screenPeer{}
	fresh.capable.Store(true)
	expect(t, fresh.rejected(screenFrameRequest{}),
		`{"t":"screen-capability","v":1}`,
		`{"t":"screen-none","reason":"invalid-request"}`)
}

// Без регрессии темпа: кадр не чаще раза в секунду, сторож шва 750 мс.
func TestScreenPacingConstantsUnchanged(t *testing.T) {
	if screenFrameMinGap != time.Second || screenHoldMax != 750*time.Millisecond {
		t.Fatalf("min-gap=%v hold=%v", screenFrameMinGap, screenHoldMax)
	}
}

// flush can cross pendingScreenBase and call writeScreenNow directly. The
// revision lease must therefore guard that exact path, preserving raw prefix
// and suffix while suppressing the stale synthetic frame.
func TestScreenBaseCrossingStillChecksGeometryRevision(t *testing.T) {
	raw := []byte("0123456789")
	cut := splitAtScreenBase(95, len(raw), 100, true)
	if cut != 5 {
		t.Fatalf("unexpected seam cut %d", cut)
	}
	delivered := append([]byte(nil), raw[:cut]...)
	wroteScreen := false
	current, err := writeScreenFrameIfCurrent(&fakeScreenRevisionLease{current: false}, 9, func() error {
		wroteScreen = true
		return nil
	})
	if err != nil || current || wroteScreen {
		t.Fatalf("stale revision crossed flush seam: current=%v wrote=%v err=%v", current, wroteScreen, err)
	}
	delivered = append(delivered, raw[cut:]...)
	if string(delivered) != string(raw) {
		t.Fatalf("revision cancellation changed raw stream: %q", delivered)
	}

	wantErr := errors.New("write failed")
	if current, err := writeScreenFrameIfCurrent(&fakeScreenRevisionLease{current: true}, 10, func() error { return wantErr }); !current || !errors.Is(err, wantErr) {
		t.Fatalf("current revision did not propagate write result: current=%v err=%v", current, err)
	}
}

// Epoch сменился внутри уже открытого WS: даже пустой новый ring обязан дать
// reset. `resumed` оставил бы старый xterm и приклеил к нему новый host stream.
func TestResyncEpochChangeForcesResetEvenWithoutPayload(t *testing.T) {
	d := decideResyncMarker("old-host", "new-host", 0, true)
	if !d.send || d.action != "reset" || d.gap {
		t.Fatalf("epoch change decision=%+v, want send reset without gap", d)
	}

	d = decideResyncMarker("same", "same", 0, false)
	if d.send {
		t.Fatalf("пустой same-epoch resync послал лишний marker: %+v", d)
	}

	d = decideResyncMarker("same", "same", 10, true)
	if !d.send || d.action != "resumed" || !d.gap {
		t.Fatalf("обычный gap resync сломан: %+v", d)
	}
}

func TestDrainPTYBeforeExitPreservesReadyAndLaggedTail(t *testing.T) {
	ch := make(chan []byte, 2)
	ch <- []byte("final-ready-1")
	ch <- []byte("final-ready-2")

	var batch []byte
	var delivered []string
	taken := 0
	ok := drainPTYBeforeExit(
		ch,
		func(n int) { taken += n },
		func(p []byte) bool {
			batch = append(batch, p...)
			return false
		},
		func() bool {
			if len(batch) > 0 {
				delivered = append(delivered, "output:"+string(batch))
				batch = nil
			}
			return true
		},
		func() bool {
			// Models final output omitted from ch because the subscriber was
			// paused/lagged before Session readLoop observed n>0 + EOF.
			delivered = append(delivered, "resync:ring-only-tail")
			return true
		},
		func() bool {
			delivered = append(delivered, "screen")
			return true
		},
	)
	if !ok {
		t.Fatal("final drain failed")
	}
	wantTaken := len("final-ready-1") + len("final-ready-2")
	if taken != wantTaken {
		t.Fatalf("final queued bytes not accounted: took=%d want=%d", taken, wantTaken)
	}
	want := []string{"output:final-ready-1final-ready-2", "resync:ring-only-tail", "screen"}
	if !reflect.DeepEqual(delivered, want) {
		t.Fatalf("final delivery order=%v, want %v", delivered, want)
	}
}
