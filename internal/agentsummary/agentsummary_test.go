package agentsummary

import (
	"strings"
	"testing"
	"time"
)

// Кадр alt-screen агента: рамка, спиннер с меняющимся таймером и один и тот же
// экран, перерисованный трижды. Ровно это лежит в буфере к моменту «агент
// закончил», и ровно это уехало бы в модель без очистки.
const frames = "\x1b[?1049h\x1b[2J\x1b[H" +
	"╭──────────────────────────────╮\n" +
	"│ Обновил README и запустил тесты │\n" +
	"│ 12 файлов изменено              │\n" +
	"╰──────────────────────────────╯\n" +
	"✻ Crunched for 1s · esc to interrupt\n" +
	"╭──────────────────────────────╮\n" +
	"│ Обновил README и запустил тесты │\n" +
	"│ 12 файлов изменено              │\n" +
	"╰──────────────────────────────╯\n" +
	"✻ Crunched for 2s · esc to interrupt\n" +
	"╭──────────────────────────────╮\n" +
	"│ Обновил README и запустил тесты │\n" +
	"│ 12 файлов изменено              │\n" +
	"╰──────────────────────────────╯\n" +
	"✻ Crunched for 3s · esc to interrupt\n"

func TestCleanDropsAnsiFramesAndRepeatedRedraws(t *testing.T) {
	got := Clean([]byte(frames))

	if strings.Contains(got, "\x1b") || strings.Contains(got, "[?1049h") {
		t.Fatalf("ANSI осталась в тексте: %q", got)
	}
	if strings.Contains(got, "╭") || strings.Contains(got, "╰") {
		t.Fatalf("строки-рамки не вырезаны: %q", got)
	}
	if !strings.Contains(got, "Обновил README и запустил тесты") {
		t.Fatalf("смысл потерян: %q", got)
	}
	// Главное: три копии одного кадра схлопнуты в одну. Без дедупа сюда уехало
	// бы втрое больше текста — и втрое больше денег за него.
	if n := strings.Count(got, "Обновил README"); n != 1 {
		t.Fatalf("кадр повторён %d раз, ожидалась одна копия:\n%s", n, got)
	}
	// Подвал со спиннером отличается только числом — это тот же кадр.
	if n := strings.Count(got, "Crunched for"); n != 1 {
		t.Fatalf("подвал с таймером повторён %d раз:\n%s", n, got)
	}
}

func TestCleanKeepsTailWithinLimit(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 4000; i++ {
		sb.WriteString("строка вывода номер ")
		sb.WriteString(strings.Repeat("x", 20))
		sb.WriteString("\n")
	}
	sb.WriteString("ИТОГ: сборка прошла\n")
	got := Clean([]byte(sb.String()))
	if len(got) > MaxInputChars {
		t.Fatalf("не ужали до лимита: %d > %d", len(got), MaxInputChars)
	}
	if !strings.Contains(got, "ИТОГ: сборка прошла") {
		t.Fatal("обрезали не с той стороны — потерян конец эпизода, а он и есть итог")
	}
}

// Секреты реальных форм. Проверяем не «сработало правило», а то, что исходной
// строки в выходе НЕТ: именно она утекла бы в чужой сервис.
func TestMaskHidesSecrets(t *testing.T) {
	cases := []struct{ name, secret, line string }{
		{"openrouter", "sk-or-v1-9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c", "export OPENROUTER_API_KEY=sk-or-v1-9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c"},
		{"github", "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz012345", "git remote add origin https://ghp_AbCdEfGhIjKlMnOpQrStUvWxYz012345@github.com/x/y"},
		{"github-pat", "github_pat_11ABCDEFG0abcdefghijklmnop", "token: github_pat_11ABCDEFG0abcdefghijklmnop"},
		{"slack", "xoxb-123456789012-abcdefghijkl", "SLACK=xoxb-123456789012-abcdefghijkl"},
		{"aws", "AKIAIOSFODNN7EXAMPLE", "aws_access_key_id = AKIAIOSFODNN7EXAMPLE"},
		{"google", "AIzaSyC1234567890abcdefghijklmnopqrs", "GOOGLE_API_KEY=AIzaSyC1234567890abcdefghijklmnopqrs"},
		{"telegram", "7123456789:AAH1234567890abcdefghijklmnopqrstuv", "BOT_TOKEN=7123456789:AAH1234567890abcdefghijklmnopqrstuv"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
		{"url-pass", "hunter2secret", "psql postgres://admin:hunter2secret@db.example.com:5432/main"},
		{"password", "S3cretPa55word", "PASSWORD=S3cretPa55word"},
		{"hex", "da39a3ee5e6b4b0d3255bfef95601890afd80709", "signature da39a3ee5e6b4b0d3255bfef95601890afd80709"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Mask(c.line)
			if strings.Contains(got, c.secret) {
				t.Fatalf("секрет уехал бы наружу целиком:\nбыло:  %s\nстало: %s", c.line, got)
			}
			if !strings.Contains(got, maskLabel) {
				t.Fatalf("маркер замены не поставлен: %s", got)
			}
		})
	}
}

// Обратная сторона: маска не должна съедать обычный вывод — иначе пересказывать
// будет нечего, а человек получит уведомление из одних «скрыто».
func TestMaskKeepsOrdinaryOutput(t *testing.T) {
	line := "Обновил README.md и internal/web/server.go, тесты прошли: ok 12.480s"
	if got := Mask(line); got != line {
		t.Fatalf("обычная строка изменена:\nбыло:  %s\nстало: %s", line, got)
	}
}

func TestPrepareSkipsTooShort(t *testing.T) {
	if _, ok := Prepare([]byte("$ ls\nok\n")); ok {
		t.Fatal("на трёх словах звать модель незачем — квота не бесконечна")
	}
	if _, ok := Prepare([]byte(frames)); !ok {
		t.Fatal("настоящий эпизод работы должен пройти")
	}
}

func TestPrepareMasksBeforeSending(t *testing.T) {
	raw := "запускаю деплой\nexport TOKEN=sk-or-v1-9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c\nготово, сервис поднят и отвечает\n"
	text, ok := Prepare([]byte(raw))
	if !ok {
		t.Fatal("материал есть — должен пройти")
	}
	if strings.Contains(text, "sk-or-v1-9f8a7b6c") {
		t.Fatalf("Prepare отдал наружу ключ: %s", text)
	}
}

func TestSummaryCleansModelChatter(t *testing.T) {
	cases := map[string]string{
		"Обновил README и прогнал тесты.":     "Обновил README и прогнал тесты.",
		"```\nСобрал проект, ошибок нет\n```": "Собрал проект, ошибок нет",
		"\"Починил авторизацию\"":             "Починил авторизацию",
		"Развернул сервис.\nОшибок нет.":      "Развернул сервис. Ошибок нет.",
		unclear: "",
		"":      "",
	}
	for in, want := range cases {
		if got := Summary(in); got != want {
			t.Fatalf("Summary(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// ЖИВОЙ СЛУЧАЙ 12.08.2026: бесплатный роутер вернул размышление вслух на
// английском вместо пересказа. Без отбраковки этот черновик уехал бы в Telegram
// как «что сделал агент», да ещё обрезанным на полуслове.
func TestSummaryRejectsModelDraft(t *testing.T) {
	draft := `We need to summarize what the AI agent did in this turn, based on terminal output. ` +
		`The output is garbled with many "Embellishing…" and other noise. We need to extract meaningful actions: ` +
		`The agent seems to be reading a file, looking at screenshots, digging into logs, running a shell command.`
	if got := Summary(draft); got != "" {
		t.Fatalf("черновик модели принят за выжимку: %q", got)
	}
	// Английский короткий ответ — тоже мимо задания (просили по-русски).
	if got := Summary("The agent updated README and ran the tests."); got != "" {
		t.Fatalf("ответ не по-русски принят: %q", got)
	}
	// А нормальный русский пересказ обязан проходить — иначе фичи нет вовсе.
	const good = "Обновил README и прогнал тесты, ошибок нет."
	if got := Summary(good); got != good {
		t.Fatalf("нормальный пересказ отбракован: %q", got)
	}
	// Имена файлов и команд латиницей внутри русской фразы — это норма.
	const mixed = "Записал build/qa/summaryprobe/main.go и запустил go run для проверки."
	if got := Summary(mixed); got != mixed {
		t.Fatalf("русская фраза с путями отбракована: %q", got)
	}
}

func TestQuestionRejectsDraft(t *testing.T) {
	if q, ok := Question("ВОПРОС: We need to determine whether the agent is waiting for the user to confirm the tool call or not, based on the screen."); ok {
		t.Fatalf("черновик принят за вопрос: %q", q)
	}
}

// Спиннер, вклеенный ПРЯМО В СТРОКУ (живой хвост Claude Code), — главная причина,
// по которой модель начинала гадать вместо пересказа.
func TestCleanStripsInlineSpinnersAndPadding(t *testing.T) {
	raw := "✽Embellishing… ✻(1s · ↓2 tokens)✶*✢   ● Reading 1 file…                    \n" +
		"⏵⏵ bypass permissions on          ● I'll look at the logs.\n"
	got := Clean([]byte(raw))
	for _, bad := range []string{"✽", "✻", "✶", "●", "⏵"} {
		if strings.Contains(got, bad) {
			t.Fatalf("декорация %q осталась внутри строки: %q", bad, got)
		}
	}
	if strings.Contains(got, "   ") {
		t.Fatalf("выравнивание пробелами не схлопнуто: %q", got)
	}
	if !strings.Contains(got, "Reading 1 file") || !strings.Contains(got, "look at the logs") {
		t.Fatalf("смысл потерян: %q", got)
	}
}

// Ответ длиннее показа, но ещё не черновик (между MaxSummaryChars и draftLimit),
// обязан ОБРЕЗАТЬСЯ, а не пропасть: пересказ есть, просто он многословный.
func TestSummaryClampsLength(t *testing.T) {
	long := strings.Repeat("подробный пересказ работы ", 15) // ~390 символов
	if n := len([]rune(long)); n <= MaxSummaryChars || n > draftLimit {
		t.Fatalf("проверка настроена неверно: длина %d вне диапазона (%d; %d]", n, MaxSummaryChars, draftLimit)
	}
	got := Summary(long)
	if got == "" {
		t.Fatal("многословный, но осмысленный пересказ выброшен целиком")
	}
	if n := len([]rune(got)); n > MaxSummaryChars {
		t.Fatalf("длина %d больше лимита %d", n, MaxSummaryChars)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("обрезка не помечена многоточием: %q", got)
	}
}

// Ложное «агент ждёт вас» будит человека ночью зря — из-за таких срабатываний
// распознавание вопросов и было выключено в 2.49.4. Поэтому «ждёт» признаём
// только по явному маркеру, а всё остальное считаем «не ждёт».
func TestQuestionOnlyOnExplicitMarker(t *testing.T) {
	if q, ok := Question("ВОПРОС: разрешить запуск npm test?"); !ok || q != "разрешить запуск npm test?" {
		t.Fatalf("явный вопрос не распознан: %q %v", q, ok)
	}
	for _, answer := range []string{
		"НЕТ",
		"",
		"Агент, похоже, что-то печатает",
		"Не могу определить",
		"ВОПРОС:",
	} {
		if q, ok := Question(answer); ok {
			t.Fatalf("ответ %q принят за вопрос (%q) — это ночное уведомление на пустом месте", answer, q)
		}
	}
}

func TestBudgetDailyLimit(t *testing.T) {
	b := NewBudget()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	allowed := 0
	for i := 0; i < DefaultDailyLimit*2; i++ {
		// Разные терминалы, чтобы упереться именно в дневной потолок.
		if b.Allow("pty-"+string(rune('a'+i%26))+string(rune('a'+i/26)), now.Add(time.Duration(i)*time.Minute)) {
			allowed++
		}
	}
	if allowed != DefaultDailyLimit {
		t.Fatalf("за сутки разрешено %d запросов, ожидался потолок %d", allowed, DefaultDailyLimit)
	}
	// Через сутки окно открывается заново.
	if !b.Allow("pty-a", now.Add(25*time.Hour)) {
		t.Fatal("после суток бюджет обязан обнулиться")
	}
}

func TestBudgetPerPtyGap(t *testing.T) {
	b := NewBudget()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if !b.Allow("pty-1", now) {
		t.Fatal("первый запрос обязан пройти")
	}
	if b.Allow("pty-1", now.Add(DefaultPerPtyGap-time.Second)) {
		t.Fatal("второй запрос по тому же терминалу раньше паузы прошёл")
	}
	if !b.Allow("pty-2", now.Add(time.Second)) {
		t.Fatal("другой терминал не должен зависеть от чужой паузы")
	}
	if !b.Allow("pty-1", now.Add(DefaultPerPtyGap+time.Second)) {
		t.Fatal("после паузы запрос обязан пройти")
	}
}

func TestBudgetPausesAfterRateLimit(t *testing.T) {
	b := NewBudget()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	b.NoteRateLimited(now)
	if b.Allow("pty-1", now.Add(time.Minute)) {
		t.Fatal("после отказа по частоте бьёмся в исчерпанную квоту")
	}
	if !b.Allow("pty-1", now.Add(rateLimitPause+time.Minute)) {
		t.Fatal("пауза обязана заканчиваться")
	}
}
