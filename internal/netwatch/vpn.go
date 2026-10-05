package netwatch

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrNoVPN — выключать нечего: ни одного знакомого VPN-клиента не запущено.
var ErrNoVPN = errors.New("VPN-клиент на этом компьютере не найден")

// VPN — найденный клиент туннеля.
type VPN struct {
	Name      string `json:"name"`                 // как называть человеку: «Hiddify»
	PID       int    `json:"pid,omitempty"`        // процесс, который держит туннель
	Exe       string `json:"exe,omitempty"`        // чем запускать заново
	Task      string `json:"task,omitempty"`       // задача планировщика (Windows, запуск с правами)
	Running   bool   `json:"running"`              // запущен прямо сейчас
	StartHint string `json:"start_hint,omitempty"` // как включить словами
}

// knownVPN — клиенты, которые ставят системный tun-интерфейс и потому способны
// увести в никуда ВЕСЬ трафик машины, оставшись «подключёнными» на вид.
//
// СПИСОК НАМЕРЕННО КОРОТКИЙ И ЯВНЫЙ. Сторож выключает чужую программу — здесь
// не место эвристикам вроде «в имени есть vpn»: ошибка стоила бы человеку
// работающего соединения. Ключ — имя исполняемого файла в нижнем регистре.
var knownVPN = map[string]string{
	"hiddify.exe":     "Hiddify",
	"hiddifynext.exe": "Hiddify",
	"sing-box.exe":    "sing-box",
	"nekobox.exe":     "NekoBox",
	"nekoray.exe":     "NekoRay",
	"v2rayn.exe":      "v2rayN",
	"clash-verge.exe": "Clash Verge",
	// POSIX-имена (на сервере тот же sing-box запускают из консоли).
	"hiddify":  "Hiddify",
	"sing-box": "sing-box",
}

// KnownVPNNames — какие клиенты сторож умеет находить и перезапускать, без
// повторов и в стабильном порядке.
//
// Наружу это нужно интерфейсу: на вопрос «а как он выбирает, какой VPN?»
// (владелец, 09.08.2026) честный ответ — «никак не выбирает, знает вот этих
// поимённо». Список приходит с компьютера, чтобы экран не пришлось править
// вслед за каждым новым клиентом.
func KnownVPNNames() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(knownVPN))
	// Порядок фиксируем перечислением, а не обходом карты: у карты он случайный,
	// и список в интерфейсе прыгал бы при каждом запросе.
	for _, exe := range []string{
		"hiddify.exe", "sing-box.exe", "nekobox.exe", "nekoray.exe",
		"v2rayn.exe", "clash-verge.exe",
	} {
		if name, ok := knownVPN[exe]; ok && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func vpnDisplayName(exeName string) (string, bool) {
	n, ok := knownVPN[strings.ToLower(strings.TrimSpace(exeName))]
	return n, ok
}

// detectCache — снимок процессов дорог (сотни записей), а Snapshot() зовут из
// приложения на каждом открытии экрана. Десять секунд свежести достаточно:
// сторож всё равно принимает решения минутами.
var detectCache struct {
	sync.Mutex
	at  time.Time
	vpn *VPN
}

// DetectVPN возвращает запущенный VPN-клиент или nil.
func DetectVPN() *VPN {
	detectCache.Lock()
	if time.Since(detectCache.at) < 10*time.Second {
		v := detectCache.vpn
		detectCache.Unlock()
		return v
	}
	detectCache.Unlock()

	v := detectRunningVPN()
	detectCache.Lock()
	detectCache.at, detectCache.vpn = time.Now(), v
	detectCache.Unlock()
	return v
}

// invalidateDetect сбрасывает кэш: после нашего же выключения/запуска состояние
// обязано читаться заново, иначе приложение десять секунд показывает прошлое.
func invalidateDetect() {
	detectCache.Lock()
	detectCache.at = time.Time{}
	detectCache.vpn = nil
	detectCache.Unlock()
}
