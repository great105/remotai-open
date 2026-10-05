package cdp

import (
	"encoding/json"
	"strings"
	"testing"
)

// Профиль устройства обязан быть каталожным: сайт, увидевший «Pixel 7» с
// экраном 600×1222, знает, что телефона такого не существует. Размер зрителя
// вправе диктовать только рабочий стол.
func TestDeviceProfileIgnoresViewerSizeForPhones(t *testing.T) {
	dev := DeviceProfile("android", 600, 1222, 2)
	if dev.Name != "pixel7" {
		t.Fatalf("device = %q, want pixel7 (алиас android)", dev.Name)
	}
	if dev.Width != 412 || dev.Height != 915 {
		t.Fatalf("размер %dx%d — взят у зрителя вместо каталожного", dev.Width, dev.Height)
	}
	if dev.Model == "" || dev.PlatformVersion == "" {
		t.Fatalf("профиль без модели/версии системы: %+v", dev)
	}

	desk := DeviceProfile("desktop", 1600, 900, 1)
	if desk.Width != 1600 || desk.Height != 900 {
		t.Fatalf("рабочий стол обязан брать окно зрителя, получено %dx%d", desk.Width, desk.Height)
	}
}

// User-Agent строится из НАСТОЯЩЕЙ версии Chrome и модели профиля: расхождение
// между строкой и подсказками клиента — первое, что читает детектор подмены.
func TestMobileUAKeepsRealEngineVersion(t *testing.T) {
	real := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.7000.11 Safari/537.36"

	ua := mobileUA(real, DeviceProfile("pixel7", 0, 0, 0))
	if !strings.Contains(ua, "Chrome/150.") || !strings.Contains(ua, "Pixel 7") ||
		!strings.Contains(ua, "Android 14") || !strings.Contains(ua, "Mobile Safari") {
		t.Fatalf("UA телефона собран неверно: %s", ua)
	}

	// Планшеты Android слова Mobile не несут — по нему сайты и различают вёрстку.
	tab := mobileUA(real, DeviceProfile("tablet", 0, 0, 0))
	if strings.Contains(tab, " Mobile ") {
		t.Fatalf("UA планшета содержит Mobile: %s", tab)
	}

	ios := mobileUA(real, DeviceProfile("iphone", 0, 0, 0))
	if !strings.Contains(ios, "iPhone; CPU iPhone OS 17_5") || !strings.Contains(ios, "CriOS/150.") {
		t.Fatalf("UA iPhone собран неверно: %s", ios)
	}
}

// Скрипт профиля правит ровно то, чем замер на живой машине выдавал сервер:
// WebGL от дата-центра, десктопные плагины, одно ядро.
func TestStealthSourceFixesServerGiveaways(t *testing.T) {
	src := stealthSource(DeviceProfile("pixel7", 0, 0, 0))
	if src == "" {
		t.Fatal("пустой скрипт профиля")
	}
	for _, want := range []string{"Mali-G710", "pdfViewerEnabled", "hardwareConcurrency", "37446", "maxTouchPoints"} {
		if !strings.Contains(src, want) {
			t.Fatalf("в скрипте нет %q", want)
		}
	}
	// Профиль подставляется одним JSON-литералом: любая другая склейка — дыра,
	// через которую имя устройства попадало бы прямо в код страницы.
	raw, ok := profileLiteral(src)
	if !ok {
		t.Fatal("профиль не подставлен")
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		t.Fatalf("профиль не JSON: %v (%s)", err, raw)
	}
	if cores, _ := probe["cores"].(float64); cores < 4 {
		t.Fatalf("ядер меньше четырёх — телефонов таких не бывает: %v", probe["cores"])
	}
	if gpu, _ := probe["gpuRenderer"].(string); gpu == "" {
		t.Fatal("видеокарта не задана")
	}

	// У рабочего стола плагины не выключаем: там они и должны быть.
	deskSrc := stealthSource(DeviceProfile("desktop", 1280, 800, 1))
	if !strings.Contains(deskSrc, "\"mobile\":false") {
		t.Fatalf("десктопный профиль помечен мобильным: %s", deskSrc[:200])
	}
}

// profileLiteral достаёт из скрипта строку `const P = {...};`.
func profileLiteral(src string) (string, bool) {
	const marker = "const P = "
	start := strings.Index(src, marker)
	if start < 0 {
		return "", false
	}
	rest := src[start+len(marker):]
	end := strings.Index(rest, "\n")
	if end < 0 {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimSpace(rest[:end]), ";"), true
}

func TestAcceptLanguageHeader(t *testing.T) {
	// Без q-факторов: Chrome строит из этой строки и заголовок, и
	// navigator.languages — с весами страница видела «язык en;q=0.9», какого не
	// бывает ни у одного настоящего браузера (замер на живом сервере).
	if got := AcceptLanguageHeader("en-US"); got != "en-US,en" {
		t.Fatalf("Accept-Language = %q", got)
	}
	if got := AcceptLanguageHeader("ru"); got != "ru" {
		t.Fatalf("Accept-Language = %q", got)
	}
	if got := AcceptLanguageHeader(""); got != "" {
		t.Fatalf("пустой язык дал %q", got)
	}
}
