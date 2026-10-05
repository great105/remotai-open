package relayhub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"tgcontrol-relay/internal/protocol"
)

// Все пути записи агенту обязаны идти через ОДИН сериализованный writer.
//
// Регресс, пойманный красным CI: обработчик подключения писал welcome прямо в
// conn, минуя writeMu, хотя агент уже лежал в реестре хаба и ему могли слать
// команды. Две параллельные записи в websocket дают панику gorilla
// «concurrent write to websocket connection» — соединение рвётся сразу после
// подключения, и для человека это «ПК то в сети, то нет».
func TestAgentConnWritesAreSerialized(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srvConnCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		srvConnCh <- conn
	}))
	defer srv.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	// Читатель на стороне «агента»: иначе буфер соединения переполнится и
	// записи начнут блокироваться на дедлайне.
	go func() {
		for {
			if _, _, err := client.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ac := newAgentConn("dev-1", 1, "sess-1", <-srvConnCh)
	defer ac.Close()

	// Все три двери в агента одновременно: приветствие, keepalive и команда.
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _ = ac.SendWelcome(protocol.Welcome{SessionID: "s"}) }()
		go func() { defer wg.Done(); _ = ac.Ping() }()
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			// Ответа не ждём — важна сама запись; таймаут вернёт ошибку, и это
			// нормально: тест проверяет отсутствие паники, а не доставку.
			_, _ = ac.Send(ctx, protocol.Cmd{Type: protocol.MsgCmd, RequestID: "r"}, 20*time.Millisecond)
		}()
	}
	wg.Wait()
}
