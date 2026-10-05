package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Свои команды человека — ОДИН список на компьютер, а не по списку на пульт.
//
// Раньше их было два, и оба лежали в localStorage ПУЛЬТА: ряд под терминалом
// (`ptyQuickCmdsCustom`) и шторка ⚡ (`tg.snippets.custom.v1`). Хранилище своё у
// каждого пульта — у приложения на телефоне, у окна exe на 127.0.0.1, у
// браузера, у вебвью Telegram, — поэтому команда, заведённая с телефона, в окне
// на компьютере просто отсутствовала. И два списка не знали друг о друге: одна
// и та же команда, заведённая «не там», не находилась ни поиском, ни глазами.
//
// Теперь список один и живёт на компьютере — по образцу пресетов
// (`api_presets.go`, `~/.tgcontrol-presets.json`), которые ровно так и работают
// с самого начала. Пульт при первом открытии молча переносит сюда обе свои
// локальные пачки и дальше читает с компьютера.
type UserCommand struct {
	ID  string `json:"id"`
	Cmd string `json:"cmd"`
	// Label — как подписать кнопку; пусто = показывать саму команду.
	Label string `json:"label,omitempty"`
	// Pinned — стоит в ряду под терминалом (иначе живёт только в шторке
	// «Команды»). Это не только место кнопки, но и ПУТЬ ОТПРАВКИ: закреплённая
	// уходит в терминал сразу, потому что ряд держит и текстовые заготовки для
	// агента (решение владельца, 2.46.12), а из шторки команда вставляется в
	// поле ввода и отправляется человеком.
	Pinned bool `json:"pinned"`
	// Sort — ручной порядок; 0 = не задан.
	Sort float64 `json:"sort,omitempty"`
}

type commandsFile struct {
	Commands []UserCommand `json:"commands"`
}

var commandsMu sync.Mutex

func commandsPath() string {
	return filepath.Join(realUserHome(), ".tgcontrol-commands.json")
}

// loadCommands — список из файла, ВСЕГДА в нормальном виде.
//
// Нормализация здесь, а не в вызывающих: список читают четыре ручки, и «а тут
// забыли» — вопрос времени. Битый файл читается как пустой список.
func loadCommands() []UserCommand {
	list, _ := loadCommandsRaw()
	return list
}

// loadCommandsRaw — то же плюс признак «файл на диске отличается от нормального
// вида». По нему ручки чинят файл, не дожидаясь следующей правки от человека.
func loadCommandsRaw() ([]UserCommand, bool) {
	data, err := os.ReadFile(commandsPath())
	if err != nil {
		return nil, false
	}
	var f commandsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, false
	}
	list, changed := normalizeCommands(f.Commands)
	if len(list) == 0 {
		return nil, changed
	}
	return list, changed
}

// normalizeCommands — схлопывает дубли и чинит пустые/повторяющиеся id.
//
// Живая жалоба 09.08.2026: «нажимаю на телефоне крестик — и ничего». В боевом
// файле лежали ДВЕ записи с одним id и одинаковым текстом — `pinned:false` и
// `pinned:true`. Клиент снимает кнопку из ряда правкой по id; ручка правила
// ПЕРВОЕ совпадение, а закреплён был дубль. Кнопка оставалась на месте, ошибки
// не было, тоста не было: ровно «ничего».
//
// Откуда дубль: id выдавался как `c-<time.Now().UnixNano()>`, а часы Windows
// идут ТИКАМИ. Замер на машине владельца: 2000 вызовов `UnixNano()` подряд дали
// РОВНО ОДНО значение (тик ~15 мс). Две записи, заведённые подряд, получали
// общий id — и дальше правка по id била мимо: человек снимает из ряда команду B,
// сервер находит по этому id первую запись A и переписывает ЕЁ текстом B. В
// файле остаются две одинаковые команды с одним id, одна из них закреплена, а
// команда A исчезает совсем.
//
// ПРАВИЛО: id обязан быть уникальным САМ (см. newCommandID), а не потому, что
// «часы же тикают». И файл, однажды испорченный, чиним при чтении: списку
// пользуются четыре ручки, и каждая обязана видеть его в правильном виде.
//
// Ключ схлопывания — сам ТЕКСТ команды: две одинаковые кнопки это промах мимо
// нужной, а не две команды (то же правило у ряда быстрых команд и у импорта).
// `pinned` при схлопывании берём по «хотя бы одна закреплена»: человек видит
// кнопку в ряду, и пропасть без его действия она не должна.
func normalizeCommands(list []UserCommand) ([]UserCommand, bool) {
	out := make([]UserCommand, 0, len(list))
	atCmd := make(map[string]int, len(list))
	usedID := make(map[string]bool, len(list))
	changed := false
	for _, c := range list {
		cmd := strings.TrimSpace(c.Cmd)
		if cmd != c.Cmd {
			changed = true
		}
		c.Cmd = cmd
		if c.Cmd == "" {
			changed = true
			continue
		}
		if i, ok := atCmd[c.Cmd]; ok {
			changed = true
			if c.Pinned {
				out[i].Pinned = true
			}
			if out[i].Label == "" && c.Label != "" {
				out[i].Label = c.Label
			}
			if out[i].Sort == 0 && c.Sort != 0 {
				out[i].Sort = c.Sort
			}
			continue
		}
		if c.ID == "" || usedID[c.ID] {
			c.ID = newCommandID()
			changed = true
		}
		usedID[c.ID] = true
		atCmd[c.Cmd] = len(out)
		out = append(out, c)
	}
	return out, changed
}

// saveCommands — запись ЦЕЛИКОМ или никак: временный файл рядом и переименование.
//
// В этот файл пишет не только этот процесс (см. normalizeCommands), а
// `os.WriteFile` обрезает файл в самом начале — чужое чтение в этот момент
// получало бы обрывок и трактовало его как «команд нет». Переименование в
// пределах одного каталога заменяет содержимое одним шагом.
func saveCommands(list []UserCommand) error {
	list, _ = normalizeCommands(list)
	data, err := json.MarshalIndent(commandsFile{Commands: list}, "", "  ")
	if err != nil {
		return err
	}
	path := commandsPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// GET /api/commands — весь список. Ключ `commands` есть всегда, пусть и пустой:
// по нему клиент отличает нового агента от старого и решает, переносить ли свои
// локальные команды.
func (s *Server) apiCommandsList(w http.ResponseWriter, r *http.Request, uid int64) {
	commandsMu.Lock()
	list, changed := loadCommandsRaw()
	// Испорченный снаружи файл чиним прямо здесь, а не ждём следующей правки от
	// человека: до неё каждая кнопка вела бы себя непредсказуемо. Отказ записи
	// не повод не отдать список — в памяти он уже правильный.
	if changed {
		_ = saveCommands(list)
	}
	commandsMu.Unlock()
	if list == nil {
		list = []UserCommand{}
	}
	jsonResp(w, map[string]any{"commands": list})
}

// POST /api/commands — добавить или изменить. Без id — добавление, с id —
// правка существующей.
//
// Дубликаты схлопываются по САМОЙ КОМАНДЕ: две одинаковые кнопки — это промах
// мимо нужной, а не две команды (то же правило, что у ряда быстрых команд).
func (s *Server) apiCommandsUpsert(w http.ResponseWriter, r *http.Request, uid int64) {
	var req UserCommand
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	req.Cmd = strings.TrimSpace(req.Cmd)
	req.Label = strings.TrimSpace(req.Label)
	if req.Cmd == "" {
		jsonErrorCode(w, 400, "empty_cmd", "команда пустая", nil)
		return
	}

	commandsMu.Lock()
	defer commandsMu.Unlock()
	list := loadCommands()
	if req.ID != "" {
		for i := range list {
			if list[i].ID == req.ID {
				list[i].Cmd, list[i].Label, list[i].Pinned = req.Cmd, req.Label, req.Pinned
				if req.Sort != 0 {
					list[i].Sort = req.Sort
				}
				if err := saveCommands(list); err != nil {
					jsonError(w, err.Error(), 500)
					return
				}
				jsonResp(w, map[string]any{"commands": list, "command": list[i]})
				return
			}
		}
	}
	for i := range list {
		if list[i].Cmd == req.Cmd {
			// Такая команда уже есть — не плодим вторую, а применяем то, что
			// человек попросил сейчас (например «закрепить в ряду»).
			list[i].Pinned = req.Pinned
			if req.Label != "" {
				list[i].Label = req.Label
			}
			if err := saveCommands(list); err != nil {
				jsonError(w, err.Error(), 500)
				return
			}
			jsonResp(w, map[string]any{"commands": list, "command": list[i], "existed": true})
			return
		}
	}
	req.ID = newCommandID()
	list = append(list, req)
	if err := saveCommands(list); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResp(w, map[string]any{"commands": list, "command": req})
}

// DELETE /api/commands?id=… — удалить.
func (s *Server) apiCommandsDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.URL.Query().Get("id")
	if id == "" {
		jsonErrorCode(w, 400, "bad_request", "нужен id", nil)
		return
	}
	commandsMu.Lock()
	defer commandsMu.Unlock()
	list := loadCommands()
	out := make([]UserCommand, 0, len(list))
	for _, c := range list {
		if c.ID != id {
			out = append(out, c)
		}
	}
	if err := saveCommands(out); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResp(w, map[string]any{"commands": out})
}

// POST /api/commands/import — разовый перенос своих команд с пульта.
//
// Пульт присылает то, что накопилось у него в localStorage; сервер добавляет
// только НОВОЕ (по тексту команды). Повторный вызов безопасен — это важно,
// потому что пультов несколько и каждый принесёт своё в своё время.
func (s *Server) apiCommandsImport(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		Commands []UserCommand `json:"commands"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	commandsMu.Lock()
	defer commandsMu.Unlock()
	list := loadCommands()
	have := make(map[string]bool, len(list))
	for _, c := range list {
		have[c.Cmd] = true
	}
	added := 0
	for _, c := range req.Commands {
		c.Cmd = strings.TrimSpace(c.Cmd)
		c.Label = strings.TrimSpace(c.Label)
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
			jsonError(w, err.Error(), 500)
			return
		}
	}
	if list == nil {
		list = []UserCommand{}
	}
	jsonResp(w, map[string]any{"commands": list, "added": added})
}

// newCommandID — время плюс счётчик процесса.
//
// Одного времени мало: на Windows системные часы идут ТИКАМИ, и два вызова
// внутри тика возвращают одинаковое значение. Записи с общим id ломают правку
// по id молча (см. normalizeCommands), поэтому уникальность обеспечиваем сами,
// а не полагаемся на разрешение часов.
var commandIDSeq atomic.Uint64

func newCommandID() string {
	return fmt.Sprintf("c-%d-%d", time.Now().UnixNano(), commandIDSeq.Add(1))
}
