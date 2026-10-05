package agentupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner — подмена запуска: отвечает по имени команды и считает вызовы.
// Настоящие CLI в тестах не запускаются вовсе.
type fakeRunner struct {
	mu      sync.Mutex
	calls   map[string]int
	answers map[string]string
	slow    map[string]bool
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[key]++
	ans, ok := f.answers[key]
	slow := f.slow[key]
	f.mu.Unlock()
	if slow {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if !ok {
		return []byte("not found"), errors.New("exit status 1")
	}
	return []byte(ans), nil
}

func (f *fakeRunner) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestChecker(f *fakeRunner, clk *clock, hist *History) *Checker {
	return &Checker{
		Run:            f.run,
		Env:            winEnv,
		History:        hist,
		Now:            clk.now,
		VersionTimeout: 50 * time.Millisecond,
		LatestTimeout:  50 * time.Millisecond,
		PrefixTimeout:  50 * time.Millisecond,
	}
}

const claudeCmd = `C:\Users\user\AppData\Roaming\npm\claude.cmd`

func claudeAgent() Agent {
	return Agent{ID: "claude", Name: "Claude Code", Path: claudeCmd, Install: "npm i -g @anthropic-ai/claude-code"}
}

func TestCheckUpdateAvailableAndCache(t *testing.T) {
	f := &fakeRunner{answers: map[string]string{
		"npm.cmd prefix -g":                              `C:\Users\user\AppData\Roaming\npm` + "\r\n",
		claudeCmd + " --version":                         "2.1.3 (Claude Code)\n",
		"npm.cmd view @anthropic-ai/claude-code version": "2.1.5\n",
	}}
	clk := &clock{t: time.Unix(1_000_000, 0)}
	c := newTestChecker(f, clk, NewHistory(""))
	items := c.Check(context.Background(), []Agent{claudeAgent()}, false)
	it := items[0]
	if it.Version != "2.1.3" || it.Latest != "2.1.5" || !it.UpdateAvailable {
		t.Fatalf("item = %+v", it)
	}
	if it.Owner != OwnerNPM || it.UpdateCommand != "npm.cmd i -g @anthropic-ai/claude-code@latest" || !it.CanUpdate {
		t.Fatalf("owner/command = %q %q", it.Owner, it.UpdateCommand)
	}
	// Повтор через минуту — всё из кэша.
	clk.t = clk.t.Add(time.Minute)
	c.Check(context.Background(), []Agent{claudeAgent()}, false)
	if n := f.count(claudeCmd + " --version"); n != 1 {
		t.Fatalf("--version вызван %d раз, ждали 1 (кэш)", n)
	}
	if n := f.count("npm.cmd view @anthropic-ai/claude-code version"); n != 1 {
		t.Fatalf("npm view вызван %d раз, ждали 1 (кэш)", n)
	}
	// «Проверить сейчас»: версия заново, реестр — раз в минуту.
	c.Check(context.Background(), []Agent{claudeAgent()}, true)
	if n := f.count(claudeCmd + " --version"); n != 2 {
		t.Fatalf("refresh: --version %d раз, ждали 2", n)
	}
	if n := f.count("npm.cmd view @anthropic-ai/claude-code version"); n != 2 {
		t.Fatalf("refresh: npm view %d раз, ждали 2", n)
	}
	// Реестр живёт 6 часов.
	clk.t = clk.t.Add(5 * time.Hour)
	c.Check(context.Background(), []Agent{claudeAgent()}, false)
	if n := f.count("npm.cmd view @anthropic-ai/claude-code version"); n != 2 {
		t.Fatalf("через 5 ч npm view %d раз, ждали 2", n)
	}
	clk.t = clk.t.Add(2 * time.Hour)
	c.Check(context.Background(), []Agent{claudeAgent()}, false)
	if n := f.count("npm.cmd view @anthropic-ai/claude-code version"); n != 3 {
		t.Fatalf("через 7 ч npm view %d раз, ждали 3", n)
	}
}

func TestCheckVersionCacheDropsOnMtime(t *testing.T) {
	f := &fakeRunner{answers: map[string]string{claudeCmd + " --version": "2.1.3"}}
	clk := &clock{t: time.Unix(1_000_000, 0)}
	c := newTestChecker(f, clk, nil)
	mt := int64(1)
	c.Mtime = func(string) int64 { return mt }
	c.Check(context.Background(), []Agent{claudeAgent()}, false)
	mt = 2 // npm переписал обёртку — значит, переставили
	c.Check(context.Background(), []Agent{claudeAgent()}, false)
	if n := f.count(claudeCmd + " --version"); n != 2 {
		t.Fatalf("--version %d раз, ждали 2 (смена mtime)", n)
	}
}

func TestCheckTimeouts(t *testing.T) {
	f := &fakeRunner{
		answers: map[string]string{},
		slow: map[string]bool{
			claudeCmd + " --version":                         true,
			"npm.cmd view @anthropic-ai/claude-code version": true,
			"npm.cmd prefix -g":                              true,
		},
	}
	c := newTestChecker(f, &clock{t: time.Unix(1_000_000, 0)}, nil)
	start := time.Now()
	it := c.Check(context.Background(), []Agent{claudeAgent()}, false)[0]
	if time.Since(start) > 2*time.Second {
		t.Fatalf("проверка ждала %v — таймаут не сработал", time.Since(start))
	}
	if it.Version != "" || it.VersionError == "" || it.LatestError == "" || it.UpdateAvailable {
		t.Fatalf("item = %+v", it)
	}
	// Владелец определился по пути и без ответа npm.
	if it.Owner != OwnerNPM || !it.CanUpdate {
		t.Fatalf("owner = %q can=%v", it.Owner, it.CanUpdate)
	}
}

// --version молчит (агент в эту секунду переставляется) — у npm-пакета версия
// берётся из его package.json.
func TestCheckPackageJSONFallback(t *testing.T) {
	f := &fakeRunner{slow: map[string]bool{claudeCmd + " --version": true}}
	c := newTestChecker(f, &clock{t: time.Unix(1_000_000, 0)}, nil)
	var asked string
	c.ReadFile = func(p string) ([]byte, error) {
		asked = p
		return []byte(`{"name":"@anthropic-ai/claude-code","version":"2.1.284"}`), nil
	}
	it := c.Check(context.Background(), []Agent{claudeAgent()}, false)[0]
	if it.Version != "2.1.284" || it.VersionError != "" {
		t.Fatalf("item = %+v", it)
	}
	if asked != "C:/Users/user/AppData/Roaming/npm/node_modules/@anthropic-ai/claude-code/package.json" {
		t.Fatalf("читали %q", asked)
	}
}

func TestCheckNativeClaudeLatestByInstaller(t *testing.T) {
	native := `C:\Users\user\.local\bin\claude.exe`
	f := &fakeRunner{answers: map[string]string{native + " --version": "2.1.284 (Claude Code)"}}
	c := newTestChecker(f, &clock{t: time.Unix(1_000_000, 0)}, nil)
	it := c.Check(context.Background(), []Agent{{ID: "claude", Path: native, Install: "npm i -g @anthropic-ai/claude-code"}}, false)[0]
	if it.Owner != OwnerClaudeNative || it.UpdateCommand != "claude.exe update" || it.LatestKnown || it.Latest != "" {
		t.Fatalf("item = %+v", it)
	}
	if n := f.count("npm.cmd view @anthropic-ai/claude-code version"); n != 0 {
		t.Fatal("для нативного claude в npm не ходим")
	}
}

func TestHistoryRecordsChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-versions.json")
	h := NewHistory(path)
	if ch, _ := h.Record("claude", "2.1.3", OwnerNPM, 100); !ch {
		t.Fatal("первый замер должен записаться")
	}
	if ch, _ := h.Record("claude", "2.1.3", OwnerNPM, 200); ch {
		t.Fatal("та же версия — не событие")
	}
	if ch, _ := h.Record("claude", "2.1.5", OwnerNPM, 300); !ch {
		t.Fatal("смена версии — событие")
	}
	// Переживает перезапуск: новый объект читает файл.
	ev := NewHistory(path).Events("claude")
	if len(ev) != 2 || ev[0].From != "2.1.3" || ev[0].To != "2.1.5" || ev[0].At != 300 || ev[1].From != "" {
		t.Fatalf("events = %+v", ev)
	}
	if got := NewHistory(path).Events("codex"); got == nil || len(got) != 0 {
		t.Fatalf("пустая история должна быть пустым массивом, got %#v", got)
	}
	for i := 0; i < maxEvents+10; i++ {
		_, _ = h.Record("codex", "0.1."+string(rune('0'+i%10))+string(rune('a'+i/10)), OwnerNPM, int64(i))
	}
	if n := len(h.Events("codex")); n != maxEvents {
		t.Fatalf("история не обрезается: %d", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRecordsHistory(t *testing.T) {
	f := &fakeRunner{answers: map[string]string{claudeCmd + " --version": "2.1.3"}}
	clk := &clock{t: time.Unix(1_000_000, 0)}
	h := NewHistory("")
	c := newTestChecker(f, clk, h)
	c.Check(context.Background(), []Agent{claudeAgent()}, false)
	f.answers[claudeCmd+" --version"] = "2.1.5"
	c.Check(context.Background(), []Agent{claudeAgent()}, true)
	ev := h.Events("claude")
	if len(ev) != 2 || ev[0].To != "2.1.5" || ev[0].From != "2.1.3" || ev[0].Owner != OwnerNPM {
		t.Fatalf("events = %+v", ev)
	}
}
