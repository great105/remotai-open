package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"tgcontrol/internal/localize"
	"time"

	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"

	"tgcontrol/internal/web"
)

// activateExisting asks an already-running Remotai instance to restore its
// native window. The GET fallback recognises older versions that do not yet
// implement open-window and opens their panel in the browser.
//
// Таймаут 3 секунды, а не 900 мс: живой агент отвечает мгновенно, только пока
// не занят. У человека с полутора десятками терминалов и работающими агентами
// (поток вывода, WebRTC, переигровка буфера) ответ на loopback легко уходит за
// секунду — и тогда сторож автозапуска считал агент чужой программой и
// поднимал ВТОРОЙ экземпляр на соседнем порту (см. resolveStartupPort). Ждать
// три секунды раз в пять минут дешевле, чем разбираться с двумя агентами.
func activateExisting(port int) bool {
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, base+"/api/setup/open-window", nil)
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return true
		}
	}
	resp, err := client.Get(base + "/api/setup/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var marker struct {
		Version  string `json:"version"`
		DeviceID string `json:"device_id"`
	}
	if json.NewDecoder(resp.Body).Decode(&marker) != nil || marker.Version == "" || marker.DeviceID == "" {
		return false
	}
	openBrowser(base + "/setup?force=1")
	return true
}

// resolveStartupPort performs a real bind probe before any UI is opened. If a
// different program owns the configured port, a nearby free port is selected;
// the caller persists it and publishes the diagnostic in the setup panel.
// silent — запуск сторожем автозапуска: живой агент не трогаем и окно ему не
// поднимаем, просто сообщаем «уже работает».
func resolveStartupPort(requested int, silent bool) (int, *web.StartupBindError, bool) {
	if silent {
		if alive(requested) {
			return requested, nil, true
		}
	} else if activateExisting(requested) {
		return requested, nil, true
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", requested))
	if err == nil {
		_ = ln.Close()
		return requested, nil, false
	}

	owner := startupPortOwner(requested)
	// Порт занят НАМИ ЖЕ, просто прошлый экземпляр не успел ответить на
	// loopback-проверку (был занят). Второй агент на соседнем порту — худшее,
	// что можно сделать: у обоих один device_id, и релей пускает только одного,
	// поэтому они выбивают друг друга по кругу. Живой случай 31.07: сторож
	// автозапуска поднял второй экземпляр на :8081, и связь с телефоном рвалась
	// каждые 20–40 секунд, пока лишний процесс не сняли руками (88 разрывов за
	// час). Выходим молча — сторож попробует снова через пять минут.
	if ownerIsRemotai(owner) {
		return requested, nil, true
	}

	selected := freeStartupPort(requested + 1)
	label := localize.Text("другой программой")
	if owner != "" {
		label = owner
	}
	return selected, &web.StartupBindError{
		Port:         requested,
		Owner:        owner,
		SelectedPort: selected,
		Message:      fmt.Sprintf(localize.Text("Порт %d занят программой %s. Remotai выбрал свободный порт %d."), requested, label, selected),
	}, false
}

func freeStartupPort(start int) int {
	if start < 1024 || start > 65535 {
		start = 8080
	}
	for port := start; port < start+200 && port <= 65535; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err == nil {
			_ = ln.Close()
			return port
		}
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err == nil {
		defer ln.Close()
		if _, p, splitErr := net.SplitHostPort(ln.Addr().String()); splitErr == nil {
			if n, convErr := strconv.Atoi(p); convErr == nil {
				return n
			}
		}
	}
	return requestedFallbackPort
}

const requestedFallbackPort = 18080

// alive — на порту отвечает ЖИВОЙ агент Remotai (а не чужая программа). Тот же
// запрос, что и в activateExisting, но без побочного действия: окно не трогаем.
// Нужен сторожу автозапуска, который проверяет «агент на месте?» каждые
// несколько минут.
func alive(port int) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/setup/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var marker struct {
		Version  string `json:"version"`
		DeviceID string `json:"device_id"`
	}
	if json.NewDecoder(resp.Body).Decode(&marker) != nil {
		return false
	}
	return marker.Version != "" || marker.DeviceID != ""
}

// ownerIsRemotai — строку из startupPortOwner («remotai.exe (PID 123)») написал
// наш же процесс? Сравниваем по имени файла без учёта регистра: на Windows его
// пишут и как «Remotai.exe».
func ownerIsRemotai(owner string) bool {
	return strings.Contains(strings.ToLower(owner), "remotai")
}

func startupPortOwner(port int) string {
	conns, err := gnet.Connections("tcp")
	if err != nil {
		return ""
	}
	for _, conn := range conns {
		if int(conn.Laddr.Port) != port || conn.Pid <= 0 {
			continue
		}
		p, err := process.NewProcess(conn.Pid)
		if err != nil {
			continue
		}
		if name, err := p.Name(); err == nil && name != "" {
			return fmt.Sprintf("%s (PID %d)", name, conn.Pid)
		}
		return fmt.Sprintf("PID %d", conn.Pid)
	}
	return ""
}
