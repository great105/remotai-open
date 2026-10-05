package agentupdate

import (
	"strings"
)

// Command — чем обновлять и можно ли вообще.
type Command struct {
	Text      string // что напечатать в терминал ("" = нечем)
	CanUpdate bool
	Reason    string // почему нельзя (для человека)
	// LatestKnown — последнюю версию можно узнать без обновления (npm view).
	// У нативного установщика Claude нельзя: её проверяет сам установщик.
	LatestKnown bool
}

// LookPath — есть ли команда в PATH (для выбора npm.cmd/pnpm.exe на Windows).
type LookPath func(name string) bool

// winTool выбирает, как назвать программу на Windows.
//
// Грабля та же, что у InstallCommand в реестре: npm, pnpm и прочие ставят рядом
// с .cmd ещё и .ps1, PowerShell предпочитает .ps1, а политика по умолчанию
// (Restricted) его запрещает — обновление падало бы раньше, чем начнётся.
// Поэтому на Windows всегда явное расширение: .exe, если есть, иначе .cmd.
func winTool(name string, look LookPath) string {
	if look != nil {
		if look(name + ".exe") {
			return name + ".exe"
		}
		if look(name + ".cmd") {
			return name + ".cmd"
		}
	}
	return name + ".cmd"
}

// safeArg — можно ли подставить строку в команду без кавычек в обоих шеллах
// (PowerShell и bash). Имена пакетов npm, id winget и формулы Homebrew в это
// укладываются; всё прочее не подставляем вовсе, а не экранируем наугад.
func safeArg(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("@/._-+", r):
		default:
			return false
		}
	}
	return true
}

// safePath — путь для --prefix: в кавычках, но без того, что PowerShell или
// bash раскроют внутри двойных кавычек.
func safePath(s string) bool {
	return s != "" && !strings.ContainsAny(s, "\"$`%!\r\n")
}

// BuildCommand собирает команду обновления ТЕМ ЖЕ менеджером, которым агент
// поставлен. goos — ОС компьютера (от неё .cmd/.exe на Windows).
func BuildCommand(in Install, env Env, look LookPath) Command {
	win := env.GOOS == "windows"
	tool := func(name string) string {
		if win {
			return winTool(name, look)
		}
		return name
	}
	needPkg := func() (Command, bool) {
		if !safeArg(in.Package) {
			return Command{Reason: "Не знаем, каким пакетом он поставлен, поэтому обновить отсюда нельзя."}, false
		}
		return Command{}, true
	}
	switch in.Owner {
	case OwnerNPM:
		if c, ok := needPkg(); !ok {
			return c
		}
		npm := "npm"
		if win {
			// Как в реестре (InstallCommand): npm.cmd, а не npm.ps1.
			npm = "npm.cmd"
		}
		text := npm + " i -g " + in.Package + "@latest"
		// Поставлен не в тот каталог, который npm считает глобальным (другой
		// Node через nvm, свой prefix) — ставим ровно туда же, где он лежит.
		// Иначе появится ВТОРАЯ копия, а запускаться продолжит старая.
		if in.Prefix != "" && env.NPMPrefix != "" && !samePath(in.Prefix, env.NPMPrefix, win) {
			if !safePath(in.Prefix) {
				return Command{Reason: "Агент стоит в необычной папке npm, команду для неё не собрать."}
			}
			text += ` --prefix "` + nativePath(in.Prefix, win) + `"`
		}
		return Command{Text: text, CanUpdate: true, LatestKnown: true}
	case OwnerPNPM:
		if c, ok := needPkg(); !ok {
			return c
		}
		return Command{Text: tool("pnpm") + " add -g " + in.Package + "@latest", CanUpdate: true, LatestKnown: true}
	case OwnerBun:
		if c, ok := needPkg(); !ok {
			return c
		}
		return Command{Text: tool("bun") + " add -g " + in.Package + "@latest", CanUpdate: true, LatestKnown: true}
	case OwnerVolta:
		if c, ok := needPkg(); !ok {
			return c
		}
		return Command{Text: tool("volta") + " install " + in.Package + "@latest", CanUpdate: true, LatestKnown: true}
	case OwnerClaudeNative:
		// Тем же бинарём, что нашёл поиск: на Windows рядом может лежать
		// claude.ps1 от npm-установки, и голое `claude` в PowerShell уйдёт к нему.
		bin := "claude"
		if win {
			bin = "claude.exe"
			if strings.EqualFold(in.Bin, "claude.cmd") {
				bin = "claude.cmd"
			}
		}
		return Command{Text: bin + " update", CanUpdate: true}
	case OwnerBrew:
		if !safeArg(in.Name) {
			return Command{Reason: "Стоит через Homebrew, но имя пакета не определилось. Обновите командой brew upgrade."}
		}
		if in.Cask {
			return Command{Text: "brew upgrade --cask " + in.Name, CanUpdate: true}
		}
		return Command{Text: "brew upgrade " + in.Name, CanUpdate: true}
	case OwnerWinget:
		if !safeArg(in.Name) {
			return Command{Reason: "Стоит через winget, но имя пакета не определилось. Обновите командой winget upgrade."}
		}
		return Command{Text: "winget upgrade -e --id " + in.Name, CanUpdate: true}
	case OwnerScoop:
		if !safeArg(in.Name) {
			return Command{Reason: "Стоит через Scoop, но имя пакета не определилось. Обновите командой scoop update."}
		}
		return Command{Text: "scoop update " + in.Name, CanUpdate: true}
	}
	return Command{Reason: "Не знаем, чем он установлен, поэтому обновить отсюда нельзя: чужая команда поставила бы вторую копию."}
}

func samePath(a, b string, win bool) bool {
	a, b = normPath(a), normPath(b)
	if win {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func nativePath(p string, win bool) string {
	p = normPath(p)
	if win {
		return strings.ReplaceAll(p, "/", `\`)
	}
	return p
}
