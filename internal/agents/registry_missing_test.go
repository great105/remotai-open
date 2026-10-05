package agents

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Приговор «агент пропал» обязан истекать, а приговор «есть» — нет.
//
// 09.09.2026: в журнале агента владельца «Agent detection: 7/15 available:
// codex, gemini, kimi, grok…» — без claude, при живом claude 2.1.266 в
// %APPDATA%\npm. Причина в чужом инструменте: npm на время установки
// переименовывает свои обёртки («reify mark retired»: claude.cmd →
// .claude.cmd-2nQ89reZ), а Claude Code обновляет себя сам — шесть установок за
// двадцать минут. Снимок, снятый в эту секунду, жил до перезапуска Remotai.
func TestMissingAgentIsRecheckedAndFoundIsKept(t *testing.T) {
	gone := &AgentDescriptor{ID: "later", CLINames: []string{"later"}}
	found := &AgentDescriptor{ID: "already", CLINames: []string{"already"}, DetectedPath: "/старый/путь"}
	builtin := &AgentDescriptor{ID: "shell", DetectedPath: "built-in"}
	list := []*AgentDescriptor{gone, found, builtin}
	markEverFound(t, "later", "already")

	// Пока обёртки нет — ничего не меняется и никто не «находится».
	if got := refreshMissingIn(list, func(string) string { return "" }); len(got) != 0 {
		t.Fatalf("на пустом поиске найдены агенты: %v", got)
	}
	if gone.DetectedPath != "" {
		t.Fatalf("ненайденному агенту приписан путь %q", gone.DetectedPath)
	}

	// Установка закончилась — обёртка на месте.
	got := refreshMissingIn(list, func(name string) string {
		if name == "later" {
			return "/новый/путь/later"
		}
		// Найденного искать заново не должны вовсе; если спросят — соврём, что
		// он исчез, и подмена пути провалит проверку ниже.
		return ""
	})
	if len(got) != 1 || got[0] != "later" {
		t.Fatalf("перепроверка вернула %v, ждали [later]", got)
	}
	if gone.DetectedPath != "/новый/путь/later" {
		t.Fatalf("путь вернувшегося агента = %q", gone.DetectedPath)
	}
	if found.DetectedPath != "/старый/путь" {
		t.Fatalf("найденного агента переискали и потеряли: %q", found.DetectedPath)
	}
	if builtin.DetectedPath != "built-in" {
		t.Fatalf("встроенного агента тронули: %q", builtin.DetectedPath)
	}
}

// Агента, которого этот процесс не видел НИ РАЗУ, не ищем: он «не установлен»
// честно, а плата за поиск — замер 09.09.2026 на 65 каталогах PATH — 35 мс за
// промах, то есть 285 мс на каждый список агентов, если обходить весь реестр.
func TestNeverSeenAgentIsNotSearchedAgain(t *testing.T) {
	stranger := &AgentDescriptor{ID: "stranger", CLINames: []string{"stranger"}}
	asked := 0
	got := refreshMissingIn([]*AgentDescriptor{stranger}, func(string) string {
		asked++
		return "/нашли/бы/stranger"
	})
	if asked != 0 {
		t.Fatalf("незнакомого агента искали %d раз", asked)
	}
	if len(got) != 0 || stranger.DetectedPath != "" {
		t.Fatalf("незнакомый агент помечен найденным: %v %q", got, stranger.DetectedPath)
	}
}

// Троттлинг: список агентов — частый эндпоинт, и перепроверка не должна
// превращаться в обход PATH на каждый запрос.
func TestRefreshMissingIsThrottled(t *testing.T) {
	missingRecheckMu.Lock()
	saved := missingRecheckAt
	missingRecheckMu.Unlock()
	t.Cleanup(func() {
		missingRecheckMu.Lock()
		missingRecheckAt = saved
		missingRecheckMu.Unlock()
	})

	RefreshMissing() // первый проход занимает окно
	before := stateOfRegistry()
	RefreshMissing() // второй внутри TTL обязан быть пустышкой
	if after := stateOfRegistry(); after != before {
		t.Fatalf("повторный вызов внутри TTL изменил реестр:\n%s\n%s", before, after)
	}
}

func stateOfRegistry() string {
	out := ""
	for _, d := range Registry {
		out += d.ID + "=" + d.Path() + ";"
	}
	return out
}

// И то же самое ЗАПУСКОМ, а не на выдуманном поиске: обёртка появляется в
// каталоге из PATH уже после того, как агент объявлен пропавшим.
func TestMissingAgentFoundInRealPathAfterInstall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const cli = "remotai-test-late-agent"
	name := cli
	if runtime.GOOS == "windows" {
		name = cli + ".cmd"
	}
	desc := &AgentDescriptor{ID: "late", CLINames: []string{cli}}
	list := []*AgentDescriptor{desc}
	markEverFound(t, "late")

	if got := refreshMissingIn(list, findCLI); len(got) != 0 {
		t.Fatalf("агент найден до установки: %v", got)
	}

	shim := filepath.Join(dir, name)
	if err := os.WriteFile(shim, []byte("@echo off\r\nexit /b 0\r\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := refreshMissingIn(list, findCLI); len(got) != 1 {
		t.Fatalf("после установки перепроверка вернула %v", got)
	}
	if desc.DetectedPath != shim {
		t.Fatalf("путь = %q, ждали %q", desc.DetectedPath, shim)
	}
}

// Весь случай владельца целиком и НА ФАЙЛАХ: агент найден, npm увёл обёртку
// переименованием, детект в эту секунду объявил «не установлен», установка
// закончилась — и агент обязан вернуться сам, без перезапуска Remotai.
func TestAgentSurvivesNpmRetireWindow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	restoreRegistry(t)
	isolateCLISearch(t)

	// findCLI смотрит и МИМО PATH: на Windows в профили пользователей (папку
	// npm — служба под LocalSystem не наследует пользовательский PATH), на
	// Linux/macOS в ~/.local/bin, /usr/local/bin и прочие каталоги, которых нет
	// в PATH службы. Подменой одного PATH установленного на стенде агента не
	// спрятать (WSL Ubuntu с opencode в /usr/local/bin: «детект нашёл агента,
	// которого на диске нет»), поэтому каталоги поиска подменены тоже, и проба
	// не зависит от того, что стоит на машине.
	const id = "opencode"
	name := id
	if runtime.GOOS == "windows" {
		name = id + ".cmd"
	}
	shim := filepath.Join(dir, name)
	if err := os.WriteFile(shim, []byte("@echo off\r\nexit /b 0\r\n"), 0700); err != nil {
		t.Fatal(err)
	}

	DetectAgents()
	if !IsAvailable(id) {
		t.Fatal("агент не найден при живой обёртке в PATH")
	}

	// Ровно то, что делает npm: «reify mark retired» — переименование.
	retired := filepath.Join(dir, "."+name+"-2nQ89reZ")
	if err := os.Rename(shim, retired); err != nil {
		t.Fatal(err)
	}
	DetectAgents()
	if IsAvailable(id) {
		t.Fatal("детект нашёл агента, которого на диске нет")
	}

	// Установка закончилась, npm написал новую обёртку.
	if err := os.Rename(retired, shim); err != nil {
		t.Fatal(err)
	}
	RefreshMissing()
	if !IsAvailable(id) {
		t.Fatal("агент вернулся на диск, а в реестре остался «не установлен»")
	}
	if got := GetDescriptor(id).Path(); got != shim {
		t.Fatalf("путь после возврата = %q, ждали %q", got, shim)
	}
}

// restoreRegistry — DetectAgents переписывает пути ВСЕМ агентам, а PATH в пробе
// подменён. Возвращаем реестр и окно троттлинга как было, иначе соседние пробы
// (и живой процесс, если проба идёт в нём) увидят пустой реестр.
func restoreRegistry(t *testing.T) {
	t.Helper()
	detectMu.Lock()
	saved := make(map[string]string, len(Registry))
	savedFound := make(map[string]bool, len(everFound))
	for _, d := range Registry {
		saved[d.ID] = d.DetectedPath
	}
	for id, v := range everFound {
		savedFound[id] = v
	}
	detectMu.Unlock()
	missingRecheckMu.Lock()
	savedAt := missingRecheckAt
	missingRecheckAt = time.Time{}
	missingRecheckMu.Unlock()

	t.Cleanup(func() {
		detectMu.Lock()
		for _, d := range Registry {
			d.DetectedPath = saved[d.ID]
		}
		everFound = savedFound
		detectMu.Unlock()
		missingRecheckMu.Lock()
		missingRecheckAt = savedAt
		missingRecheckMu.Unlock()
	})
}

// isolateCLISearch — findCLI ищет только в PATH пробы: каталог профилей
// Windows пуст, пользовательских каталогов Unix нет. Иначе результат пробы
// зависит от того, какие агенты установлены на машине, где она идёт.
func isolateCLISearch(t *testing.T) {
	t.Helper()
	savedProfiles, savedBinDirs := windowsProfilesRoot, unixUserBinDirs
	windowsProfilesRoot = t.TempDir()
	unixUserBinDirs = func(string) []string { return nil }
	t.Cleanup(func() {
		windowsProfilesRoot, unixUserBinDirs = savedProfiles, savedBinDirs
	})
}

// markEverFound — «этих агентов процесс уже находил», с уборкой за собой:
// everFound общий на пакет, и мусор из пробы менял бы поведение соседних.
func markEverFound(t *testing.T, ids ...string) {
	t.Helper()
	detectMu.Lock()
	for _, id := range ids {
		everFound[id] = true
	}
	detectMu.Unlock()
	t.Cleanup(func() {
		detectMu.Lock()
		for _, id := range ids {
			delete(everFound, id)
		}
		detectMu.Unlock()
	})
}
