package wsutil

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Буфер склеивает мелкие кадры и отдаёт их пачкой строго в исходном порядке.
func TestBatcherAccumulatesInOrder(t *testing.T) {
	b := &Batcher{}
	var whole []byte
	for i := 0; i < 300; i++ {
		data := []byte(fmt.Sprintf("<%04d>", i))
		whole = append(whole, data...)
		if full := b.Add(data); full {
			t.Fatalf("буфер полон на %d байтах — порог BatchMax=%d не достигнут", b.Len(), BatchMax)
		}
	}
	if b.C() == nil {
		t.Fatal("после первого Add таймер накопления обязан быть взведён")
	}
	got := b.Flush()
	if string(got) != string(whole) {
		t.Fatalf("порядок байт нарушен: накоплено %d, отдано %d", len(whole), len(got))
	}
	if b.Len() != 0 || b.C() != nil {
		t.Fatal("после Flush буфер и таймер обязаны обнуляться")
	}
	// Пустой Flush — безопасен и ничего не возвращает.
	if out := b.Flush(); out != nil {
		t.Fatalf("пустой Flush вернул %d байт", len(out))
	}
}

// Достигнув BatchMax, буфер просится наружу не дожидаясь таймера.
func TestBatcherFullAtThreshold(t *testing.T) {
	b := &Batcher{}
	if full := b.Add(make([]byte, BatchMax-1)); full {
		t.Fatal("буфер на байт меньше порога — ещё не полон")
	}
	if full := b.Add([]byte("x")); !full {
		t.Fatal("буфер достиг BatchMax — обязан сигналить о сбросе")
	}
	if n := b.Len(); n != BatchMax {
		t.Fatalf("накоплено %d, ожидалось %d", n, BatchMax)
	}
}

// Таймер накопления стреляет примерно через BatchDelay — интерактивное эхо
// не ждёт дольше этого окна.
func TestBatcherTimerFires(t *testing.T) {
	b := &Batcher{}
	b.Add([]byte("echo"))
	select {
	case <-b.C():
	case <-time.After(BatchDelay * 4):
		t.Fatalf("таймер не выстрелил за %v", BatchDelay*4)
	}
	// Сброс после выстрелившего таймера не зависает и не паникует.
	if got := b.Flush(); string(got) != "echo" {
		t.Fatalf("после таймера Flush вернул %q", got)
	}
}

// Сквозная проверка поверх настоящего websocket: цикл писателя устроен ровно
// как в wsPtyHandler (Add + неблокирующий добор + flush по порогу/таймеру,
// текстовый маркер только после flush). Залп из тысяч мелких кадров обязан
// дойти байт в байт, в том же порядке, ДЕСЯТКАМИ сообщений, а маркер — строго
// после всего вывода, накопленного до него.
func TestBatcherOverWebSocket(t *testing.T) {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		ch := make(chan []byte, 4096)
		go func() {
			// Имитация залпа ConPTY: 2000 кадров по ~72 байта (~140 КБ).
			for i := 0; i < 2000; i++ {
				ch <- []byte(fmt.Sprintf("<frame-%04d-padding-padding-padding-padding-padding-padding-padding>", i))
			}
		}()

		b := &Batcher{}
		flush := func() bool {
			data := b.Flush()
			if len(data) == 0 {
				return true
			}
			_ = conn.SetWriteDeadline(time.Now().Add(WriteWait))
			return conn.WriteMessage(websocket.BinaryMessage, data) == nil
		}
		for i := 0; i < 2000; {
			select {
			case data := <-ch:
				full := b.Add(data)
				i++
			fill:
				for !full && i < 2000 {
					select {
					case d := <-ch:
						full = b.Add(d)
						i++
					default:
						break fill
					}
				}
				if full && !flush() {
					return
				}
			case <-b.C():
				if !flush() {
					return
				}
			}
		}
		// Маркер — только после сброса накопленного вывода.
		if !flush() {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(WriteWait))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"t":"resumed"}`))
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var whole []byte
	for i := 0; i < 2000; i++ {
		whole = append(whole, []byte(fmt.Sprintf("<frame-%04d-padding-padding-padding-padding-padding-padding-padding>", i))...)
	}

	var got []byte
	msgs := 0
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt == websocket.TextMessage {
			if string(data) != `{"t":"resumed"}` {
				t.Fatalf("неожиданный текстовый кадр: %q", data)
			}
			break // маркер — конец проверки
		}
		msgs++
		got = append(got, data...)
	}
	if string(got) != string(whole) {
		t.Fatalf("поток разошёлся: принято %d байт, отправлено %d", len(got), len(whole))
	}
	// 2000 кадров ~140 КБ при пачках по 16 КБ — это десятки сообщений, не тысячи.
	if msgs > 100 {
		t.Fatalf("коалесцирования нет: 140 КБ ушли %d сообщениями", msgs)
	}
	t.Logf("2000 кадров (%d байт) ушли %d сообщениями", len(whole), msgs)
}
