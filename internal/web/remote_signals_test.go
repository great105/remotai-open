package web

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"tgcontrol/internal/input"
)

// Заблокированный рабочий стол отбивает КАЖДОЕ событие ввода (до 120 движений в
// секунду). Клиенту нужна одна стойкая плашка: первое предупреждение — сразу,
// повтор — не чаще inputWarnEvery, а первая удачная инъекция её гасит.
func TestReportInputThrottlesAndClears(t *testing.T) {
	var got []string
	a := &inputApplier{notify: func(v map[string]any) { got = append(got, v["code"].(string)) }}

	a.reportInput(input.ErrInputBlocked)
	a.reportInput(input.ErrInputBlocked)
	a.reportInput(input.ErrInputBlocked)
	if want := []string{"input_blocked"}; !equalCodes(got, want) {
		t.Fatalf("ожидали %v, получили %v", want, got)
	}

	// Прошло больше окна — напоминаем (для тех, кто подключился позже).
	a.warnAt = time.Now().Add(-inputWarnEvery - time.Second)
	a.reportInput(fmt.Errorf("клик мимо: %w", input.ErrInputBlocked))
	if want := []string{"input_blocked", "input_blocked"}; !equalCodes(got, want) {
		t.Fatalf("ожидали %v, получили %v", want, got)
	}

	// Ввод снова доходит → снимаем плашку ровно один раз.
	a.reportInput(nil)
	a.reportInput(nil)
	if want := []string{"input_blocked", "input_blocked", "input_ok"}; !equalCodes(got, want) {
		t.Fatalf("ожидали %v, получили %v", want, got)
	}
}

// Нет xdotool / нет X — это другая беда с другим лечением, и говорить «компьютер
// заблокирован» здесь нельзя. Прочие ошибки (неизвестная клавиша) клиента не
// касаются вовсе.
func TestReportInputDistinguishesCauses(t *testing.T) {
	var got []string
	a := &inputApplier{notify: func(v map[string]any) { got = append(got, v["code"].(string)) }}

	a.reportInput(fmt.Errorf("xdotool keydown: %w", input.ErrInputUnavailable))
	a.reportInput(errors.New("xdotool key F25: No such key name"))
	if want := []string{"input_unavailable"}; !equalCodes(got, want) {
		t.Fatalf("ожидали %v, получили %v", want, got)
	}
}

func equalCodes(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Единичный сбой захвата экрана самолечится за кадр-другой — молчим. Серия
// длиннее captureFailGrace означает «кадров не будет», и об этом надо сказать,
// иначе клиент вечно стоит на «Жду первый кадр». Повтор — редкий.
func TestCaptureFailStreakReportsAfterGrace(t *testing.T) {
	var c captureFailStreak
	start := time.Now()

	if c.fail(start) {
		t.Fatal("первый сбой не повод пугать человека")
	}
	if c.fail(start.Add(captureFailGrace - 100*time.Millisecond)) {
		t.Fatal("сообщили раньше окна ожидания")
	}
	if !c.fail(start.Add(captureFailGrace + 10*time.Millisecond)) {
		t.Fatal("серия сбоев длиннее окна обязана дойти до клиента")
	}
	if c.fail(start.Add(captureFailGrace + time.Second)) {
		t.Fatal("повтор пришёл раньше captureFailRepeat")
	}
	if !c.fail(start.Add(captureFailGrace + captureFailRepeat + time.Second)) {
		t.Fatal("напоминание после captureFailRepeat не пришло")
	}

	// Захват ожил → серия начинается заново (следом полетят кадры, и клиент сам
	// снимет плашку).
	c.ok()
	if c.fail(start.Add(time.Hour)) {
		t.Fatal("после успешного кадра серия не обнулилась")
	}
}
