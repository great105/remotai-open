package selfheal

import (
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t
}

func noLookup(time.Time, time.Time) *ShutdownInfo { return nil }

func TestFirstRunНеПугает(t *testing.T) {
	r := analyze(at("2026-08-07 10:00:00"), at("2026-08-07 09:00:00"), nil, noLookup)
	if r.Kind != "first_run" || r.Unexpected {
		t.Fatalf("первый запуск должен быть тихим: %+v", r)
	}
}

func TestПерезагрузкаСПричиной(t *testing.T) {
	boot := at("2026-08-07 03:14:00")
	prev := &beat{At: at("2026-08-07 03:05:00").Unix(), BootAt: at("2026-08-01 00:00:00").Unix()}
	lookup := func(since, b time.Time) *ShutdownInfo {
		return &ShutdownInfo{Kind: "planned", At: at("2026-08-07 03:10:00"), Reason: "установка обновлений Windows"}
	}
	r := analyze(at("2026-08-07 03:16:00"), boot, prev, lookup)
	if r.Kind != "reboot" || !r.Rebooted {
		t.Fatalf("ожидалась перезагрузка: %+v", r)
	}
	if !strings.Contains(r.Title, "установка обновлений Windows") {
		t.Fatalf("причина не попала в отчёт: %q", r.Title)
	}
	if !strings.Contains(r.Title, "11 мин") {
		t.Fatalf("перерыв посчитан неверно: %q", r.Title)
	}
	if r.Unexpected {
		t.Fatalf("плановая перезагрузка на 11 минут — не повод будить владельца: %+v", r)
	}
}

func TestПропалоПитание(t *testing.T) {
	boot := at("2026-08-07 09:00:00")
	prev := &beat{At: at("2026-08-07 04:47:00").Unix()}
	lookup := func(since, b time.Time) *ShutdownInfo {
		return &ShutdownInfo{Kind: "power", At: at("2026-08-07 09:00:10")}
	}
	r := analyze(at("2026-08-07 09:01:00"), boot, prev, lookup)
	if r.Kind != "power_loss" || !r.Unexpected {
		t.Fatalf("нештатное выключение обязано быть тревогой: %+v", r)
	}
	if r.OffSeconds != int64(4*3600+13*60) {
		t.Fatalf("время «выключен» посчитано неверно: %d", r.OffSeconds)
	}
}

func TestСинийЭкранВажнееВсего(t *testing.T) {
	boot := at("2026-08-07 09:00:00")
	prev := &beat{At: at("2026-08-07 08:50:00").Unix()}
	lookup := func(since, b time.Time) *ShutdownInfo {
		return &ShutdownInfo{Kind: "bugcheck", Detail: "0xd1"}
	}
	r := analyze(at("2026-08-07 09:02:00"), boot, prev, lookup)
	if r.Kind != "bugcheck" || !r.Unexpected || !strings.Contains(r.Detail, "0xd1") {
		t.Fatalf("синий экран должен называться кодом: %+v", r)
	}
}

// Ради этого случая пакет и писался иначе, чем «показать аптайм»: система на
// месте, а агента не было. Раньше это выглядело как перезагрузка компьютера.
func TestАгентПадалБезПерезагрузки(t *testing.T) {
	boot := at("2026-08-01 00:00:00")
	prev := &beat{At: at("2026-08-07 08:00:00").Unix(), BootAt: boot.Unix()}
	r := analyze(at("2026-08-07 08:40:00"), boot, prev, noLookup)
	if r.Kind != "agent_crash" || r.Rebooted {
		t.Fatalf("падение агента не должно читаться как перезагрузка ПК: %+v", r)
	}
	if !r.Unexpected {
		t.Fatalf("40 минут без управления — новость: %+v", r)
	}
}

func TestОбновлениеАгентаНеТревога(t *testing.T) {
	boot := at("2026-08-01 00:00:00")
	prev := &beat{
		At: at("2026-08-07 08:00:00").Unix(), BootAt: boot.Unix(),
		StopAt: at("2026-08-07 08:00:00").Unix(), StopWhy: "update", Version: "2.53.3",
	}
	r := analyze(at("2026-08-07 08:00:25"), boot, prev, noLookup)
	if r.Kind != "agent_restart" || r.Unexpected {
		t.Fatalf("штатный перезапуск ради обновления — не тревога: %+v", r)
	}
}

// Метка загрузки на Windows вычисляется вычитанием аптайма и слегка плавает;
// без запаса обычный рестарт агента периодически объявлялся бы перезагрузкой.
func TestДрожаниеМеткиЗагрузкиНеПерезагрузка(t *testing.T) {
	boot := at("2026-08-07 08:00:30")
	prev := &beat{At: at("2026-08-07 08:00:00").Unix(), BootAt: at("2026-08-07 07:59:50").Unix()}
	r := analyze(at("2026-08-07 08:00:40"), boot, prev, noLookup)
	if r.Rebooted {
		t.Fatalf("расхождение в 30 секунд — не перезагрузка: %+v", r)
	}
}

func TestДлительностиСловами(t *testing.T) {
	cases := map[int64]string{45: "45 с", 200: "3 мин", 3600: "1 ч", 5400: "1 ч 30 мин", 90000: "1 сут 1 ч"}
	for sec, want := range cases {
		if got := humanDur(sec); got != want {
			t.Errorf("humanDur(%d) = %q, ожидалось %q", sec, got, want)
		}
	}
}

func TestИнициаторПереводится(t *testing.T) {
	cases := map[string]string{
		`C:\WINDOWS\servicing\TrustedInstaller.exe (WIN-MNT62H2M8ST)`:                                                       "установка обновлений Windows",
		`C:\Windows\SystemApps\Microsoft.Windows.StartMenuExperienceHost_cw5n1h2txyewy\StartMenuExperienceHost.exe (WIN-X)`: "перезагрузку попросил человек за компьютером",
		`C:\Windows\System32\shutdown.exe`:                                                                                  "команда shutdown",
	}
	for in, want := range cases {
		if got := initiatorReason(in); got != want {
			t.Errorf("initiatorReason(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// wevtutil печатает локализованные поля в кодировке консоли — один такой байт
// раньше валил разбор всего документа вместе с нужными нам ASCII-полями.
func TestБитыеБайтыНеЛомаютРазбор(t *testing.T) {
	broken := "ok-\xd0\xd1\xd2-tail"
	got := sanitizeUTF8(broken)
	if !strings.HasPrefix(got, "ok-") || !strings.HasSuffix(got, "-tail") {
		t.Fatalf("полезные части потерялись: %q", got)
	}
}
