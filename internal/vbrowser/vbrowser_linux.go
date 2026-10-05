//go:build linux

// Package vbrowser runs a virtual display (Xvfb) with a browser on it, so a
// headless Linux agent can offer Remote Desktop browsing ("open sites from
// the server"). The display env of the agent process is switched to the
// virtual display for the session lifetime: screenshot capture and xdotool
// input read DISPLAY at call time, so RD picks the virtual screen up without
// an agent restart. Starting is refused when a real display already exists —
// native RD serves that case.
package vbrowser

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"tgcontrol/internal/paths"
)

// Границы виртуального экрана. Меньше — интерфейс сайтов ломается, больше —
// растёт цена каждого кадра (захват копирует W×H×4 байта), а на VPS с одним
// ядром это ядро нужно самому браузеру.
const (
	minScreenSide = 480
	maxScreenSide = 2048
)

// sanitizeScreen приводит запрошенный размер к рабочему: пустой — привычные
// 1280×800, прочие — в границах и с чётными сторонами (нечётную ширину не любят
// ни кодеки, ни масштабирование).
func sanitizeScreen(width, height int) (int, int) {
	if width <= 0 || height <= 0 {
		return 1280, 800
	}
	clamp := func(v int) int {
		if v < minScreenSide {
			return minScreenSide
		}
		if v > maxScreenSide {
			return maxScreenSide
		}
		return v &^ 1
	}
	return clamp(width), clamp(height)
}

// defaultBrowserLang — язык браузера на сервере, если клиент не задал свой.
const defaultBrowserLang = "en-US"

// browserCandidates maps a preference name to the binaries tried in order.
var browserCandidates = map[string][]string{
	"chromium": {"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"},
	"firefox":  {"firefox"},
}

// session describes a running virtual display + browser pair.
type session struct {
	display    string // ":99"
	xCmd       *exec.Cmd
	browserCmd *exec.Cmd
	browser    string // resolved binary name
	startedAt  time.Time
	debugPort  int // порт CDP, только на 127.0.0.1
	width      int // размер виртуального экрана — он же размер окна браузера
	height     int

	// Relaunch params, captured at Start so the supervisor can bring a crashed
	// process back with the same profile, flags, display and debug port.
	xvfbPath string
	xArgs    []string
	bin      string   // resolved browser binary path
	bArgs    []string // full browser flag set, including the debug port
	env      []string // browser environment (DISPLAY + LANGUAGE)
	// cred — пользователь браузера (вместо --no-sandbox от root). Без него
	// рестарт супервизора запускал Chrome от root, и тот умирал на проверке
	// zygote «Running as root without --no-sandbox» (живой случай 2.46.0).
	cred *syscall.Credential

	// Supervisor state. restarts is the shared health budget of the pair;
	// done cancels a pending backoff restart and the idle ticker on Stop.
	restarts    int
	lastErr     string
	xvfbUpAt    time.Time
	browserUpAt time.Time
	done        chan struct{}
	events      chan procEvent

	// Stderr живого браузера (только хвост): падение GPU/рендерера объясняет
	// себя именно там — до этого «не работают касания» было глухой загадкой.
	bErr *errRing

	// Idle stop state: the session ends itself when nobody watches the stream
	// and nothing (input, navigation, keepalive) touches it for idleTimeout().
	lastActivity time.Time
	viewers      int
	idleWarned   bool

	// Файлы, выбранные человеком для <input type=file>. Они должны жить до
	// конца браузерной сессии: сайт может прочитать File не в момент выбора, а
	// только при отправке формы. На Stop каталоги удаляются целиком.
	uploadDirs []string
}

// procEvent reports an exited child to the supervise loop; cmd identifies
// which generation of the process died (late events of killed processes are
// stale and ignored).
type procEvent struct {
	kind string // "browser" or "xvfb"
	cmd  *exec.Cmd
}

var (
	mu           sync.Mutex
	sess         *session
	savedDisplay string // agent DISPLAY captured at first Start (usually "")
	starting     bool   // start in flight (guard against double-tap)

	// Terminal failure of the last session, kept for Status after the session
	// itself is gone — the panel must show WHY the browser disappeared.
	stoppedErr      string
	stoppedRestarts int
)

// Status is the JSON view of the feature state.
type Status struct {
	Available bool     `json:"available"` // Xvfb found AND at least one browser
	XvfbPath  string   `json:"xvfb_path,omitempty"`
	Browsers  []string `json:"browsers,omitempty"` // resolvable browser binaries
	Running   bool     `json:"running"`
	Display   string   `json:"display,omitempty"`
	Browser   string   `json:"browser,omitempty"`
	Since     int64    `json:"since,omitempty"` // unix ms
	Hint      string   `json:"hint,omitempty"`  // install hint when unavailable

	// Ввод. Экран без xdotool показывается, но не принимает ни одного нажатия —
	// живой случай владельца: Xvfb и google-chrome на сервере были, xdotool не
	// было, и человек получил картинку, по которой нельзя кликнуть, с советом
	// «установите его на компьютере» вместо кнопки.
	InputReady      bool   `json:"input_ready"`
	InputHint       string `json:"input_hint,omitempty"`        // команда установки для ручного пути
	CanInstallInput bool   `json:"can_install_input,omitempty"` // агент поставит сам, по кнопке
	InputInstalling bool   `json:"input_installing,omitempty"`  // установка идёт прямо сейчас
	InputError      string `json:"input_error,omitempty"`       // чем кончилась прошлая попытка

	// Порт протокола отладки Chrome — ТОЛЬКО на 127.0.0.1. По нему агент берёт
	// кадры у самого браузера (дешевле захвата экрана) и шлёт ввод настоящими
	// событиями страницы. Наружу порт не публикуется никогда.
	DebugPort int `json:"debug_port,omitempty"`

	// Supervisor bookkeeping: how many restarts the current session has spent
	// and the last failure (empty while everything runs fine). After a
	// budget-exhausted stop these keep the terminal error of the dead session.
	Restarts  int    `json:"restarts"`
	LastError string `json:"last_error,omitempty"`

	// Звук: null-sink поднят и libopus на месте — браузеру можно отдавать
	// PULSE_SINK, а WebRTC-сессии — аудиотрек. audio_hint — короткая подсказка,
	// когда звуку мешает отсутствие pactl (пакеты ставятся руками, демон
	// pulse/pipewire должен отвечать).
	Audio     bool   `json:"audio"`
	AudioHint string `json:"audio_hint,omitempty"`
}

// findXvfb locates the Xvfb binary.
func findXvfb() string {
	if p, err := exec.LookPath("Xvfb"); err == nil {
		return p
	}
	return ""
}

// detectBrowsers returns the resolvable browser binaries (deduped, preferred
// order: chromium family, then firefox).
func detectBrowsers() []string {
	seen := map[string]bool{}
	var out []string
	for _, names := range browserCandidates {
		for _, n := range names {
			if seen[n] {
				continue
			}
			if _, err := exec.LookPath(n); err == nil {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// GetStatus reports the current feature state.
func GetStatus() Status {
	mu.Lock()
	defer mu.Unlock()
	return statusLocked()
}

func statusLocked() Status {
	xvfb := findXvfb()
	browsers := detectBrowsers()
	st := Status{
		Available: xvfb != "" && len(browsers) > 0,
		XvfbPath:  xvfb,
		Browsers:  browsers,
	}
	if !st.Available {
		st.Hint = "sudo apt install xvfb chromium-browser xdotool fonts-liberation fonts-noto-color-emoji libopenh264-8 pulseaudio-utils libopus0"
	}
	st.Audio = AudioAvailable()
	if _, err := exec.LookPath("pactl"); err != nil {
		st.AudioHint = "sudo apt install pulseaudio-utils libopus0 (и запущенный pulse/pipewire)"
	}
	st.InputReady = FindInputTool() != ""
	if !st.InputReady {
		pm := detectPkgManager()
		st.InputHint = manualInstallCmd(pm)
		st.CanInstallInput = pm != nil && installPrivilege(pm) != nil
		busy, errText, _ := inputInstallState()
		st.InputInstalling = busy
		st.InputError = errText
	}
	if sess != nil {
		st.Running = true
		st.Display = sess.display
		st.Browser = sess.browser
		st.Since = sess.startedAt.UnixMilli()
		st.DebugPort = sess.debugPort
		st.Restarts = sess.restarts
		st.LastError = sess.lastErr
	} else {
		// The session is gone but its terminal failure is still worth showing.
		st.Restarts = stoppedRestarts
		st.LastError = stoppedErr
	}
	return st
}

// Running reports whether a virtual display session is active (cheap, for
// gating in hot paths).
func Running() bool {
	mu.Lock()
	defer mu.Unlock()
	return sess != nil
}

// displayFree reports whether X display number n looks usable: no live socket
// and no live lock owner. Stale sockets/locks (dead owner) are removed.
func displayFree(n int) bool {
	sock := fmt.Sprintf("/tmp/.X11-unix/X%d", n)
	lock := fmt.Sprintf("/tmp/.X%d-lock", n)

	if _, err := os.Stat(sock); err == nil {
		// Socket exists — probe whether a server actually accepts.
		c, err := net.DialTimeout("unix", sock, 300*time.Millisecond)
		if err == nil {
			c.Close()
			return false // live server on this display
		}
		// Stale socket — remove and reuse.
		os.Remove(sock)
	}
	if data, err := os.ReadFile(lock); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 0 {
			if p, err := os.FindProcess(pid); err == nil && p.Signal(syscall.Signal(0)) == nil {
				return false // lock owned by a live process
			}
		}
		os.Remove(lock)
	}
	return true
}

// ensureX11SocketDir makes /tmp/.X11-unix exist with mode 1777 — Xvfb
// refuses to bind its socket otherwise ("Mode should be set to 1777").
// On minimal/container images the dir is absent or mis-moded.
func ensureX11SocketDir() error {
	const dir = "/tmp/.X11-unix"
	if err := os.MkdirAll(dir, 0o1777); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o1777); err != nil {
		return fmt.Errorf("chmod 1777 %s: %w (need root or ownership)", dir, err)
	}
	return nil
}

// waitDisplayReady polls until the X socket accepts connections.
func waitDisplayReady(display string, timeout time.Duration) error {
	sock := "/tmp/.X11-unix/X" + strings.TrimPrefix(display, ":")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("Xvfb did not open %s within %s", sock, timeout)
}

// Start brings up Xvfb on a free display and launches the preferred browser
// ("" = first detected). width/height default to 1280x800.
//
// lang — язык браузера; "" означает defaultBrowserLang. По умолчанию он
// АНГЛИЙСКИЙ, и это осознанно: браузер живёт на сервере в чужой стране, и
// русский интерфейс рядом с зарубежным адресом выглядит для сайтов страннее,
// чем нейтральный английский (решение владельца после первой же регистрации).
func Start(prefer string, width, height int, lang string) (Status, error) {
	if lang == "" {
		lang = defaultBrowserLang
	}
	mu.Lock()
	if sess != nil {
		st := statusLocked()
		mu.Unlock()
		return st, nil // idempotent
	}
	if starting {
		mu.Unlock()
		return Status{}, fmt.Errorf("start already in progress")
	}
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		mu.Unlock()
		return Status{}, fmt.Errorf("real display already present — use native Remote Desktop")
	}
	xvfb := findXvfb()
	if xvfb == "" {
		mu.Unlock()
		return Status{}, fmt.Errorf("Xvfb not found — sudo apt install xvfb")
	}
	bin := ""
	if prefer != "" {
		for _, n := range browserCandidates[prefer] {
			if p, err := exec.LookPath(n); err == nil {
				bin = p
				break
			}
		}
		if bin == "" {
			mu.Unlock()
			return Status{}, fmt.Errorf("browser %q not found", prefer)
		}
	} else {
		for _, names := range browserCandidates {
			for _, n := range names {
				if p, err := exec.LookPath(n); err == nil {
					bin = p
					break
				}
			}
			if bin != "" {
				break
			}
		}
	}
	if bin == "" {
		mu.Unlock()
		return Status{}, fmt.Errorf("no browser found — sudo apt install chromium-browser")
	}
	starting = true
	mu.Unlock()
	defer func() { mu.Lock(); starting = false; mu.Unlock() }()

	// Размер приходит от того, кто смотрит: у телефона экран вертикальный, и
	// альбомные 1280×800 показывались на нём узкой полоской — человеку
	// приходилось поворачивать телефон, чтобы прочитать текст (подсказка
	// «Поверните телефон» ровно про это). Экран под пропорции зрителя —
	// страница как на планшете в портрете, без «щипков».
	width, height = sanitizeScreen(width, height)

	// Pick a free display in :99..:119.
	display := ""
	for n := 99; n < 120; n++ {
		if displayFree(n) {
			display = fmt.Sprintf(":%d", n)
			break
		}
	}
	if display == "" {
		return Status{}, fmt.Errorf("no free X display in :99..:119")
	}
	if err := ensureX11SocketDir(); err != nil {
		return Status{}, err
	}
	// Лок-файл убитого (SIGKILL) Xvfb прошлой сессии: displayFree про сокет,
	// а лок остаётся, и свежий Xvfb умирает мгновенно с «Server is already
	// active» — дальше супервизор рестартовал и браузер, который без
	// Credential падал на проверке zygote (живой случай 2.46.0).
	_ = os.Remove("/tmp/.X" + strings.TrimPrefix(display, ":") + "-lock")

	// ПОСТОЯННЫЙ профиль. Без него каждый запуск поднимал чистый браузер:
	// вход в почту, в аккаунт агента, сохранённые вкладки — всё пропадало при
	// остановке, и человеку приходилось логиниться заново каждый раз. Плюс
	// сайты видели «браузер без единого куки и без истории» и встречали его
	// проверками для роботов. Профиль лежит рядом с остальным состоянием
	// агента, права 0700: там живые сессии сайтов.
	profile := paths.StateFile("vbrowser-profile")
	if os.Geteuid() == 0 {
		// Корень состояния нужен браузеру лишь для прохода к приватным
		// подкаталогам profile/downloads/uploads. На чистой установке MkdirAll
		// с mode=0700 создавал и сам /var/lib/remotai-vb закрытым для
		// пользователя remotai-vb; миграция старого профиля случайно маскировала
		// эту проблему.
		if err := os.MkdirAll(StateDir(), 0o755); err != nil {
			return Status{}, fmt.Errorf("vbrowser state dir: %w", err)
		}
		_ = os.Chmod(StateDir(), 0o755)
		// Браузер идёт под отдельным пользователем remotai-vb, а /root — это
		// 0700: непривилегированный процесс даже не пройдёт по пути (живая
		// находка: FATAL path_service + «mkdir: Permission denied»). Поэтому от
		// root состояние браузера живёт в общем каталоге. Старый профиль (с
		// входами и куками) переносим как есть — переименование в пределах
		// одной ФС мгновенное.
		rootProfile := profile
		profile = filepath.Join(StateDir(), "profile")
		if _, err := os.Stat(profile); os.IsNotExist(err) {
			if _, errOld := os.Stat(rootProfile); errOld == nil {
				if err := os.MkdirAll(filepath.Dir(profile), 0o755); err == nil {
					if err := os.Rename(rootProfile, profile); err != nil {
						log.Printf("[VBROWSER] профиль %s не переехал (%v) — начнём с чистого", rootProfile, err)
					} else {
						log.Printf("[VBROWSER] профиль переехал %s → %s", rootProfile, profile)
					}
				}
			}
		}
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		log.Printf("[VBROWSER] profile dir %s: %v — работаем без него", profile, err)
		profile = ""
	}

	// Сироты прошлых сессий (агент перезапускался при живом браузере —
	// автообновление, OOM) держат SingletonLock профиля: новый Chrome видит
	// лок, молча передаёт страницу сироте и выходит — дальше супервизор
	// сжигает бюджет рестартов и фича мертва до ручной зачистки (живой случай
	// 2026-07-29 после автоапдейта 2.45.0→2.45.1). Зачистка строго ДО запуска
	// нашего Xvfb: её шаблон «Xvfb на :99–:119» не отличает чужой процесс от
	// только что поднятого нами, и 2026-07-29 каждый Start убивал СОБСТВЕННЫЙ
	// Xvfb — супервизор поднимал пару секундой позже, а клиент, успевший
	// послать WebRTC-оффер в это окно, получал фатальный no_display.
	if profile != "" {
		reapOrphans(profile)
	}
	// Подготовленные для <input type=file> копии нужны лишь живой сессии.
	// После SIGKILL/автообновления Stop не успевает их удалить, поэтому новый
	// браузер начинает с чистого upload-каталога. Профиль, куки и downloads
	// при этом не затрагиваются.
	_ = os.RemoveAll(filepath.Join(StateDir(), "uploads"))

	// Xvfb: -ac disables access control (the socket is loopback-only anyway,
	// -nolisten tcp), so xgb/xdotool connect without XAUTHORITY dance.
	xArgs := []string{
		display,
		"-screen", "0", fmt.Sprintf("%dx%dx24", width, height),
		"-ac", "-nolisten", "tcp",
	}
	xCmd := exec.Command(xvfb, xArgs...)
	xCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var xErr strings.Builder
	xCmd.Stderr = &xErr // diag tail goes into the start error
	if err := xCmd.Start(); err != nil {
		return Status{}, fmt.Errorf("start Xvfb: %w", err)
	}
	if err := waitDisplayReady(display, 5*time.Second); err != nil {
		killTree(xCmd)
		if tail := strings.TrimSpace(xErr.String()); tail != "" {
			return Status{}, fmt.Errorf("%w — %s", err, lastLines(tail, 3))
		}
		return Status{}, err
	}

	// Browser on the virtual display. Root (common for services) needs
	// --no-sandbox; small /dev/shm (containers) needs --disable-dev-shm-usage.
	bArgs := []string{
		"--no-first-run", "--no-default-browser-check",
		"--disable-session-crashed-bubble", "--hide-crash-restore-bubble",
		// На Xvfb НЕТ оконного менеджера, а «развернуть окно» — его работа:
		// --start-maximized там просто игнорировался, и браузер открывался
		// своим окном по умолчанию, оставляя вокруг серую пустоту (её и видно
		// на экране телефона). Размер и позицию задаём сами.
		"--window-position=0,0",
		fmt.Sprintf("--window-size=%d,%d", width, height),
		// Без WM окно считается неактивным/перекрытым, и Chrome душит ему
		// таймеры и отрисовку: страница «подтормаживает» именно тогда, когда на
		// неё смотрят через удалёнку.
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		// Чёткость текста в кадре: hinting на headless даёт иной рендер, чем
		// ожидают страницы (находка browserless/Cloudflare), а плавающий
		// цветовой профиль мылит мелкий текст после кодирования.
		"--font-render-hinting=none",
		"--force-color-profile=srgb",
		// GPU на headless-сервере нет: GPU-процесс Chrome умирает при старте, и
		// композитор остаётся в деградированном состоянии, где молча ломается
		// сенсорный ввод — мышь работает, касания в страницу не доходят (живая
		// находка на проде 2.44.0). Софтверный рендер для стрима ничем не хуже.
		"--disable-gpu",
		// …но БЕЗ WebGL страница выглядит не телефоном, а виртуальной машиной:
		// у любого настоящего устройства WebGL есть всегда, и его отсутствие —
		// один из первых признаков, по которым защита сайта требует проверку
		// вместо регистрации. Замер на живом сервере (2026-07-30): с
		// --disable-gpu WebGL 1 и 2 недоступны совсем, а с этим флагом
		// поднимаются на программном растеризаторе — при том же --disable-gpu,
		// то есть без риска для касаний.
		"--enable-unsafe-swiftshader",
		// Признак «браузером управляет программа». Chrome ставит его при
		// --enable-automation, но убирает свойство только этим флагом: сайты
		// читают navigator.webdriver раньше всего остального.
		"--disable-blink-features=AutomationControlled",
	}
	base := filepath.Base(bin)
	if base == "firefox" {
		if profile != "" {
			bArgs = append(bArgs, "-profile", profile)
		}
	} else {
		bArgs = append(bArgs, "--disable-dev-shm-usage")
		if profile != "" {
			bArgs = append(bArgs, "--user-data-dir="+profile)
		}
		bArgs = append(bArgs, "--lang="+lang)
		// --no-sandbox сознательно НЕ добавляем: от root браузер идёт под
		// отдельным пользователем (Credential ниже), Chrome включает свой
		// sandbox через user-namespace и не рисует плашку «unsupported flag».
	}
	// Протокол отладки Chrome — источник кадров и путь ввода дешевле, чем
	// снимать и кодировать экран самим (см. internal/cdp). Порт слушается
	// ТОЛЬКО на петле: тот, кто до него дотянется, управляет браузером целиком,
	// поэтому наружу он не выходит ни при каких условиях.
	debugPort := 0
	if base != "firefox" {
		if p, err := freeLoopbackPort(); err == nil {
			debugPort = p
			bArgs = append(bArgs,
				fmt.Sprintf("--remote-debugging-port=%d", p),
				"--remote-debugging-address=127.0.0.1",
			)
		} else {
			log.Printf("[VBROWSER] порт отладки не выделен (%v) — кадры пойдут через захват экрана", err)
		}
	}

	// Звук: sink и демон pulse готовим ДО запуска браузера — PULSE_SINK читается
	// libpulse при первом подключении, и поздно выставленная переменная звук уже
	// не перенаправит. Без pactl/libopus SetupAudio просто логирует и сессия
	// остаётся беззвучной, видео это не касается.
	audioOK := SetupAudio()

	bCmd := exec.Command(bin, bArgs...)
	bCmd.Env = append(os.Environ(), "DISPLAY="+display)
	if audioOK {
		// libpulse уважает PULSE_SINK: Chromium и Firefox молча уходят в наш
		// null-sink, чей monitor мы и пишем через parec.
		bCmd.Env = append(bCmd.Env, "PULSE_SINK="+audioSinkName)
		if audioSystemMode.Load() {
			// Браузер пойдёт под remotai-vb, а демон PulseAudio — системный.
			// Без явного сокета libpulse ищет несуществующий /run/user/<uid>.
			bCmd.Env = append(bCmd.Env, "PULSE_SERVER="+systemPulseServer)
		}
	}
	if lang != "" {
		// LANGUAGE читают и Chrome, и Firefox — без него язык страниц (заголовок
		// Accept-Language) остаётся системным «C», то есть английским.
		bCmd.Env = append(bCmd.Env, "LANGUAGE="+strings.ReplaceAll(lang, "-", "_"))
	}
	bCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 && base != "firefox" {
		// Отдельный системный пользователь вместо --no-sandbox: Chrome
		// включает свой обычный sandbox (user-namespace), не рисует плашку
		// «unsupported flag», а профиль не принадлежит root-у. Xvfb с -ac
		// пускает любого локального пользователя, debug-порт — на петле.
		if cred, home, err := vbUserCredential(profile); err != nil {
			log.Printf("[VBROWSER] пользователь для браузера не создан (%v) — работаем от root", err)
			bCmd.Args = append(bCmd.Args, "--no-sandbox")
		} else {
			bCmd.SysProcAttr.Credential = cred
			bCmd.Env = append(bCmd.Env, "HOME="+home)
		}
	}
	// Stderr браузера — только хвост: там видны падения GPU и рендерера.
	bErr := newErrRing(32 << 10)
	bCmd.Stderr = bErr
	if err := bCmd.Start(); err != nil {
		killTree(xCmd)
		return Status{}, fmt.Errorf("start browser: %w", err)
	}

	mu.Lock()
	savedDisplay = os.Getenv("DISPLAY")
	os.Setenv("DISPLAY", display)
	now := time.Now()
	s := &session{
		display:      display,
		xCmd:         xCmd,
		browserCmd:   bCmd,
		browser:      base,
		startedAt:    now,
		debugPort:    debugPort,
		width:        width,
		height:       height,
		xvfbPath:     xvfb,
		xArgs:        xArgs,
		bin:          bin,
		bArgs:        bArgs,
		env:          bCmd.Env,
		cred:         bCmd.SysProcAttr.Credential,
		xvfbUpAt:     now,
		browserUpAt:  now,
		done:         make(chan struct{}),
		events:       make(chan procEvent, 4),
		lastActivity: now,
		bErr:         bErr,
	}
	sess = s
	// A clean start erases the terminal error of the previous session.
	stoppedErr, stoppedRestarts = "", 0
	st := statusLocked()
	mu.Unlock()

	// The supervisor owns the children from here: unexpected exits are
	// restarted within a shared budget, an idle session stops itself.
	s.watch("browser", bCmd)
	s.watch("xvfb", xCmd)
	go supervise(s)

	log.Printf("[VBROWSER] started display=%s browser=%s (%dx%d)", display, base, width, height)
	return st, nil
}

// Stop tears the session down and restores the agent DISPLAY.
func Stop() {
	mu.Lock()
	s := sess
	sess = nil
	if s == nil {
		mu.Unlock()
		return
	}
	// The supervisor may be mid-restart swapping these pointers; read them
	// under the lock so the kills below target real processes.
	bCmd, xCmd := s.browserCmd, s.xCmd
	uploadDirs := append([]string(nil), s.uploadDirs...)
	mu.Unlock()
	close(s.done)  // cancels a pending backoff restart, the idle ticker, watchers
	stopAudioAll() // отписать слушателей и убить parec; sink-модуль остаётся (дёшев, переиспользуется)
	killTree(bCmd)
	killTree(xCmd)
	for _, dir := range uploadDirs {
		_ = os.RemoveAll(dir)
	}
	os.Setenv("DISPLAY", savedDisplay)
	log.Printf("[VBROWSER] stopped display=%s", s.display)
}

// watch reaps one child and reports its exit to the supervise loop. The send
// gives up on a closed session so no goroutine outlives Stop.
func (s *session) watch(kind string, cmd *exec.Cmd) {
	go func() {
		_ = cmd.Wait()
		select {
		case s.events <- procEvent{kind: kind, cmd: cmd}:
		case <-s.done:
		}
	}()
}

// isCurrent reports whether the exited process is still the live one (and not
// the corpse of a process the supervisor itself killed for a pair restart).
// Callers must hold mu.
func (s *session) isCurrent(ev procEvent) bool {
	if ev.kind == "xvfb" {
		return ev.cmd == s.xCmd
	}
	return ev.cmd == s.browserCmd
}

// supervise is the per-session control loop: unexpected process exits spend
// the shared restart budget (backoff 1s→16s), and the idle ticker stops the
// session when it sits unwatched and untouched for too long.
func supervise(s *session) {
	ticker := time.NewTicker(idleTick)
	defer ticker.Stop()
	var backoff *time.Timer
	var backoffC <-chan time.Time
	pending := "" // process kind waiting for its backoff restart
	defer func() {
		if backoff != nil {
			backoff.Stop()
		}
	}()
	for {
		select {
		case <-s.done:
			return
		case ev := <-s.events:
			mu.Lock()
			if sess != s {
				mu.Unlock()
				return
			}
			if !s.isCurrent(ev) {
				mu.Unlock()
				continue // late exit of a process the supervisor killed itself
			}
			if pending != "" {
				// A restart is already scheduled. An Xvfb exit supersedes a
				// browser-only restart: the pair comes back together anyway.
				if ev.kind == "xvfb" {
					pending = "xvfb"
				}
				mu.Unlock()
				continue
			}
			upAt := s.browserUpAt
			if ev.kind == "xvfb" {
				upAt = s.xvfbUpAt
			}
			fails, delay, ok := restartDecision(s.restarts, time.Since(upAt) >= restartStableAfter)
			s.restarts = fails
			if !ok {
				s.lastErr = ev.kind + " keeps exiting — restart budget exhausted"
				errText := s.lastErr
				mu.Unlock()
				log.Printf("[VBROWSER] %s — stopping session", errText)
				failSession(s)
				return
			}
			s.lastErr = fmt.Sprintf("%s exited — restart %d/%d in %s", ev.kind, fails, restartMaxFails, delay)
			errText := s.lastErr
			mu.Unlock()
			log.Printf("[VBROWSER] %s", errText)
			if ev.kind == "browser" {
				// Хвост stderr: падение GPU/рендерера — причина номер один
				// тихих смертей браузера на headless-машинах.
				if tail := strings.TrimSpace(s.bErr.Tail()); tail != "" {
					log.Printf("[VBROWSER] browser stderr tail: %s", lastLines(tail, 3))
				}
			}
			backoff = time.NewTimer(delay)
			backoffC = backoff.C
			pending = ev.kind
		case <-backoffC:
			kind := pending
			pending = ""
			backoffC = nil
			mu.Lock()
			alive := sess == s
			mu.Unlock()
			if !alive {
				return // Stop landed during the backoff wait
			}
			if kind == "xvfb" {
				restartPair(s)
			} else {
				restartBrowser(s)
			}
		case now := <-ticker.C:
			timeout := idleTimeout()
			mu.Lock()
			if sess != s {
				mu.Unlock()
				return
			}
			warn, stop := idleVerdict(now, s.lastActivity, s.viewers, timeout, idleWarnBefore, s.idleWarned)
			if warn {
				s.idleWarned = true
			}
			left := int((timeout - now.Sub(s.lastActivity)).Seconds())
			mu.Unlock()
			if stop {
				log.Printf("[VBROWSER] idle for %s without viewers — stopping", timeout)
				emitEvent("idle_stopped", nil)
				Stop()
				return
			}
			if warn {
				if left < 0 {
					left = 0
				}
				emitEvent("idle_warning", map[string]any{"seconds_left": left})
			}
		}
	}
}

// restartBrowser relaunches the browser on the same display with the same
// profile and flags. The debug port is reused: a crashed browser has released
// it, and the CDP layer reconnects to the same endpoint by retrying.
func restartBrowser(s *session) {
	cmd := exec.Command(s.bin, s.bArgs...)
	cmd.Env = s.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: s.cred}
	// Кольцо stderr переиспользуем: хвост важен у текущего процесса, а не у
	// покойника — чистим перед запуском.
	s.bErr.Reset()
	cmd.Stderr = s.bErr
	if err := cmd.Start(); err != nil {
		log.Printf("[VBROWSER] browser restart failed: %v", err)
		s.inject("browser", s.browserCmd)
		return
	}
	mu.Lock()
	if sess != s {
		mu.Unlock()
		killTree(cmd) // Stop landed while we were starting
		return
	}
	s.browserCmd = cmd
	s.browserUpAt = time.Now()
	s.lastErr = ""
	mu.Unlock()
	s.watch("browser", cmd)
	log.Printf("[VBROWSER] browser restarted display=%s", s.display)
}

// restartPair brings the whole stack back after an Xvfb exit: the browser
// cannot live without its display, so it goes down first and comes back once
// the display accepts connections again.
func restartPair(s *session) {
	mu.Lock()
	if sess != s {
		mu.Unlock()
		return
	}
	deadBrowser := s.browserCmd
	s.browserCmd = nil // its watcher's late exit event is stale from here on
	mu.Unlock()
	killTree(deadBrowser)

	// Лок дисплея от только что убитого Xvfb: без снятия свежий Xvfb падает
	// с «Server is already active», и пара уходит в цикл рестартов.
	_ = os.Remove("/tmp/.X" + strings.TrimPrefix(s.display, ":") + "-lock")
	xCmd := exec.Command(s.xvfbPath, s.xArgs...)
	xCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var xErr strings.Builder
	xCmd.Stderr = &xErr
	if err := xCmd.Start(); err != nil {
		log.Printf("[VBROWSER] Xvfb restart failed: %v", err)
		s.inject("xvfb", s.xCmd)
		return
	}
	if err := waitDisplayReady(s.display, 5*time.Second); err != nil {
		killTree(xCmd)
		if tail := strings.TrimSpace(xErr.String()); tail != "" {
			log.Printf("[VBROWSER] Xvfb restart: %v — %s", err, lastLines(tail, 3))
		} else {
			log.Printf("[VBROWSER] Xvfb restart: %v", err)
		}
		s.inject("xvfb", s.xCmd)
		return
	}
	mu.Lock()
	if sess != s {
		mu.Unlock()
		killTree(xCmd)
		return
	}
	s.xCmd = xCmd
	s.xvfbUpAt = time.Now()
	mu.Unlock()
	s.watch("xvfb", xCmd)
	log.Printf("[VBROWSER] Xvfb restarted display=%s", s.display)
	restartBrowser(s)
}

// inject feeds a failed relaunch back into the supervise loop as a synthetic
// exit event, so it spends the same restart budget instead of looping hot.
// cmd must be the pointer the isCurrent check still expects (the dead one).
func (s *session) inject(kind string, cmd *exec.Cmd) {
	select {
	case s.events <- procEvent{kind: kind, cmd: cmd}:
	case <-s.done:
	}
}

// failSession keeps the terminal error visible in Status and stops everything:
// a display left running browserless serves no one.
func failSession(s *session) {
	mu.Lock()
	stoppedErr = s.lastErr
	stoppedRestarts = s.restarts
	mu.Unlock()
	Stop()
}

// NoteActivity pushes the idle deadline out: real interaction only (input,
// navigation, keepalive) — status polling deliberately does not call this.
func NoteActivity() {
	mu.Lock()
	if sess != nil {
		sess.lastActivity = time.Now()
		sess.idleWarned = false // activity re-arms the one-shot warning
	}
	mu.Unlock()
}

// SetViewers tells the idle timer how many screens are watching the stream:
// the session never goes idle while at least one viewer is attached.
func SetViewers(n int) {
	mu.Lock()
	if sess != nil {
		sess.viewers = n
	}
	mu.Unlock()
}

// eventCb is the sink for session lifecycle events ("idle_warning",
// "idle_stopped"), registered by the web layer which forwards them to open
// screens. Kept behind its own mutex: emitters must not take the session lock.
var eventCb struct {
	mu sync.Mutex
	fn func(kind string, fields map[string]any)
}

// SetEventCallback registers the lifecycle event sink (nil detaches it).
func SetEventCallback(fn func(kind string, fields map[string]any)) {
	eventCb.mu.Lock()
	eventCb.fn = fn
	eventCb.mu.Unlock()
}

func emitEvent(kind string, fields map[string]any) {
	eventCb.mu.Lock()
	fn := eventCb.fn
	eventCb.mu.Unlock()
	if fn != nil {
		fn(kind, fields)
	}
}

// freeLoopbackPort занимает свободный порт на петле и сразу отпускает его:
// Chrome откроет тот же номер через миллисекунды, и гонка здесь практически
// невозможна (в отличие от угадывания фиксированного порта, который на сервере
// может быть занят чем угодно).
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// DebugEndpoint — адрес протокола отладки браузера ("127.0.0.1:PORT") или ""
// когда его нет (Firefox, не выделился порт, браузер не запущен).
func DebugEndpoint() string {
	mu.Lock()
	defer mu.Unlock()
	if sess == nil || sess.debugPort == 0 {
		return ""
	}
	return fmt.Sprintf("127.0.0.1:%d", sess.debugPort)
}

// ScreenSize — размер виртуального экрана (0,0 если браузер не запущен).
func ScreenSize() (int, int) {
	mu.Lock()
	defer mu.Unlock()
	if sess == nil {
		return 0, 0
	}
	return sess.width, sess.height
}

// lastLines returns the last n lines of s (for compact error tails).
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// killTree terminates the process group of cmd (Setpgid makes the pid the
// group leader, so -pid hits the whole tree: Xvfb, browser + renderer procs).
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(2 * time.Second) // grace period before the force-kill
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// reapOrphans убивает процессы прошлых сессий виртуального браузера: Chrome с
// нашим --user-data-dir и Xvfb на дисплеях :99–:119. Заодно снимает файлы
// Singleton-блокировки профиля — с мёртвым держателем они уже ни на что не
// влияют, но и нужды в них нет.
func reapOrphans(profile string) {
	// Маркер — ПОЛНЫЙ путь профиля: короткое слово «profile» встречается в
	// командных строках чужих программ, и по нему мы бы убивали невинных.
	marker := profile
	own := os.Getpid()
	var killed []int
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == own {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cl := strings.ReplaceAll(string(raw), "\x00", " ")
		ours := strings.Contains(cl, marker)
		if !ours && strings.Contains(cl, "Xvfb") {
			for n := 99; n < 120; n++ {
				if strings.Contains(cl, fmt.Sprintf(" :%d ", n)) {
					ours = true
					break
				}
			}
		}
		if !ours {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
		killed = append(killed, pid)
	}
	if len(killed) == 0 {
		return
	}
	log.Printf("[VBROWSER] reapOrphans: останавливаю %d сирот прошлой сессии: %v", len(killed), killed)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range killed {
			if err := syscall.Kill(pid, 0); err == nil {
				alive = true
				break
			}
		}
		if !alive {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range killed {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	for _, f := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		_ = os.Remove(filepath.Join(profile, f))
	}
}

// vbUserName — системный пользователь, под которым живёт браузер виртуального
// дисплея (вместо --no-sandbox от root).
const vbUserName = "remotai-vb"

// vbUserCredential заводит (один раз) системного пользователя для браузера и
// отдаёт его Credential + HOME. Профиль и его содержимое передаются этому
// пользователю, иначе Chrome от root не сможет туда писать, а от пользователя
// — читать настройки прошлых сессий.
func vbUserCredential(profile string) (*syscall.Credential, string, error) {
	u, err := user.Lookup(vbUserName)
	if err != nil {
		if _, isUnknown := err.(user.UnknownUserError); !isUnknown {
			return nil, "", err
		}
		if out, err := exec.Command("useradd", "--system", "--no-create-home",
			"--shell", "/usr/sbin/nologin", vbUserName).CombinedOutput(); err != nil {
			return nil, "", fmt.Errorf("useradd %s: %v (%s)", vbUserName, err, strings.TrimSpace(string(out)))
		}
		u, err = user.Lookup(vbUserName)
		if err != nil {
			return nil, "", err
		}
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, "", fmt.Errorf("uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, "", fmt.Errorf("gid %q: %w", u.Gid, err)
	}
	// Владелец профиля — этот пользователь. Chown каждый раз и РЕКУРСИВНО:
	// после сбоев внутри остаются root-овые файлы (Chrome когда-то шёл от
	// root), и проверка только верхнего каталога их не ловит — браузер тогда
	// падает с «Permission denied» глубоко внутри профиля (живой случай).
	if profile != "" {
		_ = filepath.Walk(profile, func(p string, _ os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chown(p, uid, gid)
			}
			return nil
		})
	}
	// Каталог загрузок — тоже его: браузер качает файлы под этим пользователем.
	if dl := filepath.Join(StateDir(), "downloads"); dl != "" {
		if err := os.MkdirAll(dl, 0o700); err == nil {
			_ = os.Chown(dl, uid, gid)
		}
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if pulseGID, ok := pulseAccessGroup(); ok {
		// PulseAudio system-mode проверяет primary GID клиента; supplementary
		// группы недостаточно. UID остаётся браузерным, его обычный GID
		// сохраняем supplementary для совместимости с файлами профиля.
		cred.Gid = pulseGID
		cred.Groups = []uint32{uint32(gid)}
	}
	return cred, profile, nil
}

// PrepareUploads копирует выбранные на телефоне файлы в приватный каталог,
// доступный системному пользователю браузера. Исходники лежат в ~/Remotai/files
// владельца агента; Chrome под remotai-vb туда (особенно в /root) не пройдёт.
func PrepareUploads(files []UploadFile) ([]string, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("no files")
	}

	mu.Lock()
	s := sess
	if s == nil {
		mu.Unlock()
		return nil, fmt.Errorf("virtual browser is not running")
	}
	cred := s.cred
	mu.Unlock()

	root := filepath.Join(StateDir(), "uploads")
	if os.Geteuid() == 0 {
		if err := os.MkdirAll(StateDir(), 0o755); err != nil {
			return nil, err
		}
		_ = os.Chmod(StateDir(), 0o755)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("upload dir: %w", err)
	}
	if cred != nil {
		_ = os.Chown(root, int(cred.Uid), int(cred.Gid))
	}
	dir, err := os.MkdirTemp(root, "chooser-")
	if err != nil {
		return nil, fmt.Errorf("upload session: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	if cred != nil {
		_ = os.Chown(dir, int(cred.Uid), int(cred.Gid))
	}

	cleanup := func() {
		_ = os.RemoveAll(dir)
	}
	result := make([]string, 0, len(files))
	used := map[string]int{}
	for _, item := range files {
		name := browserUploadName(item.Name)
		if n := used[name]; n > 0 {
			name = numberedUploadName(name, n+1)
		}
		used[browserUploadName(item.Name)]++
		dst := filepath.Join(dir, name)
		if err := copyBrowserUpload(item.Path, dst); err != nil {
			cleanup()
			return nil, err
		}
		if cred != nil {
			_ = os.Chown(dst, int(cred.Uid), int(cred.Gid))
		}
		result = append(result, dst)
	}

	mu.Lock()
	if sess != s {
		mu.Unlock()
		cleanup()
		return nil, fmt.Errorf("virtual browser stopped during upload")
	}
	s.uploadDirs = append(s.uploadDirs, dir)
	mu.Unlock()
	return result, nil
}

func copyBrowserUpload(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open upload: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create browser upload: %w", err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("copy browser upload: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close browser upload: %w", closeErr)
	}
	return nil
}

func browserUploadName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\`, r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "upload"
	}
	r := []rune(name)
	if len(r) > 180 {
		name = string(r[:180])
	}
	return name
}

func numberedUploadName(name string, n int) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s (%d)%s", base, n, ext)
}

// DiscardUploads removes prepared copies after a failed/stale chooser. Paths
// are accepted only under StateDir()/uploads; callers cannot turn this helper
// into arbitrary RemoveAll.
func DiscardUploads(paths []string) {
	root, err := filepath.Abs(filepath.Join(StateDir(), "uploads"))
	if err != nil {
		return
	}
	dirs := map[string]bool{}
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		dir := filepath.Dir(abs)
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		dirs[dir] = true
	}
	for dir := range dirs {
		_ = os.RemoveAll(dir)
	}
}
