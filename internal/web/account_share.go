package web

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"tgcontrol/internal/agents"
	"tgcontrol/internal/procutil"
)

// Общие знания и настройки между аккаунтами одного агента.
//
// ЗАЧЕМ ЭТО ВООБЩЕ ЕСТЬ. Аккаунт CLI-агента — это каталог целиком, поэтому у
// второй подписки нет ничего: ни скиллов, ни плагинов, ни настроек (замер на
// живой машине: skills 112 МБ, plugins 84 МБ). Человек переключается «на
// вторую подписку», а получает чистого агента — и читает это как поломку.
//
// Приём стандартный (так же делают community-профили Claude Code): вход и
// переписки у каждого аккаунта свои, а знания — общие.
//
// ПАПКУ СВЯЗЫВАЕМ, ФАЙЛ КОПИРУЕМ, и это не вкусовщина:
//   - папка: связь означает «правишь скилл один раз — работает везде»;
//   - файл: и редакторы, и сами CLI пишут настройки через «временный файл +
//     переименование», после чего связь молча рвётся и человек остаётся с
//     копией, которая больше не обновляется. Честнее сразу копия.
//
// ПРАВА: на Windows берём junction (`mklink /J`) — он, в отличие от симлинка,
// НЕ требует прав администратора (проверено на машине владельца: связка на
// ~/.claude/skills создалась под обычным пользователем, все 10 скиллов видны).
// На POSIX обычный симлинк.

// SharedResult — что удалось сделать общим; едет в ответ API, чтобы интерфейс
// сказал человеку правду, а не обещал «всё перенесено».
type SharedResult struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Linked bool   `json:"linked"`          // папка связана
	Copied bool   `json:"copied"`          // файл скопирован
	Error  string `json:"error,omitempty"` // не вышло — говорим прямо
}

// shareAccountResources делает общими знания и настройки нового аккаунта.
//
// mainDir — каталог основного аккаунта (тот, которым человек уже пользуется),
// newDir — каталог заведённого. Отсутствующий ресурс просто пропускается: у
// человека может не быть ни скиллов, ни своей памяти, и это не ошибка.
func shareAccountResources(d *agents.AgentDescriptor, mainDir, newDir string) []SharedResult {
	if d == nil || mainDir == "" || newDir == "" {
		return nil
	}
	out := make([]SharedResult, 0, len(d.AccountShared))
	for _, res := range d.AccountShared {
		src := filepath.Join(mainDir, res.Name)
		info, err := os.Stat(src)
		if err != nil {
			continue // нечего делать общим — это норма, а не отказ
		}
		dst := filepath.Join(newDir, res.Name)
		if _, err := os.Lstat(dst); err == nil {
			continue // уже есть (повторный вызов) — не трогаем
		}
		item := SharedResult{Name: res.Name, Title: res.Title}
		if res.Dir && info.IsDir() {
			if err := linkDir(src, dst); err != nil {
				item.Error = err.Error()
				log.Printf("[ACCOUNTS] link %s -> %s: %v", dst, src, err)
			} else {
				item.Linked = true
			}
		} else if !res.Dir && !info.IsDir() {
			if err := copyFile(src, dst); err != nil {
				item.Error = err.Error()
				log.Printf("[ACCOUNTS] copy %s -> %s: %v", dst, src, err)
			} else {
				item.Copied = true
			}
		} else {
			continue // тип не совпал с ожидаемым — молча мимо
		}
		out = append(out, item)
	}
	return out
}

// linkDir связывает каталог: junction на Windows, симлинк на POSIX.
func linkDir(src, dst string) error {
	if runtime.GOOS != "windows" {
		return os.Symlink(src, dst)
	}
	// Junction создаётся только внешней командой: в стандартной библиотеке Go
	// её нет, а os.Symlink на Windows требует прав администратора или режима
	// разработчика — то есть у обычного человека не сработает.
	cmd := procutil.Hidden(exec.Command("cmd", "/c", "mklink", "/J", dst, src))
	if output, err := cmd.CombinedOutput(); err != nil {
		return errors.New(string(output))
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// copyClaudeMCP переносит MCP-серверы основного аккаунта в новый.
//
// Отдельно от остального, потому что у Claude Code они лежат в `.claude.json`
// — В ОДНОМ ФАЙЛЕ С ВХОДОМ И ИСТОРИЕЙ ПРОЕКТОВ. Связать такой файл нельзя
// (аккаунты затёрли бы друг друга), скопировать целиком — тоже (переехал бы
// чужой вход). Поэтому берём ровно секцию `mcpServers` и кладём её в новый
// файл; всё остальное агент допишет сам при первом запуске.
//
// ГРАБЛЯ, пойманная живым прогоном: у ОСНОВНОГО аккаунта этот файл лежит НЕ в
// каталоге конфига (`~/.claude`), а в корне домашней папки (`~/.claude.json`)
// — историческая асимметрия Claude Code. У аккаунта со своим каталогом он,
// наоборот, внутри. Поэтому источник передаётся ПУТЁМ К ФАЙЛУ, а не папкой:
// на догадке «файл лежит рядом» перенос молча давал ноль серверов.
func copyClaudeMCP(srcFile, newDir string) (int, error) {
	raw, err := os.ReadFile(srcFile)
	if err != nil {
		return 0, nil // нет файла — нечего переносить
	}
	var main map[string]json.RawMessage
	if err := json.Unmarshal(raw, &main); err != nil {
		return 0, err
	}
	servers, ok := main["mcpServers"]
	if !ok {
		return 0, nil
	}
	var list map[string]json.RawMessage
	if err := json.Unmarshal(servers, &list); err != nil || len(list) == 0 {
		return 0, err
	}

	target := filepath.Join(newDir, ".claude.json")
	dst := map[string]json.RawMessage{}
	if existing, err := os.ReadFile(target); err == nil {
		if err := json.Unmarshal(existing, &dst); err != nil {
			dst = map[string]json.RawMessage{}
		}
	}
	if _, already := dst["mcpServers"]; already {
		return 0, nil // повторный вызов ничего не перезаписывает
	}
	dst["mcpServers"] = servers
	data, err := json.MarshalIndent(dst, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return 0, err
	}
	return len(list), nil
}
