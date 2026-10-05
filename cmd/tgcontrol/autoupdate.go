package main

// Фоновое автообновление: при старте (через 2 минуты) и далее каждые 6 часов
// сверяемся с https://remotai.ru/download/latest.json. Если вышла новая
// версия — скачиваем, проверяем sha256, подменяем бинарник и перезапускаемся.
//
// Хэндовер порта: новый процесс запускается с флагом --update-handoff и ждёт
// ~2.5 сек на старте, а старый сразу делает os.Exit — слушающий сокет
// освобождается до того, как новый начнёт биндиться.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/connstat"
	"tgcontrol/internal/desktopui"
	"tgcontrol/internal/observability"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/selfheal"
	"tgcontrol/internal/service"
	"tgcontrol/internal/update"
	"tgcontrol/internal/vbrowser"
	"tgcontrol/internal/version"
)

const updateHandoffFlag = update.HandoffFlag

// pendingUpdate — бинарь уже подменён на диске, рестарт ждёт тихой минуты.
type pendingUpdate struct {
	version string
	args    []string // без --background: он добавляется в момент рестарта
	since   time.Time
	// viaSystemd — перезапуск делает systemd (Restart=always), поэтому наш
	// «рестарт» — это чистый os.Exit(0): detached self-restart уехал бы в cgroup
	// юнита и был бы убит следующим `systemctl restart`.
	viaSystemd bool
	// viaManager — перезапуск делает сервис-менеджер по нашей просьбе (launchd:
	// `kickstart -k`). Самим перезапускаться нельзя: новый процесс родился бы
	// вне домена службы и был бы убит первым же kickstart.
	viaManager bool
}

// maxDeferral — предел ожидания тихой минуты. Открытое окно панели (а теперь и
// живой сеанс с телефона) больше не откладывает обновление навсегда: в проде
// агент так простоял двое суток на v2.24.0 (окно висело с 23.07), пропустив два
// релиза. По истечении лимита перезапускаемся всё равно — окно тут же
// открывается снова (ровно так делает обновление из панели), а клиенты успевают
// получить предупреждение (warnLiveSessions).
const maxDeferral = 2 * time.Hour

// deferredRestartReason — пора ли применять отложенный рестарт; "" = ещё ждём.
// Отдельная функция, чтобы политику можно было проверить тестом.
//
// Ждём ДВУХ вещей: закрытого окна панели на ПК и отсутствия живых клиентских
// сеансов. Раньше смотрелось только окно — а обычное состояние агента как раз
// «окна нет», поэтому обновление рвало сеанс с телефона посреди работы:
// картинка удалёнки замирала, поверх появлялось «Переподключение… (1/10)», ввод
// последних секунд и текущая передача файла терялись, и слова «обновление»
// человек не видел нигде.
func deferredRestartReason(windowOpen bool, liveSessions int, waited time.Duration) string {
	if !windowOpen && liveSessions <= 0 {
		return "окно закрыто, живых сеансов нет"
	}
	if waited >= maxDeferral {
		return "лимит ожидания истёк — " + waitCause(windowOpen, liveSessions)
	}
	return ""
}

// waitCause — чего именно ждёт отложенный рестарт (для лога).
func waitCause(windowOpen bool, liveSessions int) string {
	switch {
	case windowOpen && liveSessions > 0:
		return fmt.Sprintf("окно панели открыто, живых сеансов: %d", liveSessions)
	case windowOpen:
		return "окно панели открыто"
	default:
		return fmt.Sprintf("живых сеансов: %d", liveSessions)
	}
}

// liveClientSessions — сколько клиентских сеансов держит ПК прямо сейчас: WS
// терминала, стрим экрана, WebRTC (учёт ведёт internal/connstat, где эти
// соединения и так регистрируются для диагностики). limit=1: из снимка нужен
// только срез активных, кольцо событий не читаем.
//
// Плюс ОДИН за виртуальный браузер, даже когда на него никто не смотрит:
// перезапуск агента осиротит его Xvfb и Chrome (дети переживают родителя), а
// сироты держат лок профиля — следующий старт браузера после обновления
// сгорал в «keeps exiting» до ручной зачистки (живой случай 2026-07-29).
// Подождать лучше idle-стопа, чем лечить это на проде.
func liveClientSessions() int {
	n := len(connstat.Default.Snapshot(1).Active)
	if vbrowser.Running() {
		n++
	}
	return n
}

// updateNotifier — всё, что автообновлению нужно от веб-сервера: разослать
// клиентам предупреждение о рестарте. Интерфейс, чтобы не тащить сюда весь
// web.Server (и чтобы политику можно было гонять в тестах без него).
type updateNotifier interface {
	Broadcast(uid int64, event any)
}

// warnLiveSessions предупреждает владельцев сеансов, которые оборвёт рестарт.
// Событие уходит и в локальные WS, и через релей (Broadcast кормит
// relayEventSink), поэтому телефон получает его и в облаке. Возвращает число
// предупреждённых аккаунтов.
func warnLiveSessions(n updateNotifier, newVersion string) int {
	if n == nil {
		return 0
	}
	warned := make(map[int64]bool)
	for _, c := range connstat.Default.Snapshot(1).Active {
		if warned[c.UID] {
			continue
		}
		warned[c.UID] = true
		n.Broadcast(c.UID, map[string]any{
			"type":    "agent_updating",
			"version": newVersion,
		})
	}
	return len(warned)
}

func runAutoUpdate(ctx context.Context, notify updateNotifier) {
	// Анонс свежеприменённого обновления на релей (→ сообщение в TG-боте
	// владельца). Независимо от DisableAutoUpdate: маркер мог оставить и
	// ручной `remotai update`.
	if notify != nil {
		go func() {
			defer observability.RecoverPanic("update-announcer")
			runUpdateAnnouncer(ctx, func(event any) { notify.Broadcast(0, event) })
		}()
	}
	cfg := config.GetNoSetup()
	if cfg.DisableAutoUpdate {
		return
	}

	const checkInterval = 6 * time.Hour
	const quietPollInterval = 2 * time.Minute

	var pending *pendingUpdate
	lastCheck := time.Time{}

	// restartNow применяет отложенное обновление. Маркер «обновлён до vX»
	// пишется ЗДЕСЬ, а не в момент подмены бинарника: иначе панель показывала
	// «Обновлён до v2.25.0», пока процесс ещё работал на v2.24.0.
	restartNow := func(reason string) {
		log.Printf("[UPDATE] %s — перезапуск на v%s", reason, pending.version)
		// Кого рестарт всё-таки задевает (сработал лимит ожидания) — тот должен
		// увидеть «ПК обновляется», а не безликое «Переподключение…».
		if warned := warnLiveSessions(notify, pending.version); warned > 0 {
			time.Sleep(500 * time.Millisecond) // дать событию уйти в сокеты/релей
		}
		markUpdateApplied(pending.version)
		// Прощаемся в heartbeat: без этой отметки следующий запуск объявит
		// штатный перезапуск ради обновления «падением агента» и разбудит
		// владельца сообщением (см. internal/selfheal).
		selfheal.MarkStop("update")
		if pending.viaManager {
			log.Printf("[UPDATE] прошу сервис-менеджер перезапустить агента на v%s", pending.version)
			if err := service.RestartByManager(); err != nil {
				log.Printf("[UPDATE] перезапуск менеджером не удался: %v", err)
			}
			return
		}
		if pending.viaSystemd {
			log.Printf("[UPDATE] выхожу — systemd поднимет сервис на v%s", pending.version)
			os.Exit(0)
		}
		args := slices.Clone(pending.args)
		// Окно открыто (сработал лимит ожидания) → перезапускаемся без
		// --background, чтобы панель вернулась на экран, а не просто исчезла.
		if !desktopui.WindowOpen() && !slices.Contains(args, "--background") {
			args = append(args, "--background")
		}
		if err := update.Restart(args); err != nil {
			log.Printf("[UPDATE] перезапуск не удался: %v", err)
		}
	}

	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		// Рестарт при первой возможности: окно закрыто, живых сеансов нет — либо
		// вышел лимит ожидания. До этого НЕ блокируемся (раньше горутина висла в
		// waitWindowClosed и не делала новых проверок — версии, вышедшие позже
		// отложенной, были не видны; в проде 2.18.1 висела 2 дня, а 2.19.0 даже
		// не скачивалась).
		if pending != nil {
			if reason := deferredRestartReason(desktopui.WindowOpen(), liveClientSessions(), time.Since(pending.since)); reason != "" {
				restartNow(reason)
				return
			}
		}

		// Проверка манифеста: раз в checkInterval. Во время ожидания тихой
		// минуты — тоже продолжаем: вышедшую позже версию применим поверх
		// отложенной.
		if pending == nil || time.Since(lastCheck) >= checkInterval {
			lastCheck = time.Now()
			info, err := version.CheckForUpdate("")
			switch {
			case err != nil:
				// Нет сети/манифест битый — просто логируем и ждём следующий цикл.
				log.Printf("[UPDATE] проверка не удалась: %v", err)
			case info != nil && info.Available && info.DownloadURL != "":
				if pending != nil && !version.IsNewer(info.Version, pending.version) {
					// Эта (или более старая) версия уже применена — ждём тишины.
					break
				}
				log.Printf("[UPDATE] доступна v%s (текущая v%s) — скачиваю…", info.Version, version.Version)
				if err := update.Apply(info.DownloadURL, info.SHA256); err != nil {
					log.Printf("[UPDATE] не удалось применить: %v", err)
					break
				}
				managed := service.RunAsService()
				if managed && !service.ManagerCanRestart() {
					// SCM сам перезапустит / применится при следующем старте.
					// Маркер «обновлено» здесь не пишем: новый бинарник вступит в
					// силу неизвестно когда, а баннер врёт уже сейчас.
					log.Printf("[UPDATE] v%s применена — вступит в силу после перезапуска сервиса", info.Version)
					return
				}
				// launchd перезапуск умеет — значит и под ним обновление проходит
				// общим путём: ждём тихой минуты и просим менеджер. Раньше здесь
				// стоял безусловный `return`, и Mac-агент не только оставался на
				// старой версии, но и выходил из цикла проверок насовсем: за
				// следующей версией он уже не шёл (живой мак 10.08.2026 — час на
				// 2.55.16 при подменённом бинаре 2.55.18).
				args := slices.Clone(os.Args[1:])
				if !slices.Contains(args, updateHandoffFlag) {
					args = append(args, updateHandoffFlag)
				}
				// --background добавляет restartNow, по состоянию окна.
				args = slices.DeleteFunc(args, func(a string) bool { return a == "--background" })
				since := time.Now()
				if pending != nil {
					since = pending.since // лимит считаем с ПЕРВОЙ отложки
				}
				pending = &pendingUpdate{
					version:    info.Version,
					args:       args,
					since:      since,
					viaSystemd: service.UnderSystemd(),
				}
				log.Printf("[UPDATE] v%s применена", info.Version)

				windowOpen, live := desktopui.WindowOpen(), liveClientSessions()
				if deferredRestartReason(windowOpen, live, time.Since(pending.since)) == "" {
					// Ждём тихой минуты коротким поллом (не дольше maxDeferral),
					// проверки манифеста продолжаются. Панель тем временем
					// показывает плашку «обновление готово» и кнопку рестарта —
					// иначе о готовом обновлении знает только лог.
					update.SetPending(info.Version, func() {
						restartNow("перезапуск по кнопке в панели")
					})
					log.Printf("[UPDATE] перезапуск отложен: %s (не дольше %s)", waitCause(windowOpen, live), maxDeferral)
				}
			}
		}

		// Немедленный рестарт для тихого случая (трей/автозапуск без сеансов) —
		// поведение как раньше; иначе короткий полл вместо 6-часового сна.
		if pending != nil {
			if reason := deferredRestartReason(desktopui.WindowOpen(), liveClientSessions(), time.Since(pending.since)); reason != "" {
				restartNow(reason)
				return
			}
			timer.Reset(quietPollInterval)
		} else {
			timer.Reset(checkInterval)
		}
	}
}

// markUpdateApplied оставляет одноразовый маркер для панели: при следующем
// открытии она покажет «Обновлено до vX» (читается в apiSetupStatus).
func markUpdateApplied(newVersion string) {
	writeUpdateMarker("update-applied", newVersion)
}

// ── Анонс «агент обновился» на релей → сообщение в TG-боте ────────────────

// announceDelays — паузы между попытками анонса после старта (итого попытки
// на ~1, ~6 и ~36 минутах). Событие уходит через Broadcast → relayEventSink →
// ForwardEvent, а тот при отсутствии коннекта к релею МОЛЧА дропает
// (internal/relay/client.go): подтверждения доставки не существует, поэтому
// три попытки. Дубли гасятся дедупликацией на релее по (устройство, версия) —
// лучше лишняя попытка, чем потерянное сообщение.
var announceDelays = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// shouldAnnounce — надо ли анонсировать applied-версию: она есть, и её ещё не
// анонсировали (update-notified гасит повторы между перезапусками агента).
func shouldAnnounce(applied, notified string) bool {
	return applied != "" && applied != notified
}

// runUpdateAnnouncer сообщает на релей версию, на которую обновился агент.
// Маркер update-applied читается ОДИН РАЗ на старте: панель удаляет его при
// открытии (apiSetupStatus), и дожидаться первой попытки значило бы потерять
// анонс, если человек открыл панель в первую минуту после обновления.
func runUpdateAnnouncer(ctx context.Context, broadcast func(event any)) {
	applied := readUpdateMarker("update-applied")
	if !shouldAnnounce(applied, readUpdateMarker("update-notified")) {
		return
	}
	for _, d := range announceDelays {
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
		broadcast(map[string]any{
			// Контракт с релеем (tgcontrol-relay/internal/server/notifier.go):
			// type+version обязательны, ts — unix ms для отсева реплеев.
			"type":    "agent_updated",
			"version": applied,
			"ts":      time.Now().UnixMilli(),
		})
	}
	// Помечаем после последней попытки: подтверждения доставки нет, но три
	// попытки по живому коннекту + дедуп на релее — достаточно.
	writeUpdateMarker("update-notified", applied)
}

func readUpdateMarker(name string) string {
	data, err := os.ReadFile(filepath.Join(paths.Base(), name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeUpdateMarker(name, version string) {
	p := filepath.Join(paths.Base(), name)
	if err := os.WriteFile(p, []byte(version), 0o600); err != nil {
		log.Printf("[UPDATE] маркер %s не записан: %v", name, err)
	}
}

// waitUpdateHandoff — пауза на старте нового процесса, пока старый отпускает
// порт (см. runAutoUpdate).
func waitUpdateHandoff() {
	if slices.Contains(os.Args[1:], updateHandoffFlag) {
		time.Sleep(2500 * time.Millisecond)
	}
}
