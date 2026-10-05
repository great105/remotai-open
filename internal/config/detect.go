package config

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/procutil"
)

// SystemCapabilities holds auto-detected system info for the setup wizard.
type SystemCapabilities struct {
	DeviceInfo *DeviceInfo     `json:"device_info"`
	Agents     []DetectedAgent `json:"agents"`
	NetworkIPs []string        `json:"network_ips"`
	Platform   string          `json:"platform"`
	IsAdmin    bool            `json:"is_admin"`
	HasTunnel  bool            `json:"has_tunnel"`
	Tunnel     *TunnelInfo     `json:"tunnel"`
}

// TunnelInfo describes the cloudflared installation status.
type TunnelInfo struct {
	Installed bool   `json:"installed"`
	Path      string `json:"path"`
	Version   string `json:"version"`
}

// DetectedAgent describes a CLI agent found on the system.
type DetectedAgent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	Path    string `json:"path"`
	Version string `json:"version"`
	BuiltIn bool   `json:"built_in"`
}

// KnownCLIAgent describes a CLI agent to look for during detection.
type KnownCLIAgent struct {
	ID       string
	Name     string
	Icon     string
	CLINames []string // binary names to search in PATH
	BuiltIn  bool
}

// KnownAgents lists all agents to detect during setup.
var KnownAgents = []KnownCLIAgent{
	{ID: "claude", Name: "Claude Code", Icon: "\U0001F7E0", CLINames: []string{"claude"}},
	{ID: "codex", Name: "Codex CLI", Icon: "\U0001F7E2", CLINames: []string{"codex"}},
	{ID: "aider", Name: "Aider", Icon: "\U0001F535", CLINames: []string{"aider"}},
	{ID: "gemini", Name: "Gemini CLI", Icon: "\U0001F537", CLINames: []string{"gemini"}},
	{ID: "amazon-q", Name: "Amazon Q", Icon: "\U0001F7E1", CLINames: []string{"q"}},
	{ID: "copilot", Name: "GitHub Copilot", Icon: "\u26AB", CLINames: []string{"github-copilot-cli", "copilot"}},
	{ID: "opencode", Name: "OpenCode", Icon: "\U0001F7E3", CLINames: []string{"opencode"}},
	{ID: "cline", Name: "Cline CLI", Icon: "\U0001F7E4", CLINames: []string{"cline"}},
	{ID: "kilo", Name: "Kilo Code", Icon: "\U0001F534", CLINames: []string{"kilo"}},
	{ID: "cursor-agent", Name: "Cursor Agent", Icon: "\u2B1B", CLINames: []string{"cursor-agent"}},
	{ID: "shell", Name: "Shell", Icon: "\U0001F41A", BuiltIn: true},
	{ID: "orchestrator", Name: "Orchestrator", Icon: "\U0001F3AF", BuiltIn: true},
	{ID: "researcher", Name: "Researcher", Icon: "\U0001F52C", BuiltIn: true},
}

// DetectCapabilities runs all system checks and returns results.
func DetectCapabilities() *SystemCapabilities {
	ti := detectTunnelInfo()
	caps := &SystemCapabilities{
		DeviceInfo: GetDeviceInfo(),
		Platform:   runtime.GOOS,
		NetworkIPs: detectNetworkIPs(),
		IsAdmin:    detectIsAdmin(),
		HasTunnel:  ti.Installed,
		Tunnel:     ti,
	}

	caps.Agents = detectAgents()
	return caps
}

func detectAgents() []DetectedAgent {
	var result []DetectedAgent
	for _, known := range KnownAgents {
		agent := DetectedAgent{
			ID:      known.ID,
			Name:    known.Name,
			Icon:    known.Icon,
			BuiltIn: known.BuiltIn,
		}

		if known.BuiltIn {
			agent.Path = "built-in"
			result = append(result, agent)
			continue
		}

		for _, cliName := range known.CLINames {
			if path := FindCLI(cliName); path != "" {
				agent.Path = path
				agent.Version = getAgentVersion(cliName, path)
				break
			}
		}

		result = append(result, agent)
	}
	return result
}

// agentVersionEntry — запомненный ответ агента о своей версии.
type agentVersionEntry struct {
	version string
	size    int64     // размер бинаря на момент опроса
	mod     time.Time // время правки бинаря на момент опроса
	at      time.Time // когда опрашивали
}

var (
	agentVersionMu    sync.Mutex
	agentVersionCache = map[string]agentVersionEntry{}
)

// agentVersionTTL — страховка на случай, когда сам файл не изменился, а версия
// внутри уехала: на Windows `claude`/`codex`/`npm` в PATH — это .cmd-обёртки,
// и обновление npm-пакета их не трогает.
const agentVersionTTL = 6 * time.Hour

// getAgentVersion возвращает версию агента, по возможности из памяти.
//
// Раньше на КАЖДОЕ открытие «Панели ПК» для каждого найденного агента
// запускалось до трёх процессов подряд (`--version`, `-v`, `version`) — до двух
// десятков запусков за один заход. Версия агента между открытиями панели не
// меняется, поэтому ответ (в том числе пустой — агент не отвечает ни на один
// флаг, и это самый дорогой случай: три запуска впустую) запоминается по пути
// к бинарю. Запись теряет силу, если файл изменился или устарела по TTL.
func getAgentVersion(name, path string) string {
	fi, statErr := os.Stat(path)
	now := time.Now()

	if statErr == nil {
		agentVersionMu.Lock()
		e, ok := agentVersionCache[path]
		agentVersionMu.Unlock()
		if ok && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) && now.Sub(e.at) < agentVersionTTL {
			return e.version
		}
	}

	version := probeAgentVersion(path)

	if statErr == nil {
		agentVersionMu.Lock()
		agentVersionCache[path] = agentVersionEntry{version: version, size: fi.Size(), mod: fi.ModTime(), at: now}
		agentVersionMu.Unlock()
	}
	return version
}

// probeAgentVersion спрашивает у бинаря его версию.
//
// Окно гасим: это фоновая проба, смотреть человеку не на что, а видел он ровно
// вспышки этих окон («сами открываются пустые терминалы»).
func probeAgentVersion(path string) string {
	// Try --version first, then -v
	for _, flag := range []string{"--version", "-v", "version"} {
		// Таймаут обязателен: `--version` у CLI-агента иногда лезет в сеть или
		// ждёт входа, и без ограничения одна зависшая проба вешала бы весь ответ
		// панели («Панель ПК» грузится молча и бесконечно).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := procutil.Hidden(exec.CommandContext(ctx, path, flag)).Output()
		cancel()
		if err == nil {
			version := strings.TrimSpace(string(out))
			// Take first line only
			if idx := strings.IndexByte(version, '\n'); idx > 0 {
				version = version[:idx]
			}
			// Limit length
			if len(version) > 100 {
				version = version[:100]
			}
			if version != "" {
				return version
			}
		}
	}
	return ""
}

func detectNetworkIPs() []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			ips = append(ips, ip.String())
		}
	}
	return ips
}

func detectIsAdmin() bool {
	if runtime.GOOS == "windows" {
		// Try to open a privileged registry key
		// Окно гасим: `net session` — фоновая проба прав, её вспышка на панели
		// выглядит как «сам открылся пустой терминал».
		_, err := procutil.Hidden(exec.Command("net", "session")).Output()
		return err == nil
	}
	// Linux/Mac: check if uid == 0
	out, err := procutil.Hidden(exec.Command("id", "-u")).Output()
	return err == nil && strings.TrimSpace(string(out)) == "0"
}

func detectTunnelInfo() *TunnelInfo {
	ti := &TunnelInfo{}

	// Check next to executable first
	exe, _ := os.Executable()
	if exe != "" {
		name := "cloudflared"
		if runtime.GOOS == "windows" {
			name = "cloudflared.exe"
		}
		local := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(local); err == nil {
			ti.Installed = true
			ti.Path = local
		}
	}

	// Fallback: check PATH
	if !ti.Installed {
		if path := FindCLI("cloudflared"); path != "" {
			ti.Installed = true
			ti.Path = path
		}
	}

	if ti.Installed && ti.Path != "" {
		// Окно гасим: версию cloudflared спрашиваем для панели, а не для показа.
		out, err := procutil.Hidden(exec.Command(ti.Path, "--version")).Output()
		if err == nil {
			v := strings.TrimSpace(string(out))
			if idx := strings.IndexByte(v, '\n'); idx > 0 {
				v = v[:idx]
			}
			ti.Version = v
		}
	}

	return ti
}
