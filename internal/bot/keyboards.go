package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol/internal/agents"
)

// ── Reply keyboard ───────────────────────────────────────────────────

func (tb *Bot) buildMainKeyboard(uid int64) *models.ReplyKeyboardMarkup {
	sessList := tb.store.List(int(uid))
	activeName := tb.store.GetActiveName(int(uid))

	var rows [][]models.KeyboardButton

	if len(sessList) > 0 {
		var row []models.KeyboardButton
		count := 0
		for _, sess := range sessList {
			if count >= maxKBSessions {
				break
			}
			prefix := sessionPrefix
			if sess.Name == activeName {
				prefix = sessionActivePrefix
			}
			row = append(row, models.KeyboardButton{Text: prefix + sess.Name})
			if len(row) == 2 {
				rows = append(rows, row)
				row = nil
			}
			count++
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}

	rows = append(rows, []models.KeyboardButton{
		{Text: "➕ Новая"},
		{Text: "📋 Сессии"},
		{Text: "📁 Файлы"},
		{Text: "📊 Статус"},
	})

	return &models.ReplyKeyboardMarkup{
		Keyboard:       rows,
		ResizeKeyboard: true,
	}
}

func (tb *Bot) sendKB(ctx context.Context, chatID, uid int64, text string, threadID ...int) {
	kb := tb.buildMainKeyboard(uid)
	tid := 0
	if len(threadID) > 0 {
		tid = threadID[0]
	}
	tb.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: tid,
		Text:            text,
		ParseMode:       models.ParseModeHTML,
		ReplyMarkup:     kb,
	})
}

// ── Inline keyboards ─────────────────────────────────────────────────

func postResponseKB() *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "▶ Продолжи", CallbackData: "qr:cont"},
				{Text: "🔄 Повтори", CallbackData: "qr:retry"},
			},
			{
				{Text: "📦 Compact", CallbackData: "qr:compact"},
				{Text: "⚙ Настройки", CallbackData: "cf:back"},
				{Text: "📋 Сессии", CallbackData: "se:ls"},
			},
		},
	}
}

func stopKB() *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "🛑 Стоп", CallbackData: "xa:stop"},
		}},
	}
}

func agentPickerKB(prefix string) *models.InlineKeyboardMarkup {
	// Та же асимметрия, что и в приложении: «нет» — снимок момента, и агент,
	// пропавший на секунды своего самообновления, иначе исчезал бы из меню бота
	// до перезапуска Remotai.
	agents.RefreshMissing()
	detected := agents.GetDetected()
	var rows [][]models.InlineKeyboardButton

	// Primary agents first
	var primary []models.InlineKeyboardButton
	for _, id := range []string{"claude", "codex", "shell"} {
		if agents.IsAvailable(id) {
			desc := agents.GetDescriptor(id)
			primary = append(primary, models.InlineKeyboardButton{
				Text: fmt.Sprintf("%s %s", desc.Icon, desc.Name), CallbackData: prefix + id,
			})
		}
	}
	if len(primary) > 0 {
		rows = append(rows, primary)
	}

	// Other agents
	var others []models.InlineKeyboardButton
	for _, d := range detected {
		if d.ID == "claude" || d.ID == "codex" || d.ID == "shell" {
			continue
		}
		others = append(others, models.InlineKeyboardButton{
			Text: fmt.Sprintf("%s %s", d.Icon, d.Name), CallbackData: prefix + d.ID,
		})
		if len(others) == 3 {
			rows = append(rows, others)
			others = nil
		}
	}
	if len(others) > 0 {
		rows = append(rows, others)
	}

	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// ── Folder browser ───────────────────────────────────────────────────

func listSubdirs(path string) []string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
			continue
		}
		lower := strings.ToLower(name)
		if lower == "system volume information" || lower == "recovery" {
			continue
		}
		dirs = append(dirs, filepath.Join(path, name))
	}
	sort.Strings(dirs)
	return dirs
}

func (tb *Bot) buildFolderBrowser(uid int64, path string, page int) (string, *models.InlineKeyboardMarkup) {
	subdirs := listSubdirs(path)
	total := len(subdirs)
	totalPages := max(1, (total+dirsPerPage-1)/dirsPerPage)
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	curPID := tb.pid(uid, path)

	start := page * dirsPerPage
	end := start + dirsPerPage
	if end > total {
		end = total
	}
	pageDirs := subdirs[start:end]

	var rows [][]models.InlineKeyboardButton
	var row []models.InlineKeyboardButton
	for _, d := range pageDirs {
		name := filepath.Base(d)
		idx := tb.pid(uid, d)
		row = append(row, models.InlineKeyboardButton{
			Text: "📁 " + name, CallbackData: fmt.Sprintf("fb:g:%d", idx),
		})
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}

	// Pagination
	if totalPages > 1 {
		var pagRow []models.InlineKeyboardButton
		if page > 0 {
			pagRow = append(pagRow, models.InlineKeyboardButton{
				Text: "◀", CallbackData: fmt.Sprintf("fb:p:%d:%d", curPID, page-1),
			})
		}
		pagRow = append(pagRow, models.InlineKeyboardButton{
			Text: fmt.Sprintf("%d/%d", page+1, totalPages), CallbackData: "xa:noop",
		})
		if page < totalPages-1 {
			pagRow = append(pagRow, models.InlineKeyboardButton{
				Text: "▶", CallbackData: fmt.Sprintf("fb:p:%d:%d", curPID, page+1),
			})
		}
		rows = append(rows, pagRow)
	}

	// Parent
	parent := filepath.Dir(path)
	if parent != path {
		parentIdx := tb.pid(uid, parent)
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "⬆ ..", CallbackData: fmt.Sprintf("fb:g:%d", parentIdx)},
		})
	}

	// Select
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "✅ Выбрать эту", CallbackData: fmt.Sprintf("fb:s:%d", curPID)},
	})

	text := fmt.Sprintf("📂 <b>Выбор папки</b>\nТекущая: <code>%s</code>", esc(path))
	return text, &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// ── File manager ─────────────────────────────────────────────────────

func listDirContents(path string) (dirs []string, files []string) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
			continue
		}
		lower := strings.ToLower(name)
		if lower == "system volume information" || lower == "recovery" {
			continue
		}
		full := filepath.Join(path, name)
		if e.IsDir() {
			dirs = append(dirs, full)
		} else {
			files = append(files, full)
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	return
}

func humanSize(size int64) string {
	f := float64(size)
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if f < 1024 {
			if unit == "B" {
				return fmt.Sprintf("%.0f%s", f, unit)
			}
			return fmt.Sprintf("%.1f%s", f, unit)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1fTB", f)
}

func (tb *Bot) buildFileManager(uid int64, path string, page int) (string, *models.InlineKeyboardMarkup) {
	dirs, files := listDirContents(path)
	items := append(dirs, files...)
	total := len(items)
	totalPages := max(1, (total+filesPerPage-1)/filesPerPage)
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	curPID := tb.pid(uid, path)

	start := page * filesPerPage
	end := start + filesPerPage
	if end > total {
		end = total
	}
	pageItems := items[start:end]

	var rows [][]models.InlineKeyboardButton
	dirSet := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		dirSet[d] = true
	}

	for _, itemPath := range pageItems {
		idx := tb.pid(uid, itemPath)
		name := filepath.Base(itemPath)
		if len(name) > 22 {
			name = name[:19] + "..."
		}

		if dirSet[itemPath] {
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: "📁 " + name, CallbackData: fmt.Sprintf("fm:g:%d", idx)},
			})
		} else {
			size := "?"
			if info, err := os.Stat(itemPath); err == nil {
				size = humanSize(info.Size())
			}
			rows = append(rows, []models.InlineKeyboardButton{
				{Text: fmt.Sprintf("📄 %s (%s)", name, size), CallbackData: fmt.Sprintf("fm:d:%d", idx)},
				{Text: "🗑", CallbackData: fmt.Sprintf("fm:r:%d:%d", idx, curPID)},
			})
		}
	}

	// Pagination
	if totalPages > 1 {
		var pagRow []models.InlineKeyboardButton
		if page > 0 {
			pagRow = append(pagRow, models.InlineKeyboardButton{
				Text: "◀", CallbackData: fmt.Sprintf("fm:p:%d:%d", curPID, page-1),
			})
		}
		pagRow = append(pagRow, models.InlineKeyboardButton{
			Text: fmt.Sprintf("%d/%d", page+1, totalPages), CallbackData: "xa:noop",
		})
		if page < totalPages-1 {
			pagRow = append(pagRow, models.InlineKeyboardButton{
				Text: "▶", CallbackData: fmt.Sprintf("fm:p:%d:%d", curPID, page+1),
			})
		}
		rows = append(rows, pagRow)
	}

	// Parent dir
	parentDir := filepath.Dir(path)
	if parentDir != path {
		parentIdx := tb.pid(uid, parentDir)
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "⬆ ..", CallbackData: fmt.Sprintf("fm:g:%d", parentIdx)},
		})
	}

	// Bottom bar
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "📎 Загрузить", CallbackData: fmt.Sprintf("fm:u:%d", curPID)},
		{Text: "✖ Закрыть", CallbackData: "fm:x"},
	})

	text := fmt.Sprintf("📂 <b>Файлы</b> — <code>%s</code>", esc(path))
	return text, &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}
