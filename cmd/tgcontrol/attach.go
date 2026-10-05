package main

// `remotai attach [id]` — tmux-style attach: подключает текущее окно
// терминала к managed PTY-сессии локального сервера. Та же сессия в этот же
// момент доступна с телефона, поэтому переход ПК ↔ телефон бесшовный.
//
// Без аргумента: одна живая сессия → подключаемся к ней; ни одной → создаём
// новую (в текущей папке); несколько → печатаем список. Ctrl+] отцепляется,
// сессия продолжает жить.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"tgcontrol/internal/localize"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"

	"tgcontrol/internal/config"
)

const detachKey = 0x1d // Ctrl+]

type ptySession struct {
	ID         string `json:"id"`
	CWD        string `json:"cwd"`
	Shell      string `json:"shell"`
	Alive      bool   `json:"alive"`
	Name       string `json:"name,omitempty"`
	FgProcess  string `json:"fg_process,omitempty"`
	LastActive int64  `json:"last_active"`
}

func runAttach(args []string) int {
	cfg := config.GetNoSetup()
	if cfg.APIToken == "" {
		fmt.Fprintln(os.Stderr, localize.Text("remotai: api_token не найден — запустите Remotai хотя бы один раз"))
		return 1
	}
	port := cfg.Port()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 5 * time.Second}

	var id string
	newSession := false
	for _, a := range args {
		switch {
		case a == "--new" || a == "-n":
			newSession = true
		case a != "" && !strings.HasPrefix(a, "-") && id == "":
			id = a
		}
	}

	// --new: всегда создаём свежую сессию в текущей папке (кнопка «Терминал
	// на ПК» и ярлык «Remotai Терминал») — без угадывания по списку живых.
	if newSession && id == "" {
		var err error
		id, err = ptyCreate(client, base, cfg.APIToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, localize.Text("remotai: не удалось создать сессию: %v\n"), err)
			return 1
		}
		fmt.Printf(localize.Text("remotai: новая сессия %s\n"), id)
		return attachTo(port, cfg.APIToken, id)
	}

	sessions, err := ptyList(client, base, cfg.APIToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, localize.Text("remotai: сервер на %s не отвечает — Remotai запущен? (%v)\n"), base, err)
		return 1
	}

	if id == "" {
		alive := sessions[:0]
		for _, s := range sessions {
			if s.Alive {
				alive = append(alive, s)
			}
		}
		switch len(alive) {
		case 0:
			id, err = ptyCreate(client, base, cfg.APIToken)
			if err != nil {
				fmt.Fprintf(os.Stderr, localize.Text("remotai: не удалось создать сессию: %v\n"), err)
				return 1
			}
			fmt.Printf(localize.Text("remotai: новая сессия %s\n"), id)
		case 1:
			id = alive[0].ID
		default:
			fmt.Println(localize.Text("Несколько живых сессий — укажите id (remotai attach <id>) или remotai attach --new:"))
			for _, s := range alive {
				label := s.Name
				if label == "" {
					label = s.FgProcess
				}
				fmt.Printf("  %-12s %-14s %s\n", s.ID, label, s.CWD)
			}
			return 0
		}
	}

	return attachTo(port, cfg.APIToken, id)
}

func ptyList(client *http.Client, base, token string) ([]ptySession, error) {
	req, _ := http.NewRequest("GET", base+"/api/pty?sort=last_active", nil)
	req.Header.Set("X-API-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Sessions []ptySession `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

func ptyCreate(client *http.Client, base, token string) (string, error) {
	cols, rows := termSize()
	cwd, _ := os.Getwd()
	body, _ := json.Marshal(map[string]any{"cwd": cwd, "cols": cols, "rows": rows})
	req, _ := http.NewRequest("POST", base+"/api/pty", bytes.NewReader(body))
	req.Header.Set("X-API-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func termSize() (cols, rows int) {
	cols, rows = 80, 24
	if c, r, err := term.GetSize(int(os.Stdout.Fd())); err == nil && c > 0 && r > 0 {
		cols, rows = c, r
	}
	return
}

func attachTo(port int, token, id string) int {
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/ws/pty/%s?initData=%s",
		port, id, url.QueryEscape("token:"+token))
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, localize.Text("remotai: подключение к сессии %s: %v\n"), id, err)
		return 1
	}
	defer conn.Close()

	fmt.Printf(localize.Text("remotai: attach %s — Ctrl+] чтобы отцепиться (сессия останется жить)\n"), id)

	stdinFd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai: raw mode: %v\n", err)
		return 1
	}
	defer term.Restore(stdinFd, oldState)
	enableVTOutput()
	enableVTInput()

	// Один writer на соединение: stdin-горутина и resize-цикл пишут конкурентно.
	var wmu sync.Mutex
	wsWrite := func(mt int, data []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(mt, data)
	}

	sendResize := func(cols, rows int) {
		msg, _ := json.Marshal(map[string]any{"t": "resize", "cols": cols, "rows": rows})
		_ = wsWrite(websocket.TextMessage, msg)
	}
	lastCols, lastRows := termSize()
	sendResize(lastCols, lastRows)

	// PTY → экран. exited: сам процесс в сессии завершился (а не detach).
	done := make(chan struct{})
	exited := false
	go func() {
		defer close(done)
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				os.Stdout.Write(msg)
			case websocket.TextMessage:
				var ctrl struct {
					T string `json:"t"`
				}
				if json.Unmarshal(msg, &ctrl) == nil && ctrl.T == "exit" {
					exited = true
					return
				}
			}
		}
	}()

	// Клавиатура → PTY; Ctrl+] = detach.
	detach := make(chan struct{})
	go func() {
		defer close(detach)
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if i := bytes.IndexByte(chunk, detachKey); i >= 0 {
					if i > 0 {
						_ = wsWrite(websocket.BinaryMessage, chunk[:i])
					}
					return
				}
				if wsWrite(websocket.BinaryMessage, chunk) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Следим за размером окна (опрос — переносимо и для Windows).
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-done:
			break loop
		case <-detach:
			break loop
		case <-ticker.C:
			if c, r := termSize(); c != lastCols || r != lastRows {
				lastCols, lastRows = c, r
				sendResize(c, r)
			}
		}
	}

	conn.Close()
	term.Restore(stdinFd, oldState)
	if exited {
		fmt.Printf(localize.Text("\r\nremotai: сессия %s завершена\r\n"), id)
	} else {
		fmt.Printf(localize.Text("\r\nremotai: отцепился — сессия %s продолжает работать (remotai attach %s чтобы вернуться)\r\n"), id, id)
	}
	return 0
}
