//go:build windows

package web

import (
	"testing"
	"time"
)

// Клавиша PrtScr обязана ЗАНИМАТЬСЯ и ОТПУСКАТЬСЯ на ходу: тумблер в настройках
// не должен требовать перезапуска агента, а выключенный тумблер не должен
// держать чужую клавишу.
//
// ⚠ Нажатие НЕ эмулируем. `internal/input` умеет SendInput, но подсунуть PrtScr
// в живой сеанс владельца — значит дёрнуть его рабочий стол (а при занятой
// клавише ещё и открыть чужие «Ножницы» поверх его работы). Здесь проверяется
// то, что можно проверить без вмешательства: регистрация, освобождение и
// ЧЕСТНЫЙ отчёт о занятой клавише. Само нажатие проверяет человек — один раз.
func TestScreenshotHotkeyRegistersAndReleases(t *testing.T) {
	if active, _ := screenshotHotkeyActive(), screenshotHotkeyError(); active {
		t.Fatal("клавиша занята ещё до включения — тест не изолирован")
	}

	setScreenshotHotkey(true)
	// Регистрация уходит в отдельный поток ОС: ждём результат, а не спим наугад.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if screenshotHotkeyActive() || screenshotHotkeyError() != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	active, reason := screenshotHotkeyActive(), screenshotHotkeyError()
	switch {
	case active:
		if reason != "" {
			t.Fatalf("клавиша занята нами, но осталась причина отказа %q", reason)
		}
	case reason == "hotkey_taken":
		// Нормальный исход: PrtScr уже забрали системные «Ножницы» или сторонняя
		// программа. Главное — что мы об этом СКАЗАЛИ, а не молчим.
		t.Log("PrtScr занята другим приложением — проверен честный отказ hotkey_taken")
		return
	default:
		t.Fatalf("клавиша не занята и причина непонятна: active=%v reason=%q", active, reason)
	}

	// Повторное включение не должно поднимать второй поток и вторую регистрацию.
	setScreenshotHotkey(true)
	time.Sleep(100 * time.Millisecond)
	if !screenshotHotkeyActive() {
		t.Fatal("повторное включение сбросило уже занятую клавишу")
	}

	setScreenshotHotkey(false)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !screenshotHotkeyActive() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if screenshotHotkeyActive() {
		t.Fatal("выключенный тумблер продолжает держать PrtScr")
	}
}
