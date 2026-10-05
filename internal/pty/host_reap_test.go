package pty

import (
	"os"
	"testing"
)

// Разбор командной строки — единственное, что отделяет «снять осиротевший
// хост» от «убить чужую программу», поэтому проверяем оба платформенных
// формата и отказы.
func TestHostIDFromCmdline(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		want    string
	}{
		{
			name:    "windows: аргументы в кавычках",
			cmdline: `"C:\Users\u\AppData\Local\Programs\Remotai\remotai.exe" "--pty-host" "--id" "8dbf3fd97686dd7f" "--cwd" "C:\work" "--shell" "powershell.exe" "--cols" "80" "--rows" "24"`,
			want:    "8dbf3fd97686dd7f",
		},
		{
			name:    "linux: аргументы через пробел",
			cmdline: "/usr/local/bin/remotai --pty-host --id 4f3b940960239cbe --cwd /srv --shell /bin/bash --cols 80 --rows 24",
			want:    "4f3b940960239cbe",
		},
		{
			name:    "верхний регистр приводится к нижнему",
			cmdline: "remotai --pty-host --id 4AFFB99A0379A29D",
			want:    "4affb99a0379a29d",
		},
		{
			name:    "сам агент — не хост",
			cmdline: `"C:\Users\u\AppData\Local\Programs\Remotai\remotai.exe" --update-handoff`,
			want:    "",
		},
		{
			name:    "чужая программа со словом id в пути",
			cmdline: `C:\tools\videoid\player.exe --id 1234567890abcdef`,
			want:    "",
		},
		{
			name:    "хост без --id (сломанный запуск) не считается",
			cmdline: "remotai --pty-host --cwd /srv",
			want:    "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hostIDFromCmdline(c.cmdline); got != c.want {
				t.Fatalf("hostIDFromCmdline(%q) = %q, ожидалось %q", c.cmdline, got, c.want)
			}
		})
	}
}

// killHostProcess не должен убивать процесс, если по этому PID уже живёт
// что-то другое: PID переиспользуются, а Close зовёт добивание всегда.
func TestKillHostProcessIgnoresForeignPID(t *testing.T) {
	// PID 0 и пустой id — быстрые отказы без обращения к системе.
	if killHostProcess("", 1234) {
		t.Fatal("пустой id не должен приводить к убийству")
	}
	if killHostProcess("8dbf3fd97686dd7f", 0) {
		t.Fatal("нулевой PID не должен приводить к убийству")
	}
	// PID 4 (System на Windows) существует, но это не наш pty-host: командная
	// строка не совпадёт, и функция обязана отказаться.
	if killHostProcess("8dbf3fd97686dd7f", 4) {
		t.Fatal("процесс с чужой командной строкой снимать нельзя")
	}
}

// Самая дорогая ошибка уборки — не узнать СВОЙ хост. Агент обновляется
// переименованием занятого файла (update.Apply), поэтому у долгоживущих
// хостов путь к образу оканчивается на «.old-<ms>» (Windows) или
// « (deleted)» (Linux). Замер 02.08.2026: 3 из 7 живых хостов владельца были
// именно такими — буквальное сравнение путей пропускало бы ровно тех сирот,
// ради которых уборка написана.
func TestSameAgentExecutableAcceptsUpdatedPaths(t *testing.T) {
	self := `C:\Users\u\AppData\Local\Programs\Remotai\remotai.exe`
	same := []string{
		`C:\Users\u\AppData\Local\Programs\Remotai\remotai.exe`,
		`C:\Users\u\AppData\Local\Programs\Remotai\remotai.exe.old-1785337946174`,
		`c:/users/u/appdata/local/programs/remotai/remotai.exe`,
	}
	for _, exe := range same {
		if !sameAgentExecutable(exe, self) {
			t.Fatalf("свой хост не опознан: %q", exe)
		}
	}
	other := []string{
		`C:\Users\u\Desktop\dev\remotai.exe`,               // другой каталог — dev-runtime
		`C:\Users\u\AppData\Local\Programs\Remotai\qq.exe`, // другое имя
		``,
	}
	for _, exe := range other {
		if sameAgentExecutable(exe, self) {
			t.Fatalf("чужой процесс принят за свой: %q", exe)
		}
	}

	unix := "/usr/local/bin/remotai"
	if !sameAgentExecutable("/usr/local/bin/remotai (deleted)", unix) {
		t.Fatal("обновлённый на месте бинарь не опознан")
	}
	if sameAgentExecutable("/opt/other/remotai", unix) {
		t.Fatal("чужой каталог принят за свой")
	}
}

// Битое хранилище не должно выглядеть как «терминалов нет»: на этом различии
// висит уборка сирот, а её ошибка стоит живых терминалов человека.
func TestMetaStoreLoadedFlag(t *testing.T) {
	dir := t.TempDir()

	fresh := NewMetaStoreAt(dir + "/absent.json")
	if !fresh.Loaded() {
		t.Fatal("отсутствующий файл — это чистый профиль, а не сбой чтения")
	}

	good := dir + "/good.json"
	if err := os.WriteFile(good, []byte(`{"a1b2c3d4e5f60718":{"name":"Сборка"}}`), 0600); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	if !NewMetaStoreAt(good).Loaded() {
		t.Fatal("исправный файл должен считаться прочитанным")
	}

	broken := dir + "/broken.json"
	if err := os.WriteFile(broken, []byte(`{"a1b2":{`), 0600); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	s := NewMetaStoreAt(broken)
	if s.Loaded() {
		t.Fatal("повреждённый файл не должен считаться прочитанным — иначе уборка снесёт живые терминалы")
	}
	if len(s.KnownIDs()) != 0 {
		t.Fatal("из повреждённого файла не должно взяться записей")
	}
}

// KnownIDs — вход уборки сирот: служебная запись папок не должна выглядеть как
// живой терминал, иначе хост с таким id (его не бывает) никогда не убрать.
func TestKnownIDsSkipsFoldersEntry(t *testing.T) {
	s := NewMetaStoreAt(t.TempDir() + "/pty.json")
	if err := s.SetName("A1B2C3D4E5F60718", "Сборка"); err != nil {
		t.Fatalf("SetName: %v", err)
	}
	if err := s.SetFolders([]string{"Работа"}); err != nil {
		t.Fatalf("SetFolders: %v", err)
	}
	ids := s.KnownIDs()
	if _, ok := ids[metaFoldersKey]; ok {
		t.Fatal("служебная запись папок попала в список известных терминалов")
	}
	if _, ok := ids["a1b2c3d4e5f60718"]; !ok {
		t.Fatalf("терминал не попал в список известных: %v", ids)
	}
}
