// Package agentsummary — понятное уведомление вместо «агент закончил».
//
// ЗАЧЕМ ЭТО ЕСТЬ. Уведомление «Claude закончил в «терминал» · 4 мин» не отвечает
// на единственный вопрос, который у человека есть: что он там сделал. Чтобы
// узнать, приходилось открывать терминал — то есть уведомление не экономило
// ничего. Здесь вывод эпизода превращается в одну-две фразы, которые можно
// прочитать с экрана блокировки.
//
// ГРАНИЦА ОТВЕТСТВЕННОСТИ. В этом пакете нет ни сети, ни настроек, ни отправки:
// только чистые функции — очистка вывода, маскирование секретов, промпт и
// разбор ответа. Так их можно проверить тестом без ключа и без живого агента, а
// решение «звать модель или молчать» остаётся у вызывающего (internal/web).
//
// ГЛАВНОЕ ПРАВИЛО: текст терминала уходит в чужой сервис. Значит он обязан
// пройти через Mask, и Mask обязан ошибаться в сторону перестраховки — лучше
// скрыть лишнее, чем отправить наружу ключ.
package agentsummary

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Kind — о чём спрашиваем модель.
type Kind string

const (
	// KindFinished — агент отработал эпизод и затих: «что сделал».
	KindFinished Kind = "finished"
	// KindWaiting — на экране похоже на вопрос: «правда ли ждёт и о чём».
	KindWaiting Kind = "waiting"
)

const (
	// MaxInputChars — сколько текста отдаём модели. Больше не нужно: полезное
	// у агента всегда в конце эпизода, а лишнее — это и деньги, и лишний текст
	// наружу.
	MaxInputChars = 6000
	// MaxSummaryChars — длина готовой выжимки. Уведомление читают с экрана
	// блокировки; всё, что длиннее, там всё равно обрежется — но обрежется в
	// произвольном месте, а не по нашему решению.
	MaxSummaryChars = 320
	// minInputChars — короче этого звать модель незачем: пересказывать нечего,
	// а запрос из дневной квоты бесплатного роутера всё равно спишется.
	minInputChars = 40
	// maxLines — сколько строк оставляем после очистки (последние).
	maxLines = 120
	// dedupWindow — в скольких последних строках ищем повтор. Кадр телефона —
	// это 30–50 строк, и перерисованный кадр приходит целым БЛОКОМ, а не строкой
	// за строкой: сравнения с соседней строкой не хватало вовсе (поймано
	// тестом — три копии одного экрана доезжали до модели). Окно взято заметно
	// шире кадра, чтобы схлопывались и два кадра подряд.
	dedupWindow = 80
)

// ansiRe — CSI/OSC последовательности. Тот же набор, что в детекторе событий
// (internal/pty/events.go): экран рисуется ими, смысла в них нет.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z@-~]|\x1b\][^\x07]*\x07|\x1b[=>78]|\x1b\[\?[0-9;]*[hl]`)

// frameRuneRe — символы рамок, блоков и спиннеров. Целые строки из них — это
// декорация кадра, и в пересказ они не идут.
var frameRuneRe = regexp.MustCompile(`^[\s─━═│┃╭╮╰╯┌┐└┘├┤┬┴┼█▀▄▌▐░▒▓◐◓◑◒✻✽✦✧⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏·•\-=_*]+$`)

// digitsRe — числа для сравнения строк «по смыслу»: таймер «(12s · esc to
// interrupt)» и счётчик токенов агент перерисовывает каждую секунду, но новой
// строкой она от этого не становится. Без этого дедуп не срабатывал вовсе и в
// модель уезжали сотни копий одного и того же подвала.
var digitsRe = regexp.MustCompile(`\d+`)

// spacesRe — прогон пробелов. Полноэкранный агент выравнивает текст по колонкам
// и дополняет строки пробелами до края экрана, поэтому в 32 КБ хвоста половина
// объёма — это пустое место (замер на живом терминале: 5,6-кратное сжатие после
// очистки, и заметная часть выигрыша именно здесь).
var spacesRe = regexp.MustCompile(`[ \t]{2,}`)

// isDecorRune — рамки, блоки, спиннеры и маркеры интерфейса агента. Их надо
// вырезать ИЗ строки, а не только выбрасывать строки, целиком из них
// состоящие: живой замер показал строки вида «✽b7m✻E✶*✢·8 ,running1shell
// command…✢29*60✶7» — это спиннер, вклеенный прямо в текст, и модель на таком
// материале начинает гадать вместо пересказа.
func isDecorRune(r rune) bool {
	switch {
	case r >= 0x2500 && r <= 0x257F, // ─│╭╮╰╯ рамки
		r >= 0x2580 && r <= 0x259F, // ▀▄█ блоки
		r >= 0x25A0 && r <= 0x25FF, // ■□◐◓● геометрия и спиннеры
		r >= 0x2726 && r <= 0x2748, // ✦✧✻✽ «звёздочки» Claude Code
		r >= 0x2800 && r <= 0x28FF, // ⠋⠙⠹ брайль-спиннеры
		r >= 0x23E9 && r <= 0x23FA: // ⏵⏸ маркеры режима
		return true
	}
	return false
}

// stripDecor убирает декоративные руны и схлопывает пробелы.
func stripDecor(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	for _, r := range line {
		if isDecorRune(r) {
			b.WriteRune(' ') // не склеиваем соседние слова
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(spacesRe.ReplaceAllString(b.String(), " "))
}

// Clean приводит сырой вывод терминала к тексту, который имеет смысл читать:
// без ANSI, без рамок, без повторов кадров перерисовки.
//
// ПОЧЕМУ ДЕДУП ОБЯЗАТЕЛЕН. Полноэкранный агент (alt-screen) перерисовывает экран
// целиком много раз в секунду, поэтому в буфере лежат десятки почти одинаковых
// кадров. Без схлопывания модель получала бы один и тот же текст многократно —
// и платили бы мы за него столько же раз.
func Clean(raw []byte) string {
	text := ansiRe.ReplaceAllString(string(raw), "")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var out []string
	// seen — на какой выходной строке последний раз встречался этот текст (с
	// числами, схлопнутыми в «#»). Повтор внутри dedupWindow — это перерисовка
	// того же кадра, а не новая новость.
	seen := make(map[string]int)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t")
		// Рамку вокруг строки снимаем, но саму строку оставляем: у агентов в
		// рамке живёт как раз вопрос и итог работы.
		line = strings.Trim(line, "│┃|")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if frameRuneRe.MatchString(line) {
			continue // чистая декорация кадра
		}
		line = stripDecor(line)
		if line == "" || !hasLetterOrDigit(line) {
			continue
		}
		key := digitsRe.ReplaceAllString(line, "#")
		if at, ok := seen[key]; ok && len(out)-at <= dedupWindow {
			seen[key] = len(out) // тот же кадр перерисован ещё раз
			continue
		}
		seen[key] = len(out)
		out = append(out, line)
	}
	if len(out) > maxLines {
		out = out[len(out)-maxLines:]
	}
	joined := strings.Join(out, "\n")
	if len(joined) > MaxInputChars {
		// Режем с начала: конец эпизода — это его итог, он важнее начала.
		joined = joined[len(joined)-MaxInputChars:]
		if i := strings.IndexByte(joined, '\n'); i >= 0 && i < 200 {
			joined = joined[i+1:] // не начинать с обрубка строки
		}
	}
	return joined
}

func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// ── Маскирование секретов ────────────────────────────────────────────────────
//
// Список намеренно широкий и грубый. Пропущенный ключ уедет в чужой сервис
// навсегда, а лишняя замена стоит одной непонятной строки в пересказе — цена
// ошибок здесь несимметрична, и перестраховка тут правильная.

const maskLabel = "«скрыто»"

// maskRule — правило и способ замены. Способ хранится рядом с шаблоном
// намеренно: пока он выбирался по индексу правила в списке, любая вставка
// нового шаблона в середину молча меняла поведение двух чужих правил.
type maskRule struct {
	re *regexp.Regexp
	// with — чем заменить целиком; пусто, если замену считает keepField.
	with string
	// keepField — оставить имя поля (до первого «:» или «=»), спрятать значение.
	keepField bool
}

// Порядок важен: сначала блоки и ключи известной формы, потом общее «token=…»,
// иначе общее правило съело бы начало ключа и оставило хвост.
var maskRules = []maskRule{
	// Приватный ключ целиком (PEM): достаточно увидеть заголовок.
	{re: regexp.MustCompile(`(?s)-----BEGIN[^-]*PRIVATE KEY-----.*?-----END[^-]*PRIVATE KEY-----`), with: maskLabel},
	// Ключи известной формы: OpenAI/OpenRouter/Anthropic, GitHub, Slack, AWS,
	// Google, Telegram-бот.
	{re: regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`), with: maskLabel},
	{re: regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{16,}`), with: maskLabel},
	{re: regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), with: maskLabel},
	{re: regexp.MustCompile(`xox[abprs]-[A-Za-z0-9\-]{10,}`), with: maskLabel},
	{re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`), with: maskLabel},
	{re: regexp.MustCompile(`AIza[0-9A-Za-z_\-]{20,}`), with: maskLabel},
	{re: regexp.MustCompile(`\b\d{8,10}:AA[A-Za-z0-9_\-]{30,}`), with: maskLabel},
	// JWT: три части через точку. Ими подписаны и наши device-токены.
	{re: regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), with: maskLabel},
	// Пароль в URL: scheme://user:pass@host — имя пользователя и хост оставляем.
	{re: regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/@]+):[^\s@/]+@`), with: "$1:" + maskLabel + "@"},
	// Присваивание секрета в любом виде: KEY=…, "token": "…", password: ….
	{re: regexp.MustCompile(`(?i)\b(api[_\-]?key|access[_\-]?token|auth[_\-]?token|secret[_\-]?key|client[_\-]?secret|password|passwd|secret|token|bearer)\b["'\s]*[:=]\s*["']?[^\s"',;]{6,}`), keepField: true},
	// Длинная шестнадцатеричная строка — почти всегда ключ или хеш подписи.
	{re: regexp.MustCompile(`\b[0-9a-fA-F]{40,}\b`), with: maskLabel},
}

// Mask заменяет всё, что похоже на секрет, на маркер.
func Mask(s string) string {
	for _, rule := range maskRules {
		if rule.keepField {
			s = rule.re.ReplaceAllStringFunc(s, func(m string) string {
				sep := strings.IndexAny(m, ":=")
				if sep < 0 {
					return maskLabel
				}
				return m[:sep+1] + " " + maskLabel
			})
			continue
		}
		s = rule.re.ReplaceAllString(s, rule.with)
	}
	return s
}

// Prepare — полный путь материала до модели: очистка, маскирование, проверка
// «есть ли что пересказывать». ok=false означает «звать модель незачем».
func Prepare(raw []byte) (string, bool) {
	text := Mask(Clean(raw))
	if len([]rune(text)) < minInputChars {
		return "", false
	}
	return text, true
}

// ── Промпт ───────────────────────────────────────────────────────────────────

// systemFinished/systemWaiting — роль модели. Требование «не выдумывай» здесь
// не вежливость, а условие пригодности: уведомление, сочинившее результат
// работы, хуже отсутствующего — по нему человек примет решение.
const systemFinished = `Ты пересказываешь вывод терминала владельцу компьютера.
Ответь ОДНОЙ-ДВУМЯ фразами по-русски: что AI-агент сделал за этот заход.
Только по тексту, ничего не додумывай. Без вступлений, без markdown, без кавычек.
Если по выводу понять невозможно — ответь ровно: НЕЯСНО`

const systemWaiting = `Ты смотришь на экран терминала с AI-агентом.
Определи, ждёт ли агент ПРЯМО СЕЙЧАС ответа человека (вопрос, меню выбора, запрос подтверждения).
Рассказ агента о вопросах, пример меню в тексте ответа и уже отвеченный вопрос — это НЕ ожидание.
Если ждёт — ответь одной строкой: ВОПРОС: <о чём спрашивает, по-русски, коротко>
Если не ждёт — ответь ровно: НЕТ`

// System — роль для этого вида уведомления.
func System(kind Kind) string {
	if kind == KindWaiting {
		return systemWaiting
	}
	return systemFinished
}

// User собирает материал: где это происходит и сам вывод. Папку называем
// отдельной строкой — «по какому проекту» человек спрашивает первым делом, а
// сама модель угадать проект по выводу не обязана.
func User(agent, project, text string) string {
	var sb strings.Builder
	if agent != "" {
		fmt.Fprintf(&sb, "Агент: %s\n", agent)
	}
	if project != "" {
		fmt.Fprintf(&sb, "Папка: %s\n", project)
	}
	sb.WriteString("Вывод терминала:\n---\n")
	sb.WriteString(text)
	sb.WriteString("\n---")
	return sb.String()
}

// ── Разбор ответа ────────────────────────────────────────────────────────────

// unclear — модель честно сказала, что не поняла. Это не сбой: значит выжимки
// не будет, а уведомление уйдёт прежним, без неё.
const unclear = "НЕЯСНО"

// draftLimit — длина, после которой ответ считается черновиком, а не итогом.
// Просили одну-две фразы; всё, что длиннее вдвое против нашего же лимита показа,
// — это модель, которая думает вслух.
const draftLimit = 2 * MaxSummaryChars

// cyrillicShare — доля кириллицы, ниже которой ответ считаем не выполненным по
// заданию (просили по-русски).
const cyrillicShare = 0.3

// Summary приводит ответ модели к тексту уведомления. Пустая строка означает
// «выжимки нет» — и для вызывающего это штатный случай.
//
// ЖИВОЙ ЗАМЕР (12.08.2026), из-за которого здесь появилась отбраковка: бесплатный
// роутер OpenRouter сам выбирает модель под запрос, и часть моделей отвечает
// РАЗМЫШЛЕНИЕМ вслух — «We need to summarize what the agent did… The output is
// garbled…». Без проверки этот черновик уезжал бы в Telegram как «что сделал
// агент», да ещё обрезанный на полуслове. Лучше уведомление без выжимки, чем
// уведомление с чужим потоком сознания.
func Summary(answer string) string {
	s := cleanupAnswer(answer)
	if s == "" || strings.EqualFold(s, unclear) {
		return ""
	}
	if looksLikeDraft(s) {
		return ""
	}
	return clampRunes(s, MaxSummaryChars)
}

// looksLikeDraft — ответ не выполнен по заданию: длинный или не по-русски.
func looksLikeDraft(s string) bool {
	if len([]rune(s)) > draftLimit {
		return true
	}
	var cyr, letters int
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if unicode.Is(unicode.Cyrillic, r) {
			cyr++
		}
	}
	// Ответ вообще без букв (одни цифры и знаки) пересказом не является.
	if letters == 0 {
		return true
	}
	return float64(cyr)/float64(letters) < cyrillicShare
}

// Question разбирает ответ про ожидание: текст вопроса и признак «правда ждёт».
//
// Отвечать умеем только когда модель сказала «ВОПРОС:» явно. Всё остальное —
// включая пустой ответ и любую отсебятину — считаем «не ждёт»: ложное «агент
// ждёт вас» будит человека ночью зря, и именно из-за таких срабатываний
// распознавание вопросов по экрану было выключено в 2.49.4.
func Question(answer string) (string, bool) {
	s := cleanupAnswer(answer)
	const marker = "ВОПРОС:"
	idx := strings.Index(strings.ToUpper(s), marker)
	if idx < 0 {
		return "", false
	}
	q := strings.TrimSpace(s[idx+len(marker):])
	if i := strings.IndexByte(q, '\n'); i >= 0 {
		q = strings.TrimSpace(q[:i])
	}
	if q == "" || looksLikeDraft(q) {
		// Та же отбраковка, что у Summary: «ВОПРОС:» с английским рассуждением
		// после него — не вопрос агента, а черновик модели, и будить им человека
		// нельзя.
		return "", false
	}
	return clampRunes(q, MaxSummaryChars), true
}

// cleanupAnswer снимает то, чем модели любят обрамлять ответ: markdown-заборы,
// кавычки, «Вот пересказ:» в начале строки.
func cleanupAnswer(answer string) string {
	s := strings.TrimSpace(answer)
	s = strings.TrimPrefix(s, "```")
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`")
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	// Многострочный ответ схлопываем в одну строку: в уведомлении перевод
	// строки съедает место, а смысл несёт первый абзац.
	lines := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' })
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

func clampRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return strings.TrimSpace(string(r[:limit-1])) + "…"
}
