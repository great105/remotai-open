package pty

import (
	"bytes"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Broadcaster routes PTY events to user-facing transports (WebSocket / push
// notifications). Implemented by the web server, injected via Manager.SetBroadcaster.
type Broadcaster interface {
	Broadcast(uid int64, event any)
}

// EventKind enumerates the heuristic events produced by the detector.
type EventKind string

const (
	EventWaitingInput EventKind = "waiting_input"
	EventFinished     EventKind = "finished"
	EventError        EventKind = "error"
	// EventAgentReady — агент жив, но вопрос распознать не удалось и вывод
	// молчит: «освободился / вероятно ждёт». Наружу НЕ бродкастится (старый APK
	// делает switch по event без default и показал бы «undefined»), живёт только
	// как status в GET /api/pty. Раньше этот случай писался как
	// EventWaitingInput с пустым hint, из-за чего каждый закончивший агент
	// навсегда оставался в «Требует внимания» с текстом «ждёт вашего ответа».
	EventAgentReady EventKind = "agent_ready"
)

// hintKind — тип распознанного вопроса; определяет набор кнопок на экране
// терминала (Y/N, 1/2/3, один Enter). Значения ASCII и стабильные: клиент
// сравнивает строки.
const (
	HintKindYesNo  = "yes_no"
	HintKindChoice = "choice"
	HintKindEnter  = "enter"
	HintKindText   = "text"
)

const (
	detectorTick    = 1 * time.Second
	debounceWindow  = 15 * time.Second
	idleWaiting     = 3 * time.Second
	idleFinished    = 2 * time.Second
	finishedMinSize = 1024 // bytes of output required before emitting "finished"
	finishedMinDur  = 5 * time.Second

	// agentFinishedIdle — устойчивая тишина, после которой эпизод работы
	// AI-агента признаётся завершившимся (событие finished наружу). Ощутимо
	// выше idleWaiting: Kimi Code в режиме thinking не рисует ни спиннера, ни
	// таймера и молчит минутами — пауза между шагами НЕ конец работы.
	agentFinishedIdle = 45 * time.Second
	// agentFinishedIdleNoStatus — та же тишина для агента, у которого НЕТ
	// честного источника статуса (session-файл есть только у Claude Code, см.
	// agentBusyPerRuntimeFile). Kimi и Codex думают дольше 45 с молча — и
	// «агент закончил, работал N минут» уходило владельцу посреди задачи, а
	// следом второй раз, когда он закончил на самом деле (зона терминала:
	// «событие v1 даёт преждевременный finished у не-Claude агентов»). Без
	// источника ждём вдвое дольше: уведомление приходит на минуту позже, зато
	// не врёт.
	agentFinishedIdleNoStatus = 120 * time.Second
	// agentFinishedMinWork — минимальная длительность самой работы (первый байт
	// всплеска → последний). Отсекает набор текста человеком в поле ввода
	// агента: эхо клавиш и перерисовка рамки дают байты и «активность», но это
	// не эпизод работы, и «агент закончил» по нему звучать не должно.
	agentFinishedMinWork = 10 * time.Second

	// Окно, в котором ищем вопрос агента (см. lastVisibleTail).
	tailRawBytes     = 8192
	tailVisibleBytes = 2048
	tailVisibleLines = 15

	// errorStatusTTL — сколько «error» держится статусом в списке терминалов
	// (statusFrom) и латчем в детекторе. Одно значение на оба места: пока латча
	// не было, следующий же спокойный тик перебивал ошибку статусом «свободен»,
	// и красная карточка гасла через секунду после появления.
	errorStatusTTL = 5 * time.Minute

	// questionFreshTTL — сколько «вопрос виден на экране» считается свежим
	// после последнего подтверждения детектором (тик раз в секунду). Больше
	// одного пропущенного тика: агент перерисовывает меню целиком, и в
	// отдельный кадр маркер вопроса попасть не успевает — на кнопках ответа это
	// выглядело бы морганием.
	questionFreshTTL = 3 * time.Second

	// errorSettleTTL — сколько пометка «в выводе была строка ошибки» ждёт
	// остановки терминала. Ошибка, после которой агент работал ещё полторы
	// минуты, исходом не считается: он с ней справился (см. settleError).
	errorSettleTTL = 90 * time.Second

	// errorVisibleProbe — по скольким первым символам строки сверяем, что она
	// ВСЁ ЕЩЁ на экране. Хвост строки агент мог дорисовать или обрезать шириной
	// окна, начало — нет.
	errorVisibleProbe = 24

	// Окна для понятных уведомлений (см. SummaryEvent). Шире, чем окна детекции:
	// там ищут признак, а здесь нужен СМЫСЛ — что агент сделал за эпизод.
	// Перерисовки схлопнет уже получатель (internal/agentsummary.Clean), поэтому
	// брать с запасом дешевле, чем недобрать и пересказывать один подвал.
	summaryEpisodeBytes  = 96 << 10
	summaryQuestionBytes = 16 << 10

	// Окно, в котором ищем саму строку ошибки, — заметно шире окна поиска
	// вопроса: у полноэкранного агента один кадр перерисовки занимает килобайты,
	// и упавшая команда легко оказывается выше рамки ввода со всеми подсказками.
	// Ошибиться здесь в узкую сторону значит промолчать о настоящем падении.
	errorTailRawBytes     = 32768
	errorTailVisibleBytes = 8192
	errorTailLines        = 60
)

// ansiRe matches CSI / OSC escape sequences so we can inspect the visible
// tail of the buffer without ANSI noise.
//
// OSC завершается BEL ИЛИ ST (ESC \). Раньше распознавался только BEL, и OSC
// с ST оставался в «видимом» тексте: служебное `ESC]133;A ESC\` перед строкой
// ломало якорь начала строки у errorRe (настоящая ошибка терялась), а BEL
// где-нибудь дальше заставлял выражение съесть весь видимый текст до него.
// Разметка команд (OSC 133, ST-10) сама шлёт BEL, но чужие интеграции
// (fish ≥ 4, starship, oh-my-posh) шлют и ST. Тело OSC поэтому не заходит за
// ESC: следующий ESC — либо ST, либо уже другая последовательность.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[=>78]`)

// errorRe is intentionally conservative — short tokens like "error" produce
// many false positives. Anchored to line start with case-insensitive match.
//
// Отступ — ТОЛЬКО горизонтальный (`[^\S\r\n]*`, а не `\s*`): `\s` включает
// перевод строки, поэтому совпадение начиналось на пустой строке ВЫШЕ и
// захватывало её вместе с переносами. Строкой ошибки в таком случае считалась
// пустая — отсюда и карточки «мелькнула строка, похожая на ошибку» вообще без
// текста, и потерянные настоящие ошибки (их «error» был уже съеден чужим
// совпадением, и следующий поиск начинался за ним).
var errorRe = regexp.MustCompile(`(?im)^[^\S\r\n]*(error|fatal|panic|FAIL\b|Error:|Traceback)`)

// errorNoiseRe — строки, которые CLI-агенты печатают в СВОЁМ интерфейсе как
// часть нормальной работы. Они начинаются со слова error, но задача от них не
// падает, и считать их исходом «упало» нельзя.
//
// Живой случай: Codex печатает «error: hook exited with code 1» на каждый
// неудачный PreToolUse-хук и спокойно идёт дальше — в одном терминале за сутки
// набегает несколько десятков таких строк, и «Требует внимания» превращалось в
// красный шум, за которым не видно настоящей аварии.
var errorNoiseRe = regexp.MustCompile(`(?i)^\s*error:\s*hook exited with code\s+\d+\s*$`)

// oomRe — смерть от нехватки памяти. Отдельно от errorRe, потому что ядро о
// ней не кричит: `npm i` просто обрывается строкой «Killed», без единого слова
// error, — и терминал молча оставался «свободен», как будто команда прошла.
//
// Живой случай владельца: на VPS с 1 ГБ без подкачки установка Claude Code
// была убита oom-killer'ом на середине, а с телефона это выглядело как «нажал
// установку — и всё вылетело». Строка есть, распознать её было некому.
var oomRe = regexp.MustCompile(`(?im)^[^\S\r\n]*(killed\b|.*\bout of memory\b|.*\bcannot allocate memory\b|.*\bsignal SIGKILL\b)`)

// promptRe identifies common shell / agent prompts at the buffer tail.
var promptRe = regexp.MustCompile(`(?:[\$#%>❯]|PS [^\n]*>)\s*$`)

// inputRequiredPatterns are explicit "agent is waiting for human" markers.
// When any of these match the visible tail we emit EventWaitingInput
// immediately (without waiting for the idle window) — these are unambiguous.
// Поле kind → тип вопроса (см. HintKind*): по нему клиент выбирает кнопки.
// tailOnly=true — шаблон проверяется только по последним строкам хвоста: такие
// вопросы живут ровно на последней строке, а по всему окну они матчили бы уже
// отвеченный вопрос выше и hint не гас бы.
var inputRequiredPatterns = []struct {
	pattern  *regexp.Regexp
	hint     string
	kind     string
	tailOnly bool
}{
	// `(?i)` делает `(y/N)` и `(Y/n)` одним и тем же шаблоном — второй вариант
	// был недостижим и удалён (hint «Подтвердите: Y/n» не выдавался никогда).
	{regexp.MustCompile(`(?im)\(y/n\)\s*\??\s*$`), "Подтвердите: y/n", HintKindYesNo, true},
	{regexp.MustCompile(`(?im)\(yes/no\)\s*\??\s*$`), "Подтвердите: yes/no", HintKindYesNo, true},
	{regexp.MustCompile(`(?im)Do you want to continue\??\s*$`), "Подтвердите продолжение", HintKindYesNo, true},
	{regexp.MustCompile(`(?im)Press (?:Enter|RETURN) to (?:continue|accept)`), "Нажмите Enter", HintKindEnter, true},
	{regexp.MustCompile(`(?im)Continue\??\s*\[y/n\]`), "Подтвердите: y/n", HintKindYesNo, true},
	{regexp.MustCompile(`(?im)Are you sure\??\s*$`), "Подтвердите действие", HintKindYesNo, true},
	// SSH TOFU: `Are you sure you want to continue connecting (yes/no/[fingerprint])?`
	// не ловился ни одним из шаблонов выше — строка не кончается ни на `Are you
	// sure`, ни на `(yes/no)`.
	{regexp.MustCompile(`(?im)continue connecting \(yes/no`), "SSH: доверять этому серверу?", HintKindYesNo, true},
	{regexp.MustCompile(`(?im)\[INPUT REQUIRED\]`), "Требуется ввод", HintKindText, false},
	// Claude Code permission prompts
	{regexp.MustCompile(`(?im)Allow tool[^?]*\?\s*$`), "Claude: разрешить инструмент", HintKindChoice, true},
	{regexp.MustCompile(`(?im)Approve.*command\??\s*$`), "Claude: подтвердить команду", HintKindChoice, true},
	// Codex tool-use confirmation
	{regexp.MustCompile(`(?im)Approve apply_patch\??`), "Codex: подтвердить патч", HintKindChoice, false},
	{regexp.MustCompile(`(?im)Approve shell\??`), "Codex: подтвердить команду", HintKindChoice, false},
	// Замер 30.07: главные формулировки подтверждений вообще не были описаны —
	// Claude Code спрашивает «Do you want to proceed?» (и «Do you want to make
	// this edit to …?»), Codex — «Allow command?», Gemini CLI — «Apply this
	// change?». Все три идут В РАМКЕ, поэтому строка кончается на «│», а не на
	// «?»: `$` в конце шаблона тут неприменим (потому tailOnly=false и без `$`).
	{regexp.MustCompile(`(?im)Do you want to (?:proceed|make this edit|create|apply)`), "Агент ждёт подтверждения", HintKindChoice, false},
	{regexp.MustCompile(`(?im)\bAllow (?:command|this command|execution)\b`), "Разрешить команду?", HintKindChoice, false},
	{regexp.MustCompile(`(?im)Apply this change\?`), "Применить изменение?", HintKindChoice, false},
	{regexp.MustCompile(`(?im)\b(?:Select|Choose) (?:an? )?(?:option|action|answer)\b`), "Выберите вариант", HintKindChoice, false},
	// Доверие к папке: первый экран Claude Code в новом каталоге. Пока на него
	// не ответили, агент не делает ВООБЩЕ ничего — и человек с телефона видел
	// молчащий терминал без единой подсказки.
	{regexp.MustCompile(`(?im)Do you trust the (?:files|authors)`), "Доверять этой папке?", HintKindChoice, false},
	// Буквенные меню: aider «(Y)es/(N)o/(A)ll/(S)kip all», git «(y,n,q,a,d)»,
	// npm «Ok to proceed? (y)». У них нет цифр, поэтому отвечает не «1», а буква
	// — тип оставляем yes_no, чтобы клиент показал Y/N, а не 1/2/3.
	{regexp.MustCompile(`(?im)\(Y\)es\s*/\s*\(N\)o`), "Подтвердите: y/n", HintKindYesNo, false},
	{regexp.MustCompile(`(?im)Ok to proceed\?`), "Подтвердите: y/n", HintKindYesNo, true},
	// git add -p спрашивает КВАДРАТНЫМИ скобками: «Stage this hunk [y,n,q,a,d,?]?».
	{regexp.MustCompile(`(?im)[(\[]y,n(?:,[a-z?])*[)\]]\??\s*$`), "Подтвердите: y/n", HintKindYesNo, true},
}

// choiceItemRe / choiceCursorRe ловят нумерованное меню, которым современные
// Claude Code и Codex задают вопросы («1. Yes  2. Yes, and don't ask again
// 3. No»). Прежний одиночный шаблон `^\s*[›>❯]\s*\w+\s*$` требовал ровно одно
// слово после маркера и на реальном меню (`❯ 1. Yes`) не срабатывал — поэтому у
// главных потребителей фичи hint был пустым.
//
// ЗАМЕР 30.07 (жалоба владельца «не всегда понимает, что есть выбор»): на
// РЕАЛЬНОМ кадре Claude Code — а он рисует вопрос В РАМКЕ — детектор не видел
// меню вовсе. Строки там начинаются символом рамки:
//
//	│ ❯ 1. Yes                                          │
//	│   2. Yes, and don't ask again for npm commands    │
//
// а класс маркеров начинался сразу с `[›>❯*]`, поэтому ни один пункт не
// совпадал. Подписи при этом разбирались (choiceOptionRe рамку учитывал) — то
// есть кнопки не показывались ровно у главного потребителя фичи. Gemini CLI
// метит текущий пункт кружком (`● 1. Yes, allow once`) — тоже мимо.
var (
	choiceItemRe = regexp.MustCompile(`(?m)^\s*[│┃|]?\s*[›>❯●◉▶»*]?\s*\d[.)]\s+\S`)
	// Курсор — только «стрелочные» маркеры и кружок: `*` из списка исключён
	// намеренно (маркированный список в ответе агента — не меню).
	choiceCursorRe = regexp.MustCompile(`(?m)^\s*[│┃|]?\s*[›>❯●◉▶»]\s*\d?[.)]?\s*\S`)
)

// looksLikeChoice: не меньше двух нумерованных пунктов И строка с курсором —
// одиночный `> foo` (продолжение строки bash, цитата в выводе) не считается.
func looksLikeChoice(tail []byte) bool {
	return len(choiceItemRe.FindAll(tail, 3)) >= 2 && choiceCursorRe.Match(tail)
}

// choiceOptionRe разбирает пункт меню на цифру и ПОДПИСЬ: «│ ❯ 2. Yes, and
// don't ask again │» → («2», «Yes, and don't ask again»). Рамку и маркер
// курсора снимаем прямо здесь — хвост приходит сырым (только без ANSI).
var choiceOptionRe = regexp.MustCompile(`^\s*[│┃|]?\s*[›>❯●◉▶»*]?\s*(\d)[.)]\s+(.+?)\s*[│┃|]?\s*$`)

const (
	// choiceOptionsMax — больше пяти пунктов у подтверждений агентов не бывает,
	// а кнопок столько на телефоне всё равно не поместится.
	choiceOptionsMax = 5
	// choiceOptionLen — подпись на кнопке. Длиннее не читается: кнопки стоят в
	// один ряд под вопросом.
	choiceOptionLen = 40
)

// choiceOptions вытаскивает подписи пунктов ПОСЛЕДНЕГО меню в хвосте — тех
// самых, между которыми человеку и предлагают выбрать. Без них кнопка ответа
// была голой цифрой: что сделает «2» («да, и больше не спрашивать») или «3»
// («нет»), приходилось искать прокруткой вывода, а на телефоне меню к этому
// моменту обычно уже под клавиатурой.
//
// Возвращает подписи В ПОРЯДКЕ ЦИФР (индекс 0 = пункт «1»), и только когда
// нумерация сплошная от единицы: дырка означает, что мы прочитали не меню, а
// случайный нумерованный текст, и врать подписями на кнопках нельзя.
func choiceOptions(tail []byte) []string {
	lines := strings.Split(string(tail), "\n")
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if choiceOptionRe.MatchString(lines[i]) {
			last = i
			break
		}
	}
	if last < 0 {
		return nil
	}
	byDigit := make(map[int]string, choiceOptionsMax)
	maxDigit := 0
	// Идём ВВЕРХ от последнего пункта: в хвосте лежат подряд два-три кадра
	// перерисовки, и меню текущего — самое нижнее (см. isFrameRule).
	for i := last; i >= 0; i-- {
		m := choiceOptionRe.FindStringSubmatch(lines[i])
		if m == nil {
			if normalizeLine(lines[i]) == "" {
				continue // пустая строка или обломок рамки внутри меню
			}
			break // содержательная строка — выше уже сам вопрос
		}
		if isFrameRule(lines[i]) {
			break // линейка рамки — дальше прошлый кадр
		}
		digit, err := strconv.Atoi(m[1])
		if err != nil || digit < 1 || digit > choiceOptionsMax {
			break
		}
		label := strings.TrimRight(normalizeLine(m[2]), " │|")
		if label == "" {
			continue
		}
		if _, seen := byDigit[digit]; seen {
			continue // тот же пункт из прошлого кадра — берём нижний
		}
		if runes := []rune(label); len(runes) > choiceOptionLen {
			label = strings.TrimRight(string(runes[:choiceOptionLen]), " ") + "…"
		}
		byDigit[digit] = label
		if digit > maxDigit {
			maxDigit = digit
		}
	}
	if maxDigit < 2 {
		return nil // один пункт — это не меню
	}
	out := make([]string, 0, maxDigit)
	for digit := 1; digit <= maxDigit; digit++ {
		label, ok := byDigit[digit]
		if !ok {
			return nil // дырка в нумерации — меню не распознали
		}
		out = append(out, label)
	}
	return out
}

// ── Отпечаток видимого вопроса ───────────────────────────────────
//
// hint у всех подтверждений инструмента ОДИН («Claude: разрешить инструмент» —
// имя инструмента в шаблон не входит), поэтому по одному hint два разных
// вопроса подряд неотличимы: кнопка «Да», нажатая на первый вопрос, проходила
// бы guard expect_status_at и отвечала на второй. Ключ эпизода дополняем
// отпечатком того, что человек РЕАЛЬНО видел на экране.

const (
	// Сколько содержательных строк НАД меню берём в отпечаток: там стоит сам
	// вопрос («Allow tool Bash(npm test)?»), а пункты меню у всех вопросов
	// одинаковые («1. Yes / 2. No»).
	promptFPContextLines = 3
	// Вопрос без меню: то же окно, по которому его и распознали
	// (detectInputRequired → lastLines(tail, 3)).
	promptFPTailLines = 3
)

// fpMenuItemRe — пункт нумерованного меню в УЖЕ очищенной строке: рамку и
// маркер курсора вырезает visibleLine, поэтому `│`/`❯` здесь быть не может (в
// отличие от choiceItemRe, который работает по сырому хвосту).
var fpMenuItemRe = regexp.MustCompile(`^\d+[.)]\s+\S`)

// fpFrameRuleRe — горизонтальная линейка рамки: «╭────╮», «╰────╯», «─────»,
// «=====». Проверяется по СЫРОЙ строке хвоста (visibleLine рамку уже вырезал).
var fpFrameRuleRe = regexp.MustCompile(`[─━═\-=_]{4,}`)

// isFrameRule — граница кадра: ни букв, ни цифр, зато длинный горизонтальный
// прогон.
//
// Зачем это нужно: полноэкранный агент перерисовывает экран ДОПИСЫВАНИЕМ (ANSI
// вырезается ещё в lastVisibleTail), поэтому в хвосте лежат подряд ДВА-ТРИ
// кадра. Всё, что выше верхней линейки текущего кадра, принадлежит прошлому
// кадру — в первую очередь его подвал со статусом/спиннером. Числа там гасит
// stableLine, но нечисловая смена («✻ Cogitating…» → «✻ Herding…») при
// НЕИЗМЕННОМ вопросе двигала отпечаток, и тот же вопрос объявлялся новым
// эпизодом: лишнее уведомление и 409 prompt_changed на уже разосланные кнопки.
func isFrameRule(raw string) bool {
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return fpFrameRuleRe.MatchString(raw)
}

// lastMenuBlock — границы ПОСЛЕДНЕГО блока пунктов меню в хвосте: ищем с конца
// вверх, пока строки подряд остаются пунктами. Чисто декоративные строки
// (рамка, пустая строка внутри окна) блок не рвут, а линейка рамки — рвёт: за
// ней начинается прошлый кадр.
//
// Раньше брались ПЕРВЫЙ и последний пункт по всему видимому хвосту, и на
// накопившихся кадрах окно захватывало сразу два меню вместе с подвалом между
// ними (см. isFrameRule).
func lastMenuBlock(raw, vis []string) (first, last int, ok bool) {
	last = -1
	for i := len(vis) - 1; i >= 0; i-- {
		if fpMenuItemRe.MatchString(vis[i]) {
			last = i
			break
		}
	}
	if last < 0 {
		return 0, 0, false
	}
	first = last
	for i := last - 1; i >= 0; i-- {
		switch {
		case fpMenuItemRe.MatchString(vis[i]):
			first = i
		case isFrameRule(raw[i]):
			return first, last, true // выше — уже другой кадр
		case vis[i] == "":
			continue // рамка/пустая строка внутри меню
		default:
			return first, last, true // содержательная строка — это уже вопрос
		}
	}
	return first, last, true
}

// isDecorRune — руны, которыми агент рисует и АНИМИРУЕТ интерфейс: рамки,
// блоки, спиннеры, маркер выбранного пункта. Смысла вопроса они не несут, а
// меняются постоянно, поэтому в отпечаток не идут.
func isDecorRune(r rune) bool {
	switch {
	case r >= 0x2500 && r <= 0x257F, // ─│╭╮╰╯ рамки
		r >= 0x2580 && r <= 0x259F, // ▀▄█ блоки (прогресс-бары)
		r >= 0x25A0 && r <= 0x25FF, // ■□◐◓ геометрия/спиннеры
		r >= 0x2726 && r <= 0x2748, // ✦✧✻✽ «звёздочки» Claude Code
		r >= 0x2800 && r <= 0x28FF: // ⠋⠙⠹ брайль-спиннеры
		return true
	}
	switch r {
	case '›', '❯', '>', '*', '|', '/', '\\', '-', '·', '•', '…':
		// Маркер выбранного пункта — тоже декорация: перемещение выбора
		// стрелками («❯ 1. Yes» → «❯ 2. No») это ТОТ ЖЕ вопрос.
		return true
	}
	return false
}

// visibleLine оставляет от строки хвоста только смысл: декорация вырезана,
// пробелы схлопнуты, регистр понижен. Цифры сохраняются — по ним ниже
// опознаются пункты меню.
func visibleLine(line string) string {
	var b strings.Builder
	space := false
	for _, r := range line {
		switch {
		case isDecorRune(r):
			continue
		case unicode.IsSpace(r):
			space = b.Len() > 0 // ведущие и хвостовые пробелы не пишем вовсе
		default:
			if space {
				b.WriteRune(' ')
				space = false
			}
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// stableLine схлопывает числа в «#»: таймер «(12s · esc to interrupt)», счётчик
// токенов и процент контекста агент перерисовывает каждую секунду, но вопрос от
// этого другим не становится.
func stableLine(visible string) string {
	var b strings.Builder
	digit := false
	for _, r := range visible {
		if unicode.IsDigit(r) {
			if !digit {
				b.WriteByte('#')
			}
			digit = true
			continue
		}
		digit = false
		b.WriteRune(r)
	}
	return b.String()
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// promptFingerprint — отпечаток видимого вопроса (fnv-1a, hex). Обязан
// одновременно:
//   - НЕ меняться, пока агент стоит на одном вопросе (иначе каждый тик — новый
//     эпизод и новое уведомление: ровно та авария «сотни сообщений за ночь»);
//   - меняться, когда вопрос стал ДРУГИМ, даже если hint у обоих одинаковый.
//
// Окно обязано лежать в ОДНОМ кадре перерисовки: у меню — сами пункты (блок с
// конца хвоста) плюс ближайшие содержательные строки над ними до границы кадра
// (всё, что НИЖЕ меню — подсказки, таймер, спиннер — не входит намеренно), без
// меню — последние строки хвоста. Про два кадра в хвосте см. isFrameRule.
func promptFingerprint(tail []byte) string {
	raw := strings.Split(string(tail), "\n")
	vis := make([]string, len(raw))
	for i, ln := range raw {
		vis[i] = visibleLine(ln)
	}

	var window []string
	if first, last, ok := lastMenuBlock(raw, vis); ok {
		ctx := make([]string, 0, promptFPContextLines)
		for i := first - 1; i >= 0 && len(ctx) < promptFPContextLines; i-- {
			// Вверх идём только по СВОЕМУ кадру: линейка рамки и чужой пункт
			// меню — граница, дальше лежит прошлая перерисовка.
			if isFrameRule(raw[i]) || fpMenuItemRe.MatchString(vis[i]) {
				break
			}
			if hasLetter(vis[i]) { // рамку и пустые строки пропускаем, вопрос — берём
				ctx = append(ctx, vis[i])
			}
		}
		// ctx собран снизу вверх; разворачивать не нужно — хешу важен не смысл
		// порядка, а только его повторяемость.
		window = append(ctx, vis[first:last+1]...)
	} else {
		window = vis[max(len(vis)-promptFPTailLines, 0):]
	}

	h := fnv.New64a()
	for _, v := range window {
		if !hasLetter(v) {
			continue // чистая декорация
		}
		h.Write([]byte(stableLine(v)))
		h.Write([]byte{'\n'})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// isAgentProcess reports whether a process name is a known interactive AI
// agent (claude/codex/kimi/…). Such agents spawn long-lived helper children
// (MCP servers, tool subprocesses) that never exit for the life of the
// session. The foreground walker must STOP at the agent rather than descend
// into those helpers — otherwise the "deepest leaf" is always a node/cmd/pwsh
// MCP process, AgentKind != "shell" pins the status to "working" forever, and
// the waiting/idle transitions (keyed on the foreground being the agent
// itself) never fire.
func isAgentProcess(name string) bool {
	return IsAgentKind(AgentKind(name))
}

// detectInputRequired returns a non-empty hint when the tail contains an
// unambiguous "waiting for human" marker, plus the question kind (HintKind*)
// so the client can show the right buttons.
func detectInputRequired(tail []byte) (hint, kind string) {
	last := lastLines(tail, 3)
	for _, p := range inputRequiredPatterns {
		probe := tail
		if p.tailOnly {
			probe = last
		}
		if p.pattern.Match(probe) {
			hint, kind = p.hint, p.kind
			// Тот же вопрос, заданный меню («Do you want to make this edit?»
			// + `1. Yes / 2. No`), отвечается цифрой, а не «y» — тип уточняем.
			if looksLikeChoice(tail) {
				kind = HintKindChoice
			}
			return hint, kind
		}
	}
	if looksLikeChoice(tail) {
		return "Выберите вариант: 1/2/3", HintKindChoice
	}
	return "", ""
}

// lastLines возвращает последние n непустых строк хвоста.
func lastLines(tail []byte, n int) []byte {
	lines := bytes.Split(tail, []byte("\n"))
	out := make([][]byte, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if len(bytes.TrimSpace(lines[i])) == 0 {
			continue
		}
		out = append([][]byte{lines[i]}, out...)
	}
	return bytes.Join(out, []byte("\n"))
}

// sessEvent records the most recent heuristic event for a session, so the PTY
// list can show a status badge (working / idle / waiting / error) instead of
// only firing a transient broadcast.
type sessEvent struct {
	kind     EventKind
	at       time.Time
	hint     string
	hintKind string
	// options — подписи пунктов меню («Yes», «Yes, and don't ask again», «No»)
	// в порядке цифр. Живут рядом с hint: вопрос у всех подтверждений
	// инструмента один и тот же, а различаются они как раз пунктами.
	options []string
}

// detectorState holds per-session bookkeeping for the heuristic detector.
type detectorState struct {
	lastOutputAt         time.Time
	activityStart        time.Time
	bytesSinceLastPrompt int
	// finActivityStart — activityStart эпизода работы агента, по которому
	// событие finished УЖЕ ушло наружу: одно событие на эпизод. Новый всплеск
	// вывода после паузы >5с меняет activityStart (см. observeChunk), и дедуп
	// открывается заново.
	finActivityStart time.Time
	// finishedIdle — порог тишины для «агент закончил» у ЭТОЙ сессии; ноль —
	// стандартный agentFinishedIdle. Ставится на каждом тике по наличию
	// источника статуса (agentStatusSourceKnown).
	finishedIdle time.Duration
	emittedAt    map[EventKind]time.Time
	lastEvt      sessEvent

	// Латч эпизода ожидания. Без него detectorState знал только debounce по
	// EventKind (15с) и наружу уходило новое событие каждые 15 секунд, пока
	// агент стоит на промпте — за ночь это сотни уведомлений. Теперь один
	// эпизод = одно событие, а episodeAt даёт честное «ждёт 12 минут» (раньше
	// setEvent вызывался каждый тик и StatusAt всегда был «сейчас»).
	episode   string    // ключ текущего эпизода; "" — эпизода нет
	episodeAt time.Time // когда эпизод начался
	clean     int       // подряд тиков без признаков ожидания

	// Отпечаток вопроса текущего эпизода ожидания (waitFP) и кандидат на его
	// смену, увиденный ровно один раз (pendingFP) — см. waitEpisodeKey.
	waitFP    string
	pendingFP string

	// sumFP — отпечаток экрана, по которому движку выжимок уже отдавали
	// кандидата в вопрос. Отдельный латч, а не episode: эпизоды ожидания при
	// выключенном распознавании (2.49.4) не ведутся вовсе, и опираться на них
	// значило бы звать модель на каждый тик.
	sumFP string

	// waitSeenAt — когда вопрос агента ПОСЛЕДНИЙ РАЗ был виден на экране.
	//
	// Живая жалоба (2026-07-27): «выбери варианты — пропадает сразу». Кнопки
	// ответа показывались только пока терминал МОЛЧИТ (statusFrom требовал
	// stillIdle — три секунды без вывода), а полноэкранный агент, стоя на
	// собственном меню, продолжает печатать: перерисовывает подсветку пункта,
	// таймер и курсор. Каждый такой байт переводил статус в «работает», и
	// кнопки исчезали через долю секунды после появления.
	//
	// Вопрос на экране — состояние ЭКРАНА, а не потока: пока детектор видит его
	// в хвосте, он и есть вопрос, сколько бы байт агент ни рисовал вокруг.
	waitSeenAt time.Time

	// Ошибка, УВИДЕННАЯ в выводе, но ещё не признанная исходом: пока терминал
	// работает дальше, «упало» — неправда (см. settleError). errPendAt — метка
	// эпизода (многострочный стек приходит пачкой чанков, и метка обязана быть
	// одна на весь эпизод), errPendHint — первая сработавшая строка.
	//
	// Пометку ставит readLoop сессии, а снимает и разбирает горутина детектора —
	// поэтому у пары свой замок (у счётчиков активности выше исторически замка
	// нет: там монотонные записи одной горутины, которые вторая только читает).
	errMu       sync.Mutex
	errPendAt   time.Time
	errPendHint string

	// errSettledAt — когда ошибка признана исходом. Держит статус «error» от
	// перезаписи статусом «свободен» ровно errorStatusTTL.
	errSettledAt time.Time

	// hooks — сигналы самого агента: хуки и файл статуса Claude (agent_hooks.go).
	hooks hookState
}

// waitEpisodePrefix помечает ключи эпизодов ожидания (в отличие от "ready").
const waitEpisodePrefix = "wait|"

// waitKey — ключ эпизода ожидания: вопрос + отпечаток видимого экрана.
func waitKey(hint, fp string) string { return waitEpisodePrefix + hint + "|" + fp }

// noteError помечает эпизод ошибки, увиденной в выводе. Событий и записей в
// журнал отсюда НЕ рождается: «в выводе мелькнула строка со словом error» и
// «терминал упал» — разные новости, и путать их нельзя (см. settleError).
//
// Зачем метка одна на эпизод: ошибка приходит НЕСКОЛЬКИМИ чанками (stack trace,
// многострочный npm ERR!). Пока метка двигалась на каждый чанк, в WS-событие
// уходил штамп первого чанка, а GET /api/pty отдавал время последнего; латч
// уведомлений в клиенте сравнивает штампы, они не совпадали — и возврат в
// приложение поднимал уведомление о той же ошибке второй раз. По той же причине
// в состоянии остаётся ПЕРВАЯ строка эпизода: именно она уедет в уведомление.
func (d *detectorState) noteError(hint string, now time.Time) {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	if d.errPendAt.IsZero() || now.Sub(d.errPendAt) >= debounceWindow {
		d.errPendAt, d.errPendHint = now, hint
	}
}

// hasError — есть ли что разбирать. Дешёвая проверка ПЕРЕД lastErrorTail:
// широкий хвост — это копия 32 КБ и прогон regexp по ней, и делать это раз в
// секунду на каждый молчащий терминал только ради «а вдруг» нельзя.
func (d *detectorState) hasError() bool {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	return !d.errPendAt.IsZero()
}

// takeError снимает пометку об ошибке (одна пометка — один разбор).
func (d *detectorState) takeError() (at time.Time, hint string) {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	at, hint = d.errPendAt, d.errPendHint
	d.errPendAt, d.errPendHint = time.Time{}, ""
	return at, hint
}

// questionOnScreen — вопрос агента виден на экране прямо сейчас.
//
// «Сейчас» — это последние questionFreshTTL: детектор тикает раз в секунду, и
// одного пропущенного тика (агент перерисовал экран целиком, и в кадр не попал
// маркер вопроса) не должно хватать, чтобы кнопки ответа мигнули.
func (d *detectorState) questionOnScreen(now time.Time) bool {
	return !d.waitSeenAt.IsZero() && now.Sub(d.waitSeenAt) < questionFreshTTL
}

// errorHeld — статус «error» ещё свежий и его нельзя перебивать «свободен».
func (d *detectorState) errorHeld(now time.Time) bool {
	return !d.errSettledAt.IsZero() && now.Sub(d.errSettledAt) < errorStatusTTL
}

// errorVisible — сработавшая строка ВСЁ ЕЩЁ в видимом хвосте. Хвост приходит
// сырым (только без ANSI), hint — уже нормализованным, поэтому сверяем
// нормализованные строки, и только по началу: ширина окна могла обрезать конец.
func errorVisible(tail []byte, hint string) bool {
	if hint == "" {
		return false
	}
	probe := hint
	if runes := []rune(probe); len(runes) > errorVisibleProbe {
		probe = string(runes[:errorVisibleProbe])
	}
	for _, line := range bytes.Split(tail, []byte("\n")) {
		if strings.HasPrefix(normalizeLine(string(line)), probe) {
			return true
		}
	}
	return false
}

// settleError признаёт увиденную ошибку ИСХОДОМ — но только если терминал на
// ней ОСТАНОВИЛСЯ: строка всё ещё на экране и с её появления прошло не больше
// errorSettleTTL. Зовётся из тех точек tickSession, где терминал затих (агент
// освободился, у команды вернулся промпт).
//
// Раньше исход писался прямо из readLoop, на любой подходящий чанк. Из-за этого
// «Требует внимания» на 12 часов занимали строки, которые ничего не значат:
// «error: hook exited with code 1» посреди работы Codex, ответ Postgres на
// пробный запрос агента, `fatal: 'main' is already used by worktree` от git —
// агент их прочитал, исправился и пошёл дальше, а карточка «упало» висела.
//
// Возвращает true, если исход зафиксирован (вызывающий не должен перебивать
// статус своим событием).
func (m *Manager) settleError(s *Session, d *detectorState, tail []byte, now time.Time) bool {
	at, hint := d.takeError()
	if at.IsZero() {
		return false
	}
	if now.Sub(at) > errorSettleTTL || !errorVisible(tail, hint) {
		return false // терминал ушёл дальше — новости нет
	}
	d.errSettledAt = at
	s.setEvent(EventError, at, hint, "") // status: error in output
	// В журнал — чтобы «упало» дожило до вечера: статус в списке гаснет через
	// errorStatusTTL, а сама сессия исчезает через 5 минут после смерти.
	m.recordOutcome(s, "error", OutcomeReasonOutputError, at, hint)
	if bc := m.broadcaster(); bc != nil && d.shouldEmit(EventError, now) {
		evt := m.buildEvent(s, sessionAgentKind(s), EventError, at)
		// Поле то же, что у вопроса агента: релей уже читает hint из pty_event и
		// печатает его в уведомлении (tgcontrol-relay/internal/bot/notify.go).
		if hint != "" {
			evt["hint"] = hint
		}
		bc.Broadcast(s.UID, evt)
	}
	return true
}

// episodeCleanTicks — сколько «рабочих» тиков подряд закрывают эпизод. Сброс
// именно по состоянию, а не по приходу байт: агент перерисовывает спиннер и
// таймер, стоя на промпте, поэтому сброс «на любой чанк» вернул бы шторм.
const episodeCleanTicks = 3

// enterEpisode: true, если эпизод НОВЫЙ и о нём стоит сообщить наружу.
func (d *detectorState) enterEpisode(key string, now time.Time) bool {
	d.clean = 0
	if d.episode == key {
		return false
	}
	d.episode, d.episodeAt = key, now
	return true
}

// waitEpisodeKey собирает ключ эпизода ожидания: hint (по нему клиент выбирает
// кнопки) + отпечаток самого вопроса (promptFingerprint). Без отпечатка два
// подряд идущих «Allow tool …?» были ОДНИМ эпизодом — самый частый ритм Claude
// Code, — и кнопка, нажатая на первый вопрос, отвечала на второй.
//
// Смена отпечатка ВНУТРИ того же hint принимается только со второго подряд
// наблюдения: одиночный тик, поймавший кадр перерисовки на середине, не должен
// объявляться новым вопросом (это лишнее уведомление), а мигающий туда-сюда
// отпечаток не подтвердится никогда. Плата — вопрос, сменившийся на такой же по
// типу, опознаётся на секунду позже; детектор и так тикает раз в секунду.
func (d *detectorState) waitEpisodeKey(hint, fp string) string {
	key := func(f string) string { return waitKey(hint, f) }
	cur := key(d.waitFP)
	if d.episode != cur {
		// Эпизода ожидания с этим вопросом сейчас нет (агент работал, вопрос был
		// другой или эпизод уже закрыт) — берём то, что видим, сразу: защищать
		// нечего, уведомления по этому вопросу ещё не было.
		d.waitFP, d.pendingFP = fp, ""
		return key(fp)
	}
	if fp == d.waitFP {
		d.pendingFP = ""
		return cur
	}
	if d.pendingFP != fp {
		d.pendingFP = fp // первое наблюдение — ждём подтверждения
		return cur
	}
	d.waitFP, d.pendingFP = fp, ""
	return key(fp)
}

// leaveEpisode закрывает эпизод после episodeCleanTicks спокойных тиков.
func (d *detectorState) leaveEpisode() {
	// Агент печатает — цепочка подтверждения отпечатка прервана: подтверждением
	// считаются только ПОДРЯД идущие тики ожидания.
	d.pendingFP = ""
	if d.episode == "" {
		return
	}
	d.clean++
	if d.clean >= episodeCleanTicks {
		d.episode, d.clean = "", 0
	}
}

func newDetectorState() *detectorState {
	return &detectorState{
		emittedAt: make(map[EventKind]time.Time),
	}
}

// observeChunk records that new output was written. Возвращает сработавшую
// строку ошибки (пустую, если её нет) — вызывающий лишь ПОМЕЧАЕТ ею эпизод,
// исход рождается позже, в settleError.
func (d *detectorState) observeChunk(chunk []byte, now time.Time) string {
	if d.activityStart.IsZero() || now.Sub(d.lastOutputAt) > 5*time.Second {
		d.activityStart = now
		d.bytesSinceLastPrompt = 0
	}
	d.lastOutputAt = now
	d.bytesSinceLastPrompt += len(chunk)
	// Strip ANSI before regex to avoid false matches on color codes.
	return errorHintIn(ansiRe.ReplaceAll(chunk, nil))
}

// shouldEmit returns true if the given kind hasn't been emitted within the
// debounce window. Records the emission time.
func (d *detectorState) shouldEmit(kind EventKind, now time.Time) bool {
	if last, ok := d.emittedAt[kind]; ok && now.Sub(last) < debounceWindow {
		return false
	}
	d.emittedAt[kind] = now
	return true
}

// agentFinishedDuration решает, завершился ли эпизод работы AI-агента, и
// возвращает его длительность (от activityStart до момента детекции — она же
// уедет наружу полем duration_ms).
//
// «Завершился» = была НАСТОЯЩАЯ работа (всплеск вывода ≥ finishedMinSize и
// ≥ agentFinishedMinWork), а затем устойчивая тишина ≥ agentFinishedIdle.
// Короткая пауза между шагами сюда не попадает по порогу тишины, набор текста
// человеком в поле ввода — по порогам работы. Уже отзвонивший эпизод
// (finActivityStart) второго события не даёт.
//
// Функция чистая: дедуп-метку (finActivityStart) ставит вызывающий и только
// когда решил, что событие состоялось, — иначе подавленный снаружи эпизод
// (скажем, Claude по session-файлу ещё busy) потерял бы своё finished навсегда.
func agentFinishedDuration(d *detectorState, now time.Time) (time.Duration, bool) {
	if d.activityStart.IsZero() || d.finActivityStart.Equal(d.activityStart) {
		return 0, false
	}
	idle := agentFinishedIdle
	if d.finishedIdle > 0 {
		idle = d.finishedIdle
	}
	if now.Sub(d.lastOutputAt) < idle {
		return 0, false
	}
	if d.lastOutputAt.Sub(d.activityStart) < agentFinishedMinWork ||
		d.bytesSinceLastPrompt < finishedMinSize {
		return 0, false
	}
	return now.Sub(d.activityStart), true
}

// agentBusyPerRuntimeFile — точный статус Claude из его session-файла. Тишина
// в терминале у Claude бывает и на половине задачи (thinking без единого байта
// дольше любого разумного порога), поэтому там, где есть честный источник,
// сверяемся с ним: файл говорит busy — это пауза, а не конец работы.
// Best-effort, как и в infoOf: файла нет или статус не busy — не мешаем.
// agentStatusSourceKnown — есть ли у foreground-процесса честный источник
// статуса (runtime-файл, как у Claude Code). Решает по ФАКТУ наличия файла, а
// не по имени агента: появится файл у Codex или Kimi — порог ужесточится сам.
func agentStatusSourceKnown(s *Session) bool {
	fg := s.ForegroundProcess()
	if fg.PID <= 0 {
		return false
	}
	_, ok := s.claudeRuntimeStatus(int(fg.PID))
	return ok
}

func agentBusyPerRuntimeFile(s *Session) bool {
	fg := s.ForegroundProcess()
	if fg.PID <= 0 {
		return false
	}
	st, ok := s.claudeRuntimeStatus(int(fg.PID))
	if !ok {
		return false
	}
	switch strings.ToLower(st.Status) {
	case "busy", "working", "running":
		return true
	}
	return false
}

// runDetector polls all alive sessions every detectorTick. It emits:
//   - EventWaitingInput when an AI-agent session shows a prompt at the tail
//     and has been idle ≥3s.
//   - EventFinished when a non-agent session shows a prompt after ≥5s of
//     ≥1KB activity followed by ≥2s of silence, И когда AI-агент закончил
//     эпизод реальной работы: всплеск вывода, затем устойчивая тишина
//     ≥ agentFinishedIdle (см. agentFinishedDuration). У агентского finished
//     есть duration_ms — длительность завершившегося эпизода.
//   - EventError, когда терминал ОСТАНОВИЛСЯ на замеченной строке ошибки
//     (см. settleError). Саму строку помечает readLoop, но новостью её делает
//     только остановка: иначе «упало» объявлялось про любую красную строку,
//     которую агент прочитал и исправил.
func (m *Manager) runDetector() {
	ticker := time.NewTicker(detectorTick)
	defer ticker.Stop()
	for range ticker.C {
		bc := m.broadcaster()
		if bc == nil {
			continue
		}
		m.mu.RLock()
		sessions := make([]*Session, 0, len(m.sessions))
		for _, s := range m.sessions {
			sessions = append(sessions, s)
		}
		m.mu.RUnlock()
		for _, s := range sessions {
			m.tickSession(s, time.Now(), bc)
		}
	}
}

func (m *Manager) tickSession(s *Session, now time.Time, bc Broadcaster) {
	if !s.IsAlive() || s.UID == 0 {
		return
	}
	state := s.evState()
	if state == nil {
		return
	}

	tail := lastVisibleTail(s)
	agentKind := sessionAgentKind(s)
	if agentKind == "codex" {
		s.observeCodexActivity(now)
	}
	sessionIsAgent := IsAgentKind(agentKind) || s.Shell == "ssh"

	// Сигналы самого агента — раньше экрана: где агент сам говорит «жду
	// ответа» и «закончил», экран не угадываем (agent_hooks.go).
	m.pollAgentHooks(s, state, now, bc)
	sig := m.agentSignals(s, state, agentKind, now)
	if sig.waiting {
		m.holdStructuredWait(s, state, tail, sig, now, bc)
		return
	}

	// В обычном терминале вопрос принимаем, только пока команда ЕЩЁ ЖИВА: если
	// внизу уже стоит приглашение шелла («PS C:\…>», «$»), команда завершилась,
	// и «(y/n)» выше — это её прошлый, уже отвеченный вопрос. Без этой проверки
	// терминал, где когда-то шёл `git add -p`, вечно числился бы ждущим.
	// У агента приглашения шелла нет вовсе — его окно не сужаем.
	askTail := tail
	if !sessionIsAgent && promptRe.Match(lastLines(tail, 1)) {
		askTail = nil
	}

	// 1) Explicit input-required marker — emit immediately (no idle wait),
	//    with a human-readable hint. Catches Claude/Codex confirm prompts,
	//    y/n questions, "Press Enter", etc.
	//
	// ВЫКЛЮЧЕНО по умолчанию с 2.49.4 (решение владельца: «ловит просто так»).
	// Ложное срабатывание, которое это решило: агент напечатал ОТВЕТ ПРО
	// вопросы — таблицу со строками «Ok to proceed? (y)» и «1. Yes / 2. … /
	// 3. No», — и терминал объявил «ждёт подтверждения» при законченной работе.
	// По тексту экрана рассказ о меню от самого меню не отличить, а другого
	// универсального источника нет. Включается в настройках.
	// У агента с честным источником (хуки, файл статуса) вопрос по тексту
	// экрана не ищем вовсе — ни для кнопок, ни для движка выжимок: ответ уже
	// известен выше, а экран умеет только ошибаться в обе стороны.
	hint, hintKind := "", ""
	if !sig.structured {
		hint, hintKind = detectInputRequired(askTail)
	}

	// Кандидат в вопрос виден, но распознавание выключено (2.49.4). Наружу
	// по-прежнему молчим — а вот движку выжимок повод интересен: модель отличит
	// настоящий вопрос от рассказа агента ПРО вопросы, чего регулярки не умеют.
	// Свой латч по отпечатку экрана (sumFP), потому что эпизоды ожидания при
	// выключенном флаге не ведутся вовсе.
	if hint != "" && !m.detectQuestions.Load() {
		if fp := promptFingerprint(tail); fp != state.sumFP {
			state.sumFP = fp
			m.fireSummary(SummaryEvent{
				Kind: SummaryQuestion, PtyID: s.ID, UID: s.UID,
				Name: m.sessionName(s), CWD: s.CurrentCWD(), AgentKind: agentKind,
				Hint: hint, Tail: summaryTail(s, summaryQuestionBytes),
			})
		}
	}

	if hint != "" && m.detectQuestions.Load() {
		// Ключ эпизода = вопрос + его отпечаток: одинаковый hint у двух РАЗНЫХ
		// вопросов подряд обязан дать разные эпизоды (иначе кнопка от первого
		// отвечает на второй, см. waitEpisodeKey).
		key := state.waitEpisodeKey(hint, promptFingerprint(tail))
		fresh := state.enterEpisode(key, now)
		// Вопрос виден ПРЯМО СЕЙЧАС — держим кнопки ответа, даже если агент
		// продолжает рисовать вокруг них (см. waitSeenAt).
		state.waitSeenAt = now
		// Подписи пунктов нужны только нумерованному меню: у y/n и «нажмите
		// Enter» выбирать не из чего, а лишний прогон regexp по хвосту делается
		// раз в секунду на каждый ждущий терминал.
		var options []string
		if hintKind == HintKindChoice {
			options = choiceOptions(tail)
		}
		// StatusAt = начало эпизода, иначе «ждёт N минут» всегда показывало 0с.
		s.setEvent(EventWaitingInput, state.episodeAt, hint, hintKind, options...)
		s.syncWaitEpisode(state)
		// Одно уведомление на эпизод: пока висит тот же вопрос, повторов нет.
		if fresh {
			evt := m.buildEvent(s, sessionAgentKind(s), EventWaitingInput, state.episodeAt)
			evt["hint"] = hint
			evt["hint_kind"] = hintKind
			// Пункты уезжают и в уведомление: «1 · Yes / 3 · No» в чате бота
			// отвечают на вопрос «а что я вообще выбираю», не открывая терминал.
			if len(options) > 0 {
				evt["hint_options"] = options
			}
			bc.Broadcast(s.UID, evt)
		}
		return
	}

	idle := now.Sub(state.lastOutputAt)
	kind := agentKind
	isAgent := IsAgentKind(kind)

	// Interactive agent: sustained silence means it is parked waiting for the
	// human. Agents stream a live spinner / elapsed-timer while actually
	// working, so a few seconds of no output is a reliable "idle at the prompt"
	// signal — more reliable than promptRe, whose box-drawing UI rarely ends in
	// a bare prompt character. (Explicit confirm prompts already fired above.)
	// Without this, an agent session would sit on "working" forever because its
	// foreground never returns to a recognisable shell prompt.
	if isAgent {
		if idle >= idleWaiting {
			// ЖИВОЙ агент на строке ошибки не «встаёт»: он её ПРОЧИТАЛ.
			//
			// Молчание агента означает «думает или ждёт человека», а не
			// «упал»: Kimi Code в режиме thinking не рисует ни спиннера, ни
			// таймера — терминал молчит минутами, пока агент работает. Ошибка
			// же в его выводе принадлежит инструменту, который он сам запустил
			// (ответ Postgres «relation does not exist» на пробный запрос,
			// неудачный хук, `fatal: worktree` от git) — это его рабочий
			// материал, и справляется он с ним сам.
			//
			// Живая жалоба владельца (2026-07-27): карточка «⚠ ошибка» с
			// SQL-строкой висела на терминале, где агент в этот момент вёл три
			// подзадачи, и ВОЗОБНОВЛЯЛАСЬ — интерфейс агента печатает историю
			// заново, строка снова попадала в хвост, и следующая же пауза на
			// раздумье записывала исход повторно.
			//
			// Новости агентского терминала остаются прежними и честными: «ждёт
			// ответа» (вопрос распознан), «работает», «свободен», а настоящее
			// падение самого агента — это смерть процесса, и её записывает
			// readLoop исходом "dead" (reason exited/detached). Пометку
			// снимаем, чтобы она не всплыла позже.
			if state.hasError() {
				state.takeError()
			}
			if state.errorHeld(now) {
				// Пока статус «error» свежий, не перебиваем его «свободен»:
				// иначе красная карточка гасла бы на следующем же тике.
				s.syncWaitEpisode(state)
				return
			}
			// Эпизод работы завершился: агент реально работал, затем наступило
			// устойчивое затишье. Это НОВОСТЬ наружу («агент закончил, работал
			// N минут») — в отличие от статуса ready ниже, который живёт только
			// в списке терминалов (см. EventAgentReady). Одно событие на эпизод:
			// finActivityStart + антишторм shouldEmit.
			// Порог тишины зависит от того, есть ли у агента честный источник
			// статуса: с ним 45 с (и сверка busy ниже), без него — вдвое дольше
			// (см. agentFinishedIdleNoStatus).
			if agentStatusSourceKnown(s) {
				state.finishedIdle = 0
			} else {
				state.finishedIdle = agentFinishedIdleNoStatus
			}
			// Конец хода, о котором агент сообщает сам (hookFinished), тишиной не
			// дублируем: иначе второе «закончил» приходило бы через 45–120 с.
			if dur, ok := agentFinishedDuration(state, now); ok &&
				!state.hooks.reportsStop(kind) &&
				!(kind == "claude" && agentBusyPerRuntimeFile(s)) {
				state.finActivityStart = state.activityStart
				// Понятное уведомление: берём вывод ЭТОГО эпизода (сколько байт
				// он вывел, столько и берём — не весь буфер, где лежит чужая
				// прошлая работа), остальное делает движок выжимок.
				m.fireSummary(SummaryEvent{
					Kind: SummaryFinished, PtyID: s.ID, UID: s.UID,
					Name: m.sessionName(s), CWD: s.CurrentCWD(), AgentKind: kind,
					Duration: dur, Tail: summaryTail(s, min(state.bytesSinceLastPrompt, summaryEpisodeBytes)),
				})
				if state.shouldEmit(EventFinished, now) {
					// status_at не кладём по той же причине, что у finished
					// не-агентской команды (см. ниже): состояния «finished» в
					// GET /api/pty нет, штамп клиенту нечем подтвердить.
					evt := m.buildEvent(s, kind, EventFinished, time.Time{})
					evt["duration_ms"] = dur.Milliseconds()
					bc.Broadcast(s.UID, evt)
				}
			}
			// Вопрос распознать не удалось → это «освободился», а не «ждёт
			// ответа»: наружу не шлём (см. EventAgentReady), в списке покажется
			// статусом ready.
			state.enterEpisode("ready", now)
			s.setEvent(EventAgentReady, state.episodeAt, "", "")
		} else {
			state.leaveEpisode() // агент снова печатает — эпизод закрываем
		}
		s.syncWaitEpisode(state)
		return
	}
	state.leaveEpisode()
	s.syncWaitEpisode(state)

	// Non-agent command: "finished" when a prompt reappears after a real burst
	// of output followed by a short silence.
	if !promptRe.Match(tail) {
		return
	}
	// Промпт вернулся, а в хвосте — та самая строка ошибки: команда упала.
	// Проверяем ДО порогов «настоящей пачки вывода» (finishedMinSize/Dur): они
	// про новость «работа закончилась», а падение в три строки — новость сама
	// по себе.
	if idle >= idleFinished && state.hasError() && m.settleError(s, state, lastErrorTail(s), now) {
		state.bytesSinceLastPrompt = 0
		return
	}
	if idle >= idleFinished &&
		state.bytesSinceLastPrompt >= finishedMinSize &&
		now.Sub(state.activityStart) >= finishedMinDur {
		s.setEvent(EventFinished, now, "", "") // status: work done, back at prompt
		if state.shouldEmit(EventFinished, now) {
			// status_at в finished НЕ кладём: в GET /api/pty состояния
			// «finished» нет вовсе — закончившая команда там idle со временем
			// последней активности, — и любой штамп в событии клиенту нечем
			// подтвердить (инвариант «штамп события == штамп из GET» ломался).
			// Добор состояния шлёт только waiting_input/error, так что латч от
			// этого не страдает; если finished когда-нибудь войдёт в добор —
			// сначала завести ему ветку в statusFrom, потом вернуть штамп.
			bc.Broadcast(s.UID, m.buildEvent(s, kind, EventFinished, time.Time{}))
		}
		state.bytesSinceLastPrompt = 0
	}
}

// summaryTail — СЫРОЙ хвост буфера для выжимки: ANSI не снимаем и по строкам не
// режем, это работа получателя (internal/agentsummary). Меньше 4 КБ не берём
// даже у короткого эпизода: у полноэкранного агента один кадр перерисовки — это
// уже килобайты, и по «сколько байт вывел эпизод» можно недобрать сам итог.
func summaryTail(s *Session, n int) []byte {
	if n < 4<<10 {
		n = 4 << 10
	}
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	buf := s.buf
	if len(buf) > n {
		buf = buf[len(buf)-n:]
	}
	out := make([]byte, len(buf))
	copy(out, buf)
	return out
}

// sessionName — имя терминала, как его назвал человек (иначе — оболочка).
func (m *Manager) sessionName(s *Session) string {
	if m.meta != nil {
		if name := m.meta.Get(s.ID).Name; name != "" {
			return name
		}
	}
	return s.Shell
}

// lastVisibleTail returns the visible tail of the scrollback без ANSI —
// последние tailVisibleLines непустых строк. Окно расширено с 1 КБ/256 байт:
// один кадр перерисовки full-screen агента — это многие килобайты, и вопрос с
// меню из трёх пунктов в прежние 256 байт целиком не влезал.
func lastVisibleTail(s *Session) []byte {
	return visibleTail(s, tailRawBytes, tailVisibleBytes, tailVisibleLines)
}

// lastErrorTail — то же окно, но шире (см. errorTail*): в нём ищут не вопрос
// агента, а строку ошибки, и она может стоять выше целой рамки ввода.
func lastErrorTail(s *Session) []byte {
	return visibleTail(s, errorTailRawBytes, errorTailVisibleBytes, errorTailLines)
}

func visibleTail(s *Session, rawBytes, visibleBytes, lines int) []byte {
	s.bufMu.Lock()
	buf := s.buf
	if len(buf) > rawBytes {
		buf = buf[len(buf)-rawBytes:]
	}
	out := make([]byte, len(buf))
	copy(out, buf)
	s.bufMu.Unlock()
	clean := ansiRe.ReplaceAll(out, nil)
	if len(clean) > visibleBytes {
		clean = clean[len(clean)-visibleBytes:]
	}
	return lastLines(clean, lines)
}

func sessionAgentKind(s *Session) string {
	kind := AgentKind(s.ForegroundProcess().Name)
	if s.Shell == "ssh" {
		if remote := remoteAgentKind(s); remote != "" {
			return remote
		}
	}
	return kind
}

// AgentMissingMarker — метка «агента здесь нет», которую печатает наша же
// строка запуска на сервере (см. composeRemoteLaunch на клиенте).
//
// Зачем метка. У SSH-сессии нет процесса на нашей машине, поэтому агент
// определяется ПО ТЕКСТУ на экране: ищем «claude», «codex», «gemini»… в хвосте
// вывода. А наша строка отказа сама содержит имя агента — и трижды: эхо
// набранной команды плюс сообщение «Агент claude не установлен на сервере».
// То есть сообщение «агента здесь нет» заставляло интерфейс показать, что агент
// ЕСТЬ И РАБОТАЕТ: значок агента в шапке, приглушённый ряд команд, статус
// занятости. Приём с меткой в проекте уже был — REMOTAI_KEY_ADDED в
// ssh_authorized_key.go.
const AgentMissingMarker = "REMOTAI_AGENT_MISSING"

func remoteAgentKind(s *Session) string {
	raw := string(lastVisibleTail(s))
	if strings.Contains(raw, AgentMissingMarker) {
		// На экране отказ запуска — имя агента в нём есть, но агента нет.
		return ""
	}
	tail := strings.ToLower(raw)
	for _, candidate := range []struct {
		marker string
		kind   string
	}{
		{"claude", "claude"}, {"codex", "codex"}, {"gemini", "gemini"},
		{"kimi", "kimi"}, {"aider", "aider"}, {"opencode", "opencode"},
		{"copilot", "copilot"}, {"cursor-agent", "cursor-agent"},
		{"cline", "cline"}, {"kilocode", "kilo"}, {"amazon q", "amazon-q"},
		{"grok", "grok"},
	} {
		if strings.Contains(tail, candidate.marker) {
			return candidate.kind
		}
	}
	return ""
}

// buildEvent собирает событие для клиента. statusAt — начало ЭПИЗОДА (то же
// значение, что уходит в setEvent и в status_at из GET /api/pty).
func (m *Manager) buildEvent(s *Session, agentKind string, kind EventKind, statusAt time.Time) map[string]any {
	name := ""
	if m.meta != nil {
		name = m.meta.Get(s.ID).Name
	}
	if name == "" {
		name = s.Shell
	}
	evt := map[string]any{
		"type":       "pty_event",
		"event":      string(kind),
		"pty_id":     s.ID,
		"name":       name,
		"agent":      agentKind,
		"fg_process": s.ForegroundProcess().Name,
		// ts (unix ms) — когда событие произошло. Нужен клиенту, чтобы не
		// показывать системное уведомление на реплей истории (`?since=`) после
		// возврата из фона: иначе телефон вываливает пачку «агент ждёт ввода»
		// про вопросы, отвеченные час назад.
		"ts": time.Now().UnixMilli(),
	}
	// status_at (unix ms) — метка эпизода, ОБЯЗАТЕЛЬНАЯ для латча уведомлений в
	// клиенте (apk/src/notifications.ts: `ev.status_at ?? ev.ts`). Без неё латч
	// откатывался на ts, а ts — всегда «сейчас»: вопрос, отзвонивший ночью по
	// WS, звенел второй раз при открытии приложения, потому что добор состояния
	// брал штамп из GET /api/pty и он никогда не совпадал.
	if !statusAt.IsZero() {
		evt["status_at"] = statusAt.UnixMilli()
	}
	return evt
}

// ── Manager broadcaster wiring ───────────────────────────────────

func (m *Manager) broadcaster() Broadcaster {
	m.bcMu.RLock()
	defer m.bcMu.RUnlock()
	return m.bc
}

// SetBroadcaster wires the manager to a transport (e.g. the web server).
// Starts the heuristic detector goroutine the first time it is set.
func (m *Manager) SetBroadcaster(bc Broadcaster) {
	m.bcMu.Lock()
	first := m.bc == nil
	m.bc = bc
	m.bcMu.Unlock()
	if first {
		go m.runDetector()
	}
}
