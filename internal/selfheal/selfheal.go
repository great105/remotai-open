// Package selfheal отвечает на вопрос «что случилось с компьютером, пока его
// не было» — и говорит это словами, а не строчками в логе.
//
// ЗАЧЕМ. Компьютер пропадает из приложения по причинам, которые снаружи
// неразличимы: перезагрузка после обновлений Windows, пропало питание, синий
// экран, кто-то закрыл приложение, автообновление самого агента. Человек с
// телефона видит одно и то же — «компьютер не в сети», а вернувшись, не узнаёт
// уже ничего: событие прошло. Раньше единственным следом были 3000 строк в
// remotai.log, и разбирать их приходилось вручную (разбор 02.08.2026 —
// 42 часа без облака, диагноз собран человеком за полчаса чтения логов).
//
// КАК УСТРОЕНО. Пока агент жив, он раз в минуту трогает файл heartbeat.json:
// «я был здесь в такое-то время, система загружена тогда-то». При старте
// сравниваем три метки — своё последнее сердцебиение, время загрузки ОС и
// «сейчас»:
//
//	сердцебиение < загрузка   → компьютер перезагружался (или выключался);
//	сердцебиение ≥ загрузка   → система не перезагружалась, исчезал сам агент.
//
// Причину перезагрузки на Windows спрашиваем у журнала System — там она есть
// всегда и без прав администратора (см. shutdown_windows.go).
//
// ПРАВИЛО ЭТОГО ПАКЕТА: отчёт обязан быть ОДНОЙ фразой, понятной человеку с
// телефона. «Kernel-Power 41» — это не отчёт; «пропало питание в 5:35, меня не
// было 4 часа» — отчёт.
package selfheal

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/host"

	"tgcontrol/internal/atomicfile"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/version"
)

// beatInterval — как часто агент отмечается в heartbeat.json. Минута выбрана
// как компромисс: точность «когда нас не стало» ±1 минута, а записей на диск —
// 1440 в сутки по 200 байт.
const beatInterval = time.Minute

// beat — то, что агент оставляет о себе на диске между запусками.
type beat struct {
	At      int64  `json:"at"`                 // unix-секунды последнего сердцебиения
	PID     int    `json:"pid"`                // чей это был процесс
	Version string `json:"version,omitempty"`  // какая версия работала
	BootAt  int64  `json:"boot_at,omitempty"`  // время загрузки ОС, каким его видел агент
	StopAt  int64  `json:"stop_at,omitempty"`  // штатный выход: когда попрощались
	StopWhy string `json:"stop_why,omitempty"` // штатный выход: почему (update/signal/service)
}

// Report — ответ на вопрос «что случилось». Уезжает в лог, в API, в `doctor` и
// (когда случилось нештатное) в Telegram владельцу.
type Report struct {
	// Kind: first_run | reboot | power_loss | bugcheck | agent_crash |
	// agent_restart | running.
	Kind string `json:"kind"`
	// Title — одна фраза для человека.
	Title string `json:"title"`
	// Detail — подробность, если она есть (кто инициировал, код ошибки).
	Detail string `json:"detail,omitempty"`
	// GapSeconds — сколько секунд компьютера не было в приложении.
	GapSeconds int64 `json:"gap_seconds"`
	// OffSeconds — сколько из них система была выключена (0, если не гасла).
	OffSeconds int64 `json:"off_seconds"`
	// Rebooted — перезагружалась ли ОС.
	Rebooted bool `json:"rebooted"`
	// Unexpected — повод сказать владельцу: нештатное выключение, падение.
	Unexpected bool `json:"unexpected"`
	// BootAt/LastSeen/At — метки времени (unix-секунды).
	BootAt   int64 `json:"boot_at"`
	LastSeen int64 `json:"last_seen,omitempty"`
	At       int64 `json:"at"`
	// Repairs — что агент починил сам при возвращении.
	Repairs []string `json:"repairs,omitempty"`
	// PrevVersion — версия агента до перерыва (видно, что это было обновление).
	PrevVersion string `json:"prev_version,omitempty"`
}

var state struct {
	sync.Mutex
	report Report
	ready  bool
}

// beatPath — heartbeat.json рядом с логом и config.json.
func beatPath() string { return filepath.Join(paths.Base(), "heartbeat.json") }

// Start снимает отчёт о прошлом перерыве и запускает сердцебиение.
// Зовётся один раз при старте агента. notify получает готовый текст, когда
// случившееся стоит сообщить владельцу (nil — не сообщать никому).
func Start(notify func(text string)) {
	bootAt := systemBootTime()
	prev, _ := readBeat()
	rep := analyze(time.Now(), bootAt, prev, func(since, boot time.Time) *ShutdownInfo {
		return lastShutdown(since, boot)
	})

	state.Lock()
	state.report = rep
	state.ready = true
	state.Unlock()

	log.Printf("[BOOT] %s", rep.Title)
	if rep.Detail != "" {
		log.Printf("[BOOT] %s", rep.Detail)
	}

	writeBeat(beat{At: time.Now().Unix(), PID: os.Getpid(), Version: version.Version, BootAt: bootAt.Unix()})
	go beatLoop()

	if notify != nil && rep.Unexpected {
		// Пауза перед сообщением: сразу после старта облако ещё не поднято, и
		// отправка ушла бы в пустоту. Полторы минуты — с запасом на первый
		// коннект к релею (backoff начинается с секунды).
		go func() {
			time.Sleep(90 * time.Second)
			notify(NotifyText(rep))
		}()
	}
}

// NotifyText — текст сообщения владельцу. Отдельно от Title, потому что в
// Telegram нужен ещё и заголовок с компьютером, и подсказка, что делать.
func NotifyText(r Report) string {
	msg := "🖥 " + r.Title
	if r.Detail != "" {
		msg += "\n" + r.Detail
	}
	if len(r.Repairs) > 0 {
		msg += "\n\nПочинено само: " + joinRu(r.Repairs)
	}
	return msg
}

// Last возвращает отчёт этого запуска. Второе значение — был ли он собран.
func Last() (Report, bool) {
	state.Lock()
	defer state.Unlock()
	return state.report, state.ready
}

// AddRepair отмечает в отчёте починку, сделанную при возвращении (автозапуск,
// VPN, уборка сирот). Видно и в приложении, и в сообщении владельцу.
func AddRepair(text string) {
	state.Lock()
	defer state.Unlock()
	for _, r := range state.report.Repairs {
		if r == text {
			return // одна и та же починка не должна попадать в отчёт дважды
		}
	}
	state.report.Repairs = append(state.report.Repairs, text)
}

// MarkStop записывает штатный выход: следующий запуск не назовёт это падением.
// why — «update» (перезапуск ради обновления), «signal» (Ctrl-C / SIGTERM),
// «user» (человек закрыл окно).
func MarkStop(why string) {
	b, _ := readBeat()
	now := time.Now().Unix()
	nb := beat{At: now, PID: os.Getpid(), Version: version.Version, StopAt: now, StopWhy: why}
	if b != nil {
		nb.BootAt = b.BootAt
	}
	writeBeat(nb)
}

func beatLoop() {
	bootAt := systemBootTime().Unix()
	t := time.NewTicker(beatInterval)
	defer t.Stop()
	for range t.C {
		writeBeat(beat{At: time.Now().Unix(), PID: os.Getpid(), Version: version.Version, BootAt: bootAt})
	}
}

func readBeat() (*beat, error) {
	data, err := os.ReadFile(beatPath())
	if err != nil {
		return nil, err
	}
	var b beat
	if err := json.Unmarshal(data, &b); err != nil || b.At == 0 {
		return nil, fmt.Errorf("heartbeat: битый файл")
	}
	return &b, nil
}

func writeBeat(b beat) {
	data, err := json.Marshal(b)
	if err != nil {
		return
	}
	_ = atomicfile.WriteFile(beatPath(), data, 0o600)
}

// systemBootTime — когда загрузилась ОС. При ошибке возвращает нулевое время:
// анализ тогда честно говорит «не знаю» вместо выдуманной перезагрузки.
func systemBootTime() time.Time {
	sec, err := host.BootTime()
	if err != nil || sec == 0 {
		return time.Time{}
	}
	return time.Unix(int64(sec), 0)
}

// analyze — вся логика отчёта, без файлов и без ОС: три метки времени на входе,
// готовая фраза на выходе. lookup спрашивает журнал ОС о причине выключения и
// зовётся ТОЛЬКО когда система действительно перезагружалась (на Windows это
// внешний процесс — дёргать его на каждом рестарте агента незачем).
func analyze(now, bootAt time.Time, prev *beat, lookup func(since, boot time.Time) *ShutdownInfo) Report {
	r := Report{At: now.Unix(), BootAt: bootAt.Unix()}
	if prev == nil {
		r.Kind = "first_run"
		r.Title = "Первый запуск на этом компьютере — следить за перерывами буду с этой минуты."
		return r
	}
	r.LastSeen = prev.At
	r.PrevVersion = prev.Version
	last := time.Unix(prev.At, 0)
	r.GapSeconds = int64(now.Sub(last).Seconds())
	if r.GapSeconds < 0 {
		r.GapSeconds = 0
	}

	// Перезагружалась ли система. Часы могут переводиться, метка загрузки на
	// Windows считается вычитанием аптайма из «сейчас» и потому слегка плавает —
	// поэтому граница с запасом в минуту, иначе обычный рестарт агента
	// периодически объявлялся бы перезагрузкой компьютера.
	rebooted := !bootAt.IsZero() && last.Before(bootAt.Add(-time.Minute))
	r.Rebooted = rebooted

	if !rebooted {
		// Система на месте — исчезал сам агент.
		if prev.StopAt > 0 {
			r.Kind = "agent_restart"
			switch prev.StopWhy {
			case "update":
				r.Title = fmt.Sprintf("Remotai перезапустился для обновления%s — перерыв %s.",
					versionSuffix(prev.Version), humanDur(r.GapSeconds))
			default:
				r.Title = fmt.Sprintf("Remotai был остановлен штатно и вернулся — перерыв %s.", humanDur(r.GapSeconds))
			}
			// Долгий перерыв даже при штатной остановке — уже новость: значит
			// приложение стояло выключенным, а компьютер всё это время был вне
			// управления.
			r.Unexpected = r.GapSeconds > 15*60
			return r
		}
		r.Kind = "agent_crash"
		r.Title = fmt.Sprintf("Компьютер не перезагружался, а Remotai пропадал на %s — приложение закрыли или оно упало.",
			humanDur(r.GapSeconds))
		r.Detail = "Вернулся сам. Если это повторяется — пришлите скриншот, разберу по логу."
		r.Unexpected = r.GapSeconds > 3*60
		return r
	}

	// Система перезагружалась: сколько она была выключена и почему.
	off := int64(bootAt.Sub(last).Seconds())
	if off < 0 {
		off = 0
	}
	r.OffSeconds = off

	var info *ShutdownInfo
	if lookup != nil {
		info = lookup(last, bootAt)
	}
	when := bootAt.Format("02.01 в 15:04")
	switch {
	case info != nil && info.Kind == "bugcheck":
		r.Kind = "bugcheck"
		r.Title = fmt.Sprintf("Компьютер упал в синий экран и перезагрузился %s. Меня не было %s.", when, humanDur(r.GapSeconds))
		r.Detail = "Код ошибки: " + info.Detail
		r.Unexpected = true
	case info != nil && info.Kind == "power":
		r.Kind = "power_loss"
		r.Title = fmt.Sprintf("Компьютер выключился нештатно и загрузился %s. Меня не было %s.", when, humanDur(r.GapSeconds))
		r.Detail = "Похоже на пропажу питания или зависание: система не завершала работу штатно."
		r.Unexpected = true
	case info != nil && info.Kind == "planned":
		r.Kind = "reboot"
		r.Title = fmt.Sprintf("Компьютер перезагрузился %s — %s. Меня не было %s.", when, info.Reason, humanDur(r.GapSeconds))
		if off > 0 {
			r.Detail = fmt.Sprintf("Выключен был %s.", humanDur(off))
		}
		// Плановая перезагрузка — не тревога, если компьютер вернулся быстро.
		// Ночь без питания после «планового» выключения новость всё равно.
		r.Unexpected = r.GapSeconds > 2*60*60
	default:
		r.Kind = "reboot"
		r.Title = fmt.Sprintf("Компьютер перезагружался и вернулся %s. Меня не было %s.", when, humanDur(r.GapSeconds))
		if off > 0 {
			r.Detail = fmt.Sprintf("Выключен был %s.", humanDur(off))
		}
		r.Unexpected = r.GapSeconds > 2*60*60
	}
	return r
}

func versionSuffix(prev string) string {
	if prev == "" || prev == version.Version {
		return ""
	}
	return " (" + prev + " → " + version.Version + ")"
}

// humanDur — длительность словами, без «0 ч 0 мин 12 с».
func humanDur(sec int64) string {
	switch {
	case sec < 90:
		return fmt.Sprintf("%d с", sec)
	case sec < 60*60:
		return fmt.Sprintf("%d мин", (sec+30)/60)
	case sec < 24*60*60:
		h := sec / 3600
		m := (sec % 3600) / 60
		if m == 0 {
			return fmt.Sprintf("%d ч", h)
		}
		return fmt.Sprintf("%d ч %d мин", h, m)
	default:
		d := sec / (24 * 3600)
		h := (sec % (24 * 3600)) / 3600
		if h == 0 {
			return fmt.Sprintf("%d сут", d)
		}
		return fmt.Sprintf("%d сут %d ч", d, h)
	}
}

func joinRu(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
