package web

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Файл своих команд живёт в домашней папке ЧЕЛОВЕКА, поэтому тест подменяет её:
// боевой список трогать нельзя (та же грабля, что однажды снесла relay_jwt).
func withTempHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"USERPROFILE", "HOME"} {
		t.Setenv(k, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestCommandsRoundTrip(t *testing.T) {
	withTempHome(t)
	commandsMu.Lock()
	defer commandsMu.Unlock()

	if got := loadCommands(); got != nil {
		t.Fatalf("на чистой машине список обязан быть пустым, а он %v", got)
	}
	list := []UserCommand{
		{ID: "c-1", Cmd: "bash deploy.sh", Label: "Деплой", Pinned: true},
		{ID: "c-2", Cmd: "git status"},
	}
	if err := saveCommands(list); err != nil {
		t.Fatal(err)
	}
	got := loadCommands()
	if len(got) != 2 || got[0].Cmd != "bash deploy.sh" || !got[0].Pinned || got[1].Cmd != "git status" {
		t.Fatalf("список вернулся не тем: %+v", got)
	}
	// Права закрытые: это рабочие строки человека. На Windows режим файла
	// os.WriteFile не применяет вовсе (POSIX-битов там нет) — проверяем только
	// там, где проверка что-то значит.
	if runtime.GOOS != "windows" {
		st, err := os.Stat(commandsPath())
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("права на файл команд %v — открыт чужим", perm)
		}
	}
	if base := filepath.Base(commandsPath()); base != ".tgcontrol-commands.json" {
		t.Fatalf("имя файла %q", base)
	}
}

func TestCommandsBrokenFileDoesNotBreakList(t *testing.T) {
	withTempHome(t)
	commandsMu.Lock()
	defer commandsMu.Unlock()
	if err := os.WriteFile(commandsPath(), []byte("не json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadCommands(); got != nil {
		t.Fatalf("битый файл должен читаться как пустой список, а вернул %v", got)
	}
}

// Перенос с пульта повторяется без вреда: пультов несколько, и каждый принесёт
// своё в своё время.
func TestCommandsImportIsIdempotent(t *testing.T) {
	withTempHome(t)
	commandsMu.Lock()
	defer commandsMu.Unlock()

	merge := func(incoming []UserCommand) (list []UserCommand, added int) {
		list = loadCommands()
		have := map[string]bool{}
		for _, c := range list {
			have[c.Cmd] = true
		}
		for _, c := range incoming {
			if c.Cmd == "" || have[c.Cmd] {
				continue
			}
			have[c.Cmd] = true
			c.ID = newCommandID()
			list = append(list, c)
			added++
		}
		if added > 0 {
			if err := saveCommands(list); err != nil {
				t.Fatal(err)
			}
		}
		return list, added
	}

	incoming := []UserCommand{{Cmd: "bash deploy.sh", Pinned: true}, {Cmd: "git status"}}
	if _, added := merge(incoming); added != 2 {
		t.Fatalf("первый перенос добавил %d, ожидалось 2", added)
	}
	if _, added := merge(incoming); added != 0 {
		t.Fatalf("повторный перенос добавил %d — дубликаты", added)
	}
	// Второй пульт со своей командой и одной общей.
	list, added := merge([]UserCommand{{Cmd: "git status"}, {Cmd: "docker ps"}})
	if added != 1 {
		t.Fatalf("второй пульт добавил %d, ожидалась одна новая", added)
	}
	if len(list) != 3 {
		t.Fatalf("в списке %d команд, ожидалось 3: %+v", len(list), list)
	}
}
