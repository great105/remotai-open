package web

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Воспроизведение живой жалобы 09.08.2026: «нажимаю на телефоне крестик — и
// ничего». В боевом `~/.tgcontrol-commands.json` нашлись ДВЕ записи с ОДНИМ id
// и одинаковым текстом: одна `pinned:false`, другая `pinned:true`.
//
// Клиент снимает кнопку из ряда через upsert `pinned:false` по id той записи,
// которую он видит закреплённой. Сервер ищет по id ПЕРВОЕ совпадение, правит
// его и выходит — а закреплён-то дубль. Кнопка остаётся в ряду, ошибки нет,
// тоста нет: ровно «ничего».
func TestUpsertWithDuplicateIDsUnpinsAll(t *testing.T) {
	withTempHome(t)
	commandsMu.Lock()
	dup := []UserCommand{
		{ID: "c-1", Cmd: "Проверь обращения", Pinned: false},
		{ID: "c-1", Cmd: "Проверь обращения", Pinned: true},
	}
	if err := saveCommands(dup); err != nil {
		commandsMu.Unlock()
		t.Fatal(err)
	}
	commandsMu.Unlock()

	s := &Server{}
	rec := httptest.NewRecorder()
	body := `{"id":"c-1","cmd":"Проверь обращения","pinned":false}`
	s.apiCommandsUpsert(rec, httptest.NewRequest("POST", "/api/commands", strings.NewReader(body)), 1)
	if rec.Code != 200 {
		t.Fatalf("код ответа %d, тело %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Commands []UserCommand `json:"commands"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Commands {
		if c.Pinned {
			t.Fatalf("после снятия из ряда команда всё ещё закреплена: %+v (весь список %+v)", c, resp.Commands)
		}
	}
	if len(resp.Commands) != 1 {
		t.Fatalf("дубликат остался в списке: %+v", resp.Commands)
	}
}

// Испорченный снаружи файл чинится при первом же чтении — иначе до следующей
// правки от человека каждая кнопка ведёт себя непредсказуемо.
func TestListHealsDuplicatesOnDisk(t *testing.T) {
	withTempHome(t)
	commandsMu.Lock()
	broken := commandsFile{Commands: []UserCommand{
		{ID: "c-1", Cmd: "ls", Pinned: false},
		{ID: "c-1", Cmd: "ls", Pinned: true},
		{ID: "c-1", Cmd: "pwd", Label: "Где я", Pinned: true},
		{ID: "", Cmd: "  git status  "},
		{ID: "c-9", Cmd: "   "},
	}}
	data, err := json.MarshalIndent(broken, "", "  ")
	if err != nil {
		commandsMu.Unlock()
		t.Fatal(err)
	}
	if err := os.WriteFile(commandsPath(), data, 0o600); err != nil {
		commandsMu.Unlock()
		t.Fatal(err)
	}
	commandsMu.Unlock()

	s := &Server{}
	rec := httptest.NewRecorder()
	s.apiCommandsList(rec, httptest.NewRequest("GET", "/api/commands", nil), 1)

	var resp struct {
		Commands []UserCommand `json:"commands"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Commands) != 3 {
		t.Fatalf("ожидались ls, pwd, git status — пришло %+v", resp.Commands)
	}
	// Закреплённость переживает схлопывание: кнопка стоит в ряду, и пропадать
	// без действия человека ей нельзя.
	if !resp.Commands[0].Pinned || resp.Commands[0].Cmd != "ls" {
		t.Fatalf("первая запись не та: %+v", resp.Commands[0])
	}
	if resp.Commands[1].Cmd != "pwd" || resp.Commands[1].ID == resp.Commands[0].ID {
		t.Fatalf("разным командам достался общий id: %+v", resp.Commands)
	}
	if resp.Commands[2].Cmd != "git status" || resp.Commands[2].ID == "" {
		t.Fatalf("команда без id не починена: %+v", resp.Commands[2])
	}

	// Починка именно на ДИСКЕ, а не только в ответе: следующий запрос придёт от
	// другого пульта и прочитает файл заново.
	commandsMu.Lock()
	onDisk, changed := loadCommandsRaw()
	commandsMu.Unlock()
	if changed {
		t.Fatalf("файл на диске остался испорченным: %+v", onDisk)
	}
	if len(onDisk) != 3 {
		t.Fatalf("на диске %d записей: %+v", len(onDisk), onDisk)
	}
}

// Часы на Windows идут тиками, и два вызова внутри тика раньше давали один id.
// Записи с общим id ломают правку по id молча — уникальность обеспечиваем сами.
func TestNewCommandIDIsUniqueInSameTick(t *testing.T) {
	seen := make(map[string]bool, 2000)
	for i := 0; i < 2000; i++ {
		id := newCommandID()
		if seen[id] {
			t.Fatalf("id %s выдан дважды на %d-м вызове", id, i)
		}
		seen[id] = true
	}
}

// Перенос с пульта — через ЖИВУЮ ручку. Прежний тест повторял её логику своей
// копией: такая проверка проходит и на сломанном коде, то есть декоративна.
func TestImportHandlerIsIdempotent(t *testing.T) {
	withTempHome(t)
	s := &Server{}
	post := func(body string) []UserCommand {
		t.Helper()
		rec := httptest.NewRecorder()
		s.apiCommandsImport(rec, httptest.NewRequest("POST", "/api/commands/import", strings.NewReader(body)), 1)
		if rec.Code != 200 {
			t.Fatalf("код %d, тело %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Commands []UserCommand `json:"commands"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Commands
	}
	body := `{"commands":[{"cmd":"bash deploy.sh","pinned":true},{"cmd":"git status"}]}`
	if got := post(body); len(got) != 2 {
		t.Fatalf("первый перенос дал %+v", got)
	}
	if got := post(body); len(got) != 2 {
		t.Fatalf("повторный перенос наплодил дубли: %+v", got)
	}
	// Второй пульт: одна общая команда и одна своя.
	got := post(`{"commands":[{"cmd":"git status"},{"cmd":"docker ps"}]}`)
	if len(got) != 3 {
		t.Fatalf("после второго пульта %d записей: %+v", len(got), got)
	}
}
