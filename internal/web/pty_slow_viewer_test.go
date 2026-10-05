package web

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/procutil"
	"tgcontrol/internal/pty"
)

// ST-09 B1: нарезка крупных бинарных записей писателя. Конкатенация кусков —
// ровно исходный payload (клиент считает offset по длине бинарных кадров,
// I-04), ни один кусок не длиннее потолка и не пуст.
func TestPayloadChunksCoverPayloadExactly(t *testing.T) {
	payload := make([]byte, 2<<20+12345)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	for _, max := range []int{0, -1, 1000, 256 << 10, len(payload) - 1, len(payload), len(payload) + 1} {
		parts := payloadChunks(payload, max)
		var joined []byte
		for _, part := range parts {
			if len(part) == 0 || max > 0 && len(part) > max {
				t.Fatalf("max=%d: кусок %d байт", max, len(part))
			}
			joined = append(joined, part...)
		}
		if !bytes.Equal(joined, payload) {
			t.Fatalf("max=%d: конкатенация кусков не равна payload", max)
		}
		want := 1
		if max > 0 {
			want = (len(payload) + max - 1) / max
		}
		if len(parts) != want {
			t.Fatalf("max=%d: кусков %d, ожидалось %d", max, len(parts), want)
		}
	}
	if payloadChunks(nil, 10) != nil {
		t.Fatal("пустой payload даёт куски")
	}
	// Мельче 256 КиБ — лишние сообщения на каждой досылке; крупнее досылки
	// 2 МиБ — нарезки нет вовсе.
	if wsPayloadChunk < 256<<10 || wsPayloadChunk >= 2<<20 {
		t.Fatalf("кусок досылки %d байт вне [256 КиБ, 2 МиБ)", wsPayloadChunk)
	}
}

type smallSendBufferListener struct{ net.Listener }

func (l smallSendBufferListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(16 << 10)
	}
	return c, err
}

// throttledConn — медленное плечо: читает не быстрее bps байт в секунду.
// read — сырые байты с сокета: по ним видно, что плечо ещё принимает, даже
// когда сообщение (2 МиБ одним кадром) не дочитано.
type throttledConn struct {
	net.Conn
	bps   float64
	start time.Time
	read  atomic.Int64
}

func (c *throttledConn) Read(p []byte) (int, error) {
	if len(p) > 4096 {
		p = p[:4096]
	}
	n, err := c.Conn.Read(p)
	total := c.read.Add(int64(n))
	if due := time.Duration(float64(total) / c.bps * float64(time.Second)); due > time.Since(c.start) {
		time.Sleep(due - time.Since(c.start))
	}
	return n, err
}

type slowViewerSide struct {
	mu      sync.Mutex
	bytes   int64
	maxMsg  int
	markers []string
	gaps    int
	err     error
	errAt   time.Duration
	last    time.Time
}

func (s *slowViewerSide) snapshot() (int64, int, []string, int, error, time.Duration, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes, s.maxMsg, append([]string(nil), s.markers...), s.gaps, s.err, s.errAt, s.last
}

// Замер ST-09 B1 на НАСТОЯЩЕМ писателе api_pty.go и нативном PTY: быстрый и
// медленный (100 КБ/с, малые буферы сокетов) зрители одной сессии, агент
// печатает ~10 МБ. Досылка отставшему одним сообщением («до») и кусками по
// 256 КиБ («после»): сколько раз медленное соединение рвётся по дедлайну
// записи. Долгий (около 2 мин) — запускается явно:
//
//	REMOTAI_SLOW_VIEWER_MEASURE=1 go test ./internal/web -run TestSlowViewerResyncLive -v -timeout 10m
func TestSlowViewerResyncLive(t *testing.T) {
	if os.Getenv("REMOTAI_SLOW_VIEWER_MEASURE") != "1" {
		t.Skip("замер ST-09 B1: REMOTAI_SLOW_VIEWER_MEASURE=1 go test ./internal/web -run TestSlowViewerResyncLive -v -timeout 10m")
	}
	if os.Getenv("REMOTAI_SLOW_VIEWER_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		profile := t.TempDir()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestSlowViewerResyncLive$", "-test.v", "-test.timeout=8m")
		procutil.Hidden(cmd)
		procutil.Prepare(cmd)
		for _, value := range os.Environ() {
			key := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
			if key != "USERPROFILE" && key != "HOME" && key != "REMOTAI_SLOW_VIEWER_CHILD" {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "USERPROFILE="+profile, "HOME="+profile, "REMOTAI_SLOW_VIEWER_CHILD=1")
		out, err := cmd.CombinedOutput()
		t.Log(string(out))
		if err != nil {
			t.Fatalf("изолированный замер: %v", err)
		}
		return
	}
	profile, _ := os.UserHomeDir()
	relative, err := filepath.Rel(profile, paths.Base())
	if err != nil || strings.HasPrefix(relative, "..") {
		t.Fatal("хранилище замера вне изолированного профиля")
	}
	manager := pty.NewLocalManager()
	shell := "/bin/sh"
	if runtime.GOOS == "windows" {
		shell = "cmd.exe"
	}
	session, err := manager.Create(12345, profile, shell, 200, 50)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(session.ID)
	s := &Server{ptyManager: manager, botToken: "synthetic-bot-token", ptyLive: make(map[string][]*ptyLiveSlot), upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/pty/{id}", s.wsPtyHandler)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &httptest.Server{Listener: smallSendBufferListener{ln}, Config: &http.Server{Handler: mux}}
	server.Start()
	defer server.Close()
	date := fmt.Sprint(time.Now().Unix())
	user := `{"id":12345}`
	hash := hex.EncodeToString(hmacSHA256(hmacSHA256([]byte("WebAppData"), []byte(s.botToken)), []byte("auth_date="+date+"\nuser="+user)))
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/pty/" + session.ID + "?initData=" +
		url.QueryEscape(url.Values{"auth_date": {date}, "user": {user}, "hash": {hash}}.Encode())
	const slowBps = 100_000
	var slowLink *throttledConn
	dial := func(slow bool) *websocket.Conn {
		t.Helper()
		dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
		if slow {
			dialer.ReadBufferSize = 4096
			dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetReadBuffer(16 << 10)
				}
				slowLink = &throttledConn{Conn: c, bps: slowBps, start: time.Now()}
				return slowLink, nil
			}
		}
		conn, _, err := dialer.Dial(address, nil)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	read := func(conn *websocket.Conn, side *slowViewerSide, start time.Time) {
		for {
			_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			kind, data, err := conn.ReadMessage()
			side.mu.Lock()
			side.last = time.Now()
			if err != nil {
				side.err, side.errAt = err, time.Since(start)
				side.mu.Unlock()
				return
			}
			if kind == websocket.BinaryMessage {
				side.bytes += int64(len(data))
				if len(data) > side.maxMsg {
					side.maxMsg = len(data)
				}
			} else {
				var marker struct {
					Type string `json:"t"`
					Gap  bool   `json:"gap"`
				}
				if json.Unmarshal(data, &marker) == nil && (marker.Type == "reset" || marker.Type == "resumed") {
					side.markers = append(side.markers, marker.Type)
					if marker.Gap {
						side.gaps++
					}
				}
			}
			side.mu.Unlock()
		}
	}
	type result struct {
		chunk                         int
		fastBytes, slowBytes, slowRaw int64
		fastGaps, slowGaps            int
		fastErr, slowErr              error
		slowBroken                    bool
		slowErrAt                     time.Duration
		slowMaxMsg                    int
		slowMarkers                   []string
	}
	line := strings.Repeat("x", 600)
	measure := func(round, chunk int) result {
		wsPayloadChunk = chunk
		start := time.Now()
		fast, slow := dial(false), dial(true)
		fastSide, slowSide := &slowViewerSide{}, &slowViewerSide{}
		go read(fast, fastSide, start)
		go read(slow, slowSide, start)
		time.Sleep(time.Second)
		done := fmt.Sprintf("SLOW_DONE_%d", round)
		command := "i=0; while [ $i -lt 16000 ]; do echo " + line + "; i=$((i+1)); done; echo " + done + "\n"
		if runtime.GOOS == "windows" {
			command = "for /L %i in (1,1,16000) do @echo " + line + "\r\necho " + done + "\r\n"
		}
		if _, err := session.Write([]byte(command)); err != nil {
			t.Fatal(err)
		}
		// Агент допечатал, затем медленное плечо опустело или соединение рвалось.
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			ch, data, _, _, _, _ := session.SubscribeResume("", 0)
			session.Unsubscribe(ch)
			if bytes.Contains(data, []byte(done)) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		// Плечо опустело: сырые байты с сокета не идут 5 с (сообщение 2 МиБ
		// дочитывается долго — по целым сообщениям конец не определить).
		lastRaw, progressAt := int64(-1), time.Now()
		for time.Now().Before(deadline) {
			_, _, _, _, err, _, _ := slowSide.snapshot()
			if err != nil {
				break
			}
			if raw := slowLink.read.Load(); raw != lastRaw {
				lastRaw, progressAt = raw, time.Now()
			} else if time.Since(progressAt) > 5*time.Second {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		fastBytes, _, _, fastGaps, fastErr, _, _ := fastSide.snapshot()
		slowBytes, slowMax, slowMarkers, slowGaps, slowErr, slowErrAt, _ := slowSide.snapshot()
		slowRaw := slowLink.read.Load()
		_ = fast.Close()
		_ = slow.Close()
		time.Sleep(500 * time.Millisecond)
		return result{chunk: chunk, fastBytes: fastBytes, slowBytes: slowBytes, slowRaw: slowRaw, fastGaps: fastGaps, slowGaps: slowGaps,
			fastErr: fastErr, slowBroken: slowErr != nil, slowErr: slowErr, slowErrAt: slowErrAt, slowMaxMsg: slowMax, slowMarkers: slowMarkers}
	}
	defer func(saved int) { wsPayloadChunk = saved }(wsPayloadChunk)
	report := func(label string, r result) {
		t.Logf("%s: кусок=%d; быстрый: %d байт, пропусков %d, ошибка %v; медленный: %d байт в целых кадрах (%d сырых), маркеры %v (с пропуском %d), самый крупный кадр %d байт, разрыв=%v на %.1f с (%v)",
			label, r.chunk, r.fastBytes, r.fastGaps, r.fastErr, r.slowBytes, r.slowRaw, r.slowMarkers, r.slowGaps, r.slowMaxMsg, r.slowBroken, r.slowErrAt.Seconds(), r.slowErr)
	}
	before := measure(1, 0)
	report("ДО (досылка одним сообщением)", before)
	after := measure(2, 256<<10)
	report("ПОСЛЕ (кусками 256 КиБ)", after)
	if after.fastErr != nil || after.fastGaps != 0 {
		t.Errorf("медленный зритель задел быстрого: ошибка %v, пропусков %d", after.fastErr, after.fastGaps)
	}
	if after.slowBroken {
		t.Errorf("с нарезкой медленное соединение рвётся: на %.1f с", after.slowErrAt.Seconds())
	}
	if after.slowGaps == 0 && len(after.slowMarkers) < 2 {
		t.Errorf("положительный контроль: медленный зритель не отстал (маркеры %v)", after.slowMarkers)
	}
}
