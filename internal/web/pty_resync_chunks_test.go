package web

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/procutil"
	"tgcontrol/internal/pty"
)

// resyncViewer — сторона клиента одного зрителя: поток байт с позиции base
// (offset первого маркера) и порядок событий (маркер / бинарный кадр).
type resyncViewer struct {
	mu      sync.Mutex
	conn    *websocket.Conn
	base    uint64
	started bool
	stream  []byte
	events  []resyncEvent
	err     error
}

type resyncEvent struct {
	marker string // "reset"/"resumed" для маркера, "" для бинарного кадра
	gap    bool
	offset uint64 // offset маркера
	pos    uint64 // позиция потока клиента в момент события (base + принято)
	size   int    // длина бинарного кадра
}

func (v *resyncViewer) read() {
	for {
		_ = v.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		kind, data, err := v.conn.ReadMessage()
		v.mu.Lock()
		if err != nil {
			v.err = err
			v.mu.Unlock()
			return
		}
		pos := v.base + uint64(len(v.stream))
		if kind == websocket.BinaryMessage {
			v.stream = append(v.stream, data...)
			v.events = append(v.events, resyncEvent{pos: pos, size: len(data)})
		} else {
			var m struct {
				T      string `json:"t"`
				Offset uint64 `json:"offset"`
				Gap    bool   `json:"gap"`
			}
			if json.Unmarshal(data, &m) == nil && (m.T == "reset" || m.T == "resumed") {
				if !v.started {
					v.base, v.started, pos = m.Offset, true, m.Offset
				}
				v.events = append(v.events, resyncEvent{marker: m.T, gap: m.Gap, offset: m.Offset, pos: pos})
			}
		}
		v.mu.Unlock()
	}
}

func (v *resyncViewer) snapshot() (base uint64, stream []byte, events []resyncEvent, started bool, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.base, append([]byte(nil), v.stream...), append([]resyncEvent(nil), v.events...), v.started, v.err
}

// ST-09 B1, обязательный L2 на НАСТОЯЩЕМ писателе wsPtyHandler и нативном PTY
// (замер TestSlowViewerResyncLive долгий и запускается явно, этот — в каждом
// go test). Два зрителя одной сессии: медленный просит {t:"pause"}, агент
// печатает ~700 КБ, медленный просит {t:"resume"}. Контракт:
//   - на паузе медленному не приходит ни байта живого вывода;
//   - после resume — маркер resumed без пропуска с offset = его позиции, за
//     ним досылка больше wsPayloadChunk несколькими бинарными кадрами, ни один
//     не длиннее wsPayloadChunk (нарезка writeBinaryChunks, порядок «маркер →
//     куски → sent»);
//   - поток медленного (хвост открытия + живое до паузы + куски досылки)
//     побайтно равен потоку быстрого на общем участке и кончается там же:
//     без потерь, повторов и перестановок (I-04);
//   - быстрый не получил ни пропуска, ни досылки.
func TestPtyResyncChunksLive(t *testing.T) {
	if os.Getenv("REMOTAI_RESYNC_CHUNKS_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		profile := t.TempDir()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestPtyResyncChunksLive$", "-test.v")
		procutil.Hidden(cmd)
		procutil.Prepare(cmd)
		for _, value := range os.Environ() {
			key := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
			if key != "USERPROFILE" && key != "HOME" && key != "REMOTAI_RESYNC_CHUNKS_CHILD" {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "USERPROFILE="+profile, "HOME="+profile, "REMOTAI_RESYNC_CHUNKS_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("изолированный L2 досылки кусками: %v\n%s", err, out)
		}
		t.Log(string(out))
		return
	}
	profile, _ := os.UserHomeDir()
	relative, err := filepath.Rel(profile, paths.Base())
	if err != nil || strings.HasPrefix(relative, "..") {
		t.Fatal("хранилище теста вне изолированного профиля")
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
	server := httptest.NewServer(mux)
	defer server.Close()
	date := fmt.Sprint(time.Now().Unix())
	user := `{"id":12345}`
	hash := hex.EncodeToString(hmacSHA256(hmacSHA256([]byte("WebAppData"), []byte(s.botToken)), []byte("auth_date="+date+"\nuser="+user)))
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/pty/" + session.ID + "?initData=" +
		url.QueryEscape(url.Values{"auth_date": {date}, "user": {user}, "hash": {hash}}.Encode())
	dial := func() *resyncViewer {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(address, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		v := &resyncViewer{conn: conn}
		go v.read()
		return v
	}
	waitFor := func(what string, limit time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("не дождались: %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// Поток затих: ни байта у зрителя за quiet.
	quiet := func(v *resyncViewer, quiet, limit time.Duration) {
		t.Helper()
		deadline := time.Now().Add(limit)
		last, since := -1, time.Now()
		for time.Now().Before(deadline) {
			_, stream, _, _, _ := v.snapshot()
			if len(stream) != last {
				last, since = len(stream), time.Now()
			} else if time.Since(since) >= quiet {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("вывод оболочки не затих")
	}

	fast, slow := dial(), dial()
	waitFor("первые маркеры обоих зрителей", 10*time.Second, func() bool {
		_, _, _, a, _ := fast.snapshot()
		_, _, _, b, _ := slow.snapshot()
		return a && b
	})
	quiet(fast, 700*time.Millisecond, 15*time.Second)
	quiet(slow, 300*time.Millisecond, 5*time.Second)

	if err := slow.conn.WriteJSON(map[string]any{"t": "pause"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // пауза дошла до читателя, канал вычерпан
	_, pausedStream, _, _, _ := slow.snapshot()

	const done = "RESYNC_CHUNKS_DONE"
	line := strings.Repeat("x", 600)
	command := "i=0; while [ $i -lt 1200 ]; do echo " + line + "; i=$((i+1)); done; echo " + done + "\n"
	if runtime.GOOS == "windows" {
		command = "for /L %i in (1,1,1200) do @echo " + line + "\r\necho " + done + "\r\n"
	}
	if _, err := session.Write([]byte(command)); err != nil {
		t.Fatal(err)
	}
	// Быстрый видит и эхо набранной команды, и её вывод: маркер дважды.
	waitFor("вывод команды у быстрого зрителя", 60*time.Second, func() bool {
		_, stream, _, _, _ := fast.snapshot()
		return bytes.Count(stream, []byte(done)) >= 2
	})
	quiet(fast, 700*time.Millisecond, 15*time.Second)

	_, heldStream, _, _, _ := slow.snapshot()
	if len(heldStream) != len(pausedStream) {
		t.Fatalf("на паузе медленному пришло %d байт живого вывода", len(heldStream)-len(pausedStream))
	}
	if err := slow.conn.WriteJSON(map[string]any{"t": "resume"}); err != nil {
		t.Fatal(err)
	}
	fastBase, fastStream, fastEvents, _, fastErr := fast.snapshot()
	fastEnd := fastBase + uint64(len(fastStream))
	waitFor("досылка медленному до позиции быстрого", 30*time.Second, func() bool {
		base, stream, _, _, _ := slow.snapshot()
		return base+uint64(len(stream)) >= fastEnd
	})
	time.Sleep(300 * time.Millisecond)
	slowBase, slowStream, slowEvents, _, slowErr := slow.snapshot()
	slowEnd := slowBase + uint64(len(slowStream))
	if fastErr != nil || slowErr != nil {
		t.Fatalf("соединение оборвалось: быстрый %v, медленный %v", fastErr, slowErr)
	}

	// Маркер досылки и куски за ним.
	resumedAt := -1
	for i, e := range slowEvents {
		if i > 0 && e.marker == "resumed" {
			resumedAt = i
			break
		}
	}
	if resumedAt < 0 {
		t.Fatalf("после resume медленный не получил маркер resumed: %+v", slowEvents)
	}
	marker := slowEvents[resumedAt]
	if marker.gap || marker.offset != marker.pos {
		t.Fatalf("маркер досылки %+v: ожидался resumed без пропуска с offset = позиции клиента %d", marker, marker.pos)
	}
	var sizes []int
	resynced := 0
	for _, e := range slowEvents[resumedAt+1:] {
		if e.marker != "" {
			break
		}
		sizes = append(sizes, e.size)
		resynced += e.size
	}
	payload := int(fastEnd - marker.offset)
	if payload <= wsPayloadChunk {
		t.Fatalf("положительный контроль: досылка %d байт не больше куска %d — нарезка не проверена", payload, wsPayloadChunk)
	}
	full := 0
	for _, n := range sizes {
		if n > wsPayloadChunk {
			t.Fatalf("бинарный кадр досылки %d байт длиннее wsPayloadChunk %d: %v", n, wsPayloadChunk, sizes)
		}
		if n == wsPayloadChunk {
			full++
		}
	}
	if len(sizes) < 2 || full < 1 || resynced < payload {
		t.Fatalf("досылка %d байт пришла кадрами %v (полных %d, всего %d байт)", payload, sizes, full, resynced)
	}

	// Побайтное равенство на общем участке и общий конец.
	if slowEnd != fastEnd {
		t.Fatalf("медленный кончился на %d, быстрый на %d", slowEnd, fastEnd)
	}
	from := max(slowBase, fastBase)
	if !bytes.Equal(slowStream[from-slowBase:], fastStream[from-fastBase:]) {
		t.Fatal("поток медленного (хвост, живое до паузы, куски досылки) не равен потоку быстрого: потеря, повтор или перестановка")
	}
	resyncBytes := slowStream[marker.offset-slowBase:]
	if len(resyncBytes) != payload || !bytes.Equal(resyncBytes, fastStream[marker.offset-fastBase:]) {
		t.Fatal("конкатенация кусков досылки не равна суффиксу потока")
	}
	for i, e := range fastEvents {
		if e.gap || i > 0 && e.marker != "" {
			t.Fatalf("быстрый зритель задет: маркер %+v", e)
		}
	}
	t.Logf("досылка %d байт после паузы: %d кадров %v (≤ %d), поток медленного равен потоку быстрого на %d байтах, быстрый без пропуска и досылок",
		payload, len(sizes), sizes, wsPayloadChunk, slowEnd-from)
}
