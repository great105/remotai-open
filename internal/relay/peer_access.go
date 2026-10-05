package relay

import (
	"net/http"
	"strings"
)

// Что можно СОСЕДНЕМУ УСТРОЙСТВУ этого же аккаунта.
//
// Запрос с сервера приходит тем же каналом, что и запрос с телефона владельца,
// и на локальном веб-сервере авторизуется тем же api_token. Разница между ними
// существует ровно в одном месте — здесь. Поэтому правило простое и жёсткое:
//
//	по умолчанию сосед видит СОСТОЯНИЕ и умеет чинить СВЯЗЬ — и больше ничего.
//
// Терминалы, файлы, экран и настройки в режим по умолчанию не входят: сервер
// стоит в чужом дата-центре, и компрометация сервера не должна означать
// компрометацию домашнего компьютера. Полный доступ включается владельцем
// осознанно (peer_access=full) — и это опасная настройка в каталоге, то есть
// AI-агент сам себе её не включит.
const (
	PeerAccessOff  = "off"
	PeerAccessDiag = "diag"
	PeerAccessFull = "full"
)

// peerDiagGET — пути, которые сосед может ЧИТАТЬ в режиме diag.
var peerDiagGET = []string{
	"/api/health",
	"/api/version",
	"/api/system/stats",
	"/api/system/boot-report",
	"/api/system/network",
	"/api/system/logs",
	"/api/diag/connections",
	"/api/agents",
	"/api/ai-usage",
	"/api/pty", // список терминалов: имена и состояние, без содержимого
}

// peerDiagPOST — единственное, что сосед может МЕНЯТЬ в режиме diag.
//
// Почему VPN здесь: ровно ради этого доступ и заводился. Компьютер уходит из
// облака чаще всего из-за зависшего туннеля, и «дотянуться и починить» без
// права включить/выключить VPN превращается в «дотянуться и посмотреть».
var peerDiagPOST = []string{
	"/api/system/vpn",
}

// NormalizePeerAccess приводит настройку к одному из трёх значений.
// Пустая строка — режим по умолчанию (diag): доступ соседа существует сразу,
// иначе о нём никто не узнает, а заводился он для случая «уже случилось».
func NormalizePeerAccess(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case PeerAccessOff:
		return PeerAccessOff
	case PeerAccessFull:
		return PeerAccessFull
	default:
		return PeerAccessDiag
	}
}

// PeerAllowed решает судьбу запроса от соседнего устройства.
// Второе значение — фраза для отказа: она уедет на сервер как есть, и по ней
// должно быть понятно, что делать (а не «403»).
func PeerAllowed(mode, method, path string) (bool, string) {
	switch NormalizePeerAccess(mode) {
	case PeerAccessOff:
		return false, "доступ с других устройств выключен на этом компьютере (peer_access=off)"
	case PeerAccessFull:
		return true, ""
	}

	clean := path
	if i := strings.IndexByte(clean, '?'); i >= 0 {
		clean = clean[:i]
	}
	clean = strings.TrimSuffix(clean, "/")
	if clean == "" {
		clean = "/"
	}

	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead:
		for _, p := range peerDiagGET {
			if clean == p {
				return true, ""
			}
		}
	case http.MethodPost:
		for _, p := range peerDiagPOST {
			if clean == p {
				return true, ""
			}
		}
	}
	return false, "в режиме диагностики соседнему устройству доступно только состояние компьютера и управление VPN; полный доступ включает владелец: remotai config set peer_access full"
}
