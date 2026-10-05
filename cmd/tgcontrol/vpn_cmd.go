package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/config"
)

// `remotai vpn` — состояние и переключение VPN на ЭТОМ компьютере.
//
// Нужна двоим: человеку у машины и AI-агенту в её терминале, которого попросили
// «разберись, почему пропала связь». Обе стороны ходят через локальный API
// агента — там же, где живёт сторож, поэтому ручное выключение и автоматическое
// не спорят друг с другом (ручное вдобавок молчит сторожа на полчаса).
func runVPN(args []string) int {
	action := "status"
	if len(args) > 0 {
		action = strings.ToLower(args[0])
	}
	port := config.GetNoSetup().Port()

	switch action {
	case "status", "":
		var st struct {
			Internet bool `json:"internet"`
			Cloud    bool `json:"cloud"`
			Watchdog bool `json:"watchdog"`
			VPN      *struct {
				Name      string `json:"name"`
				Running   bool   `json:"running"`
				PID       int    `json:"pid"`
				StartHint string `json:"start_hint"`
			} `json:"vpn"`
			Last *struct {
				Text string `json:"text"`
			} `json:"last_action"`
		}
		if code := localGETPort(port, "/api/system/network?check=1", &st); code != 0 {
			fmt.Fprintln(os.Stderr, "Remotai не отвечает на 127.0.0.1:"+strconv.Itoa(port))
			return 1
		}
		fmt.Printf("Интернет:  %s\n", yesNo(st.Internet, "есть", "нет"))
		fmt.Printf("Облако:    %s\n", yesNo(st.Cloud, "отвечает", "не отвечает"))
		if st.VPN != nil {
			fmt.Printf("VPN:       %s — %s (pid %d)\n", st.VPN.Name, yesNo(st.VPN.Running, "запущен", "выключен"), st.VPN.PID)
			if st.VPN.StartHint != "" {
				fmt.Printf("Включить:  %s\n", st.VPN.StartHint)
			}
		} else {
			fmt.Println("VPN:       не найден")
		}
		fmt.Printf("Сторож:    %s\n", yesNo(st.Watchdog, "включён", "выключен"))
		if st.Last != nil && st.Last.Text != "" {
			fmt.Printf("Последнее: %s\n", st.Last.Text)
		}
		return 0
	case "off", "stop", "on", "start":
		want := "stop"
		if action == "on" || action == "start" {
			want = "start"
		}
		body, _ := json.Marshal(map[string]string{"action": want})
		var out map[string]any
		if code := localPOSTPort(port, "/api/system/vpn", body, &out); code != 0 {
			if msg, ok := out["error"].(string); ok && msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			} else {
				fmt.Fprintln(os.Stderr, "не удалось выполнить: Remotai не отвечает на 127.0.0.1:"+strconv.Itoa(port))
			}
			return 1
		}
		if want == "stop" {
			fmt.Printf("✅ %v выключен.\n", out["vpn"])
			if hint, ok := out["start_hint"].(string); ok && hint != "" {
				fmt.Printf("   Включить обратно: remotai vpn on (%s)\n", hint)
			}
		} else {
			fmt.Printf("✅ VPN запущен (%v).\n", out["how"])
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "Использование: remotai vpn [status|off|on]")
		return 2
	}
}

// localGETPort/localPOSTPort — обращение к своему же агенту по loopback.
// Возвращают 0 при успехе (как и остальные CLI-помощники в этом пакете).
func localGETPort(port int, path string, out any) int {
	client := &http.Client{Timeout: 40 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	if out != nil && json.NewDecoder(resp.Body).Decode(out) != nil {
		return 1
	}
	return 0
}

func localPOSTPort(port int, path string, body []byte, out *map[string]any) int {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post("http://127.0.0.1:"+strconv.Itoa(port)+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	decoded := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	if out != nil {
		*out = decoded
	}
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
