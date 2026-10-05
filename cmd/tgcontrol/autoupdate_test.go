package main

import (
	"testing"
	"time"
)

// Регресс: открытое окно панели откладывало рестарт бесконечно — в проде агент
// двое суток работал на v2.24.0, пропустив два релиза. Ожидание теперь
// ограничено maxDeferral.
//
// Второй регресс (аудит 2026-07-26, N168): смотрели ТОЛЬКО на окно панели, а
// обычное состояние агента — окна нет, поэтому обновление перезапускало процесс
// посреди живого сеанса с телефона (удалёнка замирала, ввод последних секунд и
// текущая передача файла терялись). Живые сеансы откладывают рестарт так же,
// как открытое окно.
func TestDeferredRestartReason(t *testing.T) {
	tests := []struct {
		name       string
		windowOpen bool
		live       int
		waited     time.Duration
		wantWait   bool // true = ещё ждём, рестарта нет
	}{
		{"окна нет, сеансов нет — рестарт сразу", false, 0, 0, false},
		{"окно закрыли после ожидания", false, 0, 30 * time.Minute, false},
		{"окно открыто, лимит не вышел", true, 0, 30 * time.Minute, true},
		{"окно открыто, лимит на границе", true, 0, maxDeferral, false},
		{"окно открыто дольше лимита", true, 0, maxDeferral + time.Hour, false},
		{"сеанс с телефона — ждём", false, 1, 30 * time.Minute, true},
		{"два сеанса — ждём", false, 2, time.Minute, true},
		{"сеанс дольше лимита — рестарт с предупреждением", false, 1, maxDeferral, false},
		{"окно и сеансы вместе — ждём", true, 3, time.Hour, true},
		{"сеанс закрылся — рестарт", false, 0, time.Hour, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := deferredRestartReason(tt.windowOpen, tt.live, tt.waited)
			if got := reason == ""; got != tt.wantWait {
				t.Fatalf("deferredRestartReason(%v, %d, %v) = %q; ждём=%v, хотели ждём=%v",
					tt.windowOpen, tt.live, tt.waited, reason, got, tt.wantWait)
			}
		})
	}
}

// Причина рестарта уезжает в лог — она должна называть, кого именно он задел.
func TestWaitCause(t *testing.T) {
	cases := []struct {
		windowOpen  bool
		live        int
		want        string
		wantEnglish string
	}{
		{true, 0, "окно панели открыто", "panel window is open"},
		{false, 2, "живых сеансов: 2", "live sessions: 2"},
		{true, 1, "окно панели открыто, живых сеансов: 1", "panel window open, live sessions: 1"},
	}
	for _, language := range []string{"ru", "en"} {
		t.Run(language, func(t *testing.T) {
			t.Setenv("REMOTAI_LANGUAGE", language)
			for _, c := range cases {
				want := c.want
				if language == "en" {
					want = c.wantEnglish
				}
				if got := waitCause(c.windowOpen, c.live); got != want {
					t.Errorf("waitCause(%v, %d) = %q, хотели %q", c.windowOpen, c.live, got, want)
				}
			}
		})
	}
}

// warnLiveSessions без веб-сервера (nil) не должна падать: автообновление
// живёт и там, где нотификатора нет (service-режим).
func TestWarnLiveSessionsNilSafe(t *testing.T) {
	if got := warnLiveSessions(nil, "9.9.9"); got != 0 {
		t.Fatalf("warnLiveSessions(nil) = %d, хотели 0", got)
	}
}

// Анонс «обновился»: пустой маркер и уже анонсированная версия молчат,
// свежая — уходит на релей (дедуп повторов между перезапусками агента).
func TestShouldAnnounce(t *testing.T) {
	cases := []struct {
		applied, notified string
		want              bool
	}{
		{"", "", false},
		{"2.46.5", "", true},
		{"2.46.5", "2.46.5", false},
		{"2.46.5", "2.46.4", true},
	}
	for _, c := range cases {
		if got := shouldAnnounce(c.applied, c.notified); got != c.want {
			t.Errorf("shouldAnnounce(%q, %q) = %v, хотели %v", c.applied, c.notified, got, c.want)
		}
	}
}
