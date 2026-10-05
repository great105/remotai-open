package pty

import (
	"strings"
	"testing"
	"time"
)

// T-39g (ST-10): служебные OSC не должны попадать в «видимый» текст
// эвристик — ни с терминатором BEL, ни с ST (ESC \). Разметка команд
// (OSC 133) Remotai шлёт BEL, но чужие интеграции (fish ≥ 4, starship,
// oh-my-posh) шлют ST. Раньше ansiRe знал только BEL: `ESC]133;C ESC\` перед
// строкой ломал якорь начала строки, и настоящая ошибка терялась.
func TestAnsiReStripsOSCWithBELAndST(t *testing.T) {
	bel := "\x1b]133;A\x07$ \x1b]133;B\x07make\r\n\x1b]133;C\x07error: boom\r\n\x1b]133;D;2\x07"
	st := strings.ReplaceAll(bel, "\x07", "\x1b\\")
	for name, raw := range map[string]string{"BEL": bel, "ST": st} {
		clean := ansiRe.ReplaceAllString(raw, "")
		if strings.Contains(clean, "133;") || strings.Contains(clean, "\x1b") {
			t.Fatalf("%s: маркеры остались в видимом тексте: %q", name, clean)
		}
		if hint := errorHintFrom([]byte(raw)); hint != "error: boom" {
			t.Fatalf("%s: строка ошибки не найдена за маркером: %q", name, hint)
		}
	}
	// OSC с ST не должен съедать видимый текст до далёкого BEL.
	if clean := ansiRe.ReplaceAllString("\x1b]133;A\x1b\\видимый текст\x07", ""); !strings.Contains(clean, "видимый текст") {
		t.Fatalf("видимый текст съеден: %q", clean)
	}
	// Гиперссылка OSC 8 с ST — от неё остаётся только подпись.
	if clean := ansiRe.ReplaceAllString("\x1b]8;;https://example.com\x1b\\ссылка\x1b]8;;\x1b\\", ""); clean != "ссылка" {
		t.Fatalf("OSC 8: %q", clean)
	}
	// Код завершения в D — не текст ошибки.
	if hint := errorHintFrom([]byte("\x1b]133;D;1\x1b\\\r\n\x1b]133;A\x1b\\$ ")); hint != "" {
		t.Fatalf("маркер D принят за ошибку: %q", hint)
	}
}

// Регресс: у Claude Code и Codex вопрос выглядит как меню («1. Yes / 2. No»),
// а прежние шаблоны ждали текстовое «Allow tool?» или `❯ Yes` — из-за этого у
// главных потребителей фичи hint был ПУСТОЙ, и все они попадали в один и тот же
// статус «ждёт ответа» без текста вопроса.
func TestDetectInputRequired(t *testing.T) {
	tests := []struct {
		name     string
		tail     string
		wantHint bool
		wantKind string
	}{
		{
			name:     "apt: Do you want to continue? [Y/n]",
			tail:     "The following NEW packages will be installed:\n  htop\nDo you want to continue? [Y/n] ",
			wantHint: true, wantKind: HintKindYesNo,
		},
		{
			name:     "pip: Proceed (y/n)?",
			tail:     "Installing collected packages: rich\nProceed (y/n)? ",
			wantHint: true, wantKind: HintKindYesNo,
		},
		{
			name:     "git/less: Press Enter to continue",
			tail:     "hint: waiting for your editor\nPress Enter to continue...",
			wantHint: true, wantKind: HintKindEnter,
		},
		{
			name:     "ssh TOFU: fingerprint prompt",
			tail:     "The authenticity of host 'srv (1.2.3.4)' can't be established.\nED25519 key fingerprint is SHA256:abc.\nAre you sure you want to continue connecting (yes/no/[fingerprint])? ",
			wantHint: true, wantKind: HintKindYesNo,
		},
		{
			name: "claude code: вопрос с нумерованным меню",
			tail: "Do you want to make this edit to events.go?\n" +
				"❯ 1. Yes\n" +
				"  2. Yes, and don't ask again this session\n" +
				"  3. No, and tell Claude what to do differently",
			wantHint: true, wantKind: HintKindChoice,
		},
		{
			name: "codex: меню подтверждения команды",
			tail: "Approve shell command?\n" +
				"  1) Approve\n" +
				"❯ 2) Approve for session\n" +
				"  3) Deny",
			wantHint: true, wantKind: HintKindChoice,
		},
		{
			name:     "обычный вывод: одиночный маркер не вопрос",
			tail:     "$ cat notes.md\n> цитата из файла\n",
			wantHint: false,
		},
		{
			name:     "обычный вывод сборки",
			tail:     "go: downloading github.com/foo/bar v1.2.3\nok  \tpkg/foo\t0.123s\n",
			wantHint: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint, kind := detectInputRequired([]byte(tt.tail))
			if got := hint != ""; got != tt.wantHint {
				t.Fatalf("hint=%q (есть=%v), ожидали есть=%v", hint, got, tt.wantHint)
			}
			if tt.wantHint && kind != tt.wantKind {
				t.Fatalf("kind=%q, ожидали %q (hint=%q)", kind, tt.wantKind, hint)
			}
		})
	}
}

// Вопрос ищем в последних строках: отвеченный ранее вопрос не должен держать
// статус «ждёт» (иначе после ответа терминал навсегда остаётся янтарным).
func TestDetectInputRequiredIgnoresAnsweredPrompt(t *testing.T) {
	tail := "Proceed (y/n)? y\nInstalling…\ndone\n$ "
	if hint, _ := detectInputRequired([]byte(tail)); hint != "" {
		t.Fatalf("после ответа hint должен быть пустым, получили %q", hint)
	}
}

// Регресс: без латча событие уходило каждые 15 с бесконечно — за ночь сотни
// уведомлений об одном и том же простое.
func TestEpisodeLatch(t *testing.T) {
	d := newDetectorState()
	now := time.Now()

	fresh := 0
	for i := 0; i < 20; i++ {
		if d.enterEpisode("wait|Подтвердите: y/n", now.Add(time.Duration(i)*time.Second)) {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("один эпизод должен дать одно событие, получили %d", fresh)
	}
	first := d.episodeAt

	// Другой вопрос — новый эпизод.
	if !d.enterEpisode("wait|Выберите вариант: 1/2/3", now.Add(30*time.Second)) {
		t.Fatal("смена вопроса обязана считаться новым эпизодом")
	}
	if !d.episodeAt.After(first) {
		t.Fatal("episodeAt должен обновиться на новом эпизоде")
	}

	// Возврат к работе закрывает эпизод только после нескольких спокойных тиков.
	for i := 0; i < episodeCleanTicks-1; i++ {
		d.leaveEpisode()
	}
	if d.episode == "" {
		t.Fatalf("эпизод закрылся раньше %d тиков", episodeCleanTicks)
	}
	d.leaveEpisode()
	if d.episode != "" {
		t.Fatal("после episodeCleanTicks эпизод должен закрыться")
	}
	if !d.enterEpisode("wait|Подтвердите: y/n", now.Add(time.Minute)) {
		t.Fatal("после закрытия эпизода тот же вопрос — снова событие")
	}
}

// promptFingerprint обязан быть СТАБИЛЬНЫМ, пока агент стоит на одном вопросе
// (перерисовка рамки, таймера, спиннера, перемещение курсора по пунктам — это
// тот же вопрос), и РАЗНЫМ, когда вопрос стал другим. hint у всех подтверждений
// инструмента одинаковый («Claude: разрешить инструмент»), поэтому без
// отпечатка два подряд идущих вопроса были бы одним эпизодом — и кнопка от
// первого ответила бы на второй.
func TestPromptFingerprint(t *testing.T) {
	frame := func(question, cursorAt, timer string) []byte {
		lines := []string{
			"╭──────────────────────────────────────╮",
			"│ " + question + " │",
		}
		for _, item := range []string{"1. Yes", "2. No"} {
			marker := "  "
			if strings.HasPrefix(item, cursorAt) {
				marker = "❯ "
			}
			lines = append(lines, "│ "+marker+item+" │")
		}
		return []byte(strings.Join(append(lines,
			"╰──────────────────────────────────────╯",
			"✻ Waiting… ("+timer+" · esc to interrupt)"), "\n"))
	}

	base := promptFingerprint(frame("Allow tool Bash(npm test)?", "1", "12s"))
	redraw := promptFingerprint(frame("Allow tool Bash(npm test)?", "2", "347s"))
	if base != redraw {
		t.Fatalf("перерисовка того же вопроса сменила отпечаток (%s != %s) — это лишнее уведомление каждую секунду", base, redraw)
	}
	other := promptFingerprint(frame("Allow tool Edit(main.go)?", "1", "12s"))
	if base == other {
		t.Fatal("разные инструменты дали один отпечаток — кнопка от первого вопроса ответит на второй")
	}
}

// Ключ эпизода ожидания: тот же вопрос — тот же эпизод (одно уведомление,
// стабильный status_at), другой вопрос — новый эпизод (старая кнопка получит
// 409 prompt_changed). Смена отпечатка засчитывается со второго подряд
// наблюдения, поэтому одиночный «мусорный» тик эпизод не рвёт.
func TestWaitEpisodeKeyFingerprint(t *testing.T) {
	d := newDetectorState()
	t0 := time.Now()
	const hint = "Claude: разрешить инструмент"

	if !d.enterEpisode(d.waitEpisodeKey(hint, "fpA"), t0) {
		t.Fatal("первый вопрос обязан быть новым эпизодом")
	}
	start := d.episodeAt
	for i := 1; i <= 10; i++ {
		if d.enterEpisode(d.waitEpisodeKey(hint, "fpA"), t0.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("тик %d: тот же вопрос повторно объявлен новым эпизодом", i)
		}
	}

	// Одиночный сбойный отпечаток (кадр перерисовки, пойманный на середине).
	if d.enterEpisode(d.waitEpisodeKey(hint, "fpGarbage"), t0.Add(11*time.Second)) {
		t.Fatal("неподтверждённый отпечаток не должен создавать эпизод")
	}
	if d.enterEpisode(d.waitEpisodeKey(hint, "fpA"), t0.Add(12*time.Second)) {
		t.Fatal("возврат к прежнему отпечатку — тот же эпизод")
	}
	if !d.episodeAt.Equal(start) {
		t.Fatal("status_at обязан быть стабильным, пока агент стоит на одном вопросе")
	}

	// Настоящий новый вопрос: тот же hint, другой отпечаток, два тика подряд.
	if d.enterEpisode(d.waitEpisodeKey(hint, "fpB"), t0.Add(13*time.Second)) {
		t.Fatal("первое наблюдение нового отпечатка ещё не эпизод")
	}
	if !d.enterEpisode(d.waitEpisodeKey(hint, "fpB"), t0.Add(14*time.Second)) {
		t.Fatal("подтверждённая смена вопроса обязана дать новый эпизод")
	}
	if !d.episodeAt.After(start) {
		t.Fatal("новый вопрос обязан обновить status_at — иначе кнопка от старого пройдёт guard")
	}
}

// «Ждёт ответа» (есть распознанный вопрос) и «освободился» (вопрос не
// распознан) — разные статусы: раньше оба были waiting, и закончивший агент
// навсегда висел в «Требует внимания» с ложным «ждёт вашего ответа».
func TestStatusFrom(t *testing.T) {
	now := time.Now()
	askedAt := now.Add(-5 * time.Minute)
	lastActive := now.Add(-time.Minute).UnixMilli()

	tests := []struct {
		name                                    string
		le                                      sessEvent
		busy, working, stillIdle, questionFresh bool
		wantStatus                              string
		wantHint                                string
		wantHintKind                            string
		wantAtIsAskedAt                         bool
	}{
		{
			// Живая жалоба: «выбери варианты — пропадает сразу». Агент, стоя на
			// собственном меню, продолжает печатать (подсветка пункта, таймер,
			// курсор) — значит stillIdle=false, — но вопрос ВИДЕН на экране, и
			// кнопки ответа обязаны остаться.
			name: "вопрос на экране, агент дорисовывает меню → всё ещё waiting",
			le:   sessEvent{kind: EventWaitingInput, at: askedAt, hint: "1) Yes 2) No", hintKind: HintKindChoice},
			busy: true, working: true, stillIdle: false, questionFresh: true,
			wantStatus: "waiting", wantHint: "1) Yes 2) No", wantHintKind: HintKindChoice, wantAtIsAskedAt: true,
		},
		{
			// А когда вопрос с экрана ушёл (агент принял ответ и работает),
			// кнопкам там не место — иначе следующий выбор уйдёт не туда.
			name: "вопроса на экране больше нет, агент печатает → working",
			le:   sessEvent{kind: EventWaitingInput, at: askedAt, hint: "1) Yes 2) No", hintKind: HintKindChoice},
			busy: true, working: true, stillIdle: false, questionFresh: false,
			wantStatus: "working",
		},
		{
			name: "распознанный вопрос → waiting + hint + тип",
			le:   sessEvent{kind: EventWaitingInput, at: askedAt, hint: "Подтвердите: y/n", hintKind: HintKindYesNo},
			busy: true, working: true, stillIdle: true,
			wantStatus: "waiting", wantHint: "Подтвердите: y/n", wantHintKind: HintKindYesNo, wantAtIsAskedAt: true,
		},
		{
			name: "агент молчит, вопрос не распознан → ready",
			le:   sessEvent{kind: EventAgentReady, at: askedAt},
			busy: true, working: true, stillIdle: true,
			wantStatus: "ready", wantAtIsAskedAt: true,
		},
		{
			name: "старый агент: waiting с пустым hint → тоже ready",
			le:   sessEvent{kind: EventWaitingInput, at: askedAt},
			busy: true, working: true, stillIdle: true,
			wantStatus: "ready", wantAtIsAskedAt: true,
		},
		{
			name: "агент печатает → working",
			le:   sessEvent{kind: EventWaitingInput, at: askedAt, hint: "Подтвердите: y/n"},
			busy: true, working: true, stillIdle: false,
			wantStatus: "working",
		},
		{
			name: "упавшая команда у промпта → error",
			le:   sessEvent{kind: EventError, at: now.Add(-time.Minute)},
			busy: false, working: false, stillIdle: true,
			wantStatus: "error",
		},
		{
			name: "старая ошибка не липнет",
			le:   sessEvent{kind: EventError, at: now.Add(-10 * time.Minute)},
			busy: false, working: false, stillIdle: true,
			wantStatus: "idle",
		},
		{
			name: "чистый шелл → idle со временем последней активности",
			le:   sessEvent{},
			busy: false, working: false, stillIdle: true,
			wantStatus: "idle",
		},
		// SSH-сессия: busy включён (waiting/ready считаются по её выводу), но
		// молчащая сессия НЕ работает — иначе вчерашний просмотр логов вечно
		// висит в «Сейчас работает» с пульсирующей точкой.
		{
			name: "SSH молчит у промпта → idle, а не working",
			le:   sessEvent{kind: EventFinished, at: now.Add(-9 * time.Hour)},
			busy: true, working: false, stillIdle: true,
			wantStatus: "idle",
		},
		{
			name: "SSH прямо сейчас печатает → working",
			le:   sessEvent{kind: EventFinished, at: now.Add(-time.Second)},
			busy: true, working: true, stillIdle: false,
			wantStatus: "working",
		},
		// Раньше ветка busy перехватывала ошибку раньше её собственной ветки, и
		// упавшая команда в агентской/SSH-сессии до клиента не доходила.
		{
			name: "ошибка в SSH-сессии у промпта доходит до клиента",
			le:   sessEvent{kind: EventError, at: now.Add(-time.Minute)},
			busy: true, working: false, stillIdle: true,
			wantStatus: "error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, at, hint, hintKind := statusFrom(tt.le, tt.busy, tt.working, tt.stillIdle, tt.questionFresh, now, lastActive)
			if status != tt.wantStatus {
				t.Fatalf("status=%q, ожидали %q", status, tt.wantStatus)
			}
			if hint != tt.wantHint || hintKind != tt.wantHintKind {
				t.Fatalf("hint=%q/%q, ожидали %q/%q", hint, hintKind, tt.wantHint, tt.wantHintKind)
			}
			if tt.wantAtIsAskedAt && at != askedAt.UnixMilli() {
				t.Fatalf("status_at=%d, ожидали начало эпизода %d", at, askedAt.UnixMilli())
			}
			if tt.wantStatus == "idle" && at != lastActive {
				t.Fatalf("idle: status_at=%d, ожидали last_active %d", at, lastActive)
			}
		})
	}
}

// StatusAt для waiting — начало ожидания, а не «сейчас»: setEvent теперь
// вызывается с episodeAt, иначе главная показывала «0с» вместо «ждёт 12 минут».
func TestWaitingStatusAtIsEpisodeStart(t *testing.T) {
	start := time.Now().Add(-7 * time.Minute)
	s := newSession("id", "C:/tmp", "cmd.exe", 1, nil, start)
	s.setEvent(EventWaitingInput, start, "Подтвердите: y/n", HintKindYesNo)
	s.evMu.Lock()
	got := s.ev.lastEvt.at
	s.evMu.Unlock()
	if !got.Equal(start) {
		t.Fatalf("StatusAt=%v, ожидали начало эпизода %v", got, start)
	}
}

// lastLines оставляет последние непустые строки — на этом держится защита от
// залипшего вопроса при расширенном окне хвоста.
func TestLastLines(t *testing.T) {
	tail := "one\n\ntwo\nthree\n\n"
	got := string(lastLines([]byte(tail), 2))
	if got != "two\nthree" {
		t.Fatalf("lastLines = %q, ожидали \"two\\nthree\"", got)
	}
	if n := len(strings.Split(string(lastLines([]byte(tail), 10)), "\n")); n != 3 {
		t.Fatalf("при n больше числа строк должно вернуться всё непустое, строк=%d", n)
	}
}

// «Вопрос виден на экране» живёт чуть дольше одного тика детектора.
//
// Детектор тикает раз в секунду и распознаёт вопрос по хвосту вывода. Агент
// перерисовывает своё меню целиком, и в отдельный кадр маркер вопроса попасть
// не успевает — если бы признак гас мгновенно, кнопки ответа моргали бы.
func TestQuestionOnScreenOutlivesSingleTick(t *testing.T) {
	state := newDetectorState()
	now := time.Now()

	if state.questionOnScreen(now) {
		t.Fatal("вопроса не было, а признак уже стоит")
	}

	state.waitSeenAt = now
	if !state.questionOnScreen(now) {
		t.Fatal("вопрос виден прямо сейчас, а признак не встал")
	}
	// Пропущенный тик (агент рисовал кадр без маркера) кнопки не гасит.
	if !state.questionOnScreen(now.Add(2 * time.Second)) {
		t.Fatal("признак погас через два тика — кнопки ответа будут моргать")
	}
	// Но вопрос, ушедший с экрана, признаком не остаётся: следующий выбор
	// человека не должен уйти в уже закрытый вопрос.
	if state.questionOnScreen(now.Add(questionFreshTTL + time.Second)) {
		t.Fatal("вопрос ушёл с экрана, а признак остался")
	}
}

// Отказ запуска на сервере НЕ должен читаться как «агент работает».
//
// У SSH-сессии агент определяется по тексту на экране, а наша строка отказа
// сама содержит его имя («Агент claude не установлен на сервере») — и раньше
// интерфейс на этом основании рисовал значок агента, приглушал ряд команд и
// показывал занятость. Метка REMOTAI_AGENT_MISSING разводит эти два случая.
func TestRemoteAgentKindIgnoresMissingMarker(t *testing.T) {
	withTail := func(text string) *Session {
		s := &Session{}
		s.buf = []byte(text)
		return s
	}

	// Обычный вывод агента — узнаём.
	if got := remoteAgentKind(withTail("user@srv:~$ claude\nWelcome to Claude Code")); got != "claude" {
		t.Fatalf("живой claude на экране распознан как %q", got)
	}
	// Наша строка отказа — НЕ узнаём, хотя имя в ней есть.
	refusal := "user@srv:~$ if command -v claude …\n" +
		AgentMissingMarker + " Агент claude не установлен на сервере\n"
	if got := remoteAgentKind(withTail(refusal)); got != "" {
		t.Fatalf("отказ запуска прочитан как агент %q — интерфейс покажет работу, которой нет", got)
	}
}

// «Агент закончил эпизод работы» — finished с duration_ms. Срабатывает только
// на НАСТОЯЩУЮ работу с последующим устойчивым затишьем: короткая пауза между
// шагами (Kimi Code в thinking молчит минутами, но терминал к тому моменту не
// набрал ни байтов, ни длительности эпизода — см. пороги) и набор текста
// человеком в поле ввода события не дают. Один эпизод — одно событие.
func TestAgentFinishedDuration(t *testing.T) {
	t0 := time.Now()

	// Эпизод работы: агент печатал две минуты (всплески чаще, чем раз в 5с —
	// activityStart не сдвигается), затем замолчал.
	working := func() *detectorState {
		d := newDetectorState()
		for i := 0; i <= 120; i++ {
			d.observeChunk([]byte("агент что-то делает\n"), t0.Add(time.Duration(i)*time.Second))
		}
		return d
	}

	t.Run("настоящая работа + устойчивая тишина → finished с duration", func(t *testing.T) {
		d := working()
		now := t0.Add(120*time.Second + agentFinishedIdle)
		dur, ok := agentFinishedDuration(d, now)
		if !ok {
			t.Fatal("завершившийся эпизод работы не распознан")
		}
		if want := 120*time.Second + agentFinishedIdle; dur != want {
			t.Fatalf("duration=%v, ожидали %v (от activityStart до детекции)", dur, want)
		}
	})

	t.Run("тишина короче порога — ещё не finished", func(t *testing.T) {
		d := working()
		if _, ok := agentFinishedDuration(d, t0.Add(120*time.Second+agentFinishedIdle-time.Second)); ok {
			t.Fatal("тишина меньше agentFinishedIdle не должна давать finished")
		}
	})

	t.Run("набор текста человеком — не эпизод работы", func(t *testing.T) {
		d := newDetectorState()
		// Человек 8 секунд печатает вопрос в поле ввода: эхо и перерисовка
		// рамки дают байты и «активность», но работы агента здесь не было.
		for i := 0; i < 8; i++ {
			d.observeChunk([]byte("x"), t0.Add(time.Duration(i)*time.Second))
		}
		if _, ok := agentFinishedDuration(d, t0.Add(8*time.Second+agentFinishedIdle+time.Minute)); ok {
			t.Fatal("короткий мелкий ввод не должен давать finished")
		}
	})

	t.Run("один эпизод — одно событие", func(t *testing.T) {
		d := working()
		now := t0.Add(120*time.Second + agentFinishedIdle)
		if _, ok := agentFinishedDuration(d, now); !ok {
			t.Fatal("первый раз эпизод обязан распознаться")
		}
		// Вызывающий отметил эпизод отзвонившим — повторов быть не должно,
		// сколько бы тиков ни прошло.
		d.finActivityStart = d.activityStart
		if _, ok := agentFinishedDuration(d, now.Add(10*time.Minute)); ok {
			t.Fatal("по одному эпизоду finished ушёл второй раз")
		}
		// Новый всплеск вывода после паузы >5с — новый эпизод (observeChunk
		// сдвигает activityStart), и его завершение снова новость.
		work2 := t0.Add(20 * time.Minute)
		for i := 0; i <= 30; i++ {
			d.observeChunk([]byte("агент снова работает\n"), work2.Add(time.Duration(i)*time.Second))
		}
		if _, ok := agentFinishedDuration(d, work2.Add(30*time.Second+agentFinishedIdle)); !ok {
			t.Fatal("завершение НОВОГО эпизода работы обязано дать finished")
		}
	})

	// Агент без источника статуса (Kimi, Codex): 45 с тишины — ещё не конец,
	// он думает молча; конец наступает только после agentFinishedIdleNoStatus.
	t.Run("без источника статуса порог тишины вдвое длиннее", func(t *testing.T) {
		d := working()
		d.finishedIdle = agentFinishedIdleNoStatus
		if _, ok := agentFinishedDuration(d, t0.Add(120*time.Second+agentFinishedIdle)); ok {
			t.Fatal("45 с тишины у агента без источника статуса приняты за конец работы")
		}
		if _, ok := agentFinishedDuration(d, t0.Add(120*time.Second+agentFinishedIdleNoStatus)); !ok {
			t.Fatal("после удвоенной тишины finished обязан состояться")
		}
	})
}
