package cdp

// Чтобы на этом браузере можно было ЗАРЕГИСТРИРОВАТЬСЯ.
//
// Эмуляция (emulate.go) отвечает на вопрос «какой ширины экран и что в
// User-Agent». Сайту этого мало: он смотрит на десяток мелочей, которые у
// настоящего телефона выглядят одним образом, а у браузера на сервере — другим.
// Замер на живой машине (Chrome 150 с профилем Pixel 7, 2026-07-30) показал
// ровно, чем мы себя выдавали:
//
//	WebGL          не работает вовсе  → у любого телефона он есть всегда
//	plugins        5, pdfViewer true  → на Android Chrome их 0 и false
//	cores          1                  → у Pixel 7 их 8
//	deviceMemory   2                  → 8
//	maxTouchPoints 1                  → 5 (Chrome молча отвергает 5 в эмуляции)
//	timezone       UTC при IP в NL    → Europe/Amsterdam
//
// Каждая строка по отдельности — мелочь. Вместе это «браузер, который врёт про
// телефон», и защита сайта (Cloudflare, hCaptcha, антифрод регистрации) отвечает
// бесконечной проверкой или отказом. Здесь мы приводим показания в порядок:
// WebGL включается настоящим софтверным рендером (флаг браузера), а остальное
// правится крошечным скриптом, который выполняется в КАЖДОМ документе до кода
// страницы.
//
// Границы: это не «обход защиты», а согласование показаний с тем, чем мы
// притворяемся по User-Agent. Мы не скрываем факт автоматизации там, где его
// нет: браузером управляет живой человек с телефона.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// gpuProfile — как страница видит видеокарту. Настоящий рендер всё равно
// софтверный (SwiftShader): подменяются только строки, по которым сайт узнаёт
// «это виртуалка без экрана».
type gpuProfile struct {
	Vendor   string
	Renderer string
}

// gpuFor подбирает видеокарту под устройство: у телефона она своя, и «ANGLE
// (Google, SwiftShader)» рядом с User-Agent Pixel 7 — это противоречие, которое
// антифрод читает первым.
func gpuFor(dev Device) gpuProfile {
	switch dev.Platform {
	case "iOS":
		return gpuProfile{Vendor: "Apple Inc.", Renderer: "Apple GPU"}
	case "Android":
		if dev.Name == "tablet" {
			return gpuProfile{Vendor: "ARM", Renderer: "Mali-G610 MC4"}
		}
		// Pixel 7 — Tensor G2, у него Mali-G710.
		return gpuProfile{Vendor: "ARM", Renderer: "Mali-G710"}
	default:
		// Рабочий стол на сервере: обычная встроенная видеокарта Intel выглядит
		// куда обыденнее, чем SwiftShader.
		return gpuProfile{
			Vendor:   "Google Inc. (Intel)",
			Renderer: "ANGLE (Intel, Mesa Intel(R) UHD Graphics 630 (CFL GT2), OpenGL 4.6)",
		}
	}
}

// hardwareFor — сколько у устройства ядер и памяти. Сервер обычно одноядерный,
// и `navigator.hardwareConcurrency === 1` на «телефоне» — редкость, которую
// антифрод считает признаком виртуальной машины.
func hardwareFor(dev Device) (cores, memory, touchPoints int) {
	switch {
	case dev.Platform == "iOS":
		return 6, 8, 5
	case dev.Mobile && dev.Name == "tablet":
		return 8, 8, 5
	case dev.Mobile:
		return 8, 8, 5
	default:
		return 8, 8, 0
	}
}

// stealthSource собирает скрипт под конкретный профиль. Всё, что он делает, —
// приводит несколько свойств navigator/WebGL к значениям устройства, которым мы
// представляемся. Скрипт обязан быть безопасным при повторном запуске: он
// выполняется и на новых документах, и на уже открытой странице.
func stealthSource(dev Device) string {
	gpu := gpuFor(dev)
	cores, memory, touchPoints := hardwareFor(dev)
	profile := map[string]any{
		"mobile":      dev.Mobile,
		"platform":    dev.Platform,
		"gpuVendor":   gpu.Vendor,
		"gpuRenderer": gpu.Renderer,
		"cores":       cores,
		"memory":      memory,
		"touchPoints": touchPoints,
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(stealthTemplate, string(raw))
}

// stealthTemplate — сам скрипт. %s подставляет профиль устройства.
//
// Правки делаются через defineProperty на прототипах: страница, которая
// проверяет `Object.getOwnPropertyDescriptor(navigator, 'plugins')`, увидит
// обычную картину, а не собственное свойство экземпляра.
const stealthTemplate = `(() => {
  if (window.__remotaiStealth) return;
  window.__remotaiStealth = true;
  const P = %s;

  // Подменённая функция обязана и ВЫГЛЯДЕТЬ подменённой не больше, чем
  // настоящая: страница читает Function.prototype.toString у геттера, и
  // «() => value» вместо «function get plugins() { [native code] }» — готовый
  // признак вмешательства (замер на живой машине это и показал). Держим для
  // своих функций табличку с их «родным» текстом.
  const nativeToString = Function.prototype.toString;
  const masks = new WeakMap();
  const mask = (fn, text) => { try { masks.set(fn, text); } catch (e) {} return fn; };
  try {
    const patched = new Proxy(nativeToString, {
      apply(target, thisArg, args) {
        if (masks.has(thisArg)) return masks.get(thisArg);
        return Reflect.apply(target, thisArg, args);
      },
    });
    // Сам подменённый toString тоже обязан выглядеть нативным.
    masks.set(patched, nativeToString.call(nativeToString));
    Function.prototype.toString = patched;
  } catch (e) {}

  const def = (obj, name, value) => {
    try {
      const get = mask(() => value, 'function get ' + name + '() { [native code] }');
      Object.defineProperty(obj, name, { get, configurable: true, enumerable: true });
    } catch (e) {}
  };

  // 1. Признак автоматизации. Chrome без --enable-automation его и так не
  //    ставит, но страницы читают его первым — пусть ответ будет однозначным.
  try {
    if (navigator.webdriver) def(Navigator.prototype, 'webdriver', false);
  } catch (e) {}

  // 2. Ядра и память. На телефоне их 8, у сервера бывает одно — по этой паре
  //    «мобильный» браузер узнаётся как виртуальная машина.
  if (navigator.hardwareConcurrency !== P.cores) def(Navigator.prototype, 'hardwareConcurrency', P.cores);
  if (navigator.deviceMemory !== undefined && navigator.deviceMemory !== P.memory) {
    def(Navigator.prototype, 'deviceMemory', P.memory);
  }

  // 3. Касания. Chrome принимает в эмуляции ровно одну точку (пять он молча
  //    отвергает и оставляет ноль), а телефоны сообщают пять.
  if (P.touchPoints && navigator.maxTouchPoints < P.touchPoints) {
    def(Navigator.prototype, 'maxTouchPoints', P.touchPoints);
  }

  // 4. Плагины. У Chrome на Android их нет вовсе, а у нас — пять десктопных
  //    (встроенный просмотр PDF). Это прямое противоречие User-Agent.
  if (P.mobile) {
    try {
      const emptyList = (proto, name) => {
        const list = Object.create(proto);
        Object.defineProperty(list, 'length', {
          get: mask(() => 0, 'function get length() { [native code] }'),
          configurable: true,
        });
        list.item = mask(() => null, 'function item() { [native code] }');
        list.namedItem = mask(() => null, 'function namedItem() { [native code] }');
        if (name === 'plugins') list.refresh = mask(() => undefined, 'function refresh() { [native code] }');
        return list;
      };
      def(Navigator.prototype, 'plugins', emptyList(PluginArray.prototype, 'plugins'));
      def(Navigator.prototype, 'mimeTypes', emptyList(MimeTypeArray.prototype, 'mimeTypes'));
      def(Navigator.prototype, 'pdfViewerEnabled', false);
    } catch (e) {}
  }

  // 5. Видеокарта. Строки берутся из профиля устройства: рядом с «Pixel 7»
  //    обязан стоять Mali, а не программный растеризатор дата-центра.
  const patchGL = (proto) => {
    if (!proto || proto.__remotaiGL) return;
    const original = proto.getParameter;
    if (typeof original !== 'function') return;
    const patched = function (id) {
      if (id === 37445) return P.gpuVendor;   // UNMASKED_VENDOR_WEBGL
      if (id === 37446) return P.gpuRenderer; // UNMASKED_RENDERER_WEBGL
      return original.apply(this, arguments);
    };
    mask(patched, nativeToString.call(original));
    proto.getParameter = patched;
    proto.__remotaiGL = true;
  };
  patchGL(window.WebGLRenderingContext && WebGLRenderingContext.prototype);
  patchGL(window.WebGL2RenderingContext && WebGL2RenderingContext.prototype);

  // 6. Батарея. Телефон, вечно заряженный под завязку и всегда включённый в
  //    розетку, — редкость: настоящие устройства показывают что-то среднее.
  //    Значение постоянно, иначе оно скакало бы между проверками.
  if (P.mobile && navigator.getBattery) {
    try {
      const battery = {
        charging: false, chargingTime: Infinity, dischargingTime: 12600, level: 0.72,
        addEventListener() {}, removeEventListener() {}, dispatchEvent() { return false; },
        onchargingchange: null, onchargingtimechange: null,
        ondischargingtimechange: null, onlevelchange: null,
      };
      const getBattery = mask(() => Promise.resolve(battery),
        'function getBattery() { [native code] }');
      Object.defineProperty(Navigator.prototype, 'getBattery', {
        value: getBattery, configurable: true, writable: true,
      });
    } catch (e) {}
  }

  // 7. Камеры и микрофоны. У телефона они есть всегда; пустой список
  //    enumerateDevices — признак сервера. Метки остаются пустыми, как и без
  //    выданного разрешения: это честное поведение настоящего браузера.
  if (P.mobile && navigator.mediaDevices && navigator.mediaDevices.enumerateDevices) {
    const fake = [
      { deviceId: 'default', kind: 'audioinput', label: '', groupId: 'g1' },
      { deviceId: 'default', kind: 'audiooutput', label: '', groupId: 'g1' },
      { deviceId: 'front', kind: 'videoinput', label: '', groupId: 'g2' },
      { deviceId: 'back', kind: 'videoinput', label: '', groupId: 'g3' },
    ];
    const real = navigator.mediaDevices.enumerateDevices.bind(navigator.mediaDevices);
    navigator.mediaDevices.enumerateDevices = function () {
      return real().then((list) => (list && list.length ? list : fake.map((d) => ({
        ...d, toJSON() { return d; },
      }))));
    };
  }
})()`

// applyStealth ставит скрипт профиля: и на будущие документы, и на страницу,
// открытую прямо сейчас. Прошлый скрипт снимается — иначе после смены профиля
// (телефон → компьютер) в документе оказались бы оба, и страница получила бы
// показания вперемешку.
func (c *Client) applyStealth(ctx context.Context, dev Device) error {
	source := stealthSource(dev)
	if source == "" {
		return nil
	}
	// Скрипт «на новый документ» Chrome применяет к каждой странице сам:
	// переставлять его на каждой навигации (а профиль переприменяется именно
	// там) — лишняя пара вызовов на каждый переход.
	if cur := c.stealthScript.Load(); cur != nil {
		if name, _, ok := strings.Cut(*cur, "\x00"); ok && name == dev.Name {
			return nil
		}
		if _, id, ok := strings.Cut(*cur, "\x00"); ok && id != "" {
			if _, err := c.Call(ctx, "Page.removeScriptToEvaluateOnNewDocument", map[string]any{
				"identifier": id,
			}); err != nil {
				log.Printf("[CDP] прошлый профиль не снят: %v", err)
			}
		}
		c.stealthScript.Store(nil)
	}
	raw, err := c.Call(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": source,
	})
	if err != nil {
		return err
	}
	var res struct {
		Identifier string `json:"identifier"`
	}
	if json.Unmarshal(raw, &res) == nil && res.Identifier != "" {
		marker := dev.Name + "\x00" + res.Identifier
		c.stealthScript.Store(&marker)
	}
	// Открытая страница скрипт «на новый документ» не увидит — выполняем и на
	// ней: человек мог включить мобильный вид, уже стоя на сайте.
	if _, err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": source, "returnByValue": true,
	}); err != nil {
		// Не повод считать профиль непринятым: следующая же навигация всё
		// расставит по местам.
		log.Printf("[CDP] профиль на текущей странице: %v", err)
	}
	return nil
}

// ApplyEnvironment согласует часовой пояс и язык с тем, откуда браузер выходит
// в сеть. Сервер живёт по UTC, а его адрес — например, амстердамский: сайт
// видит «телефон в Нидерландах с часами по Гринвичу» и считает это подделкой.
// Ошибки здесь не фатальны: страница откроется и без правильных часов.
func (c *Client) ApplyEnvironment(ctx context.Context, timezone, locale string) {
	if timezone != "" {
		if _, err := c.Call(ctx, "Emulation.setTimezoneOverride", map[string]any{
			"timezoneId": timezone,
		}); err != nil {
			log.Printf("[CDP] часовой пояс %q: %v", timezone, err)
		}
	}
	if locale != "" {
		if _, err := c.Call(ctx, "Emulation.setLocaleOverride", map[string]any{
			"locale": locale,
		}); err != nil {
			log.Printf("[CDP] язык %q: %v", locale, err)
		}
	}
}

// applyHardware сообщает странице число ядер. Это единственная из «железных»
// характеристик, у которой есть собственная команда протокола: остальное правит
// скрипт профиля.
func (c *Client) applyHardware(ctx context.Context, dev Device) {
	cores, _, _ := hardwareFor(dev)
	if cores <= 0 {
		return
	}
	if _, err := c.Call(ctx, "Emulation.setHardwareConcurrencyOverride", map[string]any{
		"hardwareConcurrency": cores,
	}); err != nil {
		log.Printf("[CDP] число ядер: %v", err)
	}
}

// AcceptLanguageHeader — заголовок Accept-Language под язык интерфейса
// браузера. Без него страница получала бы язык системы сервера («C», то есть
// английский) вне зависимости от того, что мы объявили в эмуляции.
//
// БЕЗ q-факторов. Chrome строит из этой строки и заголовок, и
// navigator.languages: замер на живом сервере показал ровно то, чего быть не
// должно — `['en-US', 'en;q=0.9']`, то есть «язык под названием en;q=0.9».
// Такого не бывает ни у одного настоящего браузера, и это заметнее, чем сам
// неверный язык. Веса Chrome расставит сам.
func AcceptLanguageHeader(locale string) string {
	locale = strings.TrimSpace(locale)
	if locale == "" {
		return ""
	}
	base := locale
	if i := strings.IndexAny(locale, "-_"); i > 0 {
		base = locale[:i]
	}
	if base == locale {
		return locale
	}
	return fmt.Sprintf("%s,%s", locale, base)
}
