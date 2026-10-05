package server

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Сжатие стрим-плеча (permessage-deflate, заведено 11.08.2026) обязано быть
// ПРОЗРАЧНЫМ: релей остаётся байт-насосом, и то, что доехало до xterm.js, должно
// совпадать с отправленным до последнего байта. Цена ошибки здесь — испорченный
// вывод терминала у всех сразу, поэтому проверяем не «включилось ли», а именно
// совпадение содержимого, и отдельно — что на проводе стало меньше.
//
// Поток берём таким, каким его гонит TUI: повтор шапки, много SGR-последо-
// вательностей, перерисовка одних и тех же строк. Ровно это и лежит в
// pty-scrollback у боевых сессий.
func ptyLikeStream(repeat int) []byte {
	var b bytes.Buffer
	for i := 0; i < repeat; i++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[K", i%40+1)
		b.WriteString("\x1b[38;2;215;119;87m▐\x1b[38;2;157;157;157m")
		fmt.Fprintf(&b, " строка вывода агента %d ", i)
		b.WriteString("\x1b[m\x1b[?25l\x1b[37;3H\x1b[?25h")
		b.WriteString(strings.Repeat("─", 48))
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// countingDialer считает байты, реально ушедшие в сокет, — только так видно
// эффект сжатия: длина сообщения на уровне API остаётся исходной.
type countingConn struct {
	net.Conn
	read *int64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	atomic.AddInt64(c.read, int64(n))
	return n, err
}

func TestStreamCompressionIsByteExactAndSmaller(t *testing.T) {
	payload := ptyLikeStream(400)
	if len(payload) < 100*1024 {
		t.Fatalf("образец слишком мал для замера: %d Б", len(payload))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := streamUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			t.Errorf("write: %v", err)
		}
		// Дочитываем до закрытия клиентом, чтобы сервер не оборвал раньше времени.
		_, _, _ = conn.ReadMessage()
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	dial := func(compress bool) (payloadGot []byte, onWire int64) {
		var wire int64
		d := websocket.Dialer{
			EnableCompression: compress,
			NetDial: func(network, addr string) (net.Conn, error) {
				c, err := net.Dial(network, addr)
				if err != nil {
					return nil, err
				}
				return countingConn{Conn: c, read: &wire}, nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, _, err := d.DialContext(ctx, url, nil)
		if err != nil {
			t.Fatalf("dial (compress=%v): %v", compress, err)
		}
		defer conn.Close()
		_, got, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read (compress=%v): %v", compress, err)
		}
		return got, atomic.LoadInt64(&wire)
	}

	gotPlain, wirePlain := dial(false)
	gotDeflate, wireDeflate := dial(true)

	// Главное: содержимое не пострадало ни в одном из режимов.
	if !bytes.Equal(gotPlain, payload) {
		t.Fatalf("без сжатия содержимое разъехалось: получено %d Б, отправлено %d Б", len(gotPlain), len(payload))
	}
	if !bytes.Equal(gotDeflate, payload) {
		t.Fatalf("СО СЖАТИЕМ содержимое разъехалось: получено %d Б, отправлено %d Б", len(gotDeflate), len(payload))
	}

	// И ради чего всё затевалось: на проводе стало ощутимо меньше.
	if wireDeflate >= wirePlain {
		t.Fatalf("сжатие не сработало: на проводе %d Б против %d Б без сжатия", wireDeflate, wirePlain)
	}
	ratio := float64(wirePlain) / float64(wireDeflate)
	if ratio < 2 {
		t.Fatalf("выигрыш меньше двукратного (%.1f×): %d Б → %d Б", ratio, wirePlain, wireDeflate)
	}
	t.Logf("payload %d Б: на проводе %d Б → %d Б (в %.1f раза), содержимое совпадает байт в байт",
		len(payload), wirePlain, wireDeflate, ratio)
}
