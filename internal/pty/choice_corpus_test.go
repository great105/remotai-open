package pty

import (
	"strings"
	"testing"
	"time"
)

// Корпус реальных кадров: что агенты рисуют, когда ждут выбора, и что рисуют,
// когда НЕ ждут. Жалоба владельца 30.07: «не всегда понимает, что есть выбор».
//
// Замер на этом корпусе до правок показал корень: Claude Code рисует вопрос В
// РАМКЕ («│ ❯ 1. Yes │»), а класс маркеров в choiceItemRe/choiceCursorRe
// начинался сразу с `[›>❯*]` — ни один пункт не совпадал, и меню главного
// потребителя фичи не распознавалось вовсе. Gemini метит пункт кружком («●»)
// — тоже мимо. Здесь закреплены обе формы и все ложные случаи, которые
// нельзя принимать за меню.
func TestChoiceCorpus(t *testing.T) {
	frame := func(lines ...string) string { return strings.Join(lines, "\n") }

	cases := []struct {
		name    string
		tail    string
		choice  bool     // ожидаем ли «это меню выбора»
		options []string // подписи кнопок (nil — не проверяем)
	}{
		{
			name: "claude: рамка вокруг вопроса",
			tail: frame(
				"╭──────────────────────────────────────────────────────────╮",
				"│ Bash command                                             │",
				"│                                                          │",
				"│   npm test                                               │",
				"│                                                          │",
				"│ Do you want to proceed?                                  │",
				"│ ❯ 1. Yes                                                 │",
				"│   2. Yes, and don't ask again for npm commands           │",
				"│   3. No, and tell Claude what to do differently (esc)    │",
				"╰──────────────────────────────────────────────────────────╯",
			),
			choice:  true,
			options: []string{"Yes", "Yes, and don't ask again for npm command…", "No, and tell Claude what to do different…"},
		},
		{
			// Тот же кадр, но в обычной shell-сессии: ниже стоит промпт. Так
			// выглядит живой замер через API — вопрос обязан находиться и здесь.
			name: "claude: кадр, ниже промпт shell",
			tail: frame(
				"│ Do you want to proceed?                                  │",
				"│ ❯ 1. Yes                                                 │",
				"│   2. No                                                  │",
				"╰──────────────────────────────────────────────────────────╯",
				"",
				"PS C:\\Users\\user> ",
			),
			choice:  true,
			options: []string{"Yes", "No"},
		},
		{
			name: "claude: меню без рамки",
			tail: frame(
				"Do you want to make this edit to events.go?",
				"❯ 1. Yes",
				"  2. No",
			),
			choice:  true,
			options: []string{"Yes", "No"},
		},
		{
			name: "codex: подтверждение команды",
			tail: frame(
				"  Codex wants to run:",
				"    go test ./...",
				"",
				"  Allow command?",
				"  > 1. Yes, run it",
				"    2. No, tell Codex what to do",
			),
			choice:  true,
			options: []string{"Yes, run it", "No, tell Codex what to do"},
		},
		{
			name: "gemini: курсор кружком",
			tail: frame(
				"Apply this change?",
				"● 1. Yes, allow once",
				"  2. Yes, allow always",
				"  3. No (esc)",
			),
			choice:  true,
			options: []string{"Yes, allow once", "Yes, allow always", "No (esc)"},
		},
		{
			name: "claude: доверие к папке",
			tail: frame(
				"╭──────────────────────────────────────────────╮",
				"│ Do you trust the files in this folder?       │",
				"│                                              │",
				"│ ❯ 1. Yes, proceed                            │",
				"│   2. No, exit                                │",
				"╰──────────────────────────────────────────────╯",
			),
			choice:  true,
			options: []string{"Yes, proceed", "No, exit"},
		},
		{
			// Нумерованный ответ агента — самый частый ложный кандидат: он
			// приходит из живого лога владельца.
			name: "шум: нумерованный текст в ответе",
			tail: frame(
				"  4. Рабочий экран и карточка. Вид карточки,",
				"  который отдел уже считает удобным, за образец;",
				"  5. Время реакции. Час по консультациям,",
				"  уведомление РОПу и руководителю группы.",
			),
			choice: false,
		},
		{
			name: "шум: маркированный список",
			tail: frame(
				"Сделано:",
				"* 1. собрал клиент",
				"* 2. прогнал тесты",
			),
			choice: false,
		},
		{
			name: "шум: вывод git log",
			tail: frame(
				"commit 7d4c389 (HEAD -> main)",
				"    1. поправил тексты",
				"    2. добавил тест",
				"commit 04ee506",
			),
			choice: false,
		},
	}

	// Буквенные вопросы: цифр нет, отвечают буквой — клиент обязан показать
	// Y/N, а не 1/2/3. Ловятся отдельными шаблонами, поэтому проверяются здесь
	// же, но по типу вопроса, а не по подписям меню.
	letterCases := []struct{ name, tail, kind string }{
		{"aider: (Y)es/(N)o/(A)ll", "Apply edit to main.py? (Y)es/(N)o/(A)ll/(S)kip all [Yes]: ", HintKindYesNo},
		{"npm: Ok to proceed?", "Need to install the following packages:\n  vite@6\nOk to proceed? (y) ", HintKindYesNo},
		{"git: (y,n,q,a,d)", "Stage this hunk [y,n,q,a,d,?]? ", HintKindYesNo},
		{"ssh: TOFU", "Are you sure you want to continue connecting (yes/no/[fingerprint])? ", HintKindYesNo},
	}
	for _, c := range letterCases {
		t.Run(c.name, func(t *testing.T) {
			hint, kind := detectInputRequired([]byte(c.tail))
			if hint == "" {
				t.Fatalf("вопрос не распознан — на телефоне не будет ни подсказки, ни кнопок\n%s", c.tail)
			}
			if kind != c.kind {
				t.Fatalf("тип вопроса = %q, хотели %q", kind, c.kind)
			}
		})
	}

	// Распознавание вопросов ВЫКЛЮЧЕНО по умолчанию (решение владельца 30.07:
	// «ловит просто так»). Правила ниже проверяются как чистые функции — они
	// работают, когда переключатель включён; менеджер по умолчанию их не зовёт.
	t.Run("по умолчанию детектор вопросов выключен", func(t *testing.T) {
		m := &Manager{}
		if m.QuestionDetection() {
			t.Fatal("детектор включён по умолчанию — «ждёт ответа» снова появится на пустом месте")
		}
		m.SetQuestionDetection(true)
		if !m.QuestionDetection() {
			t.Fatal("переключатель не включает детектор — вернуть кнопки будет нечем")
		}
	})

	// Вопрос в ОБЫЧНОМ терминале (не агент): спрашивает не шелл, а команда в
	// нём — npm, git, ssh. Раньше статус требовал busy («сессией распоряжается
	// агент»), и такой терминал числился «свободен»: на телефоне ни подсказки,
	// ни кнопок, а работа стояла.
	t.Run("вопрос команды в обычном терминале — это waiting", func(t *testing.T) {
		now := time.Now()
		le := sessEvent{
			kind:     EventWaitingInput,
			at:       now.Add(-2 * time.Second),
			hint:     "Подтвердите: y/n",
			hintKind: HintKindYesNo,
		}
		// busy=false (голый шелл), working=false, вывод только что был
		// (stillIdle=false), но вопрос ВИДЕН на экране прямо сейчас.
		status, _, hint, kind := statusFrom(le, false, false, false, true, now, 0)
		if status != "waiting" {
			t.Fatalf("статус = %q, хотели waiting: команда ждёт ответа, а терминал числится свободным", status)
		}
		if hint == "" || kind != HintKindYesNo {
			t.Fatalf("hint=%q kind=%q — клиенту нечего показать на кнопках", hint, kind)
		}
		// Вопрос ушёл с экрана (ответили с компьютера) — статус обязан погаснуть.
		if status, _, _, _ := statusFrom(le, false, false, false, false, now, 0); status == "waiting" {
			t.Fatal("вопроса на экране нет, а терминал всё ещё «ждёт ответа»")
		}
	})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tail := []byte(c.tail)
			if got := looksLikeChoice(tail); got != c.choice {
				t.Fatalf("looksLikeChoice = %v, хотели %v\n%s", got, c.choice, c.tail)
			}
			if !c.choice {
				return
			}
			hint, kind := detectInputRequired(tail)
			if hint == "" {
				t.Fatalf("вопрос не распознан вовсе — кнопок на телефоне не будет\n%s", c.tail)
			}
			if kind != HintKindChoice {
				t.Fatalf("тип вопроса = %q, хотели %q (иначе клиент покажет Y/N вместо цифр)", kind, HintKindChoice)
			}
			if c.options == nil {
				return
			}
			got := choiceOptions(tail)
			if len(got) != len(c.options) {
				t.Fatalf("подписи кнопок = %q, хотели %q", got, c.options)
			}
			for i := range got {
				if got[i] != c.options[i] {
					t.Fatalf("подпись %d = %q, хотели %q", i+1, got[i], c.options[i])
				}
			}
		})
	}
}
