package web

import (
	"fmt"
	"strings"
	"testing"
)

// ST-01: строка лога агента для {t:"diag"} клиента. До выноса в
// formatClientDiag любой незнакомый вид печатался форматом alt-scroll, и
// snapshot-adopt / snapshot-width / alt-scroll-page-dead выходили в лог одними
// `<nil>` — разбирать по такому логу было нечего.

func TestFormatClientDiagExplicitKindsKeepTheirFields(t *testing.T) {
	cases := []struct {
		name string
		ctrl map[string]any
		want []string
	}{
		{"snapshot-adopt", map[string]any{"t": "diag", "what": "snapshot-adopt", "snap": "120x40", "was": "48x30", "client": "120x40"},
			[]string{"what=snapshot-adopt", "снапшот=120x40", "было=48x30", "клиент=120x40"}},
		{"snapshot-width", map[string]any{"t": "diag", "what": "snapshot-width", "snap": "48x30", "client": "232x40", "auth": float64(232)},
			[]string{"what=snapshot-width", "снапшот=48x30", "клиент=232x40", "авторитет=232"}},
		{"alt-scroll-page-dead", map[string]any{"t": "diag", "what": "alt-scroll-page-dead", "owner": "application", "lines": float64(31), "bytes": float64(16384), "own": float64(0), "changed": float64(3), "shifted": float64(0)},
			[]string{"what=alt-scroll-page-dead", "владелец=application", "строк=31", "байт=16384", "своя=0", "изменилось=3", "сдвинулось=0"}},
		{"trace-mark", map[string]any{"t": "diag", "what": "trace-mark", "seq": float64(4097), "events": float64(4096), "dropped": float64(1), "bytes": float64(0)},
			[]string{"what=trace-mark", "seq=4097", "событий=4096", "выброшено=1", "байт=0"}},
		{"retention-shadow", map[string]any{"t": "diag", "what": "retention-shadow", "legacy": "discard", "policy": "write", "reading": false, "gen": "codex:4242:1789000000000"},
			[]string{"what=retention-shadow", "legacy=discard", "policy=write", "чтение=false", "gen=codex:4242:1789000000000"}},
		{"nav", map[string]any{"t": "diag", "what": "nav", "executor": "application", "channel": "page", "mode": "auto", "reason": "registry", "owner": "application"},
			[]string{"what=nav executor=application channel=page mode=auto reason=registry owner=application"}},
		{"flow", map[string]any{"t": "diag", "what": "flow", "maxQueued": float64(262144), "drops": float64(0), "pauses": map[string]any{"hidden": float64(2), "backlog": float64(1)}},
			[]string{"what=flow maxQueued=262144 pauses.hidden=2 pauses.backlog=1 drops=0"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := formatClientDiag("S1", c.ctrl)
			if !strings.HasPrefix(line, "[PTY-WS] client-diag id=S1 what=") {
				t.Fatalf("prefix: %q", line)
			}
			if strings.Contains(line, "<nil>") {
				t.Fatalf("поля потеряны: %q", line)
			}
			for _, want := range c.want {
				if !strings.Contains(line, want) {
					t.Fatalf("нет %q в %q", want, line)
				}
			}
		})
	}
}

// Прежние форматы не меняются ни на байт: старые и новые логи читаются одним
// разбором. alt-scroll — только для what=="alt-scroll".
func TestFormatClientDiagLegacyFormatsUnchanged(t *testing.T) {
	alt := map[string]any{"t": "diag", "what": "alt-scroll", "alt": true, "mouse": "sgr", "cols": float64(80), "rows": float64(24), "owner": "terminal", "lines": float64(40), "bytes": float64(16384), "own": float64(120)}
	want := fmt.Sprintf("[PTY-WS] client-diag id=%s what=%v alt=%v mouse=%v cols=%v rows=%v владелец=%v строк=%v байт=%v своя=%v",
		"S1", alt["what"], alt["alt"], alt["mouse"], alt["cols"], alt["rows"], alt["owner"], alt["lines"], alt["bytes"], alt["own"])
	if got := formatClientDiag("S1", alt); got != want {
		t.Fatalf("alt-scroll:\n got %q\nwant %q", got, want)
	}
	stale := map[string]any{"t": "diag", "what": "snapshot-stale", "base": float64(100), "accepted": float64(150), "applied": float64(140)}
	if got := formatClientDiag("S1", stale); got != "[PTY-WS] client-diag id=S1 what=snapshot-stale база=100 принято=150 показано=140 — кадр устарел, клиент запросил свежий" {
		t.Fatalf("snapshot-stale: %q", got)
	}
	restored := map[string]any{"t": "diag", "what": "alt-scroll-verdict-restored", "process": "claude", "page": false, "wheel": true}
	if got := formatClientDiag("S1", restored); got != "[PTY-WS] client-diag id=S1 what=alt-scroll-verdict-restored процесс=claude страницы=false колесо=true — вердикт взят из памяти, а не измерен" {
		t.Fatalf("verdict-restored: %q", got)
	}
	// Незнакомый вид больше не печатается форматом alt-scroll.
	if got := formatClientDiag("S1", map[string]any{"what": "future-kind", "x": float64(1)}); strings.Contains(got, "владелец=") || strings.Contains(got, "<nil>") {
		t.Fatalf("unknown kind fell back to alt-scroll format: %q", got)
	}
}

// Прежние форматы печатали присланное клиентом сырым %v: перевод строки в
// значении подделывал следующую строку журнала агента, ESC уходил в журнал как
// есть, 100 КБ — целиком. Теперь каждое значение идёт через legacyDiagValue.
func TestFormatClientDiagLegacyFormatsSanitizeClientValues(t *testing.T) {
	legacy := map[string][]string{
		"tg-chrome":                   {"client", "surface", "platform", "ver", "fullscreen", "screenW", "screenH", "winW", "winH", "vpW", "vpH", "reported", "inset", "backTop"},
		"snapshot-stale":              {"base", "accepted", "applied"},
		"snapshot-geometry-stale":     {"frame_rev", "client_rev"},
		"snapshot":                    {"server", "local", "replace", "snap", "logical", "visible", "keyboard", "action"},
		"alt-scroll-verdict-restored": {"process", "page", "wheel"},
		"alt-scroll":                  {"alt", "mouse", "cols", "rows", "owner", "lines", "bytes", "own"},
	}
	hostile := map[string]any{
		"lf":     "ok\n[PTY-WS] client-diag id=S1 what=forged",
		"cr":     "ok\r[PTY-WS] forged",
		"crlf":   "ok\r\n2026/09/15 10:00:00 [PTY-WS] forged",
		"esc":    "\x1b[2J\x1b[31mred\x1b]0;title\x07",
		"huge":   strings.Repeat("Z", 100<<10), // не «x»: в tg-chrome `screen=%sx%s` сам литерал — «x»
		"nested": map[string]any{"deep": "secret\nnested"},
		"list":   []any{"secret\nlist"},
	}
	for kind, keys := range legacy {
		for name, bad := range hostile {
			t.Run(kind+"/"+name, func(t *testing.T) {
				ctrl := map[string]any{"t": "diag", "what": kind}
				for _, k := range keys {
					ctrl[k] = bad
				}
				line := formatClientDiag("S1", ctrl)
				if !strings.HasPrefix(line, "[PTY-WS] client-diag id=S1 what="+kind+" ") {
					t.Fatalf("формат вида потерян: %q", line[:min(len(line), 120)])
				}
				if strings.ContainsAny(line, "\n\r\x1b\x07") {
					t.Fatalf("управляющий символ клиента в строке журнала: %q", line[:min(len(line), 200)])
				}
				if strings.Contains(line, "secret") {
					t.Fatalf("вложенное значение напечатано: %q", line)
				}
				// 14 полей tg-chrome по 64 символа с кавычками и экранированием —
				// меньше 2 КБ; сырые 100 КБ сюда не пролезут.
				if len(line) > 2048 || strings.Contains(line, strings.Repeat("Z", diagMaxValueRunes)) {
					t.Fatalf("длина не ограничена: %d байт", len(line))
				}
			})
		}
	}
}

// Очистка не меняет обычные значения прежних форматов ни на байт — включая
// пустую строку (platform у веба и APK) и отсутствующее поле (`<nil>`), как
// печатал прежний %v.
func TestFormatClientDiagLegacyNormalValuesByteForByte(t *testing.T) {
	tg := map[string]any{"t": "diag", "what": "tg-chrome", "client": "2.71.0", "surface": "web", "platform": "", "ver": "",
		"fullscreen": false, "screenW": float64(390), "screenH": float64(844), "winW": float64(390), "winH": float64(664),
		"vpW": float64(390), "vpH": float64(664), "reported": float64(-1), "inset": "0px"} // backTop не прислан
	wantTg := fmt.Sprintf("[PTY-WS] client-diag id=%s what=tg-chrome client=%v surface=%v platform=%v ver=%v fullscreen=%v screen=%vx%v win=%vx%v vp=%vx%v reported=%v inset=%v backTop=%v",
		"S1", tg["client"], tg["surface"], tg["platform"], tg["ver"], tg["fullscreen"],
		tg["screenW"], tg["screenH"], tg["winW"], tg["winH"],
		tg["vpW"], tg["vpH"], tg["reported"], tg["inset"], tg["backTop"])
	if got := formatClientDiag("S1", tg); got != wantTg {
		t.Fatalf("tg-chrome:\n got %q\nwant %q", got, wantTg)
	}
	snap := map[string]any{"t": "diag", "what": "snapshot", "server": float64(1200), "local": float64(40), "replace": true,
		"snap": "120x40", "logical": "48x30", "visible": "48x11", "keyboard": true, "action": "adopt"}
	wantSnap := fmt.Sprintf("[PTY-WS] client-diag id=%s what=snapshot зеркало=%v своя=%v заменить=%v снапшот=%v логический=%v видимо=%v клавиатура=%v решение=%v",
		"S1", snap["server"], snap["local"], snap["replace"], snap["snap"],
		snap["logical"], snap["visible"], snap["keyboard"], snap["action"])
	if got := formatClientDiag("S1", snap); got != wantSnap {
		t.Fatalf("snapshot:\n got %q\nwant %q", got, wantSnap)
	}
	geo := map[string]any{"t": "diag", "what": "snapshot-geometry-stale", "frame_rev": float64(4), "client_rev": float64(7)}
	if got := formatClientDiag("S1", geo); got != "[PTY-WS] client-diag id=S1 what=snapshot-geometry-stale кадр_rev=4 клиент_rev=7 — кадр прежней геометрии отвергнут, клиент запросил свежий" {
		t.Fatalf("snapshot-geometry-stale: %q", got)
	}
	// Большое число — тем же %v, что и раньше (1.234567e+06), а не иначе.
	big := map[string]any{"t": "diag", "what": "alt-scroll", "alt": false, "mouse": "none", "cols": float64(80), "rows": float64(24),
		"owner": "application", "lines": float64(31), "bytes": float64(1234567), "own": float64(0)}
	if got := formatClientDiag("S1", big); !strings.Contains(got, fmt.Sprintf(" байт=%v ", big["bytes"])) {
		t.Fatalf("alt-scroll bytes: %q", got)
	}
}

// Общий формат: отсортированные скалярные поля, не больше 16, значения не
// длиннее 64 символов, без вложенных объектов, без запрещённых полей (I-15) и
// без сырого перевода строки, которым можно подделать следующую строку лога.
func TestFormatClientDiagGenericIsBoundedAndSafe(t *testing.T) {
	ctrl := map[string]any{
		"t": "diag", "what": "future-kind",
		"zeta": float64(2), "alpha": "a", "flag": true,
		"nested":    map[string]any{"deep": "secret-nested"},
		"list":      []any{"secret-list"},
		"text":      "secret-text",
		"clipboard": "secret-clip",
		"long":      strings.Repeat("ж", 200),
		"inject":    "ok\n[PTY-WS] forged line",
		"bad key!":  "x",
		"big":       float64(1234567),
	}
	line := formatClientDiag("S1", ctrl)
	if !strings.HasPrefix(line, "[PTY-WS] client-diag id=S1 what=future-kind alpha=a big=1234567 flag=true inject=") {
		t.Fatalf("порядок или числа: %q", line)
	}
	for _, leak := range []string{"secret", "nested", "list=", "bad key", "\n"} {
		if strings.Contains(line, leak) {
			t.Fatalf("в лог попало %q: %q", leak, line)
		}
	}
	idx := strings.Index(line, "long=")
	if idx < 0 {
		t.Fatalf("нет long: %q", line)
	}
	value := strings.SplitN(line[idx+len("long="):], " ", 2)[0]
	if n := len([]rune(value)); n != 64 || !strings.HasSuffix(value, "…") {
		t.Fatalf("длина значения %d (%q)", n, value)
	}

	many := map[string]any{"what": "many"}
	for i := 0; i < 40; i++ {
		many[fmt.Sprintf("k%02d", i)] = float64(i)
	}
	line = formatClientDiag("S1", many)
	// id= и what= в префиксе, 16 полей и пометка о пропущенных.
	if strings.Count(line, "=") != 2+16+1 || !strings.Contains(line, "k15=15") || strings.Contains(line, "k16=") || !strings.HasSuffix(line, "пропущено_полей=24") {
		t.Fatalf("лимит полей: %q", line)
	}
}

// I-15: из upload-diag имя файла в лог не попадает — только расширение.
func TestFormatClientDiagUploadKeepsOnlyExtension(t *testing.T) {
	ctrl := map[string]any{"t": "diag", "what": "upload", "name": "Паспорт Иванова.PDF", "size": float64(2048), "type": "application/pdf", "code": "net", "status": float64(413), "message": "HTTP 413"}
	line := formatClientDiag("S1", ctrl)
	if strings.Contains(line, "Паспорт") || strings.Contains(line, "Иванова") {
		t.Fatalf("имя файла в логе: %q", line)
	}
	for _, want := range []string{"what=upload", "расширение=.pdf", "размер=2048", "тип=application/pdf", "статус=413", `ошибка="HTTP 413"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("нет %q в %q", want, line)
		}
	}
	for name, want := range map[string]string{"": "-", "README": "-", ".bashrc": "-", `C:\docs\a.tar.GZ`: ".gz", "x.<script>": "-", "a.verylongextension": "-"} {
		if got := uploadDiagExt(name); got != want {
			t.Errorf("uploadDiagExt(%q) = %q, want %q", name, got, want)
		}
	}
}
