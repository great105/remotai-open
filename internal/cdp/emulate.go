package cdp

// Эмуляция телефона: страница должна считать, что открыта на телефоне, а не на
// сервере в дата-центре.
//
// Без этого сайт отдаёт десктопную вёрстку: мелкие кнопки, узкие поля, меню под
// курсор мыши. Человек с телефона получал картинку чужого компьютера и должен
// был целиться пальцем в интерфейс, рассчитанный на мышь, — отсюда и ощущение
// «удалёнка», а не «браузер».
//
// Три вещи делают страницу мобильной: размеры и плотность экрана
// (setDeviceMetricsOverride), признак касаний (setTouchEmulationEnabled) и
// User-Agent с подсказками клиента (setUserAgentOverride). Порознь они не
// работают: по одному размеру сайт отдаст десктоп «в узком окне», а по одному
// User-Agent — мобильную вёрстку в неверном масштабе. Всё, что остаётся за
// пределами этих трёх (видеокарта, число ядер, плагины, часовой пояс), живёт в
// stealth.go — без него связка выглядит как телефон, который врёт.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Device — профиль устройства, которым притворяется браузер.
type Device struct {
	Name   string // ключ каталога: "pixel7" | "iphone" | "tablet" | "desktop"
	Width  int    // CSS-пиксели
	Height int
	Scale  float64 // плотность пикселей (deviceScaleFactor)
	Mobile bool
	// Platform и UA собираются из настоящей версии браузера: подставлять чужую
	// версию Chrome опаснее, чем кажется — сайты сверяют её с возможностями
	// движка, и рассинхрон выдаёт «браузер врёт» надёжнее любого User-Agent.
	Platform string // "Android" | "iOS" | "Linux"
	// Модель и версия системы: они уходят и в User-Agent, и в подсказки клиента
	// (Sec-CH-UA-Model / Sec-CH-UA-Platform-Version). Расхождение между ними —
	// первое, что проверяет любой детектор подмены.
	Model           string
	PlatformVersion string
	// Title — как устройство называется человеку в меню «Вид сайта».
	Title string
}

// deviceCatalog — устройства, которыми умеет притворяться браузер.
//
// Размеры здесь НАСТОЯЩИЕ, а не «под экран зрителя»: сайт, увидевший Pixel 7 с
// экраном 600×1222, уже знает, что его обманывают, — таких телефонов не
// существует. Кадр под экран зрителя подгоняет клиент, ему это ничего не стоит.
var deviceCatalog = map[string]Device{
	"pixel7": {
		Name: "pixel7", Width: 412, Height: 915, Scale: 2.625, Mobile: true,
		Platform: "Android", Model: "Pixel 7", PlatformVersion: "14.0.0",
		Title: "Pixel 7",
	},
	"galaxy": {
		Name: "galaxy", Width: 360, Height: 780, Scale: 3, Mobile: true,
		Platform: "Android", Model: "SM-S911B", PlatformVersion: "14.0.0",
		Title: "Galaxy S23",
	},
	"iphone": {
		Name: "iphone", Width: 393, Height: 852, Scale: 3, Mobile: true,
		Platform: "iOS", Model: "iPhone", PlatformVersion: "17.5",
		Title: "iPhone 15",
	},
	"tablet": {
		Name: "tablet", Width: 800, Height: 1280, Scale: 2, Mobile: true,
		Platform: "Android", Model: "Pixel Tablet", PlatformVersion: "14.0.0",
		Title: "Планшет",
	},
	"desktop": {
		Name: "desktop", Width: 1280, Height: 800, Scale: 1, Mobile: false,
		Platform: "Linux", Title: "Компьютер",
	},
}

// deviceAliases — имена, которыми профиль просили раньше (и продолжают просить
// старые версии приложения). Молча отдавать им «неизвестное устройство» нельзя:
// это вернуло бы десктопную вёрстку на телефон.
var deviceAliases = map[string]string{
	"android": "pixel7",
	"phone":   "pixel7",
	"ios":     "iphone",
}

// Catalog отдаёт список профилей для меню «Вид сайта» (порядок постоянный:
// человек запоминает положение пунктов).
func Catalog() []Device {
	order := []string{"pixel7", "galaxy", "iphone", "tablet", "desktop"}
	out := make([]Device, 0, len(order))
	for _, key := range order {
		out = append(out, deviceCatalog[key])
	}
	return out
}

// DeviceProfile собирает профиль по имени. Размер экрана зрителя учитывается
// только у «компьютера»: у телефонов и планшетов размеры каталожные, иначе сайт
// увидит несуществующую модель.
func DeviceProfile(name string, width, height int, scale float64) Device {
	key := strings.ToLower(strings.TrimSpace(name))
	if alias, ok := deviceAliases[key]; ok {
		key = alias
	}
	dev, ok := deviceCatalog[key]
	if !ok {
		dev = deviceCatalog["pixel7"]
	}
	if dev.Name == "desktop" {
		// У рабочего стола «настоящего размера» не бывает — берём окно зрителя.
		if width > 0 && height > 0 {
			dev.Width, dev.Height = width, height
		}
		if scale > 0 {
			dev.Scale = scale
		}
	}
	return dev
}

// chromeVersionRe вытаскивает версию из User-Agent самого браузера.
var chromeVersionRe = regexp.MustCompile(`Chrome/(\d+)[\d.]*`)

// chromeFullVersionRe — та же версия целиком (для Client Hints fullVersionList).
var chromeFullVersionRe = regexp.MustCompile(`Chrome/([\d.]+)`)

// browserUA спрашивает у браузера его собственный User-Agent.
func (c *Client) browserUA(ctx context.Context) string {
	if cached := c.realUA.Load(); cached != nil && *cached != "" {
		return *cached
	}
	raw, err := c.Call(ctx, "Browser.getVersion", nil)
	if err != nil {
		return ""
	}
	var v struct {
		UserAgent string `json:"userAgent"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	c.realUA.Store(&v.UserAgent)
	return v.UserAgent
}

// mobileUA строит мобильный User-Agent НА ОСНОВЕ настоящего: подменяем только
// платформу и модель, оставляя версию движка как есть.
func mobileUA(realUA string, dev Device) string {
	version := "126"
	if m := chromeVersionRe.FindStringSubmatch(realUA); len(m) == 2 {
		version = m[1]
	}
	switch dev.Platform {
	case "iOS":
		// Движок здесь всё равно Blink, и притворяться Safari до конца вредно:
		// сайты отдадут код под WebKit, который местами не сработает. Берём
		// «Chrome на iOS» (CriOS) — он и есть Blink-обёртка на iOS.
		return fmt.Sprintf(
			"Mozilla/5.0 (iPhone; CPU iPhone OS %s like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/%s.0.0.0 Mobile/15E148 Safari/604.1",
			strings.ReplaceAll(dev.PlatformVersion, ".", "_"), version)
	case "Android":
		androidVersion := strings.SplitN(dev.PlatformVersion, ".", 2)[0]
		if androidVersion == "" {
			androidVersion = "14"
		}
		model := dev.Model
		if model == "" {
			model = "Pixel 7"
		}
		mobileMark := " Mobile"
		if dev.Name == "tablet" {
			mobileMark = "" // планшеты Android слова Mobile в User-Agent не несут
		}
		return fmt.Sprintf(
			"Mozilla/5.0 (Linux; Android %s; %s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0%s Safari/537.36",
			androidVersion, model, version, mobileMark)
	default:
		return realUA
	}
}

// Emulate применяет профиль устройства к вкладке.
//
// acceptLanguage — язык, который уйдёт сайтам заголовком; пустая строка
// оставляет язык браузера.
func (c *Client) Emulate(ctx context.Context, dev Device) error {
	if dev.Name == "desktop" {
		// Снимаем эмуляцию телефона, но профиль оставляем: страница на сервере
		// без экрана всё равно должна выглядеть обычным компьютером, а не
		// виртуальной машиной (видеокарта, ядра — см. stealth.go).
		_, _ = c.Call(ctx, "Emulation.clearDeviceMetricsOverride", nil)
		_, _ = c.Call(ctx, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": false})
		_, _ = c.Call(ctx, "Emulation.setEmitTouchEventsForMouse", map[string]any{"enabled": false})
		_, err := c.Call(ctx, "Emulation.setUserAgentOverride", map[string]any{"userAgent": ""})
		c.device.Store(&dev)
		c.applyHardware(ctx, dev)
		if serr := c.applyStealth(ctx, dev); serr != nil {
			return serr
		}
		return err
	}

	metrics := map[string]any{
		"width":             dev.Width,
		"height":            dev.Height,
		"deviceScaleFactor": dev.Scale,
		"mobile":            dev.Mobile,
		// Полосы прокрутки в мобильном режиме браузер и так не рисует, но на
		// всякий случай просим не занимать ими место: они съедают ширину и
		// ломают вёрстку, рассчитанную на телефон.
		"screenWidth":  dev.Width,
		"screenHeight": dev.Height,
		// Ориентация: телефон в руке держат вертикально, и страницы, которые
		// смотрят на screen.orientation, должны видеть то же самое.
		"screenOrientation": map[string]any{"type": "portraitPrimary", "angle": 0},
	}
	if dev.Width > dev.Height {
		metrics["screenOrientation"] = map[string]any{"type": "landscapePrimary", "angle": 90}
	}
	if _, err := c.Call(ctx, "Emulation.setDeviceMetricsOverride", metrics); err != nil {
		return fmt.Errorf("метрики экрана: %w", err)
	}
	// РОВНО ОДНА точка касания. Замер на живой машине: с maxTouchPoints=5
	// страница сообщает navigator.maxTouchPoints = 0, то есть «касаний не
	// бывает» — Chrome отвергает значение молча, и сайты, которые определяют
	// телефон по этому числу, отдают десктопную вёрстку вопреки User-Agent.
	// С единицей число появляется сразу, без перезагрузки страницы. Пятёрку,
	// которую сообщают настоящие телефоны, дорисовывает профиль (stealth.go).
	// Щипок от этого не страдает: масштаб мы меняем не двумя касаниями, а
	// setPageScaleFactor (см. SetPageScale).
	if _, err := c.Call(ctx, "Emulation.setTouchEmulationEnabled", map[string]any{
		"enabled": true, "maxTouchPoints": 1,
	}); err != nil {
		return fmt.Errorf("касания: %w", err)
	}
	// Второй вызов не дублирует первый: setTouchEmulationEnabled включает сам
	// приём касаний, а этот переводит страницу в «мобильную» конфигурацию
	// ввода. Без него navigator.maxTouchPoints остаётся нулём (проверено на
	// живом сервере), а часть сайтов определяет телефон именно по нему и
	// отдаёт десктопную вёрстку вопреки User-Agent.
	if _, err := c.Call(ctx, "Emulation.setEmitTouchEventsForMouse", map[string]any{
		"enabled": true, "configuration": "mobile",
	}); err != nil {
		return fmt.Errorf("мобильный ввод: %w", err)
	}
	// Сайту мало User-Agent: современные проверки читают подсказки клиента
	// (Client Hints), и без них Chrome честно сообщит «Linux, не мобильный» —
	// половина сайтов после этого отдаст десктопную вёрстку вопреки UA.
	// Связка обязана быть полной: UA без brands/fullVersionList — классический
	// детект подмены (настоящий Chrome шлёт Sec-CH-UA с брендами).
	realUA := c.browserUA(ctx)
	ua := mobileUA(realUA, dev)
	chromeVersion := "150.0.0.0"
	if m := chromeFullVersionRe.FindStringSubmatch(realUA); len(m) == 2 {
		chromeVersion = m[1]
	}
	major := strings.SplitN(chromeVersion, ".", 2)[0]
	brands := []map[string]any{
		{"brand": "Not_A Brand", "version": "99"},
		{"brand": "Google Chrome", "version": major},
		{"brand": "Chromium", "version": major},
	}
	fullVersionList := []map[string]any{
		{"brand": "Not_A Brand", "version": "99.0.0.0"},
		{"brand": "Google Chrome", "version": chromeVersion},
		{"brand": "Chromium", "version": chromeVersion},
	}
	params := map[string]any{
		"userAgent": ua,
		"platform":  dev.Platform,
		"userAgentMetadata": map[string]any{
			"platform":        dev.Platform,
			"platformVersion": dev.PlatformVersion,
			"architecture":    "",
			"bitness":         "64",
			"model":           dev.Model,
			"mobile":          dev.Mobile,
			"brands":          brands,
			"fullVersionList": fullVersionList,
		},
	}
	if lang := c.acceptLanguage.Load(); lang != nil && *lang != "" {
		params["acceptLanguage"] = *lang
	}
	if _, err := c.Call(ctx, "Emulation.setUserAgentOverride", params); err != nil {
		return fmt.Errorf("user-agent: %w", err)
	}
	c.device.Store(&dev)
	// Ядра и профиль «железа» — вторая половина правды об устройстве. Ошибки
	// здесь не отменяют мобильную вёрстку, поэтому они внутри логируются.
	c.applyHardware(ctx, dev)
	return c.applyStealth(ctx, dev)
}

// SetAcceptLanguage запоминает язык, который уйдёт сайтам заголовком
// Accept-Language при следующем применении профиля.
func (c *Client) SetAcceptLanguage(lang string) {
	if lang == "" {
		return
	}
	c.acceptLanguage.Store(&lang)
}

// CurrentDevice — какой профиль применён (для интерфейса).
func (c *Client) CurrentDevice() Device {
	if d := c.device.Load(); d != nil {
		return *d
	}
	return Device{}
}

// SetPageScale задаёт масштаб страницы — это щипок-зум.
//
// Настоящий жест из двух пальцев (synthesizePinchGesture) на виртуальном
// экране не работает: он требует настоящего дисплея, и на Xvfb молча ничего не
// делает. Масштаб страницей — то же самое для человека и работает везде.
func (c *Client) SetPageScale(ctx context.Context, scale float64) error {
	if scale < 0.25 {
		scale = 0.25
	}
	if scale > 5 {
		scale = 5
	}
	_, err := c.Call(ctx, "Emulation.setPageScaleFactor", map[string]any{"pageScaleFactor": scale})
	return err
}
