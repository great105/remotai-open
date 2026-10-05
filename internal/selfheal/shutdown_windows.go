//go:build windows

package selfheal

import (
	"context"
	"encoding/xml"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/procutil"
)

// Журнал System отвечает на «почему компьютер выключился» четырьмя событиями:
//
//	1074 — кто-то попросил выключиться штатно (в EventData — процесс-инициатор
//	       и код причины); пишется ДО выключения, то есть в прошлом сеансе;
//	  41 — Kernel-Power: система не завершила работу штатно. BugcheckCode ≠ 0 —
//	       был синий экран, 0 — питание/зависание. Пишется ПОСЛЕ загрузки;
//	6008 — «предыдущее завершение работы было неожиданным» (то же событие с
//	       другой стороны, оставлено для машин, где 41 подавлен);
//	1001 — WER-SystemErrorReporting, подробности синего экрана.
//
// Читаем через wevtutil: он есть в любой Windows, ЧИТАЕТ System БЕЗ ПРАВ
// АДМИНИСТРАТОРА (проверено на живой машине: 82 мс, код возврата 0) и не тянет
// за собой PowerShell, который на холодном старте стоит секунды.
const shutdownQuery = `*[System[(EventID=41 or EventID=1074 or EventID=6008 or EventID=1001)]]`

type winEvents struct {
	Events []winEvent `xml:"Event"`
}

// ГРАБЛЯ encoding/xml: путь через `>` нельзя совмещать с `,attr` — тег вида
// `xml:"System>Provider>Name,attr"` не ошибка компиляции, а ошибка РАЗБОРА в
// рантайме («chain not valid with attr flag»), которая роняет весь документ.
// Поэтому System описан вложенными структурами, а не цепочками.
type winEvent struct {
	System struct {
		EventID  int `xml:"EventID"`
		Provider struct {
			Name string `xml:"Name,attr"`
		} `xml:"Provider"`
		TimeCreated struct {
			SystemTime string `xml:"SystemTime,attr"`
		} `xml:"TimeCreated"`
	} `xml:"System"`
	Data []struct {
		Name  string `xml:"Name,attr"`
		Value string `xml:",chardata"`
	} `xml:"EventData>Data"`
}

func (e winEvent) id() int          { return e.System.EventID }
func (e winEvent) provider() string { return e.System.Provider.Name }

func (e winEvent) at() time.Time {
	t, err := time.Parse(time.RFC3339Nano, e.System.TimeCreated.SystemTime)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (e winEvent) field(name string) string {
	for _, d := range e.Data {
		if d.Name == name {
			return strings.TrimSpace(d.Value)
		}
	}
	return ""
}

// lastShutdown ищет причину выключения, случившегося между последним
// сердцебиением агента и загрузкой системы.
//
// ОКНА ПОИСКА РАЗНЫЕ, И ЭТО НЕ ПРИДИРКА: 1074 пишется перед выключением (до
// boot), а 41/6008/1001 — уже после загрузки (после boot). Один общий интервал
// либо терял бы половину событий, либо цеплял чужую перезагрузку.
func lastShutdown(since, boot time.Time) *ShutdownInfo {
	if boot.IsZero() {
		return nil
	}
	events, err := queryShutdownEvents()
	if err != nil {
		log.Printf("[BOOT] журнал System недоступен: %v", err)
		return nil
	}

	// Границы: событие «до» ищем в пределах двух часов до загрузки (длинная
	// установка обновлений укладывается), событие «после» — 15 минут после неё.
	beforeFrom := boot.Add(-2 * time.Hour)
	if !since.IsZero() && since.After(beforeFrom) {
		beforeFrom = since.Add(-time.Minute)
	}
	afterUntil := boot.Add(15 * time.Minute)

	var planned, unclean, bug *ShutdownInfo
	for _, ev := range events {
		at := ev.at()
		if at.IsZero() {
			continue
		}
		switch ev.id() {
		case 1074:
			if at.Before(beforeFrom) || at.After(boot) || planned != nil {
				continue // события идут от новых к старым — первое подходящее и есть ближайшее
			}
			// ЗАМЕР НА ЖИВОЙ МАШИНЕ: у перезагрузки из меню «Пуск» код причины
			// (param4) равен 0x0 — то есть флага SHTDN_REASON_FLAG_PLANNED нет
			// даже там, где человек нажал кнопку сам. Судить по этому флагу
			// «штатно/нештатно» значит врать в самом частом случае; нештатность
			// ловят события 41 и 6008, а 1074 сам по себе означает «кто-то
			// попросил выключиться» — остаётся только назвать, кто.
			reason := initiatorReason(sanitizeUTF8(ev.field("param1")))
			planned = &ShutdownInfo{Kind: "planned", At: at, Reason: reason}
		case 41:
			if at.Before(boot.Add(-5*time.Minute)) || at.After(afterUntil) {
				continue
			}
			code := ev.field("BugcheckCode")
			if code != "" && code != "0" && code != "0x0" {
				if bug == nil {
					bug = &ShutdownInfo{Kind: "bugcheck", At: at, Reason: "синий экран", Detail: bugcheckText(code, ev.field("BugcheckParameter1"))}
				}
				continue
			}
			if unclean == nil {
				unclean = &ShutdownInfo{Kind: "power", At: at, Reason: "питание пропало или система зависла"}
			}
		case 6008:
			if at.Before(boot.Add(-5*time.Minute)) || at.After(afterUntil) {
				continue
			}
			if unclean == nil {
				unclean = &ShutdownInfo{Kind: "power", At: at, Reason: "предыдущее завершение работы было неожиданным"}
			}
		case 1001:
			if bug != nil || at.Before(boot.Add(-5*time.Minute)) || at.After(afterUntil) {
				continue
			}
			if strings.Contains(strings.ToLower(ev.provider()), "systemerror") {
				bug = &ShutdownInfo{Kind: "bugcheck", At: at, Reason: "синий экран", Detail: "подробности в журнале Windows (WER)"}
			}
		}
	}

	// Приоритет: синий экран важнее «просто нештатного», а нештатное важнее
	// планового — при обновлениях Windows иногда есть и 1074, и 41.
	switch {
	case bug != nil:
		return bug
	case unclean != nil:
		return unclean
	default:
		return planned
	}
}

func bugcheckText(code, param1 string) string {
	out := "0x" + strings.TrimPrefix(strings.TrimPrefix(code, "0x"), "0X")
	if n, err := strconv.ParseUint(strings.TrimPrefix(code, "0x"), 0, 64); err == nil {
		out = "0x" + strconv.FormatUint(n, 16)
	}
	if param1 != "" && param1 != "0x0" {
		out += " (параметр " + param1 + ")"
	}
	return out
}

// queryShutdownEvents забирает последние события выключения из журнала System.
func queryShutdownEvents() ([]winEvent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "wevtutil", "qe", "System", "/q:"+shutdownQuery, "/rd:true", "/c:20", "/f:xml")
	procutil.Hidden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	// wevtutil отдаёт поток <Event>…</Event> без общего корня — оборачиваем.
	doc := "<Events>" + sanitizeUTF8(string(out)) + "</Events>"
	var parsed winEvents
	if err := xml.Unmarshal([]byte(doc), &parsed); err != nil {
		return nil, err
	}
	return parsed.Events, nil
}
