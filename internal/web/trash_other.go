//go:build !windows && !linux

package web

// На остальных системах (macOS, BSD) своей корзины мы не реализуем: ~/.Trash
// у macOS живёт по другим правилам, а обещать возврат «на всякий случай»
// нельзя. Удаление остаётся прежним, но интерфейс называет его безвозвратным.

func moveToTrash(string) (trashResult, error) {
	return trashResult{}, errTrashUnsupported
}

func forgetTrashEntry(string) {}
