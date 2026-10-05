package agenthooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

var now = time.UnixMilli(1789000000000)

// Вход хука — живой, снятый с claude 2.1.268 (поля, которые мы читаем).
func TestParseClaudePermissionRequest(t *testing.T) {
	raw := `{"session_id":"s1","transcript_path":"C:\\t.jsonl","cwd":"C:\\w","hook_event_name":"PermissionRequest",
		"tool_name":"Bash","tool_input":{"command":"npm test\n-- --watch","description":"run tests"}}`
	ev, ok := ParseClaude([]byte(raw), now)
	if !ok {
		t.Fatal("не разобрано")
	}
	want := Event{At: now.UnixMilli(), Agent: "claude", Kind: KindPermission, SessionID: "s1",
		Transcript: `C:\t.jsonl`, Tool: "Bash", Detail: "npm test -- --watch"}
	if !reflect.DeepEqual(ev, want) {
		t.Fatalf("got %+v\nwant %+v", ev, want)
	}
}

func TestParseClaudeRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "{}", "not json", `{"session_id":"x"}`} {
		if _, ok := ParseClaude([]byte(raw), now); ok {
			t.Fatalf("%q разобран как событие", raw)
		}
	}
}

func TestParseCodexNotify(t *testing.T) {
	arg := `{"type":"agent-turn-complete","thread-id":"th","turn-id":"tu","cwd":"C:\\w","client":"codex_exec",` +
		`"input-messages":["Reply"],"last-assistant-message":"ok"}`
	ev, ok := ParseCodexNotify(arg, now)
	if !ok || ev.Kind != KindStop || ev.Agent != "codex" || ev.SessionID != "th" || ev.TurnID != "tu" || ev.Message != "ok" {
		t.Fatalf("got %+v ok=%v", ev, ok)
	}
	if _, ok := ParseCodexNotify(`{"type":"approval-requested"}`, now); ok {
		t.Fatal("чужой тип принят за конец хода")
	}
}

func TestOneLineCutsAndCleans(t *testing.T) {
	long := strings.Repeat("я", 300)
	if got := []rune(oneLine(long, 120)); len(got) != 120 || got[119] != '…' {
		t.Fatalf("len=%d last=%q", len(got), got[len(got)-1])
	}
	if got := oneLine("a\x1b[31m\tb\r\nc", 50); got != "a[31m b c" {
		t.Fatalf("got %q", got)
	}
}

// Очередь: запись, чтение по кускам, недописанная строка, сдвиг в .old.
func TestAppendAndTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.jsonl")
	tail := &Tail{Path: path}
	if got := tail.Read(); got != nil {
		t.Fatalf("пустая очередь дала %v", got)
	}
	if err := Append(path, Event{At: 1, Agent: "claude", Kind: KindPrompt}); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, Event{At: 2, Agent: "claude", Kind: KindStop}); err != nil {
		t.Fatal(err)
	}
	got := tail.Read()
	if len(got) != 2 || got[0].Kind != KindPrompt || got[1].Kind != KindStop {
		t.Fatalf("got %+v", got)
	}
	if again := tail.Read(); again != nil {
		t.Fatalf("повторное чтение дало %v", again)
	}

	// Хук ещё пишет строку: половина — не событие, вторая половина его дополнит.
	line, _ := json.Marshal(Event{At: 3, Agent: "claude", Kind: KindPermission})
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.Write(line[:10])
	_ = f.Close()
	if half := tail.Read(); half != nil {
		t.Fatalf("недописанная строка дала %v", half)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.Write(append(line[10:], '\n'))
	_ = f.Close()
	if got := tail.Read(); len(got) != 1 || got[0].Kind != KindPermission {
		t.Fatalf("дописанная строка: %+v", got)
	}

	// Очередь начата заново (файл стал короче) — читаем с нуля.
	_ = os.Remove(path)
	_ = Append(path, Event{At: 4, Agent: "claude", Kind: KindSessionEnd})
	if got := tail.Read(); len(got) != 1 || got[0].Kind != KindSessionEnd {
		t.Fatalf("после сдвига: %+v", got)
	}
}

// Рестарт агента при живом терминале: старые события не проигрываются.
func TestTailSkipsEventsBeforeFirstRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.jsonl")
	_ = Append(path, Event{At: 1, Agent: "claude", Kind: KindStop})
	tail := &Tail{Path: path}
	if got := tail.Read(); got != nil {
		t.Fatalf("старое событие проиграно: %v", got)
	}
	_ = Append(path, Event{At: 2, Agent: "claude", Kind: KindPrompt})
	if got := tail.Read(); len(got) != 1 || got[0].Kind != KindPrompt {
		t.Fatalf("новое событие потеряно: %+v", got)
	}
}

func TestAppendRotatesOversizedSpool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.jsonl")
	if err := os.WriteFile(path, make([]byte, maxSpool+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, Event{At: 1, Agent: "claude", Kind: KindStop}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Size() > 200 {
		t.Fatalf("очередь не начата заново: %d байт", st.Size())
	}
	if _, err := os.Stat(path + ".old"); err != nil {
		t.Fatalf("старая очередь не сохранена: %v", err)
	}
}

func TestClaudeSettingsShape(t *testing.T) {
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type, Command string
				Timeout       int
			} `json:"hooks"`
		} `json:"hooks"`
	}
	// Путь — той ОС, где идёт тест: filepath.ToSlash меняет `\` только на
	// Windows, а на Linux `\` — обычная буква имени (CI 13.09.2026).
	exe, wantCmd := "/home/Иван Петров/remotai", `"/home/Иван Петров/remotai" hook claude`
	if runtime.GOOS == "windows" {
		exe, wantCmd = `C:\Users\Иван Петров\remotai.exe`, `"C:/Users/Иван Петров/remotai.exe" hook claude`
	}
	if err := json.Unmarshal(ClaudeSettings(exe), &s); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{KindSessionStart, KindPrompt, KindPermission, KindNotification, KindPreTool, KindStop, KindSessionEnd} {
		entries := s.Hooks[ev]
		if len(entries) != 1 || len(entries[0].Hooks) != 1 {
			t.Fatalf("%s: %+v", ev, entries)
		}
		h := entries[0].Hooks[0]
		if h.Type != "command" || h.Command != wantCmd || h.Timeout <= 0 {
			t.Fatalf("%s: %+v", ev, h)
		}
	}
	if m := s.Hooks[KindPreTool][0].Matcher; m != "AskUserQuestion" {
		t.Fatalf("PreToolUse на каждый инструмент: matcher=%q", m)
	}
	// PostToolUse — не на всё: самые частые инструменты процесс не запускают.
	post := s.Hooks[KindPostTool][0].Matcher
	if post == "" || post == "*" {
		t.Fatalf("PostToolUse на каждый инструмент: matcher=%q", post)
	}
	for _, frequent := range []string{"Read", "Grep", "Glob"} {
		if strings.Contains("|"+post+"|", "|"+frequent+"|") {
			t.Fatalf("PostToolUse ловит частый %s: %q", frequent, post)
		}
	}
	for _, asks := range []string{"Bash", "Write", "Edit", "WebFetch"} {
		if !strings.Contains("|"+post+"|", "|"+asks+"|") {
			t.Fatalf("PostToolUse не закроет вопрос о %s: %q", asks, post)
		}
	}
}

func TestEnsureClaudeSettingsRewritesOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	p1, err := EnsureClaudeSettings(dir, "/opt/remotai")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p1, old, old)
	if _, err := EnsureClaudeSettings(dir, "/opt/remotai"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p1); !st.ModTime().Equal(old) {
		t.Fatal("неизменённый файл перезаписан")
	}
	if _, err := EnsureClaudeSettings(dir, "/usr/bin/remotai"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p1); !strings.Contains(string(b), "/usr/bin/remotai") {
		t.Fatal("новый путь не записан")
	}
}

func TestCodexNotifyArg(t *testing.T) {
	// Windows-путь переводится в прямые слэши только на Windows (filepath.ToSlash).
	if runtime.GOOS == "windows" {
		win, ok := CodexNotifyArg(`C:\Users\user\AppData\Local\Programs\Remotai\remotai.exe`, true)
		if !ok || win != "notify=['C:/Users/user/AppData/Local/Programs/Remotai/remotai.exe','hook','codex']" {
			t.Fatalf("windows: %q %v", win, ok)
		}
	}
	posix, ok := CodexNotifyArg("/usr/local/bin/remotai", false)
	if !ok || posix != `notify=["/usr/local/bin/remotai","hook","codex"]` {
		t.Fatalf("posix: %q %v", posix, ok)
	}
	for _, bad := range []string{"", `C:\it's\remotai.exe`, `C:\100%\remotai.exe`, `C:\a&b\remotai.exe`} {
		if _, ok := CodexNotifyArg(bad, true); ok {
			t.Fatalf("опасный путь принят: %q", bad)
		}
	}
}

// Вопрос Claude: пункты берутся из входа хука — с экрана их не прочитать
// (скриншот владельца 13.09.2026: описания под пунктами, «Chat about this»).
func TestParseClaudeAskUserQuestion(t *testing.T) {
	raw := `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{"questions":[
		{"question":"Какой клиент ставить первым?","header":"Клиент","multiSelect":false,"options":[
			{"label":"Karing + запасной OneXray (Recommended)","description":"В боте и на сайте…"},
			{"label":"OneXray первым","description":"То же, но первым OneXray"},
			{"label":"Не трогать сейчас","description":"Оставлю в отчёте"}]}]}}`
	ev, ok := ParseClaude([]byte(raw), now)
	if !ok || ev.Detail != "Какой клиент ставить первым?" {
		t.Fatalf("вопрос: %+v", ev)
	}
	want := []string{"Karing + запасной OneXray (Recommended)", "OneXray первым", "Не трогать сейчас"}
	if !reflect.DeepEqual(ev.Options, want) {
		t.Fatalf("пункты: %q", ev.Options)
	}
	// multiSelect и несколько вопросов: цифра не отвечает — пунктов нет, вопрос есть.
	for _, in := range []string{
		`{"questions":[{"question":"Что включить?","multiSelect":true,"options":[{"label":"A"},{"label":"B"}]}]}`,
		`{"questions":[{"question":"Первый?","options":[{"label":"A"},{"label":"B"}]},{"question":"Второй?","options":[{"label":"C"},{"label":"D"}]}]}`,
	} {
		var input map[string]any
		if err := json.Unmarshal([]byte(in), &input); err != nil {
			t.Fatal(err)
		}
		q, opts := askQuestion(input)
		if q == "" || opts != nil {
			t.Fatalf("%s: %q %q", in, q, opts)
		}
	}
}

// Строка notify — дословно из config.toml владельца (плагин computer-use).
func TestParseCodexNotify_ChainsOwnersPlugin(t *testing.T) {
	cfg := "model = \"gpt-5\"\r\n" +
		`notify = [ "C:\\Users\\user\\.codex\\plugins\\cache\\openai-bundled\\computer-use\\26.527.31326\\node_modules\\@oai\\sky\\bin\\windows\\codex-computer-use.exe", "turn-ended" ]` + "\r\n" +
		"[features]\nhooks = true\n"
	argv, st := parseCodexNotify(cfg)
	want := []string{`C:\Users\user\.codex\plugins\cache\openai-bundled\computer-use\26.527.31326\node_modules\@oai\sky\bin\windows\codex-computer-use.exe`, "turn-ended"}
	if st != CodexNotifyChainable || !reflect.DeepEqual(argv, want) {
		t.Fatalf("got %q state=%v", argv, st)
	}
}

func TestParseCodexNotifyCases(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		argv []string
		st   CodexNotifyState
	}{
		{"нет notify", "model = \"x\"\n", nil, CodexNotifyNone},
		{"буквальные строки", "notify = ['/bin/notify-send', 'done'] # коммент\n", []string{"/bin/notify-send", "done"}, CodexNotifyChainable},
		{"notify в таблице не верхнего уровня", "[tui]\nnotify = [\"x\"]\n", nil, CodexNotifyNone},
		{"похожий ключ", "notify_on = true\n", nil, CodexNotifyNone},
		{"многострочный массив", "notify = [\n  \"a\",\n  \"b\"\n]\n", nil, CodexNotifyUnknown},
		{"не массив", "notify = \"x\"\n", nil, CodexNotifyUnknown},
		{"уже наш", "notify = ['C:/r/remotai.exe','hook','codex']\n", nil, CodexNotifyNone},
	}
	for _, c := range cases {
		argv, st := parseCodexNotify(c.cfg)
		if st != c.st || !reflect.DeepEqual(argv, c.argv) {
			t.Errorf("%s: got %q state=%v, want %q state=%v", c.name, argv, st, c.argv, c.st)
		}
	}
}

func TestSpoolPathIsSafe(t *testing.T) {
	p := SpoolPath(`..\..\evil`)
	if filepath.Dir(p) != filepath.Clean(os.TempDir()) || strings.Contains(filepath.Base(p), "..") {
		t.Fatalf("путь вышел из TempDir: %s", p)
	}
}
