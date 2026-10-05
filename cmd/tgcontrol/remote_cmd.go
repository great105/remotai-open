package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/dnsfallback"
	"tgcontrol/internal/relay"
)

// `remotai remote` — дотянуться до ДРУГОГО компьютера того же аккаунта.
//
// ЗАЧЕМ. Просьба владельца 07.08.2026: «дать серверу доступ к компьютеру —
// если что-то случилось, чтобы агент с сервера мог добраться и проверить, что
// там». До этого связь была односторонней: с компьютера на сервер ходили по
// SSH, а с сервера на компьютер — никак. Худший момент — ровно тот, когда
// компьютер «пропал»: человек с телефона видит «не в сети» и всё.
//
// ПРОПУСК — ТОТ ЖЕ, ЧТО УЖЕ ЕСТЬ. Никакого входа в аккаунт на сервере: команда
// предъявляет облаку device-JWT самого сервера (он появился при `remotai pair`),
// а облако пускает только к устройствам того же владельца. Что именно позволено
// гостю, решает принимающий компьютер настройкой peer_access — по умолчанию
// состояние и управление VPN, без терминалов и файлов.
func runRemote(args []string) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		remoteUsage()
		return 2
	}

	asJSON := false
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" {
			asJSON = true
			continue
		}
		filtered = append(filtered, a)
	}
	args = filtered

	if args[0] == "list" || args[0] == "ls" {
		return remoteList(asJSON)
	}
	if len(args) < 2 {
		remoteUsage()
		return 2
	}
	target, cmd, rest := args[0], args[1], args[2:]

	dev, code := resolvePeer(target)
	if code != 0 {
		return code
	}

	switch cmd {
	case "doctor", "check":
		return remoteDoctor(dev, asJSON)
	case "status":
		return remoteShow(dev, "GET", "/api/health", nil, asJSON)
	case "net", "network":
		return remoteShow(dev, "GET", "/api/system/network", nil, asJSON)
	case "boot", "boot-report":
		return remoteShow(dev, "GET", "/api/system/boot-report", nil, asJSON)
	case "logs":
		tail := "200"
		if len(rest) > 0 {
			if _, err := strconv.Atoi(rest[0]); err == nil {
				tail = rest[0]
			}
		}
		return remoteLogs(dev, tail, asJSON)
	case "vpn":
		if len(rest) == 0 {
			return remoteShow(dev, "GET", "/api/system/network", nil, asJSON)
		}
		action := strings.ToLower(rest[0])
		if action != "on" && action != "off" && action != "start" && action != "stop" {
			fmt.Fprintln(os.Stderr, "remotai remote <компьютер> vpn on|off")
			return 2
		}
		body, _ := json.Marshal(map[string]string{"action": action})
		return remoteShow(dev, "POST", "/api/system/vpn", body, asJSON)
	case "get":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "remotai remote <компьютер> get <путь>")
			return 2
		}
		return remoteShow(dev, "GET", rest[0], nil, asJSON)
	case "post":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "remotai remote <компьютер> post <путь> [json]")
			return 2
		}
		var body []byte
		if len(rest) > 1 {
			body = []byte(strings.Join(rest[1:], " "))
		}
		return remoteShow(dev, "POST", rest[0], body, asJSON)
	case "delete", "del":
		// Без DELETE половина API соседа недостижима: закрыть терминал
		// (DELETE /api/pty/{id}), убрать мёртвые сессии (/api/pty/dead), снять
		// закладку. 10.08.2026 на живом маке пять терминалов, открытых через
		// это же CLI, закрыть было НЕЧЕМ — POST-алиасов у них нет, и сессии
		// висели, пока их не убрали руками.
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "remotai remote <компьютер> delete <путь>")
			return 2
		}
		return remoteShow(dev, "DELETE", rest[0], nil, asJSON)
	default:
		remoteUsage()
		return 2
	}
}

func remoteUsage() {
	fmt.Fprintln(os.Stderr, `Использование: remotai remote <команда>

  list                          компьютеры этого аккаунта и кто из них на связи
  <компьютер> doctor            что с ним: связь, VPN, последний перерыв
  <компьютер> net               состояние сети и VPN
  <компьютер> boot              почему он пропадал в прошлый раз
  <компьютер> logs [N]          последние N строк его лога (по умолчанию 200)
  <компьютер> vpn on|off        включить/выключить на нём VPN
  <компьютер> get <путь>        произвольный GET к его API
  <компьютер> post <путь> [json] произвольный POST
  <компьютер> delete <путь>     произвольный DELETE (закрыть терминал и т.п.)

<компьютер> — идентификатор из «remotai remote list» или часть его имени.
Добавьте --json, если ответ читает программа, а не человек.

Доступ ограничивает сам компьютер: по умолчанию гостю видно состояние и
доступно управление VPN. Полный доступ включает владелец на ТОМ компьютере:
remotai config set peer_access full`)
}

type peerDevice struct {
	ID       string `json:"device_id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Platform string `json:"platform"`
	Version  string `json:"agent_version"`
	Online   bool   `json:"online"`
	LastSeen int64  `json:"last_seen"`
	Self     bool   `json:"self"`
}

// peerCreds — адрес облака и device-JWT этого устройства.
func peerCreds() (base, jwt string, err error) {
	cfg := config.GetNoSetup()
	base = strings.TrimRight(cfg.RelayHTTPBase(), "/")
	if base == "" {
		base = strings.TrimRight(config.DefaultRelayURL, "/")
	}
	jwt = cfg.RelayJWT
	if jwt == "" {
		jwt, _ = relay.LoadJWT()
	}
	if jwt == "" {
		return "", "", fmt.Errorf("это устройство не привязано к аккаунту — выполните: remotai pair")
	}
	return base, jwt, nil
}

// peerHTTP — клиент с тем же запасным резолвером, что и весь трафик агента:
// команду зовут как раз тогда, когда с сетью что-то не так.
func peerHTTP(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dnsfallback.DialContext, Proxy: http.ProxyFromEnvironment},
	}
}

func listPeers() ([]peerDevice, error) {
	base, jwt, err := peerCreds()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, base+"/v1/agent/peers", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := peerHTTP(20 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("облако ответило %d", resp.StatusCode)
	}
	var out struct {
		Devices []peerDevice `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Devices, nil
}

func remoteList(asJSON bool) int {
	devices, err := listPeers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai remote list: %v\n", err)
		return 1
	}
	if asJSON {
		data, _ := json.MarshalIndent(map[string]any{"devices": devices}, "", "  ")
		fmt.Println(string(data))
		return 0
	}
	if len(devices) == 0 {
		fmt.Println("В аккаунте нет ни одного компьютера.")
		return 0
	}
	for _, d := range devices {
		mark := "🔴"
		if d.Online {
			mark = "🟢"
		}
		self := ""
		if d.Self {
			self = "  (это я)"
		}
		name := d.Name
		if name == "" {
			name = d.Hostname
		}
		fmt.Printf("%s %-24s %s %s%s\n", mark, name, d.ID, d.Platform, self)
	}
	return 0
}

// resolvePeer превращает «часть имени» в устройство. Своё же устройство целью
// быть не может: запрос к самому себе через облако — это не диагностика, а
// петля (для себя есть `remotai status` и `remotai doctor`).
func resolvePeer(target string) (peerDevice, int) {
	devices, err := listPeers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai remote: %v\n", err)
		return peerDevice{}, 1
	}
	needle := strings.ToLower(strings.TrimSpace(target))
	var matches []peerDevice
	for _, d := range devices {
		if d.Self {
			continue
		}
		if strings.EqualFold(d.ID, needle) {
			return d, 0
		}
		if strings.Contains(strings.ToLower(d.Name), needle) || strings.Contains(strings.ToLower(d.Hostname), needle) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], 0
	case 0:
		fmt.Fprintf(os.Stderr, "Компьютер «%s» не найден. Список: remotai remote list\n", target)
		return peerDevice{}, 1
	default:
		fmt.Fprintf(os.Stderr, "Под «%s» подходит несколько компьютеров — уточните идентификатором:\n", target)
		for _, d := range matches {
			fmt.Fprintf(os.Stderr, "  %s  %s\n", d.ID, d.Name)
		}
		return peerDevice{}, 1
	}
}

// peerRequest — один запрос к соседнему компьютеру через облако.
func peerRequest(dev peerDevice, method, path string, body []byte) (int, []byte, error) {
	base, jwt, err := peerCreds()
	if err != nil {
		return 0, nil, err
	}
	payload := map[string]any{"method": method, "path": path}
	if len(body) > 0 {
		payload["body"] = body
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		base+"/v1/agent/peer/"+dev.ID+"/request", bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	resp, err := peerHTTP(75 * time.Second).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out := new(bytes.Buffer)
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes(), nil
}

func remoteShow(dev peerDevice, method, path string, body []byte, asJSON bool) int {
	code, data, err := peerRequest(dev, method, path, body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai remote: %v\n", err)
		return 1
	}
	if asJSON || code >= 300 {
		fmt.Println(strings.TrimSpace(string(data)))
		if code >= 300 {
			return 1
		}
		return 0
	}
	// Человеку — с отступами: ответы у нас JSON, но читают их и глазами.
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") == nil {
		fmt.Println(pretty.String())
	} else {
		fmt.Println(string(data))
	}
	return 0
}

func remoteLogs(dev peerDevice, tail string, asJSON bool) int {
	code, data, err := peerRequest(dev, "GET", "/api/system/logs?tail="+tail, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai remote logs: %v\n", err)
		return 1
	}
	if code >= 300 {
		fmt.Println(strings.TrimSpace(string(data)))
		return 1
	}
	if asJSON {
		fmt.Println(strings.TrimSpace(string(data)))
		return 0
	}
	var out struct {
		Lines []string `json:"lines"`
	}
	if json.Unmarshal(data, &out) != nil {
		fmt.Println(string(data))
		return 0
	}
	for _, l := range out.Lines {
		fmt.Println(l)
	}
	return 0
}

// remoteDoctor — то, ради чего команда и писалась: один вызов отвечает на
// «что с компьютером» без чтения JSON глазами.
func remoteDoctor(dev peerDevice, asJSON bool) int {
	name := dev.Name
	if name == "" {
		name = dev.Hostname
	}
	if !dev.Online {
		if asJSON {
			data, _ := json.MarshalIndent(map[string]any{"device": dev, "online": false}, "", "  ")
			fmt.Println(string(data))
			return 1
		}
		fmt.Printf("🔴 %s не на связи с облаком.\n", name)
		fmt.Println("   Если компьютер включён — на нём завис VPN или пропал интернет.")
		fmt.Println("   Дотянуться до него отсюда нельзя: команда идёт тем же каналом.")
		return 1
	}

	type section struct {
		title string
		path  string
	}
	result := map[string]any{"device": dev, "online": true}
	for _, s := range []section{
		{"boot", "/api/system/boot-report"},
		{"network", "/api/system/network"},
	} {
		code, data, err := peerRequest(dev, "GET", s.path, nil)
		if err != nil || code >= 300 {
			msg := "нет доступа"
			if err != nil {
				msg = err.Error()
			} else {
				msg = strings.TrimSpace(string(data))
			}
			result[s.title] = map[string]any{"error": msg}
			continue
		}
		var v any
		if json.Unmarshal(data, &v) == nil {
			result[s.title] = v
		}
	}

	if asJSON {
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(data))
		return 0
	}

	fmt.Printf("🟢 %s на связи (%s, v%s)\n", name, dev.Platform, dev.Version)
	if boot, ok := result["boot"].(map[string]any); ok {
		if rep, ok := boot["report"].(map[string]any); ok {
			if title, _ := rep["title"].(string); title != "" {
				fmt.Println("   " + title)
			}
			if detail, _ := rep["detail"].(string); detail != "" {
				fmt.Println("   " + detail)
			}
		} else if e, _ := boot["error"].(string); e != "" {
			fmt.Println("   перерыв: " + e)
		}
	}
	if net, ok := result["network"].(map[string]any); ok {
		if e, _ := net["error"].(string); e != "" {
			fmt.Println("   сеть: " + e)
		} else {
			online, _ := net["internet"].(bool)
			fmt.Printf("   интернет: %s\n", yesNo(online, "есть", "нет"))
			if vpn, ok := net["vpn"].(map[string]any); ok && vpn != nil {
				vname, _ := vpn["name"].(string)
				running, _ := vpn["running"].(bool)
				fmt.Printf("   VPN: %s — %s\n", vname, yesNo(running, "запущен", "выключен"))
			} else {
				fmt.Println("   VPN: не найден")
			}
			if last, ok := net["last_action"].(map[string]any); ok && last != nil {
				if txt, _ := last["text"].(string); txt != "" {
					fmt.Println("   последнее вмешательство: " + txt)
				}
			}
		}
	}
	return 0
}
