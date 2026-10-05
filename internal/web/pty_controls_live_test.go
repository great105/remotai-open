package web

import (
	"bufio"
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
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/procutil"
	"tgcontrol/internal/pty"
)

// Runs the actual WS handler and native PTY in a subprocess with a fresh home.
// No NewServer, persistent host, service, autostart or user agent is started.
func TestTerminalControlsLive(t *testing.T) {
	if os.Getenv("REMOTAI_TERMINAL_CONTROLS_TEST") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		profile := t.TempDir()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestTerminalControlsLive$", "-test.v")
		procutil.Hidden(cmd)
		procutil.Prepare(cmd)
		for _, value := range os.Environ() {
			key := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
			if key != "USERPROFILE" && key != "HOME" && key != "REMOTAI_TERMINAL_CONTROLS_TEST" {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "USERPROFILE="+profile, "HOME="+profile, "REMOTAI_TERMINAL_CONTROLS_TEST=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated controls: %v\n%s", err, out)
		}
		t.Log(string(out))
		return
	}
	profile, _ := os.UserHomeDir()
	relative, err := filepath.Rel(profile, paths.Base())
	if err != nil || strings.HasPrefix(relative, "..") {
		t.Fatal("test storage escaped isolated profile")
	}
	manager := pty.NewLocalManager()
	shell := "/bin/sh"
	if runtime.GOOS == "windows" {
		shell = "cmd.exe"
	}
	session, err := manager.Create(12345, profile, shell, 80, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(session.ID)
	s := &Server{ptyManager: manager, botToken: "synthetic-bot-token", ptyLive: make(map[string][]*ptyLiveSlot), upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/pty/{id}", s.wsPtyHandler)
	mux.HandleFunc("/api/pty/{id}/state", func(w http.ResponseWriter, r *http.Request) { s.apiPtyState(w, r, 12345) })
	server := httptest.NewServer(mux)
	defer server.Close()
	date := fmt.Sprint(time.Now().Unix())
	user := `{"id":12345}`
	check := "auth_date=" + date + "\nuser=" + user
	hash := hex.EncodeToString(hmacSHA256(hmacSHA256([]byte("WebAppData"), []byte(s.botToken)), []byte(check)))
	initData := url.Values{"auth_date": {date}, "user": {user}, "hash": {hash}}.Encode()
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/pty/" + session.ID + "?initData=" + url.QueryEscape(initData)
	if os.Getenv("REMOTAI_TERMINAL_BROWSER_STAND") == "1" {
		ready, _ := json.Marshal(map[string]string{"url": server.URL, "ws": address, "session": session.ID})
		fmt.Printf("QA_TERMINAL_STAND %s\n", ready)
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		return
	}
	dial := func(resume string) *websocket.Conn {
		t.Helper()
		endpoint := address
		if resume != "" {
			endpoint += "&resume=" + url.QueryEscape(resume)
		}
		conn, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	send := func(conn *websocket.Conn, data any) {
		t.Helper()
		if err := conn.WriteJSON(data); err != nil {
			t.Fatal(err)
		}
	}
	control := func(conn *websocket.Conn, matches func(pty.SizeControlState) bool) pty.SizeControlState {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			kind, raw, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var state pty.SizeControlState
			if kind == websocket.TextMessage && json.Unmarshal(raw, &state) == nil && state.Type == "terminal-controls" && matches(state) {
				return state
			}
		}
	}
	// A WS handshake can complete before AddViewer. Registration order is not
	// a client identity. Put the legacy viewer first deliberately and wait for
	// its initial marker so the readiness assertion cannot assume PC/phone
	// occupy the first two positions in the server's list.
	legacy := dial("")
	_ = legacy.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := legacy.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	pc, phone := dial(""), dial("")
	send(pc, map[string]any{"t": "resize", "cols": 80, "rows": 40})
	send(phone, map[string]any{"t": "resize", "cols": 80, "rows": 24})
	send(legacy, map[string]any{"t": "resize", "cols": 80, "rows": 30})
	for _, conn := range []*websocket.Conn{pc, phone} {
		send(conn, map[string]any{"t": "terminal-capabilities", "v": 1, "capabilities": []string{"size-owner-v1", "agent-history-v1"}})
	}
	ready := func(state pty.SizeControlState) bool {
		capable, selfCapable := 0, false
		heights := map[int]bool{}
		for _, viewer := range state.Viewers {
			if viewer.Cols != 80 {
				return false
			}
			heights[viewer.Rows] = true
			if viewer.CanOwn {
				capable++
			}
			if viewer.ID == state.You {
				selfCapable = viewer.CanOwn
			}
		}
		return len(state.Viewers) == 3 && capable == 2 && selfCapable && heights[24] && heights[30] && heights[40]
	}
	pcState := control(pc, ready)
	phoneState := control(phone, ready)
	send(pc, map[string]any{"t": "size-control", "op": "claim", "revision": pcState.Revision})
	owned := control(pc, func(state pty.SizeControlState) bool { return state.Owner == pcState.You })
	control(phone, func(state pty.SizeControlState) bool { return state.Owner == pcState.You })
	send(pc, map[string]any{"t": "size-control", "op": "transfer", "target": phoneState.You, "revision": owned.Revision})
	owned = control(phone, func(state pty.SizeControlState) bool { return state.Owner == phoneState.You })
	send(phone, map[string]any{"t": "size-control", "op": "release", "revision": owned.Revision})
	control(phone, func(state pty.SizeControlState) bool { return state.Owner == "" })
	pc.Close()
	phone.Close()
	legacy.Close()
	t.Log("80x24 / 80x40: capability, claim, explicit transfer and release reached the real WS/PTY path; legacy viewer retained")
	// Closing the last viewer schedules a delayed grow back to the original
	// 80x40 grid. Let it settle before the reconnect byte-stream checks: a
	// resize during those checks correctly forces a reset instead of a
	// resumed+gap marker, which would test geometry rather than retention.
	resizeDeadline := time.Now().Add(5 * time.Second)
	for {
		cols, rows := session.AppliedSize()
		if cols == 80 && rows == 40 {
			break
		}
		if time.Now().After(resizeDeadline) {
			t.Fatalf("PTY did not return to 80x40 after viewers left: %dx%d", cols, rows)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Every byte received over 100 retained reconnects must equal the final
	// ring snapshot. No screen request is used: only ordered stream markers.
	snapshot := func() ([]byte, uint64, string) {
		ch, data, offset, epoch, _, _ := session.SubscribeResume("", 0)
		session.Unsubscribe(ch)
		return data, offset, epoch
	}
	var collected []byte
	var position uint64
	epoch := ""
	for i := 0; i < 100; i++ {
		_, err := session.Write([]byte(fmt.Sprintf("echo RETAINED_%03d\r\n", i)))
		if err != nil {
			t.Fatal(err)
		}
		marker := []byte(fmt.Sprintf("RETAINED_%03d", i))
		deadline := time.Now().Add(2 * time.Second)
		var target uint64
		for {
			data, offset, _ := snapshot()
			if bytes.Count(data, marker) >= 2 {
				target = offset
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("native shell output missing")
			}
			time.Sleep(5 * time.Millisecond)
		}
		resume := ""
		if epoch != "" {
			resume = fmt.Sprintf("%s:%d", epoch, position)
		}
		conn := dial(resume)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for position < target || epoch == "" {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if kind == websocket.BinaryMessage {
				collected = append(collected, data...)
				position += uint64(len(data))
				continue
			}
			var marker struct {
				Type   string `json:"t"`
				Epoch  string
				Offset uint64
				Gap    bool
			}
			if json.Unmarshal(data, &marker) == nil && (marker.Type == "reset" || marker.Type == "resumed") {
				if marker.Gap || i > 0 && marker.Type != "resumed" || marker.Offset != position {
					t.Fatalf("reconnect %d marker %+v vs %d", i, marker, position)
				}
				epoch = marker.Epoch
			}
		}
		conn.Close()
	}
	final, _, _ := snapshot()
	if !bytes.Equal(collected, final[:len(collected)]) {
		t.Fatal("retained reconnect stream duplicated, lost or reordered bytes")
	}
	t.Logf("100 retained reconnects: exact ordered prefix of %d bytes", len(collected))
	// Evict the retained prefix while this viewer is away, then require an
	// explicit gap instead of silently pretending the old offset was resumed.
	line := strings.Repeat("x", 600)
	command := "i=0; while [ $i -lt 9000 ]; do echo " + line + "; i=$((i+1)); done; echo GAP_DONE\n"
	if runtime.GOOS == "windows" {
		command = "for /L %i in (1,1,9000) do @echo " + line + "\r\necho GAP_DONE\r\n"
	}
	if _, err := session.Write([]byte(command)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		data, offset, _ := snapshot()
		if offset > position+4<<20 && bytes.Contains(data, []byte("GAP_DONE")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bounded-ring eviction fixture timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
	gapConn := dial(fmt.Sprintf("%s:%d", epoch, position))
	_ = gapConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := gapConn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var gapMarker struct {
		Type   string `json:"t"`
		Gap    bool
		Offset uint64
	}
	if json.Unmarshal(raw, &gapMarker) != nil || gapMarker.Type != "resumed" || !gapMarker.Gap || gapMarker.Offset <= position {
		t.Fatalf("evicted prefix was not disclosed: %s", raw)
	}
	gapConn.Close()
	t.Log("evicted retained prefix: explicit resumed+gap marker with advanced base offset")
}
